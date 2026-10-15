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
	"errors"
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
	"unicode/utf8"

	"github.com/iansmith/aatoolkit/host"
	"github.com/iansmith/aatoolkit/telephony"
	"github.com/iansmith/aatoolkit/telephony/twilio"
	"go.opentelemetry.io/otel/trace"
)

// ---------------------------------------------------------------------------
// driver: LLM transport. One entry per tier; tier -> {url, model}. Two separate
// llama-server instances, no proxy. The policy only ever names a tier.
// ---------------------------------------------------------------------------

// sendTimeout bounds one model request end to end: the connection, the
// response headers and the whole stream. It composes with the caller's
// context, so an earlier caller deadline wins.
const sendTimeout = 300 * time.Second

// Tier describes one tier's llama-server. reasoning=false asks the model to
// skip its thinking trace; maxTokens caps a single turn so a runaway generation
// can't burn the whole sendTimeout.
type Tier struct {
	URL, Model string
	Reasoning  bool
	MaxTokens  int
	// APIKey, when non-empty, is sent as "Authorization: Bearer <APIKey>" on
	// this tier's chat request (AATK-112) -- what a hosted OpenAI-compatible
	// API needs and a local llama-server ignores. Empty sends no header.
	APIKey string
	// NoTools, when true, declares an empty tools list on this tier's chat
	// request (AATK-113). This driver is text-only -- it reads content and
	// reasoning and drops tool_calls -- and a tool-trained hosted model that
	// is merely TOLD about capabilities in the prompt will invent a function
	// call and answer nothing. An explicit "tools": [] is what stops it;
	// tool_choice "none" was measured not to. Off by default: the local
	// servers have not been exercised against an empty tools array.
	NoTools bool
	// IncludeUsage, when true, sends "stream_options": {"include_usage": true}
	// on this tier's chat request (AATK-159), asking the upstream to report
	// the request's token usage at the end of the stream. Off by default,
	// because not every OpenAI-compatible upstream accepts the key; with it
	// off the request body is byte-identical to before the field existed.
	// Measured on one hosted OpenAI-compatible provider: it accepts the key,
	// and it also sends a final usage chunk without it. A local mlx-serve has
	// not been measured, so keep IncludeUsage off on local tiers until it is.
	// Usage is recorded on the span whenever the stream carries it, whatever
	// this flag says.
	IncludeUsage bool
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
	userContext func(queryText string) string // optional user context block
	histMu      sync.Mutex
	// tracerProvider is Config.TracerProvider, fixed at New. Nil means the
	// global provider, resolved at each call and never cached.
	tracerProvider trace.TracerProvider
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
	UserContext func(queryText string) string // optional user context block injected after system prompt; queryText is what the policy passed to Context

	// RealtimeURL selects the voice transport. Empty — the default — runs the
	// existing VAD/STT/TTS sidecar path and constructs no realtime client. Set
	// to a backend websocket URL, calls run over that backend instead, which
	// also hands it the turn-taking; see twilio.NewStreamHandler for what that
	// bypasses. The engine supplies no default URL: like every other field
	// here, it is the consumer's to inject.
	RealtimeURL string

	// TracerProvider supplies the tracer for the span each model call
	// produces (AATK-159): one CLIENT span, named "chat <Tier.Model>", per
	// call whose tier is found and whose window encodes. That includes a call
	// that sends nothing because its context is already done or its tier URL
	// cannot be requested; its span records why it failed. Nil, the default,
	// means otel.GetTracerProvider() is resolved at each call, so a consumer
	// that installs its SDK globally, before or after New, gets spans. With
	// nothing installed, the no-op provider records and sends nothing. The
	// engine never installs a provider, exporter or propagator, and never adds
	// trace headers to the upstream request.
	//
	// Message content stays off the span by default: it is personal data (the
	// system prompt, the user-context block, the history and the user's own
	// text). The environment variable AATOOLKIT_CAPTURE_MESSAGE_CONTENT turns
	// it on when strconv.ParseBool reads its value as true ("1", "true"):
	// each request's messages and the model's output are then recorded as
	// gen_ai.input.messages and gen_ai.output.messages. Unset, empty, false or
	// unparsable leaves it off. It is read per request, and only for a span
	// that is recording. Values are not truncated; bound them with the SDK's
	// span limits (OTEL_ATTRIBUTE_VALUE_LENGTH_LIMIT).
	TracerProvider trace.TracerProvider
}

