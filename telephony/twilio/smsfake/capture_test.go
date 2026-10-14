package smsfake_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/iansmith/aatoolkit/telephony/twilio"
	"github.com/iansmith/aatoolkit/telephony/twilio/smsfake"
)

// smSID is the shape of the message id the capture server mints: "SM"
// followed by 32 lowercase hex characters, the shape of a real Twilio SID.
var smSID = regexp.MustCompile(`^SM[0-9a-f]{32}$`)

// freeTCPPort returns a port that was bindable on 127.0.0.1 a moment ago, by
// binding one and closing it again. Used to exercise an *explicit* bind
// without hardcoding a port number the machine may already be using.
func freeTCPPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a free port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatalf("release the reserved port: %v", err)
	}
	return port
}

// newCapture starts a capture server on a free port and closes it when the
// test ends.
func newCapture(t *testing.T) *smsfake.CaptureServer {
	t.Helper()
	capture, err := smsfake.NewCaptureServer(freeTCPPort(t))
	if err != nil {
		t.Fatalf("NewCaptureServer: %v", err)
	}
	t.Cleanup(capture.Close)
	return capture
}

// postMessage makes one Messages.json POST to capture for accountSID, the way
// RESTClient.SendSMS does, and returns the response with its body read.
func postMessage(t *testing.T, capture *smsfake.CaptureServer, accountSID string, form url.Values) (*http.Response, []byte) {
	t.Helper()
	endpoint := capture.URL + "/2010-04-01/Accounts/" + accountSID + "/Messages.json"
	resp, err := http.PostForm(endpoint, form)
	if err != nil {
		t.Fatalf("POST %s: %v", endpoint, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response from %s: %v", endpoint, err)
	}
	return resp, body
}

// TestNewCaptureServer_BindsRequestedPort pins that the capture server binds
// the port it was asked for, rather than an ephemeral one. The whole point is
// that the operator must be able to name the URL before the CLI runs — an
// ephemeral port makes that impossible, because the server reads
// TWILIO_API_BASE_URL once at startup.
func TestNewCaptureServer_BindsRequestedPort(t *testing.T) {
	port := freeTCPPort(t)

	capture, err := smsfake.NewCaptureServer(port)
	if err != nil {
		t.Fatalf("NewCaptureServer(%d): %v", port, err)
	}
	defer capture.Close()

	want := fmt.Sprintf(":%d", port)
	if !strings.Contains(capture.URL, want) {
		t.Errorf("capture server URL = %q, want it to contain %q (an explicit bind, not an ephemeral port)", capture.URL, want)
	}
}

// TestNewCaptureServer_PortInUseNamesThePort is the bind-failure edge: an
// occupied port fails immediately with an error the operator can act on — it
// names the port and the flag that changes it — rather than panicking the way
// httptest.NewServer does.
func TestNewCaptureServer_PortInUseNamesThePort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("hold a port: %v", err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	capture, err := smsfake.NewCaptureServer(port)
	if err == nil {
		capture.Close()
		t.Fatalf("NewCaptureServer(%d) succeeded against a held port, want a bind error", port)
	}
	if got := err.Error(); !strings.Contains(got, strconv.Itoa(port)) {
		t.Errorf("bind error %q does not name the port %d", got, port)
	}
	if got := err.Error(); !strings.Contains(got, "-capture-port") {
		t.Errorf("bind error %q does not mention -capture-port, the flag that changes it", got)
	}
}

// TestNewCaptureServer_RejectsUnusablePorts pins that port 0 does not fail
// open. net.Listen accepts it and assigns an ephemeral port, which is
// precisely the defect an explicit port exists to remove: the URL becomes
// unknowable before this process runs, and the guidance prints a ":0" the
// operator cannot point anything at. Out-of-range values net.Listen rejects on
// its own, but they are asserted here too so the boundary is pinned in one place.
func TestNewCaptureServer_RejectsUnusablePorts(t *testing.T) {
	for _, port := range []int{0, -1, 65536} {
		t.Run(fmt.Sprintf("port %d", port), func(t *testing.T) {
			capture, err := smsfake.NewCaptureServer(port)
			if err == nil {
				capture.Close()
				t.Fatalf("NewCaptureServer(%d) succeeded, want a rejection", port)
			}
			if got := err.Error(); !strings.Contains(got, strconv.Itoa(port)) {
				t.Errorf("error %q does not name the offending port %d", got, port)
			}
		})
	}
}

// TestCaptureServer_RespondsCreatedWithQueuedSID pins the response a send
// gets: 201, Content-Type application/json, and a JSON body of exactly two
// keys — a "sid" of "SM" plus 32 lowercase hex characters, and a "status" of
// the literal "queued".
func TestCaptureServer_RespondsCreatedWithQueuedSID(t *testing.T) {
	capture := newCapture(t)

	resp, body := postMessage(t, capture, "ACresponse000000000000000000000", url.Values{
		"From": {"+15105559999"},
		"To":   {"+15551234567"},
		"Body": {"hi back"},
	})

	if resp.StatusCode != http.StatusCreated {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusCreated)
	}
	if got, want := resp.Header.Get("Content-Type"), "application/json"; got != want {
		t.Errorf("Content-Type = %q, want %q", got, want)
	}
	var got map[string]string
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("response body %q is not a JSON object of strings: %v", body, err)
	}
	if len(got) != 2 {
		t.Errorf("response body %q has %d keys, want exactly sid and status", body, len(got))
	}
	if sid := got["sid"]; !smSID.MatchString(sid) {
		t.Errorf("sid = %q, want %q followed by 32 lowercase hex characters", sid, "SM")
	}
	if status := got["status"]; status != "queued" {
		t.Errorf("status = %q, want %q", status, "queued")
	}
}

