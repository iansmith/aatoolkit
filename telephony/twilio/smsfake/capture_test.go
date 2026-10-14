package smsfake_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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
// test ends, within SubscribeWait.
func newCapture(t *testing.T) *smsfake.CaptureServer {
	t.Helper()
	capture, err := smsfake.NewCaptureServer(freeTCPPort(t))
	if err != nil {
		t.Fatalf("NewCaptureServer: %v", err)
	}
	t.Cleanup(func() { returnsWithin(t, "Close at the end of the test", capture.Close) })
	return capture
}

// postClient makes the tests' POSTs. Its Timeout bounds each one by
// SubscribeWait, so a handler that never answers fails the POST rather than
// hanging the test. It uses http.DefaultTransport, whose idle connections
// httptest's Close drops, so a POST after Close dials afresh.
var postClient = &http.Client{Timeout: smsfake.SubscribeWait}

// messagesPath is the path of accountSID's Messages.json endpoint, the one
// RESTClient.SendSMS POSTs to.
func messagesPath(accountSID string) string {
	return "/2010-04-01/Accounts/" + accountSID + "/Messages.json"
}

// urlencoded is the Content-Type RESTClient.SendSMS posts its form as, the one
// body encoding the capture server reads.
const urlencoded = "application/x-www-form-urlencoded"

// sendRaw makes one POST through postClient to capture's Messages.json
// endpoint for accountSID, with query appended to the URL, body sent as given,
// and a Content-Type of contentType, or none when that is empty. It returns
// the response with its body read, and reports failure as an error rather than
// through a *testing.T, so a goroutine other than the test's own can call it.
func sendRaw(capture *smsfake.CaptureServer, accountSID, query, contentType, body string) (*http.Response, []byte, error) {
	endpoint := capture.URL + messagesPath(accountSID) + query
	req, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(body))
	if err != nil {
		return nil, nil, fmt.Errorf("build the POST to %s: %v", endpoint, err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := postClient.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("POST %s: %v", endpoint, err)
	}
	defer resp.Body.Close()
	read, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("read response from %s: %v", endpoint, err)
	}
	return resp, read, nil
}

// postRaw is sendRaw as subscribeAccount, for the test's own goroutine: a
// failed POST fails the test.
func postRaw(t *testing.T, capture *smsfake.CaptureServer, query, contentType, body string) *http.Response {
	t.Helper()
	resp, _, err := sendRaw(capture, subscribeAccount, query, contentType, body)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// sendMessage makes one Messages.json POST to capture for accountSID, the way
// RESTClient.SendSMS does, and returns the response with its body read. It
// reports failure as an error rather than through a *testing.T, so a goroutine
// other than the test's own can call it.
func sendMessage(capture *smsfake.CaptureServer, accountSID string, form url.Values) (*http.Response, []byte, error) {
	return sendRaw(capture, accountSID, "", urlencoded, form.Encode())
}

// postMessage is sendMessage for the test's own goroutine: a failed POST fails
// the test.
func postMessage(t *testing.T, capture *smsfake.CaptureServer, accountSID string, form url.Values) (*http.Response, []byte) {
	t.Helper()
	resp, body, err := sendMessage(capture, accountSID, form)
	if err != nil {
		t.Fatal(err)
	}
	return resp, body
}

// sendSMS sends sms to capture as accountSID through twilio.RESTClient.SendSMS,
// the client the capture server stands in for, and returns the message SID
// SendSMS parsed from the response. The client POSTs through postClient, so a
// handler that never answers fails the send within SubscribeWait. A failed
// send fails the test.
func sendSMS(t *testing.T, capture *smsfake.CaptureServer, accountSID string, sms twilio.OutboundSMS) string {
	t.Helper()
	rest := &twilio.RESTClient{AccountSID: accountSID, BaseURL: capture.URL, HTTPClient: postClient}
	sid, err := rest.SendSMS(context.Background(), sms)
	if err != nil {
		t.Fatalf("SendSMS: %v", err)
	}
	return sid
}

// requireOneRecorded returns what Captured holds, failing the test unless it
// holds exactly one message.
func requireOneRecorded(t *testing.T, capture *smsfake.CaptureServer) []smsfake.Message {
	t.Helper()
	recorded := capture.Captured()
	if len(recorded) != 1 {
		t.Fatalf("got %d recorded messages, want 1", len(recorded))
	}
	return recorded
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

	sid := sendSMS(t, capture, accountSID, twilio.OutboundSMS{From: "+15105559999", To: "+15551234567", Body: "hi back"})
	if !smSID.MatchString(sid) {
		t.Errorf("SendSMS returned message SID %q, want the capture server's SM-prefixed id — the fake's response shape and SendSMS's parse have diverged", sid)
	}

	msgs := requireOneRecorded(t, capture)
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

	first := requireOneRecorded(t, capture)
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
	if err := postBodies(capture, []string{"one", "two"}); err != nil {
		t.Fatal(err)
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

// TestCaptureServer_NotifyFiresWithSubscribersAttached pins that subscribing
// leaves the wake-up channel as it was. With one subscriber reading and one
// never reading, a recorded message still leaves a wake-up pending on Notify,
// two unread arrivals still coalesce into one, and the reading subscriber
// receives the same messages: both signals fire for every arrival.
func TestCaptureServer_NotifyFiresWithSubscribersAttached(t *testing.T) {
	capture := newCapture(t)
	notify := capture.Notify()
	subscribe(t, capture) // never read
	posted := []string{"one", "two", "three"}
	reader := collectEach(t, capture, 1, len(posted))

	// The handler sends the wake-up before it writes the response, so once a
	// POST has been answered its wake-up is already pending.
	if err := postBodies(capture, posted[:1]); err != nil {
		t.Fatal(err)
	}
	select {
	case <-notify:
	default:
		t.Fatal("no wake-up pending after a message recorded with subscribers attached, want one")
	}

	if err := postBodies(capture, posted[1:]); err != nil {
		t.Fatal(err)
	}
	select {
	case <-notify:
	default:
		t.Fatal("no wake-up pending after two more messages recorded with subscribers attached, want one")
	}
	select {
	case <-notify:
		t.Error("a second wake-up was pending after two unread arrivals with subscribers attached, want them coalesced into one")
	default:
	}

	requireEachReceived(t, reader, posted, "reading subscriber")
}

// subscribeAccount is the account postBodies and the fan-out tests post as.
// Nothing they assert depends on it.
const subscribeAccount = "ACsubscribe00000000000000000000"

// backlog is how many messages the slow-subscriber tests queue for a
// subscriber that is not reading. Its feed holds one of them in hand, so its
// queue reaches backlog-1: a queue bounded below that which blocks or drops
// when full is caught, and one bounded at or above it is not. No finite backlog
// catches every bound; this one sits well above the buffer sizes a bounded
// queue would plausibly use, 512 to 4096. The tests queue it through
// serveBodies, because a loopback round trip per message would cost seconds
// under -race.
const backlog = 10000

// numbered returns n distinct message bodies, prefix followed by a counter,
// in posting order.
func numbered(prefix string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("%s %03d", prefix, i)
	}
	return out
}

// postBodies POSTs one message per body over HTTP, in order, each answered
// before the next is sent, and returns the first failure: a transport error or
// a status other than 201. It reports through its error, so any goroutine can
// call it.
func postBodies(capture *smsfake.CaptureServer, bodies []string) error {
	return eachCreated(bodies, func(form url.Values) (int, error) {
		resp, _, err := sendMessage(capture, subscribeAccount, form)
		if err != nil {
			return 0, err
		}
		return resp.StatusCode, nil
	})
}

// serveBodies is postBodies without the network: each POST is handed straight
// to capture's handler on the calling goroutine. It is what lets the
// slow-subscriber tests queue backlog messages in well under a second, and the
// ordering test race hundreds of POSTs without a connection apiece. It
// drives the same handler a POST reaches, so a handler that blocks blocks
// serveBodies' caller just as it would hold up a POST's response.
func serveBodies(capture *smsfake.CaptureServer, bodies []string) error {
	return eachCreated(bodies, func(form url.Values) (int, error) {
		return serveForm(capture, messagesPath(subscribeAccount), strings.NewReader(form.Encode())).Code, nil
	})
}

// serveForm hands one POST to path, its body read from form and sent as
// urlencoded, straight to capture's mux on the calling goroutine — no
// connection, so nothing follows a redirect — and returns the response it got.
func serveForm(capture *smsfake.CaptureServer, path string, form io.Reader) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, form)
	req.Header.Set("Content-Type", urlencoded)
	rec := httptest.NewRecorder()
	capture.Config.Handler.ServeHTTP(rec, req)
	return rec
}

