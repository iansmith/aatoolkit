package config

import "testing"

// The string form mirrors health's "METHOD /path" shorthand, with everything
// after the path taken verbatim as the body.
func TestWarm_StringForm(t *testing.T) {
	var w Warm
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":1}`
	if err := w.UnmarshalTOML("POST /v1/chat/completions " + body); err != nil {
		t.Fatalf("UnmarshalTOML: %v", err)
	}
	if w.Method != "POST" {
		t.Errorf("Method = %q, want POST", w.Method)
	}
	if w.Path != "/v1/chat/completions" {
		t.Errorf("Path = %q, want /v1/chat/completions", w.Path)
	}
	if w.Body != body {
		t.Errorf("Body must be verbatim.\n got: %s\nwant: %s", w.Body, body)
	}
}

// A warm-up need not carry a body — a bare "GET /path" is legal.
func TestWarm_StringForm_NoBody(t *testing.T) {
	var w Warm
	if err := w.UnmarshalTOML("GET /warm"); err != nil {
		t.Fatalf("UnmarshalTOML: %v", err)
	}
	if w.Method != "GET" || w.Path != "/warm" || w.Body != "" {
		t.Errorf("got %+v, want GET /warm with empty body", w)
	}
}

func TestWarm_StringForm_Rejects(t *testing.T) {
	cases := map[string]string{
		"no path":           "POST",
		"path not absolute": "POST v1/chat",
		"empty":             "",
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			var w Warm
			if err := w.UnmarshalTOML(in); err == nil {
				t.Errorf("UnmarshalTOML(%q) = nil error, want a rejection", in)
			}
		})
	}
}

// The zero value means "no warm-up" — every server that doesn't declare one
// must keep starting its health poll immediately.
func TestWarm_ZeroValueMeansNoWarmup(t *testing.T) {
	var s Server
	if s.Warm.Path != "" {
		t.Errorf("a server with no warm key must have an empty Warm.Path, got %q", s.Warm.Path)
	}
}
