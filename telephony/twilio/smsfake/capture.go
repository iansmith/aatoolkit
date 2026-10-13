package smsfake

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
)

// Message is one request recorded by CaptureServer, which records every
// request under /2010-04-01/Accounts/, whatever its method or resource — not
// only the Messages.json POST twilio.RESTClient.SendSMS makes. AccountSID is
// the path after that prefix with any /Messages.json suffix trimmed, so for
// any other resource it holds the rest of the path; From, To and Body are
// empty unless the request posted them.
type Message struct {
	AccountSID string
	From       string
	To         string
	Body       string

	// Form is the whole posted form, every field and every value exactly as
	// sent: a StatusCallback is present when the sender set one and absent —
	// not empty — when it did not. Capture is full fidelity because a record
	// thinned when it is made cannot be enriched later. Fields are recorded,
	// never acted on: the capture server calls no StatusCallback back.
	//
	// Every Message the server hands out, from Captured or a subscription,
	// carries a Form of its own, so a reader editing it changes no other
	// reader's view.
	Form url.Values

	// ReceivedAt is when the capture server recorded the message: read inside
	// the handler at the moment of recording, not when a reader looks at it,
	// so every copy of one message carries the same instant. It is read under
	// the lock that records the message, so it never decreases along
	// Captured's order, which is every subscriber's order too. That holds as
	// Before compares two readings of time.Now, on the monotonic clock; a copy
	// that has lost its monotonic reading, as a serialised one does, compares
	// on the wall clock, which can be stepped back.
	ReceivedAt time.Time
}

// clone returns m with a Form of its own: a fresh map holding fresh value
// slices, so neither the key set nor a value can be edited through one copy
// and seen through another.
func (m Message) clone() Message {
	if m.Form != nil {
		form := make(url.Values, len(m.Form))
		for key, values := range m.Form {
			form[key] = slices.Clone(values)
		}
		m.Form = form
	}
	return m
}

// CaptureServer implements the Twilio Messages API shape used by
// twilio.RESTClient.SendSMS (POST /2010-04-01/Accounts/{sid}/Messages.json ->
// 201 + minimal JSON), recording every call it receives. It stands in for a
// real Twilio account so a server's outbound SMS reply can be captured and
// asserted on without one.
//
// A caller reads what was recorded in one of three ways: Captured for a
// snapshot of everything so far, Notify for a bare wake-up when something has
// arrived (arrivals while one is pending coalesce into it), or Subscribe for
// each arriving message itself.
type CaptureServer struct {
	*httptest.Server

	// mu guards msgs, subs and closed together. A message is appended to msgs
	// and queued for every subscriber under one hold of mu, which is what
	// makes every subscriber's order the order of msgs.
	mu     sync.Mutex
	msgs   []Message
	subs   map[*subscription]struct{}
	closed bool

	// closeOnce runs Close's work exactly once. Once.Do returns to no caller
	// before that work has finished, so a Close racing another returns only
	// once every subscription has ended, just as the one doing the work does.
	closeOnce sync.Once

	// notify carries a wake-up on every recorded message, so a waiter is woken
	// by the arrival itself rather than by polling for it.
	//
	// It signals "something arrived", not "your message arrived" — the waiter
	// re-reads the recorded count to decide. That is what lets a later waiter
	// against the same server (twilio-cli's runSMS, for one), once the earlier
	// one has finished, wait for its own reply instead of re-reading the first
	// one. Buffered by one and sent to without blocking: a dropped send only
	// ever means a wake-up is already pending, and recording a message must
	// never block the handler on nobody listening.
	notify chan struct{}
}

