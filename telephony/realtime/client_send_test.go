package realtime

import (
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"
)

// Client.Send tests for AATK-82, "Let a consumer send client events to the
// realtime backend mid-call". Before this ticket AppendAudio was the only
// client-originated write; Send opens the path for everything else the
// protocol supports (response.create, conversation.item.create, a mid-call
// session.update, function-call output) without this package modelling any
// of it — the consumer owns its own protocol correctness.
//
// The central risk the ticket states: a second writer on the same
// *websocket.Conn* is a hazard unless writes are serialised, because today
// AppendAudio is the connection's only writer and that is the only reason it
// is safe. Both tests below drive that risk directly.

// rawClientEvent builds a consumer-supplied event deliberately containing
// characters json.Marshal rewrites when it re-encodes a json.RawMessage
// (<, >, &) plus insignificant whitespace, so a test can tell verbatim wire
// delivery from a re-marshalled approximation. id disambiguates events when
// many are sent concurrently.
func rawClientEvent(id int) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(
		`{"type":"conversation.item.create","id":%d,  "tag":"<a>&b</a>"}`, id))
}

// --- byte-for-byte fidelity --------------------------------------------

// TestClientSend_EventReachesBackendByteForByte is the ticket's own RED-first
// case: a consumer-supplied event must reach the backend exactly as supplied.
// send()'s json.Marshal(json.RawMessage) re-escapes '<', '>' and '&' into
// </>/&, so this fails against the Phase 0 stub for a real
// reason, not merely because Send did not exist.
//
// slopstop:test contract
func TestClientSend_EventReachesBackendByteForByte(t *testing.T) {
	be := newFakeBackend(t)
	ctx := testCtx(t)

	c, err := Dial(ctx, be.url())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	event := rawClientEvent(0)
	if err := c.Send(ctx, event); err != nil {
		t.Fatalf("Send: %v", err)
	}

	var got string
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && got == "" {
		for _, raw := range be.events() {
			var probe struct {
				Type string `json:"type"`
			}
			// The handshake (session.update) is also recorded by the fake
			// backend; skip it so it can never be mistaken for the event
			// this test actually sent.
			if json.Unmarshal(raw, &probe) == nil && probe.Type == "conversation.item.create" {
				got = string(raw)
			}
		}
		if got == "" {
			time.Sleep(10 * time.Millisecond)
		}
	}

	if got != string(event) {
		t.Fatalf("consumer event must arrive byte-for-byte:\n got  %q\nwant %q", got, string(event))
	}
}

// --- concurrent AppendAudio and Send ------------------------------------

// TestAppendAudioAndSendConcurrently_CleanUnderRaceAndByteForByte is the
// ticket's central-risk test: AppendAudio and Send driven concurrently on one
// Client, hard enough that an unserialised write path would show it, and
// under -race so a data race on the connection is not just absent from the
// assertions but absent from the detector too.
//
// It also asserts every concurrently-Sent event lands byte-for-byte, which is
// what actually establishes RED at Phase 0: run with `go test -race`, the
// stub's json.Marshal-based Send corrupts every event's '<'/'>'/'&' exactly
// as the single-goroutine test above shows, so this fails whether or not any
// interleaving ever occurs. Run this file's test command with `-race
// -count=N` per the ticket's own instruction ("driven hard enough to fail
// without the mutex; remove the mutex and it must go red — mutation-check it,
// do not assume") — verifying that the *mutex specifically* is what a mutant
// must remove to flip this test is mutation-check's job, not this one's.
//
// slopstop:test contract
func TestAppendAudioAndSendConcurrently_CleanUnderRaceAndByteForByte(t *testing.T) {
	const workers = 50

	be := newFakeBackend(t)
	ctx := testCtx(t)

	c, err := Dial(ctx, be.url())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	var wg sync.WaitGroup
	errs := make(chan error, workers*2)

	for i := 0; i < workers; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			if err := c.AppendAudio(ctx, frameB64()); err != nil {
				errs <- fmt.Errorf("AppendAudio: %w", err)
			}
		}()
		id := i
		go func() {
			defer wg.Done()
			if err := c.Send(ctx, rawClientEvent(id)); err != nil {
				errs <- fmt.Errorf("Send(%d): %w", id, err)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent AppendAudio/Send call failed: %v", err)
	}

	// Wait for every message to arrive before checking content: the
	// backend's read loop and this test run concurrently.
	deadline := time.Now().Add(5 * time.Second)
	var appended, sent int
	var evs []json.RawMessage
	for time.Now().Before(deadline) {
		evs = be.events()
		appended, sent = 0, 0
		for _, raw := range evs {
			var probe struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(raw, &probe) != nil {
				continue
			}
			switch probe.Type {
			case EventAudioAppend:
				appended++
			case "conversation.item.create":
				sent++
			}
		}
		if appended == workers && sent == workers {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if appended != workers {
		t.Fatalf("backend received %d AppendAudio events, want %d", appended, workers)
	}
	if sent != workers {
		t.Fatalf("backend received %d Send events, want %d", sent, workers)
	}

	// Every one of the workers Send events must be byte-for-byte what this
	// test supplied — this is what fails against the marshal-based stub.
	gotByID := map[int]string{}
	for _, raw := range evs {
		var probe struct {
			Type string `json:"type"`
			ID   int    `json:"id"`
		}
		if json.Unmarshal(raw, &probe) == nil && probe.Type == "conversation.item.create" {
			gotByID[probe.ID] = string(raw)
		}
	}
	for i := 0; i < workers; i++ {
		want := string(rawClientEvent(i))
		if gotByID[i] != want {
			t.Errorf("event %d not byte-for-byte under concurrent load:\n got  %q\nwant %q", i, gotByID[i], want)
		}
	}
}