// serveWithin is serveBodies bounded by returnsWithin, for the test's own
// goroutine: a POST answered other than 201 fails the test. what names the
// POSTs if they stall.
func serveWithin(t *testing.T, capture *smsfake.CaptureServer, what string, bodies []string) {
	t.Helper()
	var err error
	returnsWithin(t, what, func() { err = serveBodies(capture, bodies) })
	if err != nil {
		t.Fatal(err)
	}
}

// eachCreated sends one message per body, in order, through send, which
// returns the response status, and returns the first failure: an error from
// send or a status other than 201.
func eachCreated(bodies []string, send func(form url.Values) (status int, err error)) error {
	for _, body := range bodies {
		status, err := send(url.Values{"Body": {body}})
		if err != nil {
			return err
		}
		if status != http.StatusCreated {
			return fmt.Errorf("POST %q: status %d, want %d", body, status, http.StatusCreated)
		}
	}
	return nil
}

// collect receives exactly n messages from msgs, and returns an error if the
// channel closes first or they have not all arrived within SubscribeWait. It
// reports through its error, so any goroutine can call it.
func collect(msgs <-chan smsfake.Message, n int) ([]smsfake.Message, error) {
	return collectUntil(msgs, fmt.Sprintf("all %d messages", n), func(got []smsfake.Message) bool {
		return len(got) == n
	})
}

// collectThrough receives from msgs up to and including the first message
// whose Body is last, failing as collect does.
func collectThrough(msgs <-chan smsfake.Message, last string) ([]smsfake.Message, error) {
	return collectUntil(msgs, fmt.Sprintf("the message %q", last), func(got []smsfake.Message) bool {
		return len(got) > 0 && got[len(got)-1].Body == last
	})
}

// collectUntil receives from msgs until done reports true of everything
// received so far, and returns an error if the channel closes first or done
// has not been met within SubscribeWait. want names what done waits for.
func collectUntil(msgs <-chan smsfake.Message, want string, done func(got []smsfake.Message) bool) ([]smsfake.Message, error) {
	deadline := time.NewTimer(smsfake.SubscribeWait)
	defer deadline.Stop()
	var got []smsfake.Message
	for !done(got) {
		select {
		case msg, ok := <-msgs:
			if !ok {
				return got, fmt.Errorf("subscription closed after %d messages, before %s arrived", len(got), want)
			}
			got = append(got, msg)
		case <-deadline.C:
			return got, fmt.Errorf("received %d messages within %s, not %s", len(got), smsfake.SubscribeWait, want)
		}
	}
	return got, nil
}

// bodiesOf returns the Body of each message, in order.
func bodiesOf(msgs []smsfake.Message) []string {
	out := make([]string, len(msgs))
	for i, msg := range msgs {
		out[i] = msg.Body
	}
	return out
}

// feed is one subscriber's result from collect: what it received, and the
// error collect returned.
type feed struct {
	got []smsfake.Message
	err error
}

// collectEach makes readers subscriptions through subscribe and starts, for
// each, a goroutine that collects n messages from it. It returns one channel
// per subscriber, which delivers that subscriber's feed once.
func collectEach(t *testing.T, capture *smsfake.CaptureServer, readers, n int) []chan feed {
	t.Helper()
	feeds := make([]chan feed, readers)
	for i := range feeds {
		msgs, _ := subscribe(t, capture)
		feeds[i] = make(chan feed, 1)
		go func() {
			got, err := collect(msgs, n)
			feeds[i] <- feed{got, err}
		}()
	}
	return feeds
}

// requireEachReceived waits for every feed and fails the test unless each
// received exactly posted, in posted order. who names a subscriber in a
// failure.
func requireEachReceived(t *testing.T, feeds []chan feed, posted []string, who string) {
	t.Helper()
	for i, ch := range feeds {
		f := <-ch
		if f.err != nil {
			t.Errorf("%s %d: %v", who, i, f.err)
			continue
		}
		got := bodiesOf(f.got)
		if reflect.DeepEqual(got, posted) {
			continue
		}
		first := 0
		for first < len(got) && first < len(posted) && got[first] == posted[first] {
			first++
		}
		t.Errorf("%s %d received %d messages, want the %d posted, in posted order; they differ from message %d", who, i, len(got), len(posted), first)
	}
}

// requireClosed fails the test unless msgs is already closed: a receive on it
// must report the channel closed at once — not deliver a message, and not
// block. It does not wait, because unsubscribe and Close promise the channel
// is closed by the time they return.
//
// To hold them to that, call it on the goroutine that called unsubscribe or
// Close, straight after the call returns. Waking another goroutine to check
// instead gives an unsubscribe or Close that returned early, leaving the
// closing to goroutines of its own, time to finish it, and so passes it. It
// fails only through t.Errorf, so any goroutine may call it.
func requireClosed(t *testing.T, msgs <-chan smsfake.Message, which string) {
	t.Helper()
	select {
	case msg, ok := <-msgs:
		if ok {
			t.Errorf("%s delivered %q, want the subscription closed", which, msg.Body)
		}
	default:
		t.Errorf("%s is still open, want it closed by the time unsubscribe or Close returned", which)
	}
}

// stillOpen reports whether msgs is not yet closed: a receive on it delivers a
// message or would block, rather than reporting the channel closed at once.
// It is requireClosed for a test that counts many channels rather than
// reporting each one, and is called as requireClosed says to be: without
// waiting, on the goroutine whose call was to have closed msgs, straight after
// that call returns.
func stillOpen(msgs <-chan smsfake.Message) bool {
	select {
	case _, ok := <-msgs:
		return ok
	default:
		return true
	}
}

// requireSubscribers fails the test unless capture holds exactly want
// subscriptions. when says at what point in the test it was checked.
func requireSubscribers(t *testing.T, capture *smsfake.CaptureServer, want int, when string) {
	t.Helper()
	if got := capture.SubscriberCount(); got != want {
		t.Errorf("%s, the server holds %d subscriptions, want %d", when, got, want)
	}
}

