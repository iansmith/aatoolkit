package interp

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func writePolicy(t *testing.T, src string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "policy.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestLoad_EvalsSymbol covers the core mechanism with no injection: read a dir,
// compile it as a named package, evaluate a function symbol, type-assert, call it.
func TestLoad_EvalsSymbol(t *testing.T) {
	dir := writePolicy(t, "package policy\n\nfunc Answer() int { return 42 }\n")

	v, err := Load(dir, "policy", nil, "policy.Answer")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	fn, ok := v.Interface().(func() int)
	if !ok {
		t.Fatalf("wrong type: got %T", v.Interface())
	}
	if got := fn(); got != 42 {
		t.Errorf("Answer() = %d, want 42", got)
	}
}

// Speaker is an interface injected into the interpreter so policy code can import
// and name it — the pattern the driver uses to hand its host to interpreted policy.
type Speaker interface{ Say() string }

type fixedSpeaker struct{ s string }

func (f fixedSpeaker) Say() string { return f.s }

// TestLoad_InjectsHostInterface covers the injection path: an interface is exposed
// under an import path, interpreted code imports it, and Load returns a function
// taking that interface which the caller invokes with a concrete implementation.
func TestLoad_InjectsHostInterface(t *testing.T) {
	// The policy imports the import PATH ("example.com/host"); yaegi's Exports key
	// is that path plus the package name ("example.com/host/host").
	dir := writePolicy(t, `package policy

import "example.com/host"

func Echo(s host.Speaker) string { return s.Say() }
`)

	inject := Inject{
		"example.com/host/host": {"Speaker": reflect.ValueOf((*Speaker)(nil))},
	}
	v, err := Load(dir, "policy", inject, "policy.Echo")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	fn, ok := v.Interface().(func(Speaker) string)
	if !ok {
		t.Fatalf("wrong type: got %T", v.Interface())
	}
	if got := fn(fixedSpeaker{"hello"}); got != "hello" {
		t.Errorf("Echo() = %q, want %q", got, "hello")
	}
}

// TestLoad_EmptyDir errors rather than returning a zero interpreter.
func TestLoad_EmptyDir(t *testing.T) {
	if _, err := Load(t.TempDir(), "policy", nil, "policy.Answer"); err == nil {
		t.Fatal("expected an error loading an empty dir")
	}
}