// StreamHandler returns the Media Streams handler for the configured
// transport, for a consumer to install on twilio.Server.HandleStream. It is
// the one place the transport switch is read.
func (c Config) StreamHandler() twilio.StreamHandler {
	return twilio.NewStreamHandler(c.RealtimeURL)
}

// New builds a driver Host from cfg, wiring the serial speech queue internally.
func New(cfg Config) *Host {
	h := &Host{
		tiers: cfg.Tiers,
		// Redirects are NOT followed (AATK-112). A tier may carry an API key,
		// and Go's client retains Authorization across a redirect to the same
		// hostname, a subdomain of it, or a different port on it (only a
		// hop to an unrelated host strips it); the fleet's standing rule is
		// not to lean on that but to surface a 30x from an LLM upstream as
		// the response it is, so the request is never replayed against a
		// host the operator did not configure. The TTS request shares this
		// client; a local sidecar does not redirect, and a
		// remote one that did would now report the 30x rather than follow it.
		client: &http.Client{
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		prompt:         cfg.Prompt,
		tts:            cfg.TTS,
		userContext:    cfg.UserContext,
		tracerProvider: cfg.TracerProvider,
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
// model/flags don't emit it), error. It is SendContext under
// context.Background(), so sendTimeout alone bounds it.
func (h *Host) Send(contextWindow []byte, tier string) ([]byte, []byte, error) {
	return h.SendContext(context.Background(), contextWindow, tier)
}

// SendContext implements host.ContextSender: Send under ctx. It's
// SendStreamContext without the per-segment callback — the full assembled
// text is all the caller wants.
func (h *Host) SendContext(ctx context.Context, contextWindow []byte, tier string) ([]byte, []byte, error) {
	return h.SendStreamContext(ctx, contextWindow, tier, func(string) {})
}

// WithContext returns a host.Host whose Send and SendStream run under ctx: their
// spans are children of ctx's span, and ctx's deadline and cancellation bound
// them. Every other method is the receiver's own, and SendContext/
// SendStreamContext keep their explicit ctx. Make one per inbound request and do
// not keep it beyond that request. For work that outlives an HTTP handler, pass
// context.WithoutCancel(ctx). A nil ctx panics, as http.Request.WithContext does.
func (h *Host) WithContext(ctx context.Context) host.Host {
	if ctx == nil {
		panic("driver: WithContext called with a nil context")
	}
	return &boundHost{Host: h, ctx: ctx}
}

// boundHost is the view WithContext returns: the receiver, with ctx bound to
// its two context-free sends. It must override both. Were Send left promoted,
// it would run (*Host).Send, which sends under context.Background() and loses
// ctx. A boundHost is never modified after WithContext builds it.
type boundHost struct {
	*Host
	ctx context.Context
}

// Send implements host.Host: SendContext under the bound context.
func (b *boundHost) Send(contextWindow []byte, tier string) ([]byte, []byte, error) {
	return b.Host.SendContext(b.ctx, contextWindow, tier)
}

// SendStream implements host.Host: SendStreamContext under the bound context.
func (b *boundHost) SendStream(contextWindow []byte, tier string, onSegment func(string)) ([]byte, []byte, error) {
	return b.Host.SendStreamContext(b.ctx, contextWindow, tier, onSegment)
}

// SystemPrompt implements host.Host: the current system prompt, reloaded from
// its file only when the file changes.
func (h *Host) SystemPrompt() string { return h.prompt() }

// buildStreamPayload assembles the chat-completions request body for tier ep:
// the fixed sampling params plus the caller's already-serialized messages
// array, with thinking disabled for non-reasoning tiers.
func buildStreamPayload(ep Tier, contextWindow []byte) map[string]any {
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
	if ep.NoTools {
		// An explicit empty list, not an absent key: absent, a tool-trained
		// hosted model invents a function to call and streams no content
		// (AATK-113). tool_choice "none" was measured not to stop it.
		payload["tools"] = []any{}
	}
	if ep.IncludeUsage {
		// Asks the upstream for a final usage chunk (AATK-159). Off, the key
		// is absent and the body is what it was before the flag existed.
		payload["stream_options"] = map[string]any{"include_usage": true}
	}
	return payload
}

// upstreamStatusError is a non-200 answer from a tier's upstream: its URL,
// status and up to 300 bytes of its body. Its text is what the driver has
// always returned for one; the type lets a span record the status alone.
type upstreamStatusError struct {
	url    string
	status int
	body   []byte
}

func (e *upstreamStatusError) Error() string {
	return fmt.Sprintf("llm %s: status %d (%.300s)", e.url, e.status, e.body)
}

// upstreamErrorPayload is an {"error": {...}} chunk in the SSE stream. Its
// text is what the driver has always returned for one; the type lets a span
// record the kind of failure without the upstream's message.
type upstreamErrorPayload struct {
	msg string
}

func (e *upstreamErrorPayload) Error() string { return "llm error: " + e.msg }

// postStreamRequest POSTs body to ep.URL and returns the still-open streaming
// response on success. On a non-200 status it reads up to 300 bytes of the
// error body into the returned *upstreamStatusError and closes the response
// itself.
func (h *Host) postStreamRequest(ctx context.Context, ep Tier, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", ep.URL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if ep.APIKey != "" {
		// The key reaches the wire and nowhere else: the error paths below
		// print the URL and the response body, never the request headers.
		req.Header.Set("Authorization", "Bearer "+ep.APIKey)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("calling %s: %w", ep.URL, err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 301))
		return nil, &upstreamStatusError{url: ep.URL, status: resp.StatusCode, body: errBody}
	}
	return resp, nil
}

// sseChunk is one decoded `data: {...}` line from the chat-completions SSE
// stream. ID, Model and Usage feed only the span, so they are decoded best
// effort: a value of an unexpected type is dropped and never fails the chunk.
type sseChunk struct {
	ID      chunkString `json:"id"`
	Model   chunkString `json:"model"`
	Choices []struct {
		Delta        sseDelta `json:"delta"`
		FinishReason string   `json:"finish_reason"`
	} `json:"choices"`
	Usage chunkUsage `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// chunkString is a chunk's id or model, decoded best effort: a JSON string is
// taken, and any other value, null included, leaves it empty; either way
// decoding the chunk carries on, as it did before the field was read at all.
// Upstreams send both on every chunk, so a string with no escapes and valid
// UTF-8, which needs no unquoting, is taken from the bytes directly: it costs
// the string's one allocation and no second decode.
type chunkString string

func (s *chunkString) UnmarshalJSON(b []byte) error {
	if len(b) < 2 || b[0] != '"' {
		return nil
	}
	if raw := b[1 : len(b)-1]; bytes.IndexByte(raw, '\\') < 0 && utf8.Valid(raw) {
		*s = chunkString(raw)
		return nil
	}
	var v string
	if json.Unmarshal(b, &v) == nil {
		*s = chunkString(v)
	}
	return nil
}

// chunkUsage is a chunk's usage, decoded best effort: an object that decodes
// as sseUsage is taken whole, never half read, and any other value, null
// included, leaves ok false without failing the chunk. Only a value that
// opens an object is decoded, so the null a hosted upstream sends on every
// chunk but the last costs nothing.
type chunkUsage struct {
	v  sseUsage
	ok bool
}

func (u *chunkUsage) UnmarshalJSON(b []byte) error {
	if len(b) == 0 || b[0] != '{' {
		return nil
	}
	var v sseUsage
	if json.Unmarshal(b, &v) == nil {
		u.v, u.ok = v, true
	}
	return nil
}

// sseDelta is the incremental text one chunk carries.
type sseDelta struct {
	Content          string `json:"content"`
	ReasoningContent string `json:"reasoning_content"`
	Reasoning        string `json:"reasoning"`
	// finishReason is the chunk's finish_reason, carried beside the delta so
	// an empty stream can say why it ended ("tool_calls", "length"). Not a
	// JSON field of the delta itself.
	finishReason string
	// model, id and usage are the chunk's own top-level fields, carried beside
	// the delta the same way finishReason is, so the stream state can say
	// which model answered and what the request cost. A choice-less chunk
	// carries them beside an empty delta. Not JSON fields of the delta itself.
	model, id string
	usage     *sseUsage
}

// sseUsage is the token usage an OpenAI-compatible stream reports. A hosted
// upstream sends it on a final choice-less chunk and "usage": null on every
// chunk before it, hence the pointer in sseDelta and streamState. Provider
// extras (totals, cached-token details) are ignored.
type sseUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

// reasoningText returns the delta's reasoning, tolerating both spellings
// providers emit for it: `reasoning_content` and bare `reasoning`.
func (d sseDelta) reasoningText() string {
	if d.ReasoningContent != "" {
		return d.ReasoningContent
	}
	return d.Reasoning
}

// decodeSSELine parses one line of the SSE stream into the delta it carries.
// ok is false for lines with nothing to apply — blank lines, lines without the
// "data: " prefix, and the "[DONE]" terminator. Every other data chunk decodes
// with ok true, including one carrying no choices, which is how an upstream
// delivers the final usage: its delta is empty, and the chunk's model, id and
// usage ride beside it as on any other chunk. Those three are decoded best
// effort, so one of an unexpected type comes back empty instead of failing
// the chunk. An upstream error payload comes back as an
// *upstreamErrorPayload, and a malformed chunk as an error.
func decodeSSELine(line string) (delta sseDelta, ok bool, err error) {
	if line == "" || !strings.HasPrefix(line, "data: ") {
		return sseDelta{}, false, nil
	}
	sseData := line[len("data: "):]
	if sseData == "[DONE]" {
		return sseDelta{}, false, nil
	}
	var chunk sseChunk
	if err := json.Unmarshal([]byte(sseData), &chunk); err != nil {
		return sseDelta{}, false, fmt.Errorf("decoding SSE chunk: %w", err)
	}
	if chunk.Error != nil {
		return sseDelta{}, false, &upstreamErrorPayload{msg: chunk.Error.Message}
	}
	if len(chunk.Choices) > 0 {
		delta = chunk.Choices[0].Delta
		delta.finishReason = chunk.Choices[0].FinishReason
	}
	delta.model, delta.id = string(chunk.Model), string(chunk.ID)
	if chunk.Usage.ok {
		u := chunk.Usage.v
		delta.usage = &u
	}
	return delta, true, nil
}

// isSegmentDelimiter reports whether b is one of the punctuation boundaries
// that can trigger a TTS segment flush.
func isSegmentDelimiter(b byte) bool {
	switch b {
	case '.', '?', '!', ',', '\n':
		return true
	}
	return false
}

// segmentAccumulator buffers streamed text and reports when it should be
// flushed to TTS: at a delimiter (. ? ! , \n) once the buffer has crossed
// min chars, keeping the delimiter, or unconditionally when force-flushed at
// end-of-stream. A pure-whitespace buffer is never flushed.
type segmentAccumulator struct {
	buf strings.Builder
	min int
}

func newSegmentAccumulator(min int) *segmentAccumulator {
	return &segmentAccumulator{min: min}
}

// write appends b to the buffer and, if b is a delimiter and the buffer has
// crossed the minimum length, flushes and returns the segment.
func (s *segmentAccumulator) write(b byte) (string, bool) {
	s.buf.WriteByte(b)
	if !isSegmentDelimiter(b) || s.buf.Len() < s.min {
		return "", false
	}
	return s.take()
}

// flush unconditionally drains the buffer — used at end-of-stream so no
// trailing text is dropped, regardless of length.
func (s *segmentAccumulator) flush() (string, bool) {
	return s.take()
}

// take drains the buffer and reports whether it held anything worth sending;
// pure whitespace is discarded rather than sent to TTS.
func (s *segmentAccumulator) take() (string, bool) {
	text := s.buf.String()
	s.buf.Reset()
	if strings.TrimSpace(text) == "" {
		return "", false
	}
	return text, true
}

// streamState carries the accumulation state threaded through a run of
// processSSELine calls: the assembled content and reasoning, the segment
// accumulator, whether any content chunk has been seen yet, and what the
// chunks said about the response itself.
type streamState struct {
	contentBuf, reasoningBuf strings.Builder
	acc                      *segmentAccumulator
	hasContent               bool
	finishReason             string // the last non-empty finish_reason seen

	chunks    int       // decoded data chunks, choice-less ones included
	model, id string    // the first non-empty value of each
	usage     *sseUsage // the last non-nil usage seen
}

// processSSELine decodes one SSE line, folds it into st, and calls onSegment
// for any TTS segment boundary the new content crosses. It returns an error
// for a malformed chunk or an upstream error payload; lines needing no work
// (blank, non-data, [DONE]) are a no-op. A choice-less chunk counts as a
// chunk and contributes its model, id and usage, but its empty delta leaves
// the content, reasoning, hasContent and finishReason as they were.
func processSSELine(line string, st *streamState, onSegment func(string)) error {
	d, ok, err := decodeSSELine(line)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	st.chunks++
	if st.model == "" {
		st.model = d.model
	}
	if st.id == "" {
		st.id = d.id
	}
	if d.usage != nil {
		st.usage = d.usage
	}
	st.contentBuf.WriteString(d.Content)
	st.reasoningBuf.WriteString(d.reasoningText())
	if d.Content != "" {
		st.hasContent = true
	}
	if d.finishReason != "" {
		st.finishReason = d.finishReason
	}
	for _, b := range []byte(d.Content) {
		if seg, ok := st.acc.write(b); ok {
			onSegment(seg)
		}
	}
	return nil
}

// SendStream is like Send but streams the response, calling onSegment each
// time a punctuation boundary is reached with enough accumulated text. The
// returned slices are the fully assembled (content, reasoning). Segments are
// flushed when the buffer crosses ~30 chars and a delimiter (. ? ! , \n) is
// seen; the delimiter is kept at the end of the segment so the TTS has the
// punctuation it needs for natural cadence. It is SendStreamContext under
// context.Background(), so sendTimeout alone bounds it.
func (h *Host) SendStream(contextWindow []byte, tier string, onSegment func(string)) ([]byte, []byte, error) {
	return h.SendStreamContext(context.Background(), contextWindow, tier, onSegment)
}

// SendStreamContext implements host.ContextSender: SendStream under ctx. The
// request runs under ctx bounded by sendTimeout, so ctx's values reach it and
// its deadline applies when it is the earlier one. Once ctx is done the call
// fails at the next read, line or segment, even when the rest of the stream
// is already buffered: no onSegment call starts after that, though one
// already running is not interrupted. Wherever the call stops once ctx is
// done (sending, waiting for the response headers, reading a non-200 status
// body or the stream, or between lines), and over HTTP/1 or HTTP/2 alike,
// the error matches both ctx.Err() and context.Cause(ctx) under errors.Is:
// for a context cancelled or timed out with a cause (context.WithCancelCause,
// WithTimeoutCause, WithDeadlineCause), it matches that cause as well as
// context.Canceled or DeadlineExceeded. It is the one implementation of a
// model request: Send, SendStream and SendContext all end here.
//
// Once its tier is found and its window encodes, a call gets one CLIENT
// span, a child of ctx's span, from Config.TracerProvider or the global
// provider. An unknown tier, or a window that does not encode (bytes that
// are not JSON, or an empty non-nil slice), gets none and sends nothing; a
// nil window encodes as null and is sent, as it always was. A call that
// then sends nothing, because ctx is already done or the tier URL cannot be
// requested, still gets its span, which records why it failed.
// The span covers the whole call, onSegment's time included, and ends after
// the last segment. It is ended by a defer, so a panicking onSegment still
// ends it, as an error of type _OTHER; the panic is not recovered.
func (h *Host) SendStreamContext(ctx context.Context, contextWindow []byte, tier string, onSegment func(string)) (content, reasoning []byte, err error) {
	ep, ok := h.tiers[tier]
	if !ok {
		return nil, nil, fmt.Errorf("unknown tier %q (have: fast, deep)", tier)
	}
	body, err := json.Marshal(buildStreamPayload(ep, contextWindow))
	if err != nil {
		return nil, nil, err
	}
	ctx, call := startModelSpan(ctx, h.tracerProvider, tier, ep.Model, ep.URL)
	st := &streamState{acc: newSegmentAccumulator(minSegmentChars)}
	// What end records if streamChat never returns, because onSegment
	// panicked.
	errType := errTypeOther
	defer func() { call.end(st, ep.MaxTokens, contextWindow, errType) }()
	content, reasoning, errType, err = h.streamChat(ctx, call, ep, body, st, onSegment)
	return content, reasoning, err
}

// streamChat sends one chat-completions request and folds its SSE stream
// into st, calling onSegment at each segment boundary. Besides the result it
// returns the error.type for call's span: "" on success. It marks on call
// when the request went out and when each line arrived, taking the time
// before the line is processed so onSegment's time is not counted.
func (h *Host) streamChat(ctx context.Context, call *modelCall, ep Tier, body []byte, st *streamState, onSegment func(string)) (content, reasoning []byte, errType string, err error) {
	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	call.requestSent()
	resp, err := h.postStreamRequest(ctx, ep, body)
	if err != nil {
		return failed(ctx, err, errTypeTransport)
	}
	defer resp.Body.Close()

	// A done ctx fails the next network read, but lines the transport has
	// already buffered need none. So ctx is also checked before each line and
	// before the end-of-stream flush, and a segment cut once it is done is
	// dropped: no onSegment call starts after ctx is done. Once ctx is done a
	// read fails with ctx's cause, whatever the transport's read returned
	// (net/http's HTTP/2 transport returns ctx.Err() where HTTP/1 returns the
	// cause), so the error does not depend on the protocol or on which site
	// saw ctx done first.
	emit := func(seg string) {
		if ctx.Err() == nil {
			onSegment(seg)
		}
	}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 256*1024)
	for scanner.Scan() {
		if err := context.Cause(ctx); err != nil {
			return failed(ctx, streamReadError(err), errTypeStreamRead)
		}
		if err := foldLine(call, st, scanner.Text(), emit); err != nil {
			return failed(ctx, err, errTypeDecode)
		}
	}
	err = scanner.Err()
	if cause := context.Cause(ctx); cause != nil {
		err = cause
	}
	if err != nil {
		return failed(ctx, streamReadError(err), errTypeStreamRead)
	}
	if !st.hasContent {
		var err error
		if st.finishReason != "" {
			err = fmt.Errorf("no content in SSE stream (finish_reason %s)", st.finishReason)
		} else {
			err = fmt.Errorf("no content in SSE stream")
		}
		return failed(ctx, err, errTypeEmpty)
	}
	// Flush any trailing text that didn't hit a boundary.
	if seg, ok := st.acc.flush(); ok {
		onSegment(seg)
	}
	return []byte(st.contentBuf.String()), []byte(st.reasoningBuf.String()), "", nil
}

// foldLine folds one SSE line into st through processSSELine and takes, on
// call, the first-arrival marks the line produced. The time is read before
// the line is processed, so the onSegment calls it triggers are not counted,
// and the marks are taken by a defer, so a panicking onSegment still leaves
// them for the span.
func foldLine(call *modelCall, st *streamState, line string, onSegment func(string)) error {
	at := call.now()
	hadChunk, hadContent := st.chunks > 0, st.hasContent
	defer call.observe(st, hadChunk, hadContent, at)
	return processSSELine(line, st, onSegment)
}

// streamReadError is the error for a stream that could not be read to its
// end: err is the read's failure, or ctx's cause when ctx was done first.
func streamReadError(err error) error {
	return fmt.Errorf("reading SSE stream: %w", err)
}

// failed is what streamChat returns for a request that failed with err: no
// content, the error.type for its span, and the error for the caller. Once
// ctx is done, that error matches both ctx.Err() and context.Cause(ctx)
// under errors.Is, whichever site failed: it is err when err already does,
// and a contextDoneError around err otherwise. ctx must be checked before
// streamChat's own cancel runs, which the return statement guarantees.
func failed(ctx context.Context, err error, fallback string) ([]byte, []byte, string, error) {
	errType := streamErrorType(ctx, err, fallback)
	if ctxErr := ctx.Err(); ctxErr != nil {
		if cause := context.Cause(ctx); !errors.Is(err, ctxErr) || !errors.Is(err, cause) {
			err = &contextDoneError{err: err, ctxErr: ctxErr, cause: cause}
		}
	}
	return nil, nil, errType, err
}

// contextDoneError is the error of a request that failed once its context
// was done. It keeps the failing site's error, text and all, and adds the
// context's error and cause to what errors.Is and errors.As see: net/http's
// HTTP/1 transport fails a request cut short by its context with the cause,
// its HTTP/2 transport with ctx.Err(), and a status error whose body read the
// context cut short with neither.
type contextDoneError struct {
	err, ctxErr, cause error
}

func (e *contextDoneError) Error() string   { return e.err.Error() }
func (e *contextDoneError) Unwrap() []error { return []error{e.err, e.ctxErr, e.cause} }

// streamErrorType classifies a failed request for its span's error.type.
// The context comes first, because a deadline or cancellation surfaces as
// whatever the transport or the stream read was doing when it hit: then an
// upstream's status code, then an SSE error payload, then fallback, the kind
// of site that failed. It never reads err's text.
func streamErrorType(ctx context.Context, err error, fallback string) string {
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return errTypeTimeout
	case errors.Is(ctx.Err(), context.Canceled):
		return errTypeCanceled
	}
	var status *upstreamStatusError
	if errors.As(err, &status) {
		return strconv.Itoa(status.status)
	}
	var payload *upstreamErrorPayload
	if errors.As(err, &payload) {
		return errTypeUpstream
	}
	return fallback
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
// into a JSON messages array ready for Send. queryText is handed to the user
// context provider so its block can depend on the current turn.
func (h *Host) Context(queryText string) []byte {
	// Outside histMu: the provider may do I/O (e.g. retrieval) and never reads
	// history, so holding the lock would stall Remember/Forget for no reason.
	// The call has no deadline and no error path: a provider bounds its own
	// I/O and returns "" on failure.
	var userCtx string
	if h.userContext != nil {
		userCtx = h.userContext(queryText)
	}
	h.histMu.Lock()
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
// rejecting any value other than "ws" or "wss". The flag itself is validated
// so an invalid scheme never reaches twilio.Server.StreamScheme.
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
