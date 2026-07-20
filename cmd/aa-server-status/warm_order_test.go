package main

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iansmith/aatoolkit/config"
	"github.com/iansmith/aatoolkit/internal/lifecycle"
)

// recordingServer answers everything 200 and records each request in order.
func recordingServer(t *testing.T, order *[]string, mu *sync.Mutex, fail map[string]int) (host string, port int) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Method + " " + r.URL.Path
		mu.Lock()
		*order = append(*order, key)
		code := http.StatusOK
		if c, ok := fail[key]; ok {
			code = c
		}
		mu.Unlock()
		w.WriteHeader(code)
	}))
	t.Cleanup(srv.Close)

	hp := strings.TrimPrefix(srv.URL, "http://")
	h, p, _ := strings.Cut(hp, ":")
	n, err := strconv.Atoi(p)
	if err != nil {
		t.Fatalf("bad test server port %q: %v", p, err)
	}
	return h, n
}

func warmServer(t *testing.T, host string, port int) config.Server {
	t.Helper()
	s := config.Server{
		Name: "warmed", Type: config.TypeExec, Enabled: true,
		Host: host, Port: port,
		Health: config.Health{Path: "/healthz"},
	}
	if err := s.Warm.UnmarshalTOML(`POST /warm {"max_tokens":1}`); err != nil {
		t.Fatalf("warm config: %v", err)
	}
	return s
}

// TestPollReady_WarmsBeforeHealth pins the ordering: the warm-up request goes
// first, and only once it answers 2xx does the health gate open. Reversing
// them would defeat the whole point — a server that needs a warm-up is one
// whose health endpoint 200s while it is still incapable of work.
func TestPollReady_WarmsBeforeHealth(t *testing.T) {
	var (
		mu    sync.Mutex
		order []string
	)
	host, port := recordingServer(t, &order, &mu, nil)
	eng := NewEngine(config.Config{Supervisor: testSupervisor(t)})

	if err := eng.pollReady(warmServer(t, host, port), &lifecycle.Process{LogPath: "warmed.log"}); err != nil {
		t.Fatalf("pollReady: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(order) < 2 {
		t.Fatalf("expected a warm-up and a health probe, got: %v", order)
	}
	if order[0] != "POST /warm" {
		t.Errorf("first request was %q, want POST /warm — the warm-up must precede the health gate", order[0])
	}
	if order[1] != "GET /healthz" {
		t.Errorf("second request was %q, want GET /healthz", order[1])
	}
}

// TestPollReady_HealthNeverProbedIfWarmFails: a server whose warm-up never
// succeeds is not ready, and must not be health-probed into looking ready.
func TestPollReady_HealthNeverProbedIfWarmFails(t *testing.T) {
	var (
		mu    sync.Mutex
		order []string
	)
	host, port := recordingServer(t, &order, &mu, map[string]int{"POST /warm": http.StatusInternalServerError})

	sup := testSupervisor(t)
	sup.ReadyTimeout = config.Duration{Duration: 40 * time.Millisecond}
	eng := NewEngine(config.Config{Supervisor: sup})

	err := eng.pollReady(warmServer(t, host, port), &lifecycle.Process{LogPath: "warmed.log"})
	if err == nil {
		t.Fatal("pollReady must fail when the warm-up never returns 2xx")
	}

	mu.Lock()
	defer mu.Unlock()
	for _, req := range order {
		if req == "GET /healthz" {
			t.Fatalf("health was probed despite the warm-up failing; requests: %v", order)
		}
	}
}

// TestPollReady_NoWarmKeySkipsStraightToHealth: every server that doesn't
// declare a warm-up must behave exactly as it did before this existed.
func TestPollReady_NoWarmKeySkipsStraightToHealth(t *testing.T) {
	var (
		mu    sync.Mutex
		order []string
	)
	host, port := recordingServer(t, &order, &mu, nil)

	s := warmServer(t, host, port)
	s.Warm = config.Warm{} // no warm key

	eng := NewEngine(config.Config{Supervisor: testSupervisor(t)})
	if err := eng.pollReady(s, &lifecycle.Process{LogPath: "plain.log"}); err != nil {
		t.Fatalf("pollReady: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(order) != 1 || order[0] != "GET /healthz" {
		t.Errorf("a server with no warm key must only be health-probed, got: %v", order)
	}
}

// TestPollReady_WarmAndHealthShareOneReadyTimeout: ready_timeout answers "how
// long may this server take to become ready". A server does not become ready
// twice, so the warm-up and the health poll draw on one budget rather than
// each getting the full amount.
//
// The warm-up must be slow *and* succeed for the difference to show: a failing
// warm-up returns before health is ever polled, so only one budget is spent
// either way and the doubling is invisible. Here warm burns half the budget
// and passes; health then never succeeds.
//
//	shared:  warm(budget/2) + health(budget/2) ~= budget
//	doubled: warm(budget/2) + health(budget)   ~= 1.5 * budget
//
// This measures a timeout, which is irreducibly temporal -- the assertion is
// that the declared budget is honoured, not that a sleep outlasted something.
func TestPollReady_WarmAndHealthShareOneReadyTimeout(t *testing.T) {
	const budget = 200 * time.Millisecond

	var mu sync.Mutex
	var order []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Method + " " + r.URL.Path
		mu.Lock()
		order = append(order, key)
		mu.Unlock()
		if key == "POST /warm" {
			time.Sleep(budget / 2) // a model loading
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusInternalServerError) // health never comes good
	}))
	t.Cleanup(srv.Close)
	hp := strings.TrimPrefix(srv.URL, "http://")
	h, p, _ := strings.Cut(hp, ":")
	port, err := strconv.Atoi(p)
	if err != nil {
		t.Fatalf("bad test server port %q: %v", p, err)
	}

	sup := testSupervisor(t)
	sup.ReadyTimeout = config.Duration{Duration: budget}
	eng := NewEngine(config.Config{Supervisor: sup})

	start := time.Now()
	if err := eng.pollReady(warmServer(t, h, port), &lifecycle.Process{LogPath: "shared.log"}); err == nil {
		t.Fatal("pollReady must fail when health never becomes healthy")
	}
	elapsed := time.Since(start)

	if elapsed > budget*5/4 {
		t.Errorf("pollReady took %s against a declared ready_timeout of %s: the warm-up and the health poll are each getting the full budget instead of sharing it",
			elapsed, budget)
	}
}
