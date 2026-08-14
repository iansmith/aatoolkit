package twilio

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// Tests for AATK-85 ("Declare tools in the realtime handshake and route
// function calls both directions") at the twilio wiring layer: WithTools
// (telephony/twilio/realtime.go) and the round trip a declared tool call
// takes through the existing WithServerEventChan (AATK-81) and
// WithClientEventChan (AATK-82) machinery — both merged before this ticket
// and reused here unmodified; see bridge.go's publishEvent-before-dispatch
// ordering, which is why an unmodelled function-call event already reaches
// Bridge.Events() today.
//
// The synthetic function-call event's wire shape below is invented for this
// test: the real protocol's field names are modelled nowhere in this
// codebase, and the ticket puts "what a tool is" entirely on the consumer.
// It follows the convention of encoding call arguments as a JSON string
// within the event, which is plausible but not verified against a live
// backend.

// toolsDeclaredRaw is the consumer's tool declaration for these tests,
// containing characters json.Marshal rewrites when it re-encodes a
// json.RawMessage (<, >, &) plus insignificant whitespace — the same
// technique clientEventRaw uses (realtime_clientevents_test.go) — so a test
// can tell verbatim wire delivery from a re-marshalled approximation.
const toolsDeclaredRaw = `[{"type":  "function","name":"lookup_weather","description":"<desc>&more"}]`

// --- observable behavior 1: declared tools reach the handshake -------------

// TestSessionUpdate_CarriesDeclaredToolsUnmodified mirrors
// TestSessionUpdate_CarriesSuppliedInstructions
// (realtime_instructions_test.go): a consumer-declared tools value must
// reach the backend's handshake exactly as supplied.
//
// slopstop:test contract
func TestSessionUpdate_CarriesDeclaredToolsUnmodified(t *testing.T) {
	be := newFakeRealtimeBackend(t)
	newRealtimeHarnessWith(t, NewStreamHandler(be.url(), WithTools(json.RawMessage(toolsDeclaredRaw))))

	waitHandshakes(t, be, 1, 5*time.Second)

	got := be.handshake(0)
	want := `"tools":` + toolsDeclaredRaw
	if !strings.Contains(got, want) {
		t.Fatalf("handshake must carry declared tools unmodified:\n got  %s\nwant substring %s", got, want)
	}
}

// TestSessionUpdate_UnsetToolsIsByteIdenticalToToday pins observable behavior
// 1's other half against the SAME frozen literal
// TestSessionUpdate_WithNoInstructionsIsByteIdenticalToToday guards
// (realtime_instructions_test.go): a consumer that never calls WithTools
// must get the exact handshake this engine sent before tools existed, not a
// near-equivalent carrying an empty array or a null field.
//
// slopstop:test regression — guards: "Unset omits the field entirely, byte-identical to today."
func TestSessionUpdate_UnsetToolsIsByteIdenticalToToday(t *testing.T) {
	be := newFakeRealtimeBackend(t)
	newRealtimeHarnessWith(t, NewStreamHandler(be.url()))

	waitHandshakes(t, be, 1, 5*time.Second)

	if got := be.handshake(0); got != handshakeBaseline {
		t.Fatalf("handshake with no tools declared must be byte-identical to today\n got %s\nwant %s", got, handshakeBaseline)
	}
}

// --- observable behaviors 2-4: the round trip -------------------------------

// toolCallEvent builds the backend's synthetic function-call event: a
// plausible, invented shape (see file comment) carrying callID and a
// JSON-string-encoded arguments payload.
func toolCallEvent(callID string) map[string]string {
	return map[string]string{
		"type":      "response.function_call_arguments.done",
		"call_id":   callID,
		"name":      "lookup_weather",
		"arguments": `{"city":"Springfield"}`,
	}
}

// toolOutputEvent builds the consumer's function-call output, returned to
// the backend on the client-event channel. Deliberately contains characters
// json.Marshal rewrites (<, >, &) plus insignificant whitespace, mirroring
// clientEventRaw's technique, so byte-for-byte delivery in THIS direction is
// provable too.
func toolOutputEvent(callID string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(
		`{"type":"conversation.item.create","item":{"type":"function_call_output","call_id":%q,  "output":"<result>72F &amp; sunny</result>"}}`,
		callID))
}

