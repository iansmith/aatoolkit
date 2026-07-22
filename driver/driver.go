// Package driver is the compiled engine an agent runs on: the LLM HTTP client
// (Host, a host.Host implementation), TTS synthesis + playback, the serial speech
// queue, and health/Twilio HTTP servers. An
// agent supplies its particulars — tiers, TTS transport, and a system-prompt
// provider — through Config and stands the driver up with New. It bakes in no
// agent identity: prompts and policy come from the caller.
package driver

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/iansmith/aatoolkit/telephony"
)

// ---------------------------------------------------------------------------
// driver: LLM transport. One entry per tier; tier -> {url, model}. Two separate
// llama-server instances, no proxy. The policy only ever names a tier.
// ---------------------------------------------------------------------------

// Tier describes one tier's llama-server. reasoning=false asks the model to
// skip its thinking trace; maxTokens caps a single turn so a runaway generation
// can't burn the whole 300s timeout.
type Tier struct {
	URL, Model string
	Reasoning  bool
	MaxTokens  int
}

// Host is the driver's concrete host.Host: it turns a (messages, tier) pair
// into an HTTP call against the tier's llama-server.
type Host struct {
	tiers       map[string]Tier
	client      *http.Client
	prompt      func() string // agent-supplied system-prompt provider (see Config.Prompt)
	tts         TTSConfig
	speech      *speechQueue // serial TTS worker so an ack and the answer don't overlap
	history     []message
	userContext func() string // optional user context block
	histMu      sync.Mutex
}

// Config is what an agent supplies to stand up a driver: its LLM tiers, TTS
// transport, and a system-prompt provider. The engine holds none of this by
// default — a different agent (with its own prompt, tiers, policy) constructs its
// own Config. Prompt is called every turn; use FilePrompt for a hot-reloading file
// or supply any func() string (embed, remote, constant).
type Config struct {
	Tiers       map[string]Tier
	TTS         TTSConfig
	Prompt      func() string
	UserContext func() string // optional user context block injected after system prompt
}

// New builds a driver Host from cfg, wiring the serial speech queue internally.
func New(cfg Config) *Host {
	h := &Host{
		tiers:       cfg.Tiers,
		client:      &http.Client{},
		prompt:      cfg.Prompt,
		tts:         cfg.TTS,
		userContext: cfg.UserContext,
	}
	h.speech = newSpeechQueue(func(text []byte, voice string, speed float64) {
		if err := h.synthesizeAndPlay(string(text), voice, speed); err != nil {
			fmt.Fprintf(os.Stderr, "voice: %v\n", err)
		}
	})
	return h
}

// message is one turn of the conversation history.
type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// TTSConfig is the driver's fixed TTS transport config for Supertonic's native
// /v1/tts endpoint (the OpenAI-compatible /v1/audio/speech has no speed knob).
// The tunable per-call settings — voice and speed — are owned by the interpreted
// policy and passed into Speak, so /voice and /speed change them live.
type TTSConfig struct {
	URL    string // .../v1/tts
	Lang   string // ISO code, e.g. "en"
	Format string // wav / flac / ogg — wav plays via afplay
}

const minSegmentChars = 30 // minimum buffer length before a punctuation boundary triggers a TTS flush

// Send implements host.Host. contextWindow is the already serialized `messages`
// array; tier is "fast" | "deep". Returns content, reasoning (empty if the
// model/flags don't emit it), error. It's SendStream without the per-segment
// callback — the full assembled text is all the caller wants.
func (h *Host) Send(contextWindow []byte, tier string) ([]byte, []byte, error) {
	return h.SendStream(contextWindow, tier, func(string) {})
}

// SystemPrompt implements host.Host: the current system prompt, reloaded from
// its file only when the file changes.
func (h *Host) SystemPrompt() string { return h.prompt() }

