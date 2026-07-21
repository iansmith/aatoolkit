package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/iansmith/aatoolkit/telephony"
)

// TestParseReplayFlags_ThreadIntoConfig proves each replay sweep flag reaches
// the VADConfig and the recording path is returned (SOP-168 behavior 2).
func TestParseReplayFlags_ThreadIntoConfig(t *testing.T) {
	cfg, file, err := parseReplayFlags([]string{
		"--end-silence-ms", "900",
		"--turn-end-silence-ms", "3000",
		"--speech-thresh", "0.7",
		"--silence-thresh", "0.2",
		"rec.ulaw",
	})
	if err != nil {
		t.Fatalf("parseReplayFlags: %v", err)
	}
	if file != "rec.ulaw" {
		t.Errorf("file: got %q, want rec.ulaw", file)
	}
	if cfg.EndSilenceMS != 900 {
		t.Errorf("EndSilenceMS: got %d, want 900", cfg.EndSilenceMS)
	}
	if cfg.TurnEndSilenceMS != 3000 {
		t.Errorf("TurnEndSilenceMS: got %d, want 3000", cfg.TurnEndSilenceMS)
	}
	if cfg.SpeechThresh != 0.7 {
		t.Errorf("SpeechThresh: got %v, want 0.7", cfg.SpeechThresh)
	}
	if cfg.SilenceThresh != 0.2 {
		t.Errorf("SilenceThresh: got %v, want 0.2", cfg.SilenceThresh)
	}
}

// TestParseReplayFlags_Defaults: no flags yields the package default config.
func TestParseReplayFlags_Defaults(t *testing.T) {
	cfg, file, err := parseReplayFlags([]string{"rec.ulaw"})
	if err != nil {
		t.Fatalf("parseReplayFlags: %v", err)
	}
	if file != "rec.ulaw" {
		t.Errorf("file: got %q, want rec.ulaw", file)
	}
	if cfg != telephony.DefaultVADConfig() {
		t.Errorf("no-flags config: got %+v, want DefaultVADConfig %+v", cfg, telephony.DefaultVADConfig())
	}
}

// TestConfigForRecording_SeedsFromSidecar proves build seeds each replay from
// the recording's captured vad_config, overriding EndSilenceMS only when the
// flag is set (SOP-168 behavior 3).
func TestConfigForRecording_SeedsFromSidecar(t *testing.T) {
	sc := recordingSidecar{
		Label: "greeting",
		VADConfig: telephony.VADConfig{
			WindowSize: 256, SampleRateHz: 8000,
			SpeechThresh: 0.6, SilenceThresh: 0.3, EndSilenceMS: 1050, TurnEndSilenceMS: 4000,
		},
	}

	// Override unset (-1): the captured (non-default) thresholds are used as-is.
	got := configForRecording(sc, -1)
	if got.EndSilenceMS != 1050 {
		t.Errorf("EndSilenceMS: got %d, want 1050 (captured)", got.EndSilenceMS)
	}
	if got.TurnEndSilenceMS != 4000 || got.SpeechThresh != 0.6 {
		t.Errorf("captured config not preserved: got %+v", got)
	}

	// Override set: EndSilenceMS is replaced, the rest stay from the sidecar.
	got = configForRecording(sc, 700)
	if got.EndSilenceMS != 700 {
		t.Errorf("overridden EndSilenceMS: got %d, want 700", got.EndSilenceMS)
	}
	if got.TurnEndSilenceMS != 4000 {
		t.Errorf("TurnEndSilenceMS should stay captured: got %d, want 4000", got.TurnEndSilenceMS)
	}
}

// TestReadRecordingSidecar_DecodesVADConfig proves the sidecar's vad_config is
// decoded (the tap writes Go-field-named keys, no json tags on VADConfig).
func TestReadRecordingSidecar_DecodesVADConfig(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "rec.json")
	if err := os.WriteFile(p, []byte(`{"label":"greeting","vad_config":{"EndSilenceMS":1050,"SpeechThresh":0.6,"TurnEndSilenceMS":4000}}`), 0o644); err != nil {
		t.Fatalf("write sidecar: %v", err)
	}
	sc, err := readRecordingSidecar(p)
	if err != nil {
		t.Fatalf("readRecordingSidecar: %v", err)
	}
	if sc.Label != "greeting" {
		t.Errorf("Label: got %q, want greeting", sc.Label)
	}
	if sc.VADConfig.EndSilenceMS != 1050 || sc.VADConfig.SpeechThresh != 0.6 || sc.VADConfig.TurnEndSilenceMS != 4000 {
		t.Errorf("decoded vad_config: got %+v", sc.VADConfig)
	}
}
