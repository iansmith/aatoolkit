package smsfake_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
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

// messagesPath is the path of accountSID's Messages.json endpoint, the one
// RESTClient.SendSMS POSTs to.
func messagesPath(accountSID string) string {
	return "/2010-04-01/Accounts/" + accountSID + "/Messages.json"
}

// sendMessage makes one Messages.json POST to capture for accountSID, the way
// RESTClient.SendSMS does, and returns the response with its body read. It
// reports failure as an error rather than through a *testing.T, so a goroutine
// other than the test's own can call it.
func sendMessage(capture *smsfake.CaptureServer, accountSID string, form url.Values) (*http.Response, []byte, error) {
	endpoint := capture.URL + messagesPath(accountSID)
	resp, err := http.PostForm(endpoint, form)
	if err != nil {
		return nil, nil, fmt.Errorf("POST %s: %v", endpoint, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("read response from %s: %v", endpoint, err)
	}
	return resp, body, nil
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

// subscribeAccount is the account postBodies and the fan-out tests post as.
// Nothing they assert depends on it.
const subscribeAccount = "ACsubscribe00000000000000000000"

// subscribeWait bounds every wait on a subscription, a POST or a shutdown in
// the tests below. It is a failure detector only: a correct server delivers in
// milliseconds, and no test sleeps to let delivery happen.
const subscribeWait = 5 * time.Second

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
// slow-subscriber tests queue backlog messages in well under a second. It
// drives the same handler a POST reaches, so a handler that blocks blocks
// serveBodies' caller just as it would hold up a POST's response.
func serveBodies(capture *smsfake.CaptureServer, bodies []string) error {
	return eachCreated(bodies, func(form url.Values) (int, error) {
		req := httptest.NewRequest(http.MethodPost, messagesPath(subscribeAccount), strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		capture.Config.Handler.ServeHTTP(rec, req)
		return rec.Code, nil
	})
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
// channel closes first or they have not all arrived within subscribeWait. It
// reports through its error, so any goroutine can call it.
func collect(msgs <-chan smsfake.Message, n int) ([]smsfake.Message, error) {
	deadline := time.NewTimer(subscribeWait)
	defer deadline.Stop()
	got := make([]smsfake.Message, 0, n)
	for len(got) < n {
		select {
		case msg, ok := <-msgs:
			if !ok {
				return got, fmt.Errorf("subscription closed after %d of %d messages", len(got), n)
			}
			got = append(got, msg)
		case <-deadline.C:
			return got, fmt.Errorf("received %d of %d messages within %s", len(got), n, subscribeWait)
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

// collectEach makes readers subscriptions and starts, for each, a goroutine
// that collects n messages from it. It returns one channel per subscriber,
// which delivers that subscriber's feed once. Every subscription is ended when
// the test is, before newCapture's Close: cleanups run last in, first out.
func collectEach(t *testing.T, capture *smsfake.CaptureServer, readers, n int) []chan feed {
	t.Helper()
	feeds := make([]chan feed, readers)
	for i := range feeds {
		msgs, unsubscribe := capture.Subscribe()
		t.Cleanup(unsubscribe)
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

// returnsWithin runs fn on a goroutine of its own and, if it has not returned
// within subscribeWait, crashes the test binary with a panic naming the test
// and what stalled. Anything fn writes is safe to read once returnsWithin has
// returned.
//
// It does not fail through t. What it guards against is a handler stuck while
// holding the server's lock, and then a t.Fatalf — or a panic on the test's own
// goroutine — runs the test's deferred unsubscribes, which block on that lock,
// and its capture.Close cleanup, which waits on the stuck POST, before anything
// is reported: the run would stall until go test's -timeout with the message
// lost. A panic on a fresh goroutine runs none of the test's defers, so it
// reports at once.
func returnsWithin(t *testing.T, what string, fn func()) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(subscribeWait):
		msg := fmt.Sprintf("%s: %s did not return within %s", t.Name(), what, subscribeWait)
		go func() { panic(msg) }()
		select {} // the panic ends the process; returning would run the blocking defers
	}
}

// TestCapture_RetainsWholePostedForm pins full-fidelity capture: a send
// carrying From, To, Body and StatusCallback is recorded with every one of
// those fields in its Form, exactly as posted — the StatusCallback included,
// recorded though never acted on.
func TestCapture_RetainsWholePostedForm(t *testing.T) {
	capture := newCapture(t)

	rest := &twilio.RESTClient{AccountSID: subscribeAccount, BaseURL: capture.URL}
	sms := twilio.OutboundSMS{
		From:           "+15105559999",
		To:             "+15551234567",
		Body:           "hi back",
		StatusCallback: "http://127.0.0.1:9/status?ref=fidelity",
	}
	if _, err := rest.SendSMS(context.Background(), sms); err != nil {
		t.Fatalf("SendSMS: %v", err)
	}

	msgs := capture.Captured()
	if len(msgs) != 1 {
		t.Fatalf("got %d recorded messages, want 1", len(msgs))
	}
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

// TestCapture_RetainsFieldsItDoesNotKnow pins that the record is the posted
// form itself rather than a list of known fields read back out of it: a field
// SendSMS never sends is kept, and a repeated field keeps every value in the
// order posted.
func TestCapture_RetainsFieldsItDoesNotKnow(t *testing.T) {
	capture := newCapture(t)

	posted := url.Values{
		"Body":            {"two pictures"},
		"MediaUrl":        {"https://example.com/1.png", "https://example.com/2.png"},
		"ProvideFeedback": {"true"},
	}
	postMessage(t, capture, subscribeAccount, posted)

	msgs := capture.Captured()
	if len(msgs) != 1 {
		t.Fatalf("got %d recorded messages, want 1", len(msgs))
	}
	if got := msgs[0].Form; !reflect.DeepEqual(got, posted) {
		t.Errorf("recorded Form = %v, want exactly what was posted: %v", got, posted)
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

	rest := &twilio.RESTClient{AccountSID: subscribeAccount, BaseURL: capture.URL}
	sms := twilio.OutboundSMS{From: "+15105559999", To: "+15551234567", Body: "no callback"}
	if _, err := rest.SendSMS(context.Background(), sms); err != nil {
		t.Fatalf("SendSMS: %v", err)
	}

	msgs := capture.Captured()
	if len(msgs) != 1 {
		t.Fatalf("got %d recorded messages, want 1", len(msgs))
	}
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
	msgs, unsubscribe := capture.Subscribe()
	defer unsubscribe()

	posted := url.Values{"Body": {"empty callback"}, "StatusCallback": {""}}
	postMessage(t, capture, subscribeAccount, posted)

	recorded := capture.Captured()
	if len(recorded) != 1 {
		t.Fatalf("got %d recorded messages, want 1", len(recorded))
	}
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

// TestCapture_ArrivalTimestampBracketsThePost pins that ReceivedAt is the
// moment of recording. It must lie between a clock read before the POST and
// one after it; Captured is read only once the bracket has closed, so a
// timestamp taken when the record is read rather than when it was made lands
// after the bracket. A second read and a subscriber's copy must carry the same
// instant: re-stamping on any read shows up as a difference.
func TestCapture_ArrivalTimestampBracketsThePost(t *testing.T) {
	capture := newCapture(t)
	msgs, unsubscribe := capture.Subscribe()
	defer unsubscribe()

	before := time.Now()
	postMessage(t, capture, subscribeAccount, url.Values{"Body": {"when"}})
	after := time.Now()

	recorded := capture.Captured()
	if len(recorded) != 1 {
		t.Fatalf("got %d recorded messages, want 1", len(recorded))
	}
	at := recorded[0].ReceivedAt
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
func TestCapture_ConcurrentPostsSameOrderForEverySubscriber(t *testing.T) {
	const subscribers, messages = 3, 40
	capture := newCapture(t)

	feeds := make([]<-chan smsfake.Message, subscribers)
	for i := range feeds {
		msgs, unsubscribe := capture.Subscribe()
		defer unsubscribe()
		feeds[i] = msgs
	}

	errs := make(chan error, messages)
	returnsWithin(t, fmt.Sprintf("%d concurrent POSTs", messages), func() {
		var wg sync.WaitGroup
		for _, body := range numbered("concurrent", messages) {
			wg.Go(func() { errs <- postBodies(capture, []string{body}) })
		}
		wg.Wait()
	})
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	recorded := bodiesOf(capture.Captured())
	if len(recorded) != messages {
		t.Fatalf("recorded %d messages, want %d", len(recorded), messages)
	}
	for i, msgs := range feeds {
		got, err := collect(msgs, messages)
		if err != nil {
			t.Errorf("subscriber %d: %v", i, err)
			continue
		}
		if bodies := bodiesOf(got); !reflect.DeepEqual(bodies, recorded) {
			t.Errorf("subscriber %d received %q, want Captured's order %q", i, bodies, recorded)
		}
	}
}

// TestCapture_SlowSubscriberDoesNotBlock pins that a subscriber which never
// reads costs nobody else anything: with it subscribed throughout, every POST
// is still answered, and the two subscribers that are reading still receive
// every message within a bounded wait.
func TestCapture_SlowSubscriberDoesNotBlock(t *testing.T) {
	capture := newCapture(t)
	_, unsubscribeSlow := capture.Subscribe() // never read
	defer unsubscribeSlow()
	fast := collectEach(t, capture, 2, backlog)

	posted := numbered("past the slow one", backlog)
	var postErr error
	returnsWithin(t, fmt.Sprintf("%d POSTs with a subscriber that never reads", backlog), func() {
		postErr = serveBodies(capture, posted)
	})
	if postErr != nil {
		t.Fatal(postErr)
	}

	requireEachReceived(t, fast, posted, "reading subscriber")
}

// TestCapture_SlowSubscriberLosesNothing pins the other half of not blocking:
// a subscriber that falls behind only falls behind. Everything posted while it
// was not reading waits for it, and once it starts reading it receives all of
// it, in posted order, with nothing dropped.
func TestCapture_SlowSubscriberLosesNothing(t *testing.T) {
	capture := newCapture(t)
	msgs, unsubscribe := capture.Subscribe()
	defer unsubscribe()

	posted := numbered("backlog", backlog)
	var postErr error
	returnsWithin(t, fmt.Sprintf("%d POSTs to a subscriber not yet reading", backlog), func() {
		postErr = serveBodies(capture, posted)
	})
	if postErr != nil {
		t.Fatal(postErr)
	}

	got, err := collect(msgs, backlog)
	if err != nil {
		t.Fatalf("subscriber that started reading late: %v", err)
	}
	if bodies := bodiesOf(got); !reflect.DeepEqual(bodies, posted) {
		t.Errorf("subscriber that started reading late received its backlog out of posted order")
	}
}

// TestCapture_UnsubscribeStopsDelivery pins that unsubscribing releases the
// subscription: nothing posted afterwards reaches it, and its channel is
// closed. A second subscriber, still subscribed, receives the later message,
// which shows it was recorded and fanned out — so the first subscriber's
// silence is the unsubscribe's doing and not an empty server's.
func TestCapture_UnsubscribeStopsDelivery(t *testing.T) {
	capture := newCapture(t)
	msgs, unsubscribe := capture.Subscribe()
	defer unsubscribe()
	witness, unsubscribeWitness := capture.Subscribe()
	defer unsubscribeWitness()

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
}

// TestCapture_UnsubscribeIsIdempotent pins that unsubscribe may be called
// again, including from several goroutines at once, without panicking or
// blocking, and that releasing one subscription leaves the server recording
// and delivering to the next.
func TestCapture_UnsubscribeIsIdempotent(t *testing.T) {
	capture := newCapture(t)
	msgs, unsubscribe := capture.Subscribe()

	unsubscribe()
	returnsWithin(t, "a second unsubscribe", unsubscribe)
	returnsWithin(t, "three concurrent unsubscribes", func() {
		var wg sync.WaitGroup
		for range 3 {
			wg.Go(unsubscribe)
		}
		wg.Wait()
	})
	requireClosed(t, msgs, "the unsubscribed subscription")

	next, unsubscribeNext := capture.Subscribe()
	defer unsubscribeNext()
	if err := postBodies(capture, []string{"still serving"}); err != nil {
		t.Fatal(err)
	}
	if _, err := collect(next, 1); err != nil {
		t.Errorf("a subscriber after the repeated unsubscribes: %v", err)
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

	returnsWithin(t, "unsubscribe with 20 messages queued and unread", unsubscribe)
	requireClosed(t, msgs, "the unsubscribed subscription")
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

	msgs, unsubscribe := capture.Subscribe()
	defer unsubscribe()
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
// one with messages queued and unread are both closed. Subscribing to a closed
// server yields a closed channel, and every unsubscribe stays safe to call.
// The channel is closed by the feed as it exits, so a closed channel is the
// observable sign that the feed is gone.
func TestCapture_CloseEndsEverySubscription(t *testing.T) {
	capture := newCapture(t)
	backlogged, unsubscribeBacklogged := capture.Subscribe()
	if err := postBodies(capture, numbered("never read", 3)); err != nil {
		t.Fatal(err)
	}
	idle, unsubscribeIdle := capture.Subscribe()

	returnsWithin(t, "Close with live subscribers", capture.Close)
	requireClosed(t, idle, "a subscription with nothing queued")
	requireClosed(t, backlogged, "a subscription with messages queued")

	afterClose, unsubscribeAfterClose := capture.Subscribe()
	requireClosed(t, afterClose, "a subscription to a closed server")

	returnsWithin(t, "unsubscribe after Close", func() {
		unsubscribeBacklogged()
		unsubscribeIdle()
		unsubscribeAfterClose()
	})
}

// TestCapture_FormsAreNotShared pins that every reader gets a Form of its
// own. Two subscribers and Captured each receive the same message; editing one
// copy — a value in place, or the set of keys — must change neither the
// other subscriber's copy nor what a later Captured returns.
func TestCapture_FormsAreNotShared(t *testing.T) {
	capture := newCapture(t)
	a, unsubscribeA := capture.Subscribe()
	defer unsubscribeA()
	b, unsubscribeB := capture.Subscribe()
	defer unsubscribeB()

	posted := url.Values{
		"Body":     {"original"},
		"MediaUrl": {"https://example.com/1.png", "https://example.com/2.png"},
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
	snapshot := capture.Captured()
	if len(snapshot) != 1 {
		t.Fatalf("got %d recorded messages, want 1", len(snapshot))
	}

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