// SendStream is like Send but streams the response, calling onSegment each
// time a punctuation boundary is reached with enough accumulated text. The
// returned slices are the fully assembled (content, reasoning). Segments are
// flushed when the buffer crosses ~30 chars and a delimiter (. ? ! , \n) is
// seen; the delimiter is kept at the end of the segment so the TTS has the
// punctuation it needs for natural cadence.
func (h *Host) SendStream(contextWindow []byte, tier string, onSegment func(string)) ([]byte, []byte, error) {
	ep, ok := h.tiers[tier]
	if !ok {
		return nil, nil, fmt.Errorf("unknown tier %q (have: fast, deep)", tier)
	}
	payload := map[string]any{
		"model":             ep.Model,
		"messages":          json.RawMessage(contextWindow),
		"temperature":       0.7,
		"stream":            true,
		"max_tokens":        ep.MaxTokens,
		"frequency_penalty": 0.6,
		"presence_penalty":  0.3,
	}
	if !ep.Reasoning {
		payload["chat_template_kwargs"] = map[string]any{"enable_thinking": false}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", ep.URL, bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.client.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("calling %s: %w", ep.URL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 301))
		return nil, nil, fmt.Errorf("llm %s: status %d (%.300s)", ep.URL, resp.StatusCode, body)
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 256*1024)
	var contentBuf strings.Builder
	var reasoningBuf strings.Builder
	var segBuf strings.Builder
	var hasContent bool

	// flushSegment sends accumulated text to onSegment if it exceeds the
	// minimum length (30 chars). Mid-stream flushes respect the threshold to
	// avoid chatty TTS calls, but the end-of-stream flush (force) sends
	// whatever's left to avoid dropping the final phrase.
	flushSegment := func(force bool) {
		s := segBuf.String()
		segBuf.Reset()
		if strings.TrimSpace(s) == "" {
			return // don't send pure whitespace to TTS
		}
		if force || len(s) >= minSegmentChars {
			onSegment(s)
		}
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		sseData := line[6:]
		if sseData == "[DONE]" {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
					Reasoning        string `json:"reasoning"`
				} `json:"delta"`
			} `json:"choices"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(sseData), &chunk); err != nil {
			return nil, nil, fmt.Errorf("decoding SSE chunk: %w", err)
		}
		if chunk.Error != nil {
			return nil, nil, fmt.Errorf("llm error: %s", chunk.Error.Message)
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		d := chunk.Choices[0].Delta
		contentBuf.WriteString(d.Content)
		reasoning := d.ReasoningContent
		if reasoning == "" {
			reasoning = d.Reasoning
		}
		reasoningBuf.WriteString(reasoning)
		if d.Content != "" {
			hasContent = true
		}

		// Walk delta content, flushing at punctuation boundaries once the
		// buffer clears the minSegmentChars threshold.
		for _, b := range []byte(d.Content) {
			segBuf.WriteByte(b)
			switch b {
			case '.', '?', '!', ',', '\n':
				if segBuf.Len() >= minSegmentChars {
					flushSegment(false)
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, nil, fmt.Errorf("reading SSE stream: %w", err)
	}
	if !hasContent {
		return nil, nil, fmt.Errorf("no content in SSE stream")
	}
	// Flush any trailing text that didn't hit a boundary.
	flushSegment(true)
	return []byte(contentBuf.String()), []byte(reasoningBuf.String()), nil
}

// ---------------------------------------------------------------------------
// driver: conversation memory (host.Host). History is durable host state that
// survives /reload; the policy decides when to append and how to use it.
// ---------------------------------------------------------------------------

func (h *Host) Remember(role string, content []byte) {
	h.histMu.Lock()
	defer h.histMu.Unlock()
	h.history = append(h.history, message{Role: role, Content: string(content)})
}

func (h *Host) Forget() {
	h.histMu.Lock()
	defer h.histMu.Unlock()
	if n := len(h.history); n > 0 {
		h.history = h.history[:n-1]
	}
}

func (h *Host) Clear() {
	h.histMu.Lock()
	defer h.histMu.Unlock()
	h.history = nil
}

func (h *Host) LastAnswer() []byte {
	h.histMu.Lock()
	defer h.histMu.Unlock()
	for i := len(h.history) - 1; i >= 0; i-- {
		if h.history[i].Role == "assistant" {
			return []byte(h.history[i].Content)
		}
	}
	return nil
}

// Context assembles [current system prompt] + optional user context + history
// into a JSON messages array ready for Send.
func (h *Host) Context() []byte {
	h.histMu.Lock()
	var userCtx string
	if h.userContext != nil {
		userCtx = h.userContext()
	}
	capacity := len(h.history) + 1
	if userCtx != "" {
		capacity++
	}
	msgs := make([]message, 0, capacity)
	msgs = append(msgs, message{Role: "system", Content: h.prompt()})
	if userCtx != "" {
		msgs = append(msgs, message{Role: "system", Content: userCtx})
	}
	msgs = append(msgs, h.history...)
	h.histMu.Unlock()
	b, _ := json.Marshal(msgs)
	return b
}

// Speak implements host.Host. It queues synthesis+playback on the serial speech
// worker and returns immediately, so the turn's text prints without waiting for
// audio; a failure is logged to stderr and never breaks the turn. No cgo — the
// ONNX runtime lives in the separate `supertonic serve` process; the driver only
// does an HTTP POST and shells out to afplay.
func (h *Host) Speak(text []byte, voice string, speed float64) error {
	if plain := speechText(text); plain != "" {
		h.speech.enqueue([]byte(plain), voice, speed) // non-blocking; plays after any queued clips
	}
	return nil
}

// CancelQueued implements host.Host: drops all pending clips from the speech
// queue without rendering them. Call alongside Forget() on stream errors so
// orphaned segments don't play from a rolled-back turn.
func (h *Host) CancelQueued() { h.speech.cancelQueued() }

// SpeakSync is Speak that blocks until playback finishes, so callers can play
// clips in sequence (used by /voicetest).
func (h *Host) SpeakSync(text []byte, voice string, speed float64) error {
	if plain := speechText(text); plain != "" {
		<-h.speech.enqueue([]byte(plain), voice, speed) // wait for this clip to finish
	}
	return nil
}

// SynthesizeWAV fetches synthesized WAV bytes from the TTS server without playing them.
func (h *Host) SynthesizeWAV(ctx context.Context, text []byte, voice string, speed float64) ([]byte, error) {
	body, err := json.Marshal(map[string]any{
		"text":            string(text),
		"voice":           voice,
		"lang":            h.tts.Lang,
		"speed":           speed,
		"response_format": h.tts.Format,
	})
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", h.tts.URL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := h.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("calling TTS %s: %w", h.tts.URL, err)
	}
	defer resp.Body.Close()

	audio, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("TTS %s: status %d: %.200s", h.tts.URL, resp.StatusCode, audio)
	}

	return audio, nil
}

func (h *Host) synthesizeAndPlay(plain, voice string, speed float64) error {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	audio, err := h.SynthesizeWAV(ctx, []byte(plain), voice, speed)
	if err != nil {
		return err
	}

	return playAudio(audio, h.tts.Format)
}

// speechText reduces model output (often markdown) to plain prose for TTS: drop
// fenced code blocks, unwrap links, strip list markers and inline markup.
func speechText(text []byte) string {
	s := string(text)
	s = reFenced.ReplaceAllString(s, " ")
	s = reLink.ReplaceAllString(s, "$1")
	s = reListLead.ReplaceAllString(s, "")
	s = reMarkup.ReplaceAllString(s, "")
	return strings.TrimSpace(s)
}

var (
	reFenced   = regexp.MustCompile("(?s)```.*?```")
	reLink     = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
	reListLead = regexp.MustCompile(`(?m)^[ \t]*(?:[-+*]|\d+\.)[ \t]+`)
	reMarkup   = regexp.MustCompile("[`*_#>]+")
)

