package config

import (
	"testing"

	"github.com/BurntSushi/toml"
)

func decodeHealth(t *testing.T, tomlSrc string) Health {
	t.Helper()
	var v struct {
		Health Health `toml:"health"`
	}
	if _, err := toml.Decode(tomlSrc, &v); err != nil {
		t.Fatalf("decoding %q: %v", tomlSrc, err)
	}
	return v.Health
}

func decodeHealthErr(t *testing.T, tomlSrc string) error {
	t.Helper()
	var v struct {
		Health Health `toml:"health"`
	}
	_, err := toml.Decode(tomlSrc, &v)
	return err
}

func TestHealth_UnmarshalTOML_TableForm(t *testing.T) {
	h := decodeHealth(t, `health = { host = "127.0.0.1", port = 2019, path = "/config/" }`)
	want := Health{Host: "127.0.0.1", Port: 2019, Path: "/config/"}
	if h != want {
		t.Errorf("got %+v, want %+v", h, want)
	}
}

func TestHealth_UnmarshalTOML_StringForm(t *testing.T) {
	h := decodeHealth(t, `health = "GET /spend?prefix=SOP"`)
	want := Health{Path: "/spend?prefix=SOP"}
	if h != want {
		t.Errorf("got %+v, want %+v (host/port left unset, default to the server's own)", h, want)
	}
}

func TestHealth_UnmarshalTOML_StringForm_CaseInsensitiveMethod(t *testing.T) {
	h := decodeHealth(t, `health = "get /healthz"`)
	if h.Path != "/healthz" {
		t.Errorf("got path %q, want /healthz", h.Path)
	}
}

func TestHealth_UnmarshalTOML_StringForm_RejectsNonGET(t *testing.T) {
	if err := decodeHealthErr(t, `health = "POST /spend"`); err == nil {
		t.Fatal("expected error for non-GET method in string form, got nil")
	}
}

func TestHealth_UnmarshalTOML_StringForm_RejectsMissingPath(t *testing.T) {
	if err := decodeHealthErr(t, `health = "GET"`); err == nil {
		t.Fatal("expected error for string form with no path, got nil")
	}
}

func TestHealth_UnmarshalTOML_RejectsOtherTypes(t *testing.T) {
	if err := decodeHealthErr(t, `health = 42`); err == nil {
		t.Fatal("expected error for non-string, non-table health value, got nil")
	}
}