// TestCaptureServer_SendSMSGetsTheSIDBack drives the capture server with the
// client it stands in for. RESTClient.SendSMS parses "sid" from the response
// to return the message id, and a 2xx carrying none yields an empty id rather
// than an error — so the id making it back is the only thing that notices the
// fake's response shape and SendSMS's parse diverging. It also pins what is
// recorded: the account from the path, and From, To and Body from the form.
func TestCaptureServer_SendSMSGetsTheSIDBack(t *testing.T) {
	const accountSID = "ACsendsms0000000000000000000000"
	capture := newCapture(t)

	rest := &twilio.RESTClient{AccountSID: accountSID, BaseURL: capture.URL}
	sid, err := rest.SendSMS(context.Background(), twilio.OutboundSMS{From: "+15105559999", To: "+15551234567", Body: "hi back"})
	if err != nil {
		t.Fatalf("SendSMS: %v", err)
	}
	if !smSID.MatchString(sid) {
		t.Errorf("SendSMS returned message SID %q, want the capture server's SM-prefixed id — the fake's response shape and SendSMS's parse have diverged", sid)
	}

	msgs := capture.Captured()
	if len(msgs) != 1 {
		t.Fatalf("capture server: got %d Messages.json POSTs, want 1", len(msgs))
	}
	if got := msgs[0].AccountSID; got != accountSID {
		t.Errorf("captured AccountSID = %q, want %q (the account in the request path)", got, accountSID)
	}
	if got := msgs[0].From; got != "+15105559999" {
		t.Errorf("captured From = %q, want %q", got, "+15105559999")
	}
	if got := msgs[0].To; got != "+15551234567" {
		t.Errorf("captured To = %q, want %q", got, "+15551234567")
	}
	if got := msgs[0].Body; got != "hi back" {
		t.Errorf("captured Body = %q, want %q", got, "hi back")
	}
}

// TestCaptureServer_CapturedIsASnapshot pins that Captured hands back a copy:
// a caller editing what it was given must not rewrite what the server
// recorded, and a later Captured must still see the original.
func TestCaptureServer_CapturedIsASnapshot(t *testing.T) {
	capture := newCapture(t)
	postMessage(t, capture, "ACsnapshot000000000000000000000", url.Values{"Body": {"first"}})

	first := capture.Captured()
	if len(first) != 1 {
		t.Fatalf("got %d recorded messages, want 1", len(first))
	}
	first[0].Body = "edited by the caller"

	if got := capture.Captured()[0].Body; got != "first" {
		t.Errorf("after editing the returned slice, Captured()[0].Body = %q, want %q", got, "first")
	}
}

// TestCaptureServer_NotifyIsBufferedByOneAndReceiveOnly pins the wake-up
// channel twilio-cli's runSMS waits on: one channel for the server's life,
// receive-only to callers, buffered by one, empty until something is
// recorded, and coalescing — two arrivals with nobody reading leave exactly
// one wake-up pending, and neither POST is blocked by the absent reader.
func TestCaptureServer_NotifyIsBufferedByOneAndReceiveOnly(t *testing.T) {
	capture := newCapture(t)
	notify := capture.Notify()

	if notify != capture.Notify() {
		t.Error("Notify returned a different channel on a second call, want the server's one wake-up channel")
	}
	if dir := reflect.TypeOf(notify).ChanDir(); dir != reflect.RecvDir {
		t.Errorf("Notify channel direction = %v, want receive-only", dir)
	}
	if got := cap(notify); got != 1 {
		t.Errorf("Notify channel capacity = %d, want 1", got)
	}

	select {
	case <-notify:
		t.Fatal("Notify delivered a wake-up before anything was recorded")
	default:
	}

	// The handler sends the wake-up before it writes the response, so once a
	// POST has been answered its wake-up is already pending.
	for _, body := range []string{"one", "two"} {
		if resp, _ := postMessage(t, capture, "ACnotify0000000000000000000000", url.Values{"Body": {body}}); resp.StatusCode != http.StatusCreated {
			t.Fatalf("POST %q: status %d, want %d", body, resp.StatusCode, http.StatusCreated)
		}
	}

	select {
	case <-notify:
	default:
		t.Fatal("no wake-up pending after two recorded messages, want one")
	}
	select {
	case <-notify:
		t.Error("a second wake-up was pending after two unread arrivals, want them coalesced into one")
	default:
	}
	if got := len(capture.Captured()); got != 2 {
		t.Errorf("recorded %d messages, want 2 — a coalesced wake-up must not lose the record", got)
	}
}