// playAudio writes audio to a temp file and plays it with afplay (macOS, no
// cgo). Blocks until playback finishes; callers run it off the main turn.
func playAudio(audio []byte, format string) error {
	audio = padWAVSilence(audio) // afplay clips the tail of playback — pad with silence
	ext := format
	if ext == "" {
		ext = "wav"
	}
	f, err := os.CreateTemp("", "tts-*."+ext)
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(audio); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return exec.Command("afplay", tmp).Run()
}

// padWAVSilence appends trailing silence to a PCM WAV so afplay doesn't clip the
// end of speech. afplay tends to exit just before its buffer fully drains,
// dropping the last fraction of a second — and it does this REGARDLESS of clip
// length (a long answer's final word gets cut just like a short clip's), so we
// always append a fixed tail. The clipped part then lands in the silence, not
// the speech. Anything it can't parse as a WAV is returned unchanged.
func padWAVSilence(wav []byte) []byte {
	// tailSeconds is the fixed pad; very short clips get mangled worse, so their
	// total length is additionally floored up to minSeconds.
	const tailSeconds = 1.0
	const minSeconds = 4.0
	if len(wav) < 12 || string(wav[0:4]) != "RIFF" || string(wav[8:12]) != "WAVE" {
		return wav
	}
	byteRate := 0
	dataSizeOff, dataEnd := -1, -1
	telephony.WalkWAVChunks(wav, func(id string, body, sz int) bool {
		switch id {
		case "fmt ":
			if body+16 <= len(wav) {
				byteRate = int(binary.LittleEndian.Uint32(wav[body+8 : body+12]))
			}
		case "data":
			dataSizeOff, dataEnd = body-4, body+sz
			return false
		}
		return true
	})
	// dataEnd < dataSizeOff+4 guards a malformed chunk size with the high bit
	// set (negative sz), which would wrap dataBytes below zero.
	if byteRate <= 0 || dataSizeOff < 0 || dataEnd < dataSizeOff+4 || dataEnd > len(wav) {
		return wav
	}
	dataBytes := dataEnd - (dataSizeOff + 4)
	// A fixed tail, raised to the short-clip floor when the clip is shorter than
	// minSeconds. byteRate > 0 and tailSeconds > 0, so this is always positive.
	padBytes := int(float64(byteRate) * tailSeconds)
	if floor := int(float64(byteRate)*minSeconds) - dataBytes; floor > padBytes {
		padBytes = floor
	}
	out := make([]byte, dataEnd, dataEnd+padBytes)
	copy(out, wav[:dataEnd])
	out = append(out, make([]byte, padBytes)...)
	binary.LittleEndian.PutUint32(out[dataSizeOff:dataSizeOff+4], uint32(dataBytes+padBytes))
	binary.LittleEndian.PutUint32(out[4:8], uint32(len(out)-8))
	return out
}