// subscribe is capture.Subscribe for a test that does not exercise
// unsubscribe's own edges. The unsubscribe it returns is bounded by
// returnsWithin, and it also runs when the test ends — before newCapture's
// Close, as cleanups run last in, first out — so a feed that will not stop
// fails the run within SubscribeWait, naming the test, rather than hanging it
// until go test's -timeout.
func subscribe(t *testing.T, capture *smsfake.CaptureServer) (<-chan smsfake.Message, func()) {
	t.Helper()
	msgs, unsubscribe := capture.Subscribe()
	return msgs, boundedUnsubscribe(t, unsubscribe)
}

// boundedUnsubscribe returns unsubscribe bounded by returnsWithin, and
// registers that bounded call to run when the test ends.
func boundedUnsubscribe(t *testing.T, unsubscribe func()) func() {
	bounded := func() { returnsWithin(t, "unsubscribe", unsubscribe) }
	t.Cleanup(bounded)
	return bounded
}

// returnsWithin runs fn on a goroutine of its own and, if it has not returned
// within SubscribeWait, crashes the test binary with a panic naming the test
// and what stalled. Anything fn writes is safe to read once returnsWithin has
// returned.
//
// It does not fail through t. The stall it guards against is a handler stuck
// while holding the server's lock. A t.Fatalf there, or a panic on the test's
// own goroutine, would first run the test's cleanups — its unsubscribes, which
// block on that lock, and its Close, which waits on a stuck POST — and the
// first of those to stall would report itself instead, a SubscribeWait later,
// losing this message. A panic on a fresh goroutine runs none of the test's
// cleanups, so it reports at once.
func returnsWithin(t *testing.T, what string, fn func()) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(smsfake.SubscribeWait):
		msg := fmt.Sprintf("%s: %s did not return within %s", t.Name(), what, smsfake.SubscribeWait)
		go func() { panic(msg) }()
		select {} // the panic ends the process; returning would run the blocking defers
	}
}

// TestCapture_RetainsWholePostedForm pins full-fidelity capture: a send
// carrying From, To, Body and StatusCallback is recorded with every one of
// those fields in its Form, exactly as posted — the StatusCallback included.
// TestCapture_StatusCallbackIsNeverCalled pins that it is recorded and nothing
// more.
func TestCapture_RetainsWholePostedForm(t *testing.T) {
	capture := newCapture(t)

	sms := twilio.OutboundSMS{
		From:           "+15105559999",
		To:             "+15551234567",
		Body:           "hi back",
		StatusCallback: "http://127.0.0.1:9/status?ref=fidelity",
	}
	sendSMS(t, capture, subscribeAccount, sms)

	msgs := requireOneRecorded(t, capture)
	want := url.Values{
		"From":           {sms.From},
		"To":             {sms.To},
		"Body":           {sms.Body},
		"StatusCallback": {sms.StatusCallback},
	}
	if got := msgs[0].Form; !reflect.DeepEqual(got, want) {
		t.Errorf("recorded Form = %v, want every posted field: %v", got, want)
	}
}

// TestCapture_StatusCallbackIsNeverCalled pins that a StatusCallback is
// recorded and nothing more: the capture server is not the party a delivery
// status is addressed to, so it never requests the callback URL. The URL
// points at a listener that counts every request it receives. A callback made
// while the send is handled has been counted by the time SendSMS returns. One
// started on a goroutine of its own can land later, so once the capture server
// has closed, the listener is given callbackGrace — the bound on a negative
// check, not a wait for delivery — and then closed, which waits for any
// request it has begun.
func TestCapture_StatusCallbackIsNeverCalled(t *testing.T) {
	const callbackGrace = 200 * time.Millisecond
	var calls atomic.Int32
	callback := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		calls.Add(1)
	}))
	defer callback.Close()
	capture := newCapture(t)

	sms := twilio.OutboundSMS{
		From:           "+15105559999",
		To:             "+15551234567",
		Body:           "track me",
		StatusCallback: callback.URL + "/status?ref=never-called",
	}
	sendSMS(t, capture, subscribeAccount, sms)
	if n := calls.Load(); n != 0 {
		t.Fatalf("the capture server requested the StatusCallback %d time(s) while handling the send, want never", n)
	}

	returnsWithin(t, "Close", capture.Close)
	time.Sleep(callbackGrace)
	returnsWithin(t, "closing the StatusCallback listener", callback.Close)
	if n := calls.Load(); n != 0 {
		t.Errorf("the capture server requested the StatusCallback %d time(s) by the time it had closed, want never", n)
	}
	if got := requireOneRecorded(t, capture)[0].Form.Get("StatusCallback"); got != sms.StatusCallback {
		t.Errorf("recorded StatusCallback = %q, want %q", got, sms.StatusCallback)
	}
}

// TestCapture_RetainsFieldsItDoesNotKnow pins that the record is the posted
// form itself rather than a list of known fields read back out of it: a field
// SendSMS never sends is kept, and a repeated field keeps every value in the
// order posted. The values are posted out of sorted order, so a capture that
// sorts or otherwise normalises them is caught.
//
// The POST's URL also carries a query string, which is not part of the posted
// form. It repeats the posted Body and adds a From that was never posted, so a
// record that mixed the query in would show a second Body value in its Form, a
// From key in its Form, or a From read back out of the query.
func TestCapture_RetainsFieldsItDoesNotKnow(t *testing.T) {
	capture := newCapture(t)

	posted := url.Values{
		"Body":            {"two pictures"},
		"MediaUrl":        {"https://example.com/2.png", "https://example.com/1.png"},
		"ProvideFeedback": {"true"},
	}
	const query = "?Body=from-the-query&From=from-the-query"
	if resp := postRaw(t, capture, query, urlencoded, posted.Encode()); resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST with query %s: status %d, want %d", query, resp.StatusCode, http.StatusCreated)
	}

	msgs := requireOneRecorded(t, capture)
	if got := msgs[0].Form; !reflect.DeepEqual(got, posted) {
		t.Errorf("recorded Form = %v, want exactly what was posted, without the URL's query: %v", got, posted)
	}
	if got := msgs[0].From; got != "" {
		t.Errorf("recorded From = %q, want it empty: no From was posted, only put in the URL's query", got)
	}
}

// TestCapture_StatusCallbackAbsentWhenNotSent pins that a send without a
// StatusCallback is recorded without one: the key is absent from the Form, not
// present with an empty value. The two are different requests — only a
// present StatusCallback asks the provider for anything — so the record must
// keep them apart; TestCapture_StatusCallbackKeptWhenSentEmpty pins the other
// direction. The whole-Form comparison also keeps the absence check from
// passing on a record with no Form at all.
func TestCapture_StatusCallbackAbsentWhenNotSent(t *testing.T) {
	capture := newCapture(t)

	sms := twilio.OutboundSMS{From: "+15105559999", To: "+15551234567", Body: "no callback"}
	sendSMS(t, capture, subscribeAccount, sms)

	msgs := requireOneRecorded(t, capture)
	if vs, present := msgs[0].Form["StatusCallback"]; present {
		t.Errorf("recorded Form has StatusCallback = %q, want the key absent — none was sent", vs)
	}
	want := url.Values{"From": {sms.From}, "To": {sms.To}, "Body": {sms.Body}}
	if got := msgs[0].Form; !reflect.DeepEqual(got, want) {
		t.Errorf("recorded Form = %v, want %v", got, want)
	}
}

