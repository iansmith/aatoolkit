package twilio

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// defaultBaseURL is Twilio's production REST API host, used when RESTClient.BaseURL
// is empty. Tests (and SOP-188's CLI fake mode) override BaseURL to point at a local
// capture server instead.
const defaultBaseURL = "https://api.twilio.com"

// maxResponseBody caps how much of a response is read. A Messages resource is
// about a kilobyte, so this never truncates a real answer — it bounds what a
// misdirected BaseURL can do. That seam exists on purpose (tests and the CLI's
// fake mode both repoint it), so the thing on the other end is not always
// Twilio, and an unbounded read now sits on the success path rather than only
// on errors.
const maxResponseBody = 64 << 10

// RESTClient sends outbound SMS via the Twilio Messages REST API. Credentials are
// supplied by the caller — the client never reads TWILIO_* environment variables
// itself (charter R8; the consumer is responsible for sourcing its own credentials).
type RESTClient struct {
	AccountSID string
	KeySID     string
	KeySecret  string

	// BaseURL defaults to defaultBaseURL when empty. Overriding it is the test/fake
	// seam (SOP-188 points it at a local capture server).
	BaseURL string

	// HTTPClient defaults to http.DefaultClient when nil.
	HTTPClient *http.Client
}

// OutboundSMS is one message to send — the sending-side counterpart of
// InboundSMS, the parsed webhook. The field names deliberately match, with one
// asymmetry: inbound carries its MessageSID as a field, because the provider
// has already minted it, while outbound learns its id only as SendSMS's return.
//
// Named fields rather than positional arguments: From and To are both E.164
// strings, and transposing them sends a real message to the wrong handset — a
// mistake the compiler cannot catch when they are adjacent parameters of the
// same type.
type OutboundSMS struct {
	From string // the Twilio number to send from, E.164
	To   string // recipient, E.164
	Body string // message text

	// StatusCallback is where the provider should POST the message's terminal
	// delivery status. Optional: leave it empty and no callback is requested,
	// which is the behavior this client had before the field existed.
	//
	// It exists because a 2xx from the send means *accepted for delivery*, not
	// *delivered*. The real outcome — including a carrier rejection — arrives
	// later, at this URL, and there is otherwise no mechanism by which it can
	// reach the caller at all.
	//
	// The URL is used verbatim, so a caller that wants its own correlation id
	// can carry one in the query string. It is never read from the environment
	// (charter R8): endpoints, like credentials, come from the caller.
	//
	// Keep it free of secrets. A provider rejecting the URL echoes it back in
	// the response body, which SendSMS puts verbatim into the returned error —
	// so anything in the query string can reach a caller's logs. A correlation
	// id is the intended use; a bearer token is not.
	StatusCallback string
}

// SendSMS POSTs a Messages.json request and returns the provider's message id
// for the message it sent. Auth is HTTP Basic with the API key pair
// (KeySID:KeySecret), per D11's "prefer the API-key pair". A non-2xx response
// yields a non-nil error carrying the status code and response body;
// credentials never appear in that error text.
//
// The returned id is the correlator for msg.StatusCallback: every callback POST
// carries the same MessageSid, so this is what matches a later delivery outcome
// back to the send it belongs to. That is deliberately *all* the synchronous
// response yields — every outcome fact arrives at the callback instead. Wanting
// another field back from here (a price, a segment count) is a reversal of that
// design, not an additive tweak, and is why the id is a bare string rather than
// a struct with room to grow.
//
// **A non-nil error means the message was not accepted — nothing else.** When
// the provider answers 2xx but the body yields no id, the returned id is empty
// and the error is nil: the message *was* accepted, so there is no send failure
// to report. A caller seeing an empty id has lost per-message tracking for that
// send and nothing more — any StatusCallback it registered will still fire, but
// arrives carrying a MessageSid the caller has never seen and cannot match.
func (c *RESTClient) SendSMS(ctx context.Context, msg OutboundSMS) (messageSID string, err error) {
	base := c.BaseURL
	if base == "" {
		base = defaultBaseURL
	}
	endpoint := fmt.Sprintf("%s/2010-04-01/Accounts/%s/Messages.json", base, c.AccountSID)

	form := url.Values{
		"From": {msg.From},
		"To":   {msg.To},
		"Body": {msg.Body},
	}
	// Omitted entirely rather than sent empty: an empty StatusCallback is a
	// different request from no StatusCallback, and only the former asks the
	// provider to do anything with it.
	if msg.StatusCallback != "" {
		form.Set("StatusCallback", msg.StatusCallback)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("twilio: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(c.KeySID, c.KeySecret)

	client := c.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("twilio: send request failed: %w", err)
	}
	defer resp.Body.Close()

	// Read on both paths, not just the error one. Beyond needing the id, a body
	// left unread is a body the Transport cannot return to the idle pool — so
	// until now every successful send burned its connection and the next one
	// paid a fresh TLS handshake.
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("twilio: send failed: status %d: %s", resp.StatusCode, string(respBody))
	}

	// Error deliberately dropped: the message is already accepted, so there is
	// no send failure left to report. See the doc comment.
	var sent struct {
		SID string `json:"sid"`
	}
	_ = json.Unmarshal(respBody, &sent)
	return sent.SID, nil
}