// ---------------------------------------------------------------------------
// driver: the system prompt file. Durable host state (survives /reload). Each
// turn does a cheap stat; the file is read only when its mtime has changed. On
// any I/O error the last good value is kept (or the built-in default if the
// file was never successfully read).
// ---------------------------------------------------------------------------

// FilePrompt returns a system-prompt provider (Config.Prompt) that reads path and
// hot-reloads it when the file's mtime changes, falling back to defaultText (or the
// last good load) if the file can't be read. Agents that don't want a file can pass
// any func() string to Config.Prompt instead.
func FilePrompt(path, defaultText string) func() string {
	return (&promptFile{path: path, def: defaultText}).get
}

type promptFile struct {
	path    string
	def     string // fallback when the file has never loaded
	mu      sync.Mutex
	modt    time.Time
	text    string
	loaded  bool
	lastErr string // last error message reported, so we don't repeat it each turn
}

func (p *promptFile) get() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	fi, err := os.Stat(p.path)
	if err != nil {
		return p.fail(err)
	}
	if p.loaded && fi.ModTime().Equal(p.modt) {
		return p.text // unchanged since last read — no re-read
	}
	b, err := os.ReadFile(p.path)
	if err != nil {
		return p.fail(err)
	}
	p.text = strings.TrimSpace(string(b))
	p.modt = fi.ModTime()
	p.loaded = true
	p.lastErr = "" // recovered — let a future failure report itself again
	return p.text
}

// fail tells the user on the terminal that a system-prompt load was attempted
// and failed, then returns the best available fallback (the last good prompt if
// we ever loaded one, else the built-in default). The message is printed only
// when it differs from the last one reported, so a persistent failure doesn't
// repeat every turn.
func (p *promptFile) fail(err error) string {
	msg := fmt.Sprintf("system prompt: attempted to load %q, failed: %v", p.path, err)
	if msg != p.lastErr {
		p.lastErr = msg
		fallback := "built-in default prompt"
		if p.loaded {
			fallback = "last successfully loaded prompt"
		}
		fmt.Fprintf(os.Stderr, "%s — using %s\n", msg, fallback)
	}
	if p.loaded {
		return p.text
	}
	return p.def
}

// ---------------------------------------------------------------------------
// driver: env + flag helpers.
// ---------------------------------------------------------------------------

func EnvOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// EnvBool reads a boolean-ish env var: "1"/"true"/"yes"/"on" (any case) is true.
func EnvBool(k string) bool {
	switch strings.ToLower(os.Getenv(k)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// EnvFloatOr reads a float env var, falling back to def when unset or unparseable.
func EnvFloatOr(k string, def float64) float64 {
	if v := os.Getenv(k); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}

// ParseStreamScheme parses the -stream-scheme flag, defaulting to "wss" and
// rejecting any value other than "ws" or "wss" (PRD D15 — the flag itself is
// validated so an invalid scheme never reaches twilio.Server.StreamScheme).
// It uses a fresh FlagSet (rather than the package-global flag.CommandLine)
// so it can be called repeatedly and in isolation from tests.
func ParseStreamScheme(args []string) (string, error) {
	var scheme string
	fs := flag.NewFlagSet("driver", flag.ContinueOnError)
	fs.StringVar(&scheme, "stream-scheme", "wss", "scheme to advertise for the Twilio Media Stream URL (ws or wss)")
	if err := fs.Parse(args); err != nil {
		return "", err
	}
	if scheme != "ws" && scheme != "wss" {
		return "", fmt.Errorf("invalid -stream-scheme %q: must be \"ws\" or \"wss\"", scheme)
	}
	return scheme, nil
}
