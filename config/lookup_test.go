package config

import "testing"

// TestConfig_ServerByName_Found covers observable behavior 1: a caller can
// look up a named server entry without hand-rolling a loop over c.Servers.
func TestConfig_ServerByName_Found(t *testing.T) {
	cfg := Config{Servers: []Server{
		{Name: "server", Listens: []int{9730, 9740}},
		{Name: "caddy", Listens: []int{80, 443, 2019}},
	}}

	s, ok := cfg.ServerByName("server")
	if !ok {
		t.Fatal("expected server server to be found")
	}
	if s.Name != "server" {
		t.Errorf("got server %+v, want name %q", s, "server")
	}
}

func TestConfig_ServerByName_NotFound(t *testing.T) {
	cfg := Config{Servers: []Server{
		{Name: "server", Listens: []int{9730, 9740}},
	}}

	_, ok := cfg.ServerByName("does-not-exist")
	if ok {
		t.Fatal("expected does-not-exist to not be found")
	}
}

// TestConfig_ServerByName_EmptyServers is the boundary case: a Config with
// no server entries at all must not panic and must report not-found.
func TestConfig_ServerByName_EmptyServers(t *testing.T) {
	cfg := Config{}

	_, ok := cfg.ServerByName("server")
	if ok {
		t.Fatal("expected not-found on a Config with no servers")
	}
}

// TestServer_WebhookPort_TwoListens covers observable behavior 2: resolving
// a named port role (the webhook/streams port) without the caller hardcoding
// an index into Listens.
func TestServer_WebhookPort_TwoListens(t *testing.T) {
	s := Server{Name: "server", Listens: []int{9730, 9740}}

	port, ok := s.WebhookPort()
	if !ok {
		t.Fatal("expected a webhook port to be resolved")
	}
	if port != 9740 {
		t.Errorf("webhook port = %d, want 9740", port)
	}
}

// TestServer_WebhookPort_OneListen is the edge case: a server declaring only
// its primary listen has no distinct webhook port.
func TestServer_WebhookPort_OneListen(t *testing.T) {
	s := Server{Name: "chat-llm", Listens: []int{8080}}

	_, ok := s.WebhookPort()
	if ok {
		t.Fatal("expected no webhook port for a server with a single listen")
	}
}

// TestServer_WebhookPort_NoListens is the boundary case: a server with no
// declared listens (e.g. an mlx/python entry using Port instead) has no
// webhook port.
func TestServer_WebhookPort_NoListens(t *testing.T) {
	s := Server{Name: "voice-in", Port: 8090}

	_, ok := s.WebhookPort()
	if ok {
		t.Fatal("expected no webhook port for a server with no listens")
	}
}

// TestServer_WebhookPort_MoreThanTwoListens locks the convention: the
// webhook port is always Listens[1], regardless of how many trailing
// entries follow (e.g. caddy's admin port at Listens[2]).
func TestServer_WebhookPort_MoreThanTwoListens(t *testing.T) {
	s := Server{Name: "caddy", Listens: []int{80, 443, 2019}}

	port, ok := s.WebhookPort()
	if !ok {
		t.Fatal("expected a webhook port to be resolved")
	}
	if port != 443 {
		t.Errorf("webhook port = %d, want 443 (Listens[1])", port)
	}
}
