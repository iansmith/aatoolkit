package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/iansmith/aatoolkit/telephony"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "Usage: probeset <command> [args]\n")
		fmt.Fprintf(os.Stderr, "Commands:\n")
		fmt.Fprintf(os.Stderr, "  replay <file.ulaw>    - replay audio through VAD+STT, output FullPass transcripts\n")
		fmt.Fprintf(os.Stderr, "  build <dir>           - build dataset rows from recordings\n")
		fmt.Fprintf(os.Stderr, "  score <prompt-file>   - score prompt over dataset rows\n")
		os.Exit(1)
	}

	switch cmd := os.Args[1]; cmd {
	case "replay":
		replayCmd(os.Args[2:])
	case "build":
		buildCmd(os.Args[2:])
	case "score":
		scoreCmd(os.Args[2:])
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", cmd)
		os.Exit(1)
	}
}

// envOr mirrors main.go's own helper (root package, not importable from
// here): read an env var, falling back to def when unset/empty.
func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// sttBaseURL is where probeset's real STTClient posts -- same env var and
// default main.go wires production sessions to (AATOOLKIT_STT_URL, default a
// local whisper sidecar), so replay drives the identical STTClient code
// path a live call does, just against whichever whisper server is running.
func sttBaseURL() string {
	return envOr("AATOOLKIT_STT_URL", "http://127.0.0.1:7789")
}

func replayCmd(args []string) {
	cfg, filename, err := parseReplayFlags(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "replay: %v\n", err)
		os.Exit(1)
	}
	if filename == "" {
		fmt.Fprintf(os.Stderr, "Usage: probeset replay [flags] <file.ulaw>\n")
		os.Exit(1)
	}

	results, err := replayFileCfg(context.Background(), filename, cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "replay: %v\n", err)
		os.Exit(1)
	}

	if err := json.NewEncoder(os.Stdout).Encode(results); err != nil {
		fmt.Fprintf(os.Stderr, "replay: encode results: %v\n", err)
		os.Exit(1)
	}
}

// parseReplayFlags parses replay's VAD sweep flags into a full VADConfig
// (defaults for any knob not overridden) and returns the recording path
// (fs.Arg(0), "" if none). Each flag threads into the replay session's config
// so a recording can be re-run at different thresholds (SOP-168). Split out from
// replayCmd so the flag->config threading is unit-testable without os.Exit.
func parseReplayFlags(args []string) (telephony.VADConfig, string, error) {
	fs := flag.NewFlagSet("replay", flag.ContinueOnError)
	def := telephony.DefaultVADConfig()
	endSilenceMS := fs.Int("end-silence-ms", def.EndSilenceMS, "VAD end-of-utterance silence threshold (ms)")
	turnEndSilenceMS := fs.Int("turn-end-silence-ms", def.TurnEndSilenceMS, "VAD turn-end silence threshold (ms)")
	speechThresh := fs.Float64("speech-thresh", float64(def.SpeechThresh), "VAD speech-onset probability threshold")
	silenceThresh := fs.Float64("silence-thresh", float64(def.SilenceThresh), "VAD silence probability threshold")
	if err := fs.Parse(args); err != nil {
		return telephony.VADConfig{}, "", err
	}
	cfg := def
	cfg.EndSilenceMS = *endSilenceMS
	cfg.TurnEndSilenceMS = *turnEndSilenceMS
	cfg.SpeechThresh = float32(*speechThresh)
	cfg.SilenceThresh = float32(*silenceThresh)
	return cfg, fs.Arg(0), nil
}

// replayFileCfg reads path's raw μ-law bytes and drives them through
// telephony.Replay at cfg, using callSID derived from the filename (its base
// name, extension stripped) so results are traceable back to the recording.
func replayFileCfg(ctx context.Context, path string, cfg telephony.VADConfig) ([]telephony.ReplayResult, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	callSID := recordingID(path)
	sttClient := telephony.NewSTTClient(sttBaseURL())

	// Reuse the replay path to reproduce the decision record + transcript from
	// the saved audio (AATOOLKIT_EVENT_LOG). Events land beside the recording,
	// keyed by its id; the session flushes on its own Close inside Replay.
	opts := []telephony.SessionOption{
		telephony.WithVADConfig(cfg),
		telephony.WithFileDecisionRecorderFromEnv(filepath.Dir(path), callSID, callSID, "", cfg, os.Stderr),
		// Conversation transcript summary (SOP-168): printed to stderr and written
		// to <id>.transcript.txt beside the recording at end of replay. Agent role
		// label is consumer-injected via AATOOLKIT_AGENT_LABEL.
		telephony.WithTranscriptOutput(filepath.Dir(path), callSID, os.Stderr),
		telephony.WithTranscriptAgentLabel(os.Getenv("AATOOLKIT_AGENT_LABEL")),
	}

	return telephony.Replay(ctx, callSID, bytes.NewReader(data), sttClient, opts...)
}