// TestCapture_StatusCallbackKeptWhenSentEmpty is the other half of keeping
// "sent empty" and "not sent" apart: a StatusCallback posted with an empty
// value is recorded as present with that one empty value, not dropped. SendSMS
// never posts an empty StatusCallback, so the form is posted directly. Captured
// and a subscriber each get a copy made on its own, so both are checked.
func TestCapture_StatusCallbackKeptWhenSentEmpty(t *testing.T) {
	capture := newCapture(t)
	msgs, _ := subscribe(t, capture)

	posted := url.Values{"Body": {"empty callback"}, "StatusCallback": {""}}
	postMessage(t, capture, subscribeAccount, posted)

	recorded := requireOneRecorded(t, capture)
	delivered, err := collect(msgs, 1)
	if err != nil {
		t.Fatalf("subscriber: %v", err)
	}
	for _, view := range []struct {
		who  string
		form url.Values
	}{
		{"Captured", recorded[0].Form},
		{"the subscriber", delivered[0].Form},
	} {
		if !reflect.DeepEqual(view.form, posted) {
			t.Errorf("%s's Form = %q, want exactly what was posted, StatusCallback present and empty: %q", view.who, view.form, posted)
		}
	}
}

// TestCapture_MalformedPostIsRejectedNotRecorded pins that a POST whose URL
// query or posted form does not parse is refused with 400 and goes nowhere: it
// is not recorded, it sends no wake-up, and no subscriber receives it. Each
// case breaks one part of the POST and leaves the rest well formed, so only
// that part can be what is refused: the body, the URL query, or the
// Content-Type, which the posted form cannot be read without. Go's form parser
// keeps the pairs it could read alongside its error, so a handler that ignored
// the error would record a thinned message — for the malformed body, one
// without its Body, the pair that fails to parse — and a handler that parsed
// only the body would record the POST whose query does not parse, whole. The
// subscriber's first delivery being the well-formed message posted next shows
// nothing was queued for it, without waiting on a silence.
func TestCapture_MalformedPostIsRejectedNotRecorded(t *testing.T) {
	for _, tc := range []struct {
		name, query, contentType, body string
	}{
		{"a body that does not parse", "", urlencoded, "From=a&Body=%zz&To=b"},
		{"a URL query that does not parse", "?Body=%zz", urlencoded, "From=a&Body=ok&To=b"},
		{"a Content-Type that does not parse", "", urlencoded + "; charset", "From=a&Body=ok&To=b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			capture := newCapture(t)
			msgs, _ := subscribe(t, capture)

			if resp := postRaw(t, capture, tc.query, tc.contentType, tc.body); resp.StatusCode != http.StatusBadRequest {
				t.Errorf("status %d, want %d", resp.StatusCode, http.StatusBadRequest)
			}
			requireWentNowhere(t, capture, msgs, "a malformed POST")
		})
	}
}

// requireWentNowhere fails the test unless the request what names, made after
// msgs subscribed, went nowhere: Captured holds nothing, no wake-up is pending,
// and nothing was queued for msgs. It shows the last by posting a well-formed
// message, which must be msgs' first delivery, without waiting on a silence.
func requireWentNowhere(t *testing.T, capture *smsfake.CaptureServer, msgs <-chan smsfake.Message, what string) {
	t.Helper()
	if recorded := capture.Captured(); len(recorded) != 0 {
		t.Errorf("Captured holds %d messages after %s, want none: %+v", len(recorded), what, recorded)
	}
	select {
	case <-capture.Notify():
		t.Errorf("%s sent a wake-up, want none", what)
	default:
	}

	if err := postBodies(capture, []string{"well formed"}); err != nil {
		t.Fatal(err)
	}
	got, err := collect(msgs, 1)
	if err != nil {
		t.Fatalf("subscriber: %v", err)
	}
	if got[0].Body != "well formed" {
		t.Errorf("subscriber's first delivery has Body %q and Form %v, want the well-formed message posted after %s", got[0].Body, got[0].Form, what)
	}
}

// TestCapture_UnroutedRequestIsNotRecorded pins the requests the capture
// server's mux does not route to its handler, which go nowhere either: one
// outside /2010-04-01/Accounts/ is answered 404 — among them SendSMS's POST
// from a client whose BaseURL carries a path of its own — and one under it
// whose path is not clean, here one with an empty account, is redirected to
// the cleaned path. Each is recorded nowhere, sends no wake-up and reaches no
// subscriber. The POSTs go straight to the mux, so the redirect is answered
// and not followed: a client that follows it makes a request of its own.
func TestCapture_UnroutedRequestIsNotRecorded(t *testing.T) {
	for _, tc := range []struct {
		name, path string
		redirected bool
	}{
		{"a path outside the prefix", "/2010-04-01/Messages.json", false},
		{"the endpoint behind a path of its own", "/twilio" + messagesPath(subscribeAccount), false},
		{"a path that is not clean", messagesPath(""), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			capture := newCapture(t)
			msgs, _ := subscribe(t, capture)

			rec := serveForm(capture, tc.path, strings.NewReader(url.Values{"Body": {"unrouted"}}.Encode()))
			switch {
			case tc.redirected && (rec.Code/100 != 3 || rec.Header().Get("Location") == ""):
				t.Errorf("POST %s: status %d, Location %q; want a redirect to the cleaned path", tc.path, rec.Code, rec.Header().Get("Location"))
			case !tc.redirected && rec.Code != http.StatusNotFound:
				t.Errorf("POST %s: status %d, want %d", tc.path, rec.Code, http.StatusNotFound)
			}
			requireWentNowhere(t, capture, msgs, "a POST to "+tc.path)
		})
	}
}

// TestCapture_OtherBodiesAreRecordedEmpty pins what the capture server makes of
// a body that is not application/x-www-form-urlencoded, the encoding SendSMS
// sends: it does not read it. The request is answered 201 and recorded like
// any other, with an empty Form and empty From, To and Body. Each body carries
// From, To and Body in its own encoding, so a capture that read it, or read
// every body as urlencoded whatever its type, would record them. Unread, a
// multipart body that does not parse is answered 201 too, and so is a
// urlencoded body sent without a Content-Type.
func TestCapture_OtherBodiesAreRecordedEmpty(t *testing.T) {
	fields := map[string]string{"From": "+15105559999", "To": "+15551234567", "Body": "hi back"}
	form := url.Values{}
	var multipartBody bytes.Buffer
	mw := multipart.NewWriter(&multipartBody)
	for key, value := range fields {
		form.Set(key, value)
		if err := mw.WriteField(key, value); err != nil {
			t.Fatalf("write multipart field %s: %v", key, err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("finish the multipart body: %v", err)
	}
	jsonBody, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("encode the JSON body: %v", err)
	}

	for _, tc := range []struct {
		name, contentType, body string
	}{
		{"multipart/form-data", mw.FormDataContentType(), multipartBody.String()},
		{"multipart/form-data that does not parse", "multipart/form-data; boundary=absent", form.Encode()},
		{"JSON", "application/json", string(jsonBody)},
		{"no Content-Type", "", form.Encode()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			capture := newCapture(t)

			if resp := postRaw(t, capture, "", tc.contentType, tc.body); resp.StatusCode != http.StatusCreated {
				t.Errorf("status %d, want %d, as for any request whose body is not read", resp.StatusCode, http.StatusCreated)
			}

			if msg := requireOneRecorded(t, capture)[0]; len(msg.Form) != 0 || msg.From != "" || msg.To != "" || msg.Body != "" {
				t.Errorf("recorded Form %v, From %q, To %q, Body %q; want all of them empty, the body unread", msg.Form, msg.From, msg.To, msg.Body)
			}
		})
	}
}