// NewCaptureServer starts a new capture server listening on the requested
// port of 127.0.0.1, returning an error naming that port if the bind fails.
//
// The bind is deliberately explicit rather than ephemeral, so the capture URL
// can be named before the capture server exists. The server whose reply is
// being captured reads TWILIO_API_BASE_URL once at startup, so when it is
// launched first (twilio-cli's flow, where the operator starts it before the
// CLI) it has to be given that URL in advance. It keeps the embedded
// *httptest.Server — only the listener is swapped — so URL behaves exactly as
// callers already expect, and a failure to bind the requested port returns an
// error instead of the panic httptest.NewServer would raise. It does not
// remove httptest's own bind: NewUnstartedServer still binds a throwaway
// ephemeral listener before the swap, and if even that bind fails it panics,
// leaving the requested port held.
//
// Close, unlike URL, is not the embedded server's: CaptureServer has its own,
// which shuts that server down and also ends every subscription. Shut the
// server down with CaptureServer.Close; the embedded Server's Close leaves
// every subscription open, its feed still running.
//
// The port-range and bind errors name -capture-port, the flag twilio-cli
// exposes this port as. A caller without such a flag should reword them rather
// than pass the hint on.
func NewCaptureServer(port int) (*CaptureServer, error) {
	// Port 0 is the one value net.Listen would accept while defeating the whole
	// point: it binds an ephemeral port, so a URL named in advance from the
	// requested port (as twilio-cli's guidance names it) would end in a :0
	// nothing can be pointed at. Everything else out of range net.Listen
	// rejects for us.
	if port < 1 || port > 65535 {
		return nil, fmt.Errorf("SMS capture port %d is not a usable port (want 1-65535; -capture-port 0 would bind an ephemeral port, which the server cannot be pointed at in advance)", port)
	}

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return nil, fmt.Errorf("bind SMS capture server on port %d: %w (pick a free port with -capture-port)", port, err)
	}

	c := &CaptureServer{
		subs:   make(map[*subscription]struct{}),
		notify: make(chan struct{}, 1),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/2010-04-01/Accounts/", c.handleMessages)

	srv := httptest.NewUnstartedServer(mux)
	// NewUnstartedServer has already bound an ephemeral listener of its own;
	// discard it in favour of the one on the requested port.
	if err := srv.Listener.Close(); err != nil {
		ln.Close()
		return nil, fmt.Errorf("discard the default capture listener: %w", err)
	}
	srv.Listener = ln
	srv.Start()

	c.Server = srv
	return c, nil
}

// handleMessages records the outbound SMS-send POST and answers with the
// minimal 201 JSON shape twilio.RESTClient.SendSMS treats as success.
//
// Recording takes the whole posted form and the arrival time, appends the
// message, and queues a copy for every subscriber — all without waiting on
// any reader, so no subscriber, however slow, can hold up the response.
//
// The "sid" key is a contract, not decoration: twilio.RESTClient.SendSMS
// parses it to return the message id, and a 2xx carrying none yields an empty
// id rather than an error — so dropping it here would go unnoticed.
// TestCaptureServer_SendSMSGetsTheSIDBack asserts the id makes it back, which
// is what keeps the two ends honest.
func (c *CaptureServer) handleMessages(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}

	msg := Message{
		AccountSID: strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/2010-04-01/Accounts/"), "/Messages.json"),
		From:       r.PostForm.Get("From"),
		To:         r.PostForm.Get("To"),
		Body:       r.PostForm.Get("Body"),
		Form:       r.PostForm,
	}.clone()

	c.mu.Lock()
	msg.ReceivedAt = time.Now()
	c.msgs = append(c.msgs, msg)
	for s := range c.subs {
		s.enqueue(msg.clone())
	}
	c.mu.Unlock()

	select {
	case c.notify <- struct{}{}:
	default: // a wake-up is already pending; the waiter re-reads the count
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"sid":    NewSID("SM"),
		"status": "queued",
	})
}

// Captured returns a snapshot of every request recorded so far (see Message).
func (c *CaptureServer) Captured() []Message {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Message, len(c.msgs))
	for i, msg := range c.msgs {
		out[i] = msg.clone()
	}
	return out
}

// Notify returns the server's one wake-up channel, so a waiter is woken by an
// arrival itself rather than by polling for it. Every recorded message sends a
// wake-up on it without blocking; it is buffered by one, so arrivals while a
// wake-up is already pending coalesce into that one. A wake-up signals
// "something arrived", not "your message arrived", so a waiter re-reads
// Captured to decide. It is receive-only to callers; only the capture server
// sends on it.
//
// It is single-consumer: every caller gets the same channel and each wake-up
// reaches one receiver only, so wait on it from one goroutine at a time. A
// second concurrent waiter can miss the wake-up for a message that was
// recorded, and block until a later arrival or its own timeout. Waiters that
// take turns, each re-reading Captured, are fine.
func (c *CaptureServer) Notify() <-chan struct{} {
	return c.notify
}

