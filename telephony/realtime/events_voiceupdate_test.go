package realtime

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"
)

// buildVoiceUpdate is the mid-call voice change on the wire (AATK-104). It is
// a separate builder from buildSessionUpdate on purpose, and these tests are
// what hold the two apart.
//
// The hazard it exists for: audioChannel.Format has no omitempty (events.go),
// so a mid-call frame built from newSessionUpdate would carry
// audio.input.format.type:"" and audio.output.format.type:"". The backend
// deep-merges exactly the fields an update actually sent, so that frame would
// overwrite the negotiated G.711 mu-law format on BOTH directions, mid-call —
// a live call losing its audio format because the voice changed.

// voiceUpdateKeyPaths walks a decoded JSON document and returns every dotted
// key path in it, sorted. "No format key anywhere" is a claim about the whole
// tree, so it is checked as a key SET rather than a substring or a byte
// compare — key order is not part of the contract.
func voiceUpdateKeyPaths(v any, prefix string, out *[]string) {
	m, ok := v.(map[string]any)
	if !ok {
		return
	}
	for k, child := range m {
		path := k
		if prefix != "" {
			path = prefix + "." + k
		}
		*out = append(*out, path)
		voiceUpdateKeyPaths(child, path, out)
	}
}

// TestBuildVoiceUpdate_CarriesVoiceAndNothingElse pins the exact shape
// AATK-104's behaviour 2 names:
// {"type":"session.update","session":{"type":"realtime","audio":{"output":{"voice":"<id>"}}}}
//
// session.type is required: the backend maps session.update onto a
// discriminated union and rejects the transcription variant, so an update
// without it does not parse as the accepted variant at all.
func TestBuildVoiceUpdate_CarriesVoiceAndNothingElse(t *testing.T) {
	raw, err := buildVoiceUpdate("cedar")
	if err != nil {
		t.Fatalf("buildVoiceUpdate: %v", err)
	}

	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode: %v (raw: %s)", err, raw)
	}
	var paths []string
	voiceUpdateKeyPaths(doc, "", &paths)
	sort.Strings(paths)

	want := []string{
		"session",
		"session.audio",
		"session.audio.output",
		"session.audio.output.voice",
		"session.type",
		"type",
	}
	if strings.Join(paths, ",") != strings.Join(want, ",") {
		t.Fatalf("key set =\n  %v\nwant\n  %v\n(raw: %s)", paths, want, raw)
	}

	var got struct {
		Type    string `json:"type"`
		Session struct {
			Type  string `json:"type"`
			Audio struct {
				Output struct {
					Voice string `json:"voice"`
				} `json:"output"`
			} `json:"audio"`
		} `json:"session"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode: %v (raw: %s)", err, raw)
	}
	if got.Type != EventSessionUpdate {
		t.Fatalf("type = %q, want %q", got.Type, EventSessionUpdate)
	}
	if got.Session.Type != "realtime" {
		t.Fatalf("session.type = %q, want realtime", got.Session.Type)
	}
	if got.Session.Audio.Output.Voice != "cedar" {
		t.Fatalf("voice = %q, want cedar", got.Session.Audio.Output.Voice)
	}
}

// TestBuildVoiceUpdate_ForwardsTheVoiceUnmodified pins that this package does
// not validate, trim, or case-fold the name — the same rule WithVoice's doc
// states for the dial voice. What is a legal voice is the backend's to say.
func TestBuildVoiceUpdate_ForwardsTheVoiceUnmodified(t *testing.T) {
	const odd = "  Cedar-Mixed_Case.99  "
	raw, err := buildVoiceUpdate(odd)
	if err != nil {
		t.Fatalf("buildVoiceUpdate: %v", err)
	}
	var got struct {
		Session struct {
			Audio struct {
				Output struct {
					Voice string `json:"voice"`
				} `json:"output"`
			} `json:"audio"`
		} `json:"session"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode: %v (raw: %s)", err, raw)
	}
	if got.Session.Audio.Output.Voice != odd {
		t.Fatalf("voice = %q, want %q unmodified", got.Session.Audio.Output.Voice, odd)
	}
}