// TestCapture_ArrivalTimestampBracketsThePost pins that ReceivedAt is read
// while the POST is handled, not when the record is read. It must lie between
// a clock read before the POST and one after it; Captured is read only once
// the bracket has closed, so a timestamp taken when the record is read rather
// than when it was made lands after the bracket. A second read and a
// subscriber's copy must carry the same instant: re-stamping on any read shows
// up as a difference. Which moment of the POST it is read at, the bracket
// cannot see; TestCapture_ArrivalTimestampFollowsRecordingOrder pins that.
func TestCapture_ArrivalTimestampBracketsThePost(t *testing.T) {
	capture := newCapture(t)
	msgs, _ := subscribe(t, capture)

	before := time.Now()
	postMessage(t, capture, subscribeAccount, url.Values{"Body": {"when"}})
	after := time.Now()

	at := requireOneRecorded(t, capture)[0].ReceivedAt
	if at.Before(before) || at.After(after) {
		t.Errorf("ReceivedAt = %v, want it within the POST: after %v and before %v", at, before, after)
	}
	if again := capture.Captured()[0].ReceivedAt; !again.Equal(at) {
		t.Errorf("a second Captured has ReceivedAt = %v, the first had %v; want the one instant of recording", again, at)
	}

	delivered, err := collect(msgs, 1)
	if err != nil {
		t.Fatalf("subscriber: %v", err)
	}
	if got := delivered[0].ReceivedAt; !got.Equal(at) {
		t.Errorf("subscriber's ReceivedAt = %v, Captured's = %v; want the one instant of recording", got, at)
	}
}

// heldBody is a request body whose content is held back until released is
// closed. reading is closed at the handler's first Read, which shows the
// handler has begun and is waiting on the form.
type heldBody struct {
	content  io.Reader
	reading  chan struct{}
	released chan struct{}
	once     sync.Once
}

func newHeldBody(content string) *heldBody {
	return &heldBody{
		content:  strings.NewReader(content),
		reading:  make(chan struct{}),
		released: make(chan struct{}),
	}
}

func (b *heldBody) Read(p []byte) (int, error) {
	b.once.Do(func() { close(b.reading) })
	<-b.released
	return b.content.Read(p)
}

// TestCapture_ArrivalTimestampFollowsRecordingOrder pins that ReceivedAt is
// read when the message is recorded, not when its POST reaches the handler.
// POST A reaches the handler first, but its body is held back until POST B
// has been recorded, so A is recorded second, and its ReceivedAt must be no
// earlier than B's. A stamp read as A's handler began, before its form was
// read, would put A first by time and second in Captured.
func TestCapture_ArrivalTimestampFollowsRecordingOrder(t *testing.T) {
	capture := newCapture(t)

	held := newHeldBody(url.Values{"Body": {"A"}}.Encode())
	var rec *httptest.ResponseRecorder
	served := make(chan struct{})
	go func() {
		defer close(served)
		rec = serveForm(capture, messagesPath(subscribeAccount), held)
	}()

	returnsWithin(t, "POST A's handler reading its body", func() { <-held.reading })
	serveWithin(t, capture, "POST B while A's body is held back", []string{"B"})
	close(held.released)
	returnsWithin(t, "POST A once its body is released", func() { <-served })
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST A: status %d, want %d", rec.Code, http.StatusCreated)
	}

	recorded := capture.Captured()
	if bodies := bodiesOf(recorded); !reflect.DeepEqual(bodies, []string{"B", "A"}) {
		t.Fatalf("Captured holds %q, want B then A: A's body was held back until B was recorded", bodies)
	}
	if a, b := recorded[1].ReceivedAt, recorded[0].ReceivedAt; a.Before(b) {
		t.Errorf("A, recorded after B, has ReceivedAt %v, before B's %v; want it read when A was recorded, not when its POST began", a, b)
	}
}

// TestCapture_FanOutAllSubscribersSeeAll pins fan-out: three subscribers,
// each reading on its own goroutine while ten messages are posted, each
// receive all ten — as the messages themselves, in posted order.
func TestCapture_FanOutAllSubscribersSeeAll(t *testing.T) {
	const subscribers, messages = 3, 10
	capture := newCapture(t)
	feeds := collectEach(t, capture, subscribers, messages)

	posted := numbered("fan-out", messages)
	if err := postBodies(capture, posted); err != nil {
		t.Fatal(err)
	}

	requireEachReceived(t, feeds, posted, "subscriber")
}

// TestCapture_ConcurrentPostsSameOrderForEverySubscriber pins that "posted
// order" has one meaning even when POSTs race: whatever order the server
// records concurrent arrivals in, every subscriber receives them in that same
// order, the order Captured lists them.
//
// It is the guard on the handler's lock scope: a message is recorded and
// queued for every subscriber under one hold of the server's lock. A handler
// that queued after letting the lock go would let two racing arrivals reach a
// subscriber in the opposite order to Captured's. That shows only when many
// arrivals race, so the POSTs go straight to the handler, as serveBodies does,
// rather than opening a connection apiece.
//
// It also checks that ReceivedAt never decreases along Captured, which a
// stamp read under that same hold of the lock guarantees. A stamp read before
// the lock is taken lets a racing arrival record in between and break it.
//
// Either slip shows only when the scheduler runs a racing arrival in the gap
// it opens, so this catches them by weight of numbers rather than by
// construction: on every run on several CPUs, but on one CPU only under -race,
// and even then not on every run. On one CPU without -race the handler is
// seldom if ever switched out within so short a gap, and no other test here
// catches either slip there.
func TestCapture_ConcurrentPostsSameOrderForEverySubscriber(t *testing.T) {
	const subscribers, messages = 3, 400
	capture := newCapture(t)
	feeds := collectEach(t, capture, subscribers, messages)

	errs := make(chan error, messages)
	returnsWithin(t, fmt.Sprintf("%d concurrent POSTs", messages), func() {
		var wg sync.WaitGroup
		for _, body := range numbered("concurrent", messages) {
			wg.Go(func() { errs <- serveBodies(capture, []string{body}) })
		}
		wg.Wait()
	})
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	captured := capture.Captured()
	recorded := bodiesOf(captured)
	if len(recorded) != messages {
		t.Fatalf("recorded %d messages, want %d", len(recorded), messages)
	}
	requireEachReceived(t, feeds, recorded, "subscriber")

	decreases, first := 0, 0
	for i := 1; i < len(captured); i++ {
		if captured[i].ReceivedAt.Before(captured[i-1].ReceivedAt) {
			if decreases == 0 {
				first = i
			}
			decreases++
		}
	}
	if decreases > 0 {
		t.Errorf("ReceivedAt decreases at %d of the %d steps along Captured, the first from Captured[%d] to Captured[%d]; want it never to decrease", decreases, len(captured)-1, first-1, first)
	}
}