// recordingID derives a recording's identity from its .ulaw path: the
// filename with any trailing ".in.ulaw" / ".ulaw" suffix stripped, matching
// the tap sidecar's <streamSID>.in.ulaw / <streamSID>.json naming
// (internal/telephony/twilio/tap.go) so a row's RecordingID lines up with
// the sidecar it was labelled from.
func recordingID(path string) string {
	base := filepath.Base(path)
	base = strings.TrimSuffix(base, ".in.ulaw")
	base = strings.TrimSuffix(base, ".ulaw")
	return base
}

func buildCmd(args []string) {
	fs := flag.NewFlagSet("build", flag.ExitOnError)
	// -1 = unset: seed EndSilenceMS from each recording's captured vad_config
	// rather than overriding it (SOP-168 behavior 3).
	endSilenceMS := fs.Int("end-silence-ms", -1, "VAD end-of-utterance silence threshold (ms); default: each recording's captured value")
	fs.Parse(args)

	if fs.NArg() == 0 {
		fmt.Fprintf(os.Stderr, "Usage: probeset build [flags] <directory>\n")
		os.Exit(1)
	}

	dirname := fs.Arg(0)
	rows, err := buildDataset(context.Background(), dirname, *endSilenceMS)
	if err != nil {
		fmt.Fprintf(os.Stderr, "build: %v\n", err)
		os.Exit(1)
	}

	enc := json.NewEncoder(os.Stdout)
	for _, row := range rows {
		if err := enc.Encode(row); err != nil {
			fmt.Fprintf(os.Stderr, "build: encode row: %v\n", err)
			os.Exit(1)
		}
	}
}

// recordingSidecar is the subset of twilio/tap.go's tapSidecar fields build
// needs. A local, minimal struct rather than importing the twilio package's
// unexported type: JSON decoding only needs field names to match, not a
// shared Go type, and internal/telephony/twilio isn't safe to import from
// here regardless (it imports internal/telephony, which cmd/probeset also
// imports directly for Replay -- an import through twilio would be the
// long way around). VADConfig is the recording's captured VAD config, the
// baseline a sweep varies one knob from (SOP-168 behavior 3).
type recordingSidecar struct {
	Label     string              `json:"label"`
	VADConfig telephony.VADConfig `json:"vad_config"`
}

// configForRecording seeds the replay config from the recording's captured
// vad_config (SOP-168 behavior 3), so a sweep varies one knob from the captured
// baseline rather than package defaults. endSilenceOverride >= 0 overrides
// EndSilenceMS; < 0 (unset) keeps the captured value.
func configForRecording(sc recordingSidecar, endSilenceOverride int) telephony.VADConfig {
	cfg := sc.VADConfig
	if endSilenceOverride >= 0 {
		cfg.EndSilenceMS = endSilenceOverride
	}
	return cfg
}

// buildDataset replays every "<id>.in.ulaw" recording under dir (falling
// back to "<id>.ulaw" if the two-channel naming isn't present), seeding each
// replay from that recording's captured "<id>.json" sidecar vad_config
// (endSilenceOverride >= 0 overrides EndSilenceMS), and returns every
// recording's structurally-derived dataset rows (RowsFromUtterances)
// concatenated in filename order.
func buildDataset(ctx context.Context, dir string, endSilenceOverride int) ([]telephony.DatasetRow, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read dir %s: %w", dir, err)
	}

	sttClient := telephony.NewSTTClient(sttBaseURL())
	var rows []telephony.DatasetRow
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".ulaw") || strings.HasSuffix(name, ".out.ulaw") {
			continue
		}

		id := recordingID(name)
		ulawPath := filepath.Join(dir, name)
		sidecarPath := filepath.Join(dir, id+".json")

		sc, err := readRecordingSidecar(sidecarPath)
		if err != nil {
			return nil, fmt.Errorf("recording %s: %w", id, err)
		}
		cfg := configForRecording(sc, endSilenceOverride)

		data, err := os.ReadFile(ulawPath)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", ulawPath, err)
		}

		results, err := telephony.Replay(ctx, id, bytes.NewReader(data), sttClient,
			telephony.WithVADConfig(cfg),
		)
		if err != nil {
			return nil, fmt.Errorf("replay %s: %w", id, err)
		}

		utterances := make([]string, len(results))
		for i, r := range results {
			utterances[i] = r.Text
		}
		if len(utterances) == 0 {
			continue
		}
		rows = append(rows, telephony.RowsFromUtterances(id, telephony.RecordingLabel(sc.Label), utterances, cfg.EndSilenceMS)...)
	}
	return rows, nil
}