// TestRealtime_ToolCallRoundTripCompletesOnOneCall is the ticket's own
// RED-first case, and the one this ticket exists to prove: declare, call,
// return, continue, completing on a single call — observable behaviors 1
// through 4 together.
//
// It is deliberately ONE test rather than several, because the ticket calls
// out that declaration-only coverage proves the least ("the declaration is
// the easy third and passing it alone would prove the least"): this asserts
// the declared tools reach the handshake, THEN drives the backend to emit a
// function-call event naming one of them, THEN returns that call's output
// over WithClientEventChan, THEN asserts the backend received it verbatim,
// THEN asserts the call is still running (media still flows) rather than
// having ended as a side effect of any of the above.
//
// The round-trip machinery itself (WithServerEventChan, WithClientEventChan)
// is AATK-81/AATK-82's, already merged and unmodified by this ticket — see
// the file comment. What is new, and what this test is red for today, is
// that no declared tools ever reach the handshake: WithTools exists but is
// not yet wired into HandleStreamRealtime's dial call, so the first
// assertion below fails before the round trip is even attempted.
//
// slopstop:test contract
func TestRealtime_ToolCallRoundTripCompletesOnOneCall(t *testing.T) {
	const callID = "call-42"

	serverCh := make(chan ServerEvent, testChanBuffer)
	clientCh := make(chan json.RawMessage, testChanBuffer)

	be := newFakeRealtimeBackend(t)
	h := newRealtimeHarnessWith(t, NewStreamHandler(be.url(),
		WithTools(json.RawMessage(toolsDeclaredRaw)),
		WithServerEventChan(serverCh),
		WithClientEventChan(clientCh),
	))
	waitBackendReady(t, be, h)

	// 1. declare: the tools this call negotiated must be on the wire before
	// any call naming one of them can mean anything.
	wantTools := `"tools":` + toolsDeclaredRaw
	if got := be.handshake(0); !strings.Contains(got, wantTools) {
		t.Fatalf("handshake must declare the call's tools:\n got  %s\nwant substring %s", got, wantTools)
	}

	// 2. call: the backend emits a function-call event naming callID, and
	// the consumer observes it via WithServerEventChan.
	be.emitOnce(t, toolCallEvent(callID))

	var gotCallID string
	select {
	case ev := <-serverCh:
		if ev.Type != "response.function_call_arguments.done" {
			t.Fatalf("consumer observed type %q, want %q", ev.Type, "response.function_call_arguments.done")
		}
		var raw struct {
			CallID string `json:"call_id"`
		}
		if err := json.Unmarshal(ev.Raw, &raw); err != nil {
			t.Fatalf("Raw did not decode as the original frame: %v", err)
		}
		gotCallID = raw.CallID
	case <-time.After(5 * time.Second):
		t.Fatal("consumer never observed the backend's function-call event")
	}
	if gotCallID != callID {
		t.Fatalf("consumer observed call_id %q, want %q", gotCallID, callID)
	}

	// 3. return: the consumer answers the call it just observed, over
	// WithClientEventChan, and the backend must receive it byte-for-byte.
	output := toolOutputEvent(gotCallID)
	clientCh <- output

	deadline := time.After(5 * time.Second)
	for {
		found := false
		for _, raw := range be.receivedEvents() {
			if string(raw) == string(output) {
				found = true
				break
			}
		}
		if found {
			break
		}
		select {
		case <-deadline:
			t.Fatal("backend never received the function-call output verbatim")
		case <-time.After(10 * time.Millisecond):
		}
	}

	// 4. continue: the call must still be running, not ended as a side
	// effect of the round trip above — proven by driving one more frame of
	// audio through it, the liveness check the *_AbsentOptionIsTodaysBehaviour
	// tests use.
	seen := h.countMediaFrames(t)
	be.emitOnce(t, map[string]string{"type": "response.output_audio.delta", "delta": carrierPayloadB64()})
	waitFor(t, 5*time.Second, func() bool { return seen() > 0 })
	assertStillRunning(t, h, "a completed tool-call round trip must not end the call")
}