// TestCapture_SubscribersJoinAndLeaveWhilePostsAndCloseRun pins the locking
// around the server's subscription set, which no other test can see: each of
// them subscribes before posting and closes after the last POST is answered.
// Here subscribers join and leave, over and over, while concurrent POSTs are
// recorded and fanned out, and go on doing so while Close runs. Under -race,
// an unlocked Subscribe, unsubscribe or Close is reported as a data race; it
// can also fault the map outright. Close cannot race the handler itself —
// httptest's Close waits for in-flight POSTs, as
// TestCapture_CloseWaitsForInFlightPost pins — so it is the joining and
// leaving that Close must be locked against.
//
// It also pins what a subscriber joining mid-stream receives: exactly the
// messages recorded after it subscribed, which is a run of Captured that
// starts between the counts read just before and just after Subscribe and
// runs, unbroken, to the last message.
func TestCapture_SubscribersJoinAndLeaveWhilePostsAndCloseRun(t *testing.T) {
	const posters, perPoster, churners, joiners = 8, 40, 4, 8
	capture := newCapture(t)

	// Each churner yields after every join and leave. Without that, on one CPU
	// the churners took nearly all of it: unsubscribe parks on the feed it
	// ends, the feed wakes it straight back, and a pair handing the CPU back
	// and forth like that keeps running ahead of the goroutines the network
	// wakes, which carry every POST. The posters managed some thirty messages
	// a second, and the joins below, waiting on them, overran SubscribeWait.
	// Yielding sends each churner to the back of the queue those goroutines
	// wait in, so the churn still overlaps the POSTs and Close without
	// crowding them out.
	stopChurn := make(chan struct{})
	var churn sync.WaitGroup
	for range churners {
		churn.Go(func() {
			for {
				select {
				case <-stopChurn:
					return
				default:
				}
				_, unsubscribe := capture.Subscribe()
				unsubscribe()
				runtime.Gosched()
			}
		})
	}
	stopOnce := sync.OnceFunc(func() {
		close(stopChurn)
		churn.Wait()
	})
	stop := func() { returnsWithin(t, "stopping the subscribers that join and leave", stopOnce) }
	t.Cleanup(stop)

	errs := make(chan error, posters)
	postingDone := make(chan struct{})
	var posting sync.WaitGroup
	for p := range posters {
		posting.Go(func() { errs <- postBodies(capture, numbered(fmt.Sprintf("poster %d", p), perPoster)) })
	}
	go func() {
		posting.Wait()
		close(postingDone)
	}()

	// Joiner i waits until about i+1 in every joiners+1 of the posters'
	// messages have been recorded, so the joins are spread through the stream,
	// each made while POSTs are in flight. A wake-up sent after the count is
	// read stays pending, so none is missed.
	type joiner struct {
		msgs          <-chan smsfake.Message
		unsubscribe   func()
		before, after int // messages recorded just before and just after Subscribe
	}
	joined := make([]joiner, joiners)
	returnsWithin(t, "subscribing while POSTs are in flight", func() {
		for i := range joined {
			target := (i + 1) * posters * perPoster / (joiners + 1)
			for waiting := true; waiting && len(capture.Captured()) < target; {
				select {
				case <-capture.Notify():
				case <-postingDone:
					waiting = false
				}
			}
			before := len(capture.Captured())
			msgs, unsubscribe := capture.Subscribe()
			after := len(capture.Captured())
			joined[i] = joiner{msgs, unsubscribe, before, after}
		}
	})
	for _, j := range joined {
		boundedUnsubscribe(t, j.unsubscribe)
	}
	returnsWithin(t, fmt.Sprintf("%d concurrent posters", posters), func() { <-postingDone })
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	// One message after every poster has finished, so every joiner has at
	// least one to receive and a known last one to stop at.
	const last = "last"
	if err := postBodies(capture, []string{last}); err != nil {
		t.Fatal(err)
	}
	recorded := bodiesOf(capture.Captured())
	if want := posters*perPoster + 1; len(recorded) != want {
		t.Fatalf("recorded %d messages, want %d", len(recorded), want)
	}

	for i, j := range joined {
		got, err := collectThrough(j.msgs, last)
		if err != nil {
			t.Errorf("subscriber %d: %v", i, err)
			continue
		}
		from := len(recorded) - len(got)
		if from < j.before || from > j.after {
			t.Errorf("subscriber %d, subscribing when %d to %d messages were recorded, received the last %d, want the run starting within that range", i, j.before, j.after, len(got))
			continue
		}
		if bodies := bodiesOf(got); !reflect.DeepEqual(bodies, recorded[from:]) {
			t.Errorf("subscriber %d received %d messages that are not Captured's last %[2]d, in Captured's order", i, len(got))
		}
	}

	returnsWithin(t, "Close while subscribers join and leave", func() {
		capture.Close()
		for i, j := range joined {
			requireClosed(t, j.msgs, fmt.Sprintf("subscriber %d, after Close", i))
		}
	})
	stop()
	requireSubscribers(t, capture, 0, "after Close")
}

// TestCapture_SlowSubscriberDoesNotBlock pins that a subscriber which never
// reads does not hold up the handler or the other subscribers: with it
// subscribed throughout, all backlog POSTs are answered within SubscribeWait,
// and the two subscribers that are reading still receive every message within
// a bounded wait. The bound covers the whole backlog, so a handler that waited
// on the idle subscriber for as little as half a millisecond a message would
// miss it; a wait shorter than that is not seen.
func TestCapture_SlowSubscriberDoesNotBlock(t *testing.T) {
	capture := newCapture(t)
	subscribe(t, capture) // never read
	fast := collectEach(t, capture, 2, backlog)

	posted := numbered("past the slow one", backlog)
	serveWithin(t, capture, fmt.Sprintf("%d POSTs with a subscriber that never reads", backlog), posted)

	requireEachReceived(t, fast, posted, "reading subscriber")
}

// TestCapture_SlowSubscriberLosesNothing pins the other half of not blocking:
// a subscriber that falls behind only falls behind. Everything posted while it
// was not reading waits for it, and once it starts reading it receives all of
// it, in posted order, with nothing dropped.
func TestCapture_SlowSubscriberLosesNothing(t *testing.T) {
	capture := newCapture(t)
	msgs, _ := subscribe(t, capture)

	posted := numbered("backlog", backlog)
	serveWithin(t, capture, fmt.Sprintf("%d POSTs to a subscriber not yet reading", backlog), posted)

	got, err := collect(msgs, backlog)
	if err != nil {
		t.Fatalf("subscriber that started reading late: %v", err)
	}
	if bodies := bodiesOf(got); !reflect.DeepEqual(bodies, posted) {
		t.Errorf("subscriber that started reading late received its backlog out of posted order")
	}
}