func readRecordingSidecar(sidecarPath string) (recordingSidecar, error) {
	data, err := os.ReadFile(sidecarPath)
	if err != nil {
		return recordingSidecar{}, fmt.Errorf("read sidecar %s: %w", sidecarPath, err)
	}
	var sc recordingSidecar
	if err := json.Unmarshal(data, &sc); err != nil {
		return recordingSidecar{}, fmt.Errorf("parse sidecar %s: %w", sidecarPath, err)
	}
	if sc.Label == "" {
		return recordingSidecar{}, fmt.Errorf("sidecar %s: empty label", sidecarPath)
	}
	return sc, nil
}

func scoreCmd(args []string) {
	fs := flag.NewFlagSet("score", flag.ExitOnError)
	passes := fs.Int("passes", 1, "number of verification passes")
	datasetPath := fs.String("dataset", "", "path to a JSONL dataset file produced by `probeset build` (required)")
	fs.Parse(args)

	if fs.NArg() == 0 {
		fmt.Fprintf(os.Stderr, "Usage: probeset score [flags] <prompt-file>\n")
		os.Exit(1)
	}
	if *datasetPath == "" {
		fmt.Fprintf(os.Stderr, "Usage: probeset score --dataset <rows.jsonl> [flags] <prompt-file>\n")
		os.Exit(1)
	}

	promptFile := fs.Arg(0)
	promptBytes, err := os.ReadFile(promptFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "score: read prompt file: %v\n", err)
		os.Exit(1)
	}
	prompt := string(promptBytes)

	rows, err := readDatasetRows(*datasetPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "score: %v\n", err)
		os.Exit(1)
	}
	if len(rows) == 0 {
		fmt.Fprintf(os.Stderr, "score: no rows in %s\n", *datasetPath)
		os.Exit(1)
	}
	endSilenceMS := rows[0].EndSilenceMS

	ctx := context.Background()
	for pass := 1; pass <= *passes; pass++ {
		report, err := telephony.Score(ctx, promptFile, rows, endSilenceMS, promptVerifier(prompt))
		if err != nil {
			fmt.Fprintf(os.Stderr, "score: pass %d: %v\n", pass, err)
			os.Exit(1)
		}
		if err := json.NewEncoder(os.Stdout).Encode(report); err != nil {
			fmt.Fprintf(os.Stderr, "score: encode report: %v\n", err)
			os.Exit(1)
		}
	}
}

func readDatasetRows(path string) ([]telephony.DatasetRow, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read dataset %s: %w", path, err)
	}
	var rows []telephony.DatasetRow
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var row telephony.DatasetRow
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			return nil, fmt.Errorf("parse dataset row: %w", err)
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// promptVerifier is score's CLI-level Verifier. This ticket owns the
// harness -- Score's aggregation and four-outcome reporting, fully
// implemented and independently tested (TestScore_ReportsOutcomes) against
// a fixed fake Verifier -- not the verifier's actual prompt-calling
// semantics, which the ticket's own Out of scope section assigns
// elsewhere ("Do NOT wire the verifier into the server" -- SOP-151). This stub
// exists so `probeset score`'s CLI is otherwise complete (flags, dataset
// loading, prompt-file loading, N-pass looping) and fails loudly with a
// clear, actionable message instead of fabricating verdicts, until SOP-151
// supplies the real one.
func promptVerifier(prompt string) telephony.Verifier {
	_ = prompt // real verifier (SOP-151) will use this; the stub doesn't
	return func(_ context.Context, promptFile string, _ telephony.DatasetRow) (telephony.VerifierOutcome, error) {
		return "", fmt.Errorf("no verifier wired for %s: SOP-151 owns the real prompt-calling verifier; this harness ticket (SOP-153) stops at Score's aggregation", promptFile)
	}
}