// Subscribe starts a subscription to arrivals. msgs delivers every message
// recorded after Subscribe returns, as the Message itself, in the order the
// server recorded them — the order Captured lists them, which is the same for
// every subscriber. There is no replay: a subscriber joining after N messages
// receives the N+1th onward, and the earlier ones are Captured's to give.
//
// Each subscription has a queue of its own with no bound. A subscriber that
// reads slowly, or not at all, only falls behind: nothing is dropped from its
// queue, and it never blocks the handler or any other subscriber.
//
// unsubscribe ends the subscription. Once it returns, nothing more is
// delivered: anything still queued is discarded, the goroutine feeding msgs
// has exited, and msgs is closed. It is safe to call more than once, and from
// several goroutines. Close ends every subscription the same way, so a
// subscriber ranging over msgs stops when the server does; subscribing to a
// closed server yields a msgs that is already closed.
func (c *CaptureServer) Subscribe() (msgs <-chan Message, unsubscribe func()) {
	s := newSubscription()

	c.mu.Lock()
	closed := c.closed
	if !closed {
		c.subs[s] = struct{}{}
	}
	c.mu.Unlock()

	if closed {
		s.end()
	}
	return s.out, func() {
		c.mu.Lock()
		delete(c.subs, s)
		c.mu.Unlock()
		s.end()
	}
}

// Close shuts the server down, waiting for in-flight POSTs to finish as
// httptest.Server.Close does, and then ends every subscription as its
// unsubscribe would: messages a subscriber had not yet received are not
// delivered — Captured still holds them — and no subscription's goroutine
// outlives the server. Call CaptureServer.Close, not the embedded Server's
// Close: that one knows nothing of subscriptions, so it shuts the server down
// and leaves every subscription open, its feed still running, until
// CaptureServer.Close ends them. Calling Close again is safe,
// and so is calling it from several goroutines at once: every call returns
// only once the server is down and every subscription has ended.
func (c *CaptureServer) Close() {
	c.closeOnce.Do(func() {
		c.Server.Close()

		c.mu.Lock()
		c.closed = true
		subs := c.subs
		c.subs = nil
		c.mu.Unlock()

		for s := range subs {
			s.end()
		}
	})
}

// subscription is one Subscribe caller's feed. The handler appends to queue
// and returns at once; pump hands queue on to out at whatever pace the
// subscriber reads, so a subscriber that falls behind only lengthens its own
// queue.
type subscription struct {
	out  chan Message  // unbuffered: pump hands each message straight to the reader
	wake chan struct{} // buffered by one: queue may have grown since pump last looked
	stop chan struct{} // closed, once, to end the subscription
	done chan struct{} // closed when pump has returned
	once sync.Once

	mu    sync.Mutex
	queue []Message
}

// newSubscription returns a subscription with its pump already running.
func newSubscription() *subscription {
	s := &subscription{
		out:  make(chan Message),
		wake: make(chan struct{}, 1),
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
	go s.pump()
	return s
}

// enqueue adds msg to the queue and wakes pump, without ever waiting: the
// queue has no bound, and a wake-up already pending covers this one too.
func (s *subscription) enqueue(msg Message) {
	s.mu.Lock()
	s.queue = append(s.queue, msg)
	s.mu.Unlock()

	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// pump delivers the queue to out in order until the subscription ends. As it
// returns it closes out and only then done, so a closed done means out is
// closed: end returns on done and promises that.
func (s *subscription) pump() {
	defer func() {
		close(s.out) // first: once done is closed, end returns, promising out closed
		close(s.done)
	}()

	for {
		// Checked first, so a subscription that has ended stops here rather
		// than racing a ready reader for one more delivery.
		select {
		case <-s.stop:
			return
		default:
		}

		s.mu.Lock()
		if len(s.queue) == 0 {
			s.queue = nil // let a drained backlog's array go
			s.mu.Unlock()
			select {
			case <-s.wake:
				continue
			case <-s.stop:
				return
			}
		}
		next := s.queue[0]
		s.queue[0] = Message{}
		s.queue = s.queue[1:]
		s.mu.Unlock()

		select {
		case s.out <- next:
		case <-s.stop:
			return
		}
	}
}

// end stops the subscription and returns once pump has: whatever is still
// queued is never delivered, and out is closed. Safe to call any number of
// times, from any goroutine.
func (s *subscription) end() {
	s.once.Do(func() { close(s.stop) })
	<-s.done
}