// TestCapture_UnsubscribeStopsDelivery pins that unsubscribing releases the
// subscription: nothing posted afterwards reaches it, its channel is closed,
// and the server no longer holds it, so it is not left collecting a copy of
// every later message for nobody. A second subscriber, still subscribed,
// receives the later message, which shows it was recorded and fanned out — so
// the first subscriber's silence is the unsubscribe's doing and not an empty
// server's.
func TestCapture_UnsubscribeStopsDelivery(t *testing.T) {
	capture := newCapture(t)
	msgs, unsubscribe := subscribe(t, capture)
	witness, unsubscribeWitness := subscribe(t, capture)
	requireSubscribers(t, capture, 2, "with two subscribed")

	if err := postBodies(capture, []string{"before"}); err != nil {
		t.Fatal(err)
	}
	got, err := collect(msgs, 1)
	if err != nil {
		t.Fatalf("subscriber, before unsubscribing: %v", err)
	}
	if got[0].Body != "before" {
		t.Fatalf("subscriber received %q, want %q", got[0].Body, "before")
	}

	unsubscribe()
	if err := postBodies(capture, []string{"after"}); err != nil {
		t.Fatal(err)
	}

	seen, err := collect(witness, 2)
	if err != nil {
		t.Fatalf("witness subscriber: %v", err)
	}
	if bodies, want := bodiesOf(seen), []string{"before", "after"}; !reflect.DeepEqual(bodies, want) {
		t.Fatalf("witness subscriber received %q, want %q", bodies, want)
	}
	requireClosed(t, msgs, "the unsubscribed subscription")
	requireSubscribers(t, capture, 1, "after one of two unsubscribed")

	unsubscribeWitness()
	requireSubscribers(t, capture, 0, "after both unsubscribed")
}

// TestCapture_UnsubscribeIsIdempotent pins that unsubscribe may be called more
// than once, and from several goroutines at once, without panicking or
// blocking, and that releasing subscriptions leaves the server recording and
// delivering to the next.
//
// The concurrent calls are each subscription's first: all of them wait at one
// start line, so they race to end a live subscription rather than finding it
// already ended. Each caller finds the channel closed once its own call
// returns, whichever call did the work. A call made after the subscription has
// ended must return too.
//
// What catches an end that is not once and only once depends on its guard.
// One guarded by an unsynchronised flag is reported by -race as a data race,
// on every run and even on one CPU. One guarded only by channel operations,
// such as a select that closes the channel in its default case, draws no race
// report: it is caught only when two callers both pass the check and the
// second close panics, which takes callers running in parallel. Measured on
// twelve CPUs, that happened on 9 of 20 runs under -race and 3 of 20 without;
// on one CPU, on none of 20 either way.
func TestCapture_UnsubscribeIsIdempotent(t *testing.T) {
	const subscriptions, callers = 100, 4
	capture := newCapture(t)

	start := make(chan struct{})
	unsubscribes := make([]func(), subscriptions)
	var wg sync.WaitGroup
	for i := range unsubscribes {
		msgs, unsubscribe := capture.Subscribe()
		unsubscribes[i] = unsubscribe
		for c := range callers {
			wg.Go(func() {
				<-start
				unsubscribe()
				requireClosed(t, msgs, fmt.Sprintf("subscription %d, after caller %d's unsubscribe", i, c))
			})
		}
	}
	returnsWithin(t, fmt.Sprintf("%d concurrent first unsubscribes of each of %d subscriptions", callers, subscriptions), func() {
		close(start)
		wg.Wait()
	})
	requireSubscribers(t, capture, 0, "after every subscription was unsubscribed")
	returnsWithin(t, "an unsubscribe after the subscription had ended", unsubscribes[0])

	next, _ := subscribe(t, capture)
	if err := postBodies(capture, []string{"still serving"}); err != nil {
		t.Fatal(err)
	}
	if _, err := collect(next, 1); err != nil {
		t.Errorf("a subscriber after the repeated unsubscribes: %v", err)
	}
}

// TestCapture_UnsubscribeRacesClose pins that a subscription's unsubscribe and
// the server's Close may end it at the same moment — a consumer's deferred
// unsubscribe running while its server shuts down — and that both return,
// leaving it ended and released. Each round holds every subscription's
// unsubscribe and a Close at one start line on a fresh server, so that some
// unsubscribe finds its subscription already taken by Close and the two end it
// together. Each unsubscribe must return with its channel closed, whether it
// or Close did the work.
//
// An end that is not once and only once is caught as
// TestCapture_UnsubscribeIsIdempotent says: on every run under -race when an
// unsynchronised flag guards it, and only by chance when channel operations
// alone do. Measured for the latter on twelve CPUs, it was caught on 14 of 20
// runs under -race and 5 of 20 without; on one CPU, on none of 20 either way.
func TestCapture_UnsubscribeRacesClose(t *testing.T) {
	const rounds, subscriptions = 50, 8
	for round := range rounds {
		capture := newCapture(t)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := range subscriptions {
			msgs, unsubscribe := capture.Subscribe()
			wg.Go(func() {
				<-start
				unsubscribe()
				requireClosed(t, msgs, fmt.Sprintf("round %d, subscription %d", round, i))
			})
		}
		wg.Go(func() {
			<-start
			capture.Close()
		})
		returnsWithin(t, fmt.Sprintf("round %d: Close racing %d unsubscribes", round, subscriptions), func() {
			close(start)
			wg.Wait()
		})
		requireSubscribers(t, capture, 0, fmt.Sprintf("round %d, after Close and every unsubscribe", round))
	}
}

// TestCapture_UnsubscribeWhileBacklogged pins that unsubscribing a subscriber
// with messages queued and unread returns promptly — its feed is not left
// waiting for a reader that has gone — and that the queued messages are not
// delivered afterwards: its channel is closed.
func TestCapture_UnsubscribeWhileBacklogged(t *testing.T) {
	capture := newCapture(t)
	msgs, unsubscribe := capture.Subscribe()

	if err := postBodies(capture, numbered("unread", 20)); err != nil {
		t.Fatal(err)
	}

	returnsWithin(t, "unsubscribe with 20 messages queued and unread", func() {
		unsubscribe()
		requireClosed(t, msgs, "the unsubscribed subscription")
	})
}

// TestCapture_ClosedByTheTimeUnsubscribeReturns pins, over many subscriptions,
// that unsubscribe returns only once msgs is closed, and that Subscribe on a
// closed server, which ends the subscription before returning it, does the
// same. A feed closes msgs as it exits, and ending a subscription waits for
// the feed to exit, so this holds only if the feed closes msgs before it
// signals that it has exited. Done the other way round, msgs stays open for a
// few instructions after the end has returned, which one check sees only by
// chance. This makes rounds of them on each path, so it catches that by
// weight of numbers rather than by construction: reliably under -race on
// several CPUs, on most runs without -race, and seldom if ever on one CPU,
// where the feed usually runs on to close msgs before the caller it woke is
// scheduled.
func TestCapture_ClosedByTheTimeUnsubscribeReturns(t *testing.T) {
	const rounds = 20000
	capture := newCapture(t)

	open := 0
	returnsWithin(t, fmt.Sprintf("%d rounds of Subscribe and unsubscribe", rounds), func() {
		for range rounds {
			msgs, unsubscribe := capture.Subscribe()
			unsubscribe()
			if stillOpen(msgs) {
				open++
			}
		}
	})
	if open != 0 {
		t.Errorf("%d of %d subscriptions were still open when unsubscribe returned, want none", open, rounds)
	}

	returnsWithin(t, "Close", capture.Close)
	open = 0
	returnsWithin(t, fmt.Sprintf("%d rounds of Subscribe to a closed server", rounds), func() {
		for range rounds {
			if msgs, _ := capture.Subscribe(); stillOpen(msgs) {
				open++
			}
		}
	})
	if open != 0 {
		t.Errorf("%d of %d subscriptions to a closed server were still open when Subscribe returned, want none", open, rounds)
	}
}

// TestCapture_LateSubscriberGetsNoReplay pins that a subscription starts when
// it is made: a subscriber joining after three messages receives the fourth
// onward — and first receives the fourth, not a replay of the three. History
// is Captured's to give, not the subscription's.
func TestCapture_LateSubscriberGetsNoReplay(t *testing.T) {
	capture := newCapture(t)
	if err := postBodies(capture, numbered("early", 3)); err != nil {
		t.Fatal(err)
	}

	msgs, _ := subscribe(t, capture)
	late := numbered("late", 2)
	if err := postBodies(capture, late); err != nil {
		t.Fatal(err)
	}

	got, err := collect(msgs, len(late))
	if err != nil {
		t.Fatalf("late subscriber: %v", err)
	}
	if bodies := bodiesOf(got); !reflect.DeepEqual(bodies, late) {
		t.Errorf("late subscriber received %q, want %q — only what was recorded after it subscribed", bodies, late)
	}
	if n := len(capture.Captured()); n != 5 {
		t.Errorf("Captured holds %d messages, want all 5", n)
	}
}

// TestCapture_CloseEndsEverySubscription pins that shutting the server down
// ends its subscriptions rather than leaving their feeds running: after Close
// returns, the channel of a subscriber with nothing queued and the channel of
// one with messages queued and unread are both closed, and the server holds
// neither. Subscribing to a closed server yields a closed channel and leaves
// nothing held, and every unsubscribe stays safe to call.
// The channel is closed by the feed as it exits, so a closed channel is the
// observable sign that the feed is gone. Each channel is checked on the
// goroutine that called Close or Subscribe, as soon as it returns, as
// requireClosed says it must be.
func TestCapture_CloseEndsEverySubscription(t *testing.T) {
	capture := newCapture(t)
	backlogged, unsubscribeBacklogged := capture.Subscribe()
	if err := postBodies(capture, numbered("never read", 3)); err != nil {
		t.Fatal(err)
	}
	idle, unsubscribeIdle := capture.Subscribe()

	returnsWithin(t, "Close with live subscribers", func() {
		capture.Close()
		requireClosed(t, idle, "a subscription with nothing queued")
		requireClosed(t, backlogged, "a subscription with messages queued")
	})
	requireSubscribers(t, capture, 0, "after Close")

	var unsubscribeAfterClose func()
	returnsWithin(t, "Subscribe to a closed server", func() {
		var afterClose <-chan smsfake.Message
		afterClose, unsubscribeAfterClose = capture.Subscribe()
		requireClosed(t, afterClose, "a subscription to a closed server")
	})
	requireSubscribers(t, capture, 0, "after subscribing to a closed server")

	returnsWithin(t, "unsubscribe after Close", func() {
		unsubscribeBacklogged()
		unsubscribeIdle()
		unsubscribeAfterClose()
	})
}

// TestCapture_CloseStopsTheServer pins that CaptureServer.Close, which stands
// in front of the embedded Server's own Close, still shuts the server down:
// once it returns, a POST to the old URL is refused and recorded nowhere, and
// the port is free to bind again.
func TestCapture_CloseStopsTheServer(t *testing.T) {
	port := freeTCPPort(t)
	capture, err := smsfake.NewCaptureServer(port)
	if err != nil {
		t.Fatalf("NewCaptureServer(%d): %v", port, err)
	}
	defer returnsWithin(t, "Close at the end of the test", capture.Close)
	postMessage(t, capture, subscribeAccount, url.Values{"Body": {"before Close"}})

	returnsWithin(t, "Close", capture.Close)

	if resp, _, err := sendMessage(capture, subscribeAccount, url.Values{"Body": {"after Close"}}); err == nil {
		t.Errorf("a POST after Close was answered with status %d, want it refused", resp.StatusCode)
	}
	if n := len(capture.Captured()); n != 1 {
		t.Errorf("Captured holds %d messages, want only the 1 posted before Close", n)
	}
	again, err := smsfake.NewCaptureServer(port)
	if err != nil {
		t.Fatalf("binding port %d again after Close: %v — want it released", port, err)
	}
	returnsWithin(t, "Close of the server bound again", again.Close)
}

// TestCapture_ConcurrentClosesEachEndEverySubscription pins that Close's
// promise is kept to every caller, not only to the one that does the work:
// when two Closes race, each finds every subscription closed once it returns —
// a shutdown path and a test cleanup closing together must not let one of
// them return with feeds still running. Nothing is posted, so a receive that
// does not report the channel closed at once means it is still open. Many
// subscribers keep the work long enough that a Close returning early is seen.
func TestCapture_ConcurrentClosesEachEndEverySubscription(t *testing.T) {
	const subscribers, closers = 2000, 2
	capture := newCapture(t)
	feeds := make([]<-chan smsfake.Message, subscribers)
	for i := range feeds {
		feeds[i], _ = capture.Subscribe() // Close ends every one
	}

	start := make(chan struct{})
	openCounts := make(chan int, closers)
	returnsWithin(t, fmt.Sprintf("%d concurrent Closes", closers), func() {
		var wg sync.WaitGroup
		for range closers {
			wg.Go(func() {
				<-start
				capture.Close()
				open := 0
				for _, msgs := range feeds {
					if stillOpen(msgs) {
						open++
					}
				}
				openCounts <- open
			})
		}
		close(start)
		wg.Wait()
	})
	close(openCounts)
	for open := range openCounts {
		if open != 0 {
			t.Errorf("a Close racing another returned with %d of %d subscriptions still open, want every one closed", open, subscribers)
		}
	}
}

// TestCapture_FormsAreNotShared pins that every reader gets a Form of its
// own. Two subscribers and Captured each receive the same message; editing one
// copy — a value in place, or the set of keys — must change neither the
// other subscriber's copy nor what a later Captured returns.
func TestCapture_FormsAreNotShared(t *testing.T) {
	capture := newCapture(t)
	a, _ := subscribe(t, capture)
	b, _ := subscribe(t, capture)

	posted := url.Values{
		"Body":     {"original"},
		"MediaUrl": {"https://example.com/2.png", "https://example.com/1.png"},
	}
	postMessage(t, capture, subscribeAccount, posted)

	fromA, err := collect(a, 1)
	if err != nil {
		t.Fatalf("subscriber A: %v", err)
	}
	fromB, err := collect(b, 1)
	if err != nil {
		t.Fatalf("subscriber B: %v", err)
	}
	snapshot := requireOneRecorded(t, capture)

	fromA[0].Form["Body"][0] = "edited through subscriber A"
	fromA[0].Form.Set("Added", "by subscriber A")
	snapshot[0].Form["MediaUrl"][1] = "edited through Captured"
	snapshot[0].Form.Del("Body")

	if got := fromB[0].Form; !reflect.DeepEqual(got, posted) {
		t.Errorf("subscriber B's Form = %v after the others edited theirs, want %v", got, posted)
	}
	if got := capture.Captured()[0].Form; !reflect.DeepEqual(got, posted) {
		t.Errorf("a later Captured's Form = %v after readers edited theirs, want %v", got, posted)
	}
}
