package twilio_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"

	"github.com/iansmith/aatoolkit/telephony/twilio"
)

const (
	testAccount = "ACtest"
	testMessage = "MMtest"
)

func mediaPathFor(media string) string {
	return "/2010-04-01/Accounts/" + testAccount + "/Messages/" + testMessage + "/Media/" + media
}

// recordedMMSForm is the shape of a real inbound-MMS webhook with two
// attachments, as Twilio posts it (account and message ids replaced, the
// numbers fictional). Every MediaUrlN pairs with its MediaContentTypeN.
func recordedMMSForm() url.Values {
	return url.Values{
		"ToCountry":         {"US"},
		"MediaContentType0": {"image/jpeg"},
		"MediaContentType1": {"image/png"},
		"ToState":           {"CA"},
		"SmsMessageSid":     {testMessage},
		"NumMedia":          {"2"},
		"ToCity":            {""},
		"FromZip":           {"94501"},
		"SmsSid":            {testMessage},
		"FromState":         {"CA"},
		"SmsStatus":         {"received"},
		"FromCity":          {"ALAMEDA"},
		"Body":              {"the flyer"},
		"FromCountry":       {"US"},
		"To":                {"+15105550000"},
		"ToZip":             {""},
		"NumSegments":       {"1"},
		"MessageSid":        {testMessage},
		"AccountSid":        {testAccount},
		"From":              {"+15105551234"},
		"MediaUrl0":         {"https://api.twilio.com" + mediaPathFor("ME0")},
		"MediaUrl1":         {"https://api.twilio.com" + mediaPathFor("ME1")},
		"ApiVersion":        {"2010-04-01"},
	}
}

func TestServeSMS_MediaItemsFromRecordedForm(t *testing.T) {
	var got twilio.InboundSMS
	s := &twilio.Server{
		AuthToken: "authtoken",
		HandleSMS: func(_ context.Context, msg twilio.InboundSMS) { got = msg },
	}
	req := signedTwilioRequest(t, "authtoken", "https", "webhook.example.com", "/sms/inbound", recordedMMSForm())
	s.ServeSMS(httptest.NewRecorder(), req)

	want := twilio.InboundSMS{
		MessageSID: testMessage,
		From:       "+15105551234",
		To:         "+15105550000",
		Body:       "the flyer",
		HasMedia:   true,
		NumMedia:   2,
	}
	want.Media[0] = twilio.MediaItem{URL: "https://api.twilio.com" + mediaPathFor("ME0"), ContentType: "image/jpeg"}
	want.Media[1] = twilio.MediaItem{URL: "https://api.twilio.com" + mediaPathFor("ME1"), ContentType: "image/png"}
	if got != want {
		t.Fatalf("InboundSMS =\n%+v\nwant\n%+v", got, want)
	}
}

// NumMedia above MaxMedia keeps the claimed count but only MaxMedia items.
func TestServeSMS_MediaCappedAtMaxMedia(t *testing.T) {
	var got twilio.InboundSMS
	s := &twilio.Server{
		AuthToken: "authtoken",
		HandleSMS: func(_ context.Context, msg twilio.InboundSMS) { got = msg },
	}
	form := url.Values{"MessageSid": {"SM1"}, "NumMedia": {"12"}}
	for i := 0; i < 12; i++ {
		n := strconv.Itoa(i)
		form.Set("MediaUrl"+n, "u"+n)
		form.Set("MediaContentType"+n, "image/jpeg")
	}
	req := signedTwilioRequest(t, "authtoken", "https", "webhook.example.com", "/sms/inbound", form)
	s.ServeSMS(httptest.NewRecorder(), req)

	if got.NumMedia != 12 || !got.HasMedia {
		t.Fatalf("NumMedia = %d, HasMedia = %v; want 12, true", got.NumMedia, got.HasMedia)
	}
	for i := 0; i < twilio.MaxMedia; i++ {
		if got.Media[i].URL != "u"+strconv.Itoa(i) {
			t.Fatalf("Media[%d].URL = %q", i, got.Media[i].URL)
		}
	}
}

// A NumMedia that is not a positive integer yields no items, even with
// MediaUrl fields on the form.
func TestServeSMS_NoMediaWithoutPositiveNumMedia(t *testing.T) {
	for _, n := range []string{"0", "-1", "abc"} {
		var got twilio.InboundSMS
		s := &twilio.Server{
			AuthToken: "authtoken",
			HandleSMS: func(_ context.Context, msg twilio.InboundSMS) { got = msg },
		}
		form := url.Values{"MessageSid": {"SM1"}, "NumMedia": {n}, "MediaUrl0": {"u0"}, "MediaContentType0": {"image/jpeg"}}
		req := signedTwilioRequest(t, "authtoken", "https", "webhook.example.com", "/sms/inbound", form)
		s.ServeSMS(httptest.NewRecorder(), req)
		if got.NumMedia != 0 || got.HasMedia || got.Media != ([twilio.MaxMedia]twilio.MediaItem{}) {
			t.Fatalf("NumMedia %q: got %+v, want no media", n, got)
		}
	}
}

// mediaServer serves one media item at mediaPathFor("ME1") with the given
// type and body, checking the API key pair, and records what it was asked.
type mediaServer struct {
	srv         *httptest.Server
	contentType string
	body        []byte
	deleted     bool
	methods     []string
	paths       []string
}

func newMediaServer(t *testing.T, contentType string, body []byte) *mediaServer {
	ms := &mediaServer{contentType: contentType, body: body}
	ms.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ms.methods = append(ms.methods, r.Method)
		ms.paths = append(ms.paths, r.URL.Path)
		if u, p, ok := r.BasicAuth(); !ok || u != "SKkey" || p != "keysecret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if ms.deleted {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == mediaPathFor("ME1"):
			w.Header().Set("Content-Type", ms.contentType)
			w.Write(ms.body)
		case r.Method == http.MethodDelete && r.URL.Path == mediaPathFor("ME1")+".json":
			ms.deleted = true
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(ms.srv.Close)
	return ms
}

func (ms *mediaServer) client() *twilio.RESTClient {
	return &twilio.RESTClient{AccountSID: testAccount, KeySID: "SKkey", KeySecret: "keysecret", BaseURL: ms.srv.URL}
}

func (ms *mediaServer) mediaURL() string { return ms.srv.URL + mediaPathFor("ME1") }

func TestFetchMedia_ReturnsImage(t *testing.T) {
	body := []byte("\xff\xd8\xff not really a jpeg")
	ms := newMediaServer(t, "image/jpeg", body)

	data, ct, err := ms.client().FetchMedia(context.Background(), ms.mediaURL(), 1024)
	if err != nil {
		t.Fatalf("FetchMedia: %v", err)
	}
	if !bytes.Equal(data, body) || ct != "image/jpeg" {
		t.Fatalf("got %q, %q", data, ct)
	}
}

// A body of exactly the cap is accepted; one byte over is not.
func TestFetchMedia_SizeCap(t *testing.T) {
	ms := newMediaServer(t, "image/png", bytes.Repeat([]byte("x"), 100))
	if _, _, err := ms.client().FetchMedia(context.Background(), ms.mediaURL(), 100); err != nil {
		t.Fatalf("at cap: %v", err)
	}
	if _, _, err := ms.client().FetchMedia(context.Background(), ms.mediaURL(), 99); !errors.Is(err, twilio.ErrMediaTooLarge) {
		t.Fatalf("over cap: err = %v, want ErrMediaTooLarge", err)
	}
}

// A body over the cap with no Content-Length (chunked) is caught by the read.
func TestFetchMedia_SizeCapWithoutContentLength(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		for i := 0; i < 10; i++ {
			w.Write(bytes.Repeat([]byte("x"), 50))
			w.(http.Flusher).Flush()
		}
	}))
	defer srv.Close()
	c := &twilio.RESTClient{AccountSID: testAccount, BaseURL: srv.URL}
	if _, _, err := c.FetchMedia(context.Background(), srv.URL+mediaPathFor("ME1"), 100); !errors.Is(err, twilio.ErrMediaTooLarge) {
		t.Fatalf("err = %v, want ErrMediaTooLarge", err)
	}
}

func TestFetchMedia_RejectsNonImage(t *testing.T) {
	for _, ct := range []string{"video/mp4", "text/vcard", "application/pdf", "", "imagex/png"} {
		ms := newMediaServer(t, ct, []byte("data"))
		if _, _, err := ms.client().FetchMedia(context.Background(), ms.mediaURL(), 1024); !errors.Is(err, twilio.ErrMediaNotImage) {
			t.Fatalf("Content-Type %q: err = %v, want ErrMediaNotImage", ct, err)
		}
	}
}

// The credentials go only to a Media resource of this account at BaseURL.
func TestMedia_RefusesForeignURLBeforeRequest(t *testing.T) {
	ms := newMediaServer(t, "image/jpeg", []byte("x"))
	c := ms.client()
	bad := []string{
		"https://evil.example.com" + mediaPathFor("ME1"),
		ms.srv.URL + "/2010-04-01/Accounts/ACother/Messages/" + testMessage + "/Media/ME1",
		ms.srv.URL + "/2010-04-01/Accounts/" + testAccount + "/Messages/" + testMessage,
		ms.srv.URL + mediaPathFor("ME1") + "?x=1",
		ms.srv.URL + mediaPathFor("ME1") + "/../../x",
		"::not a url",
	}
	for _, u := range bad {
		if _, _, err := c.FetchMedia(context.Background(), u, 1024); !errors.Is(err, twilio.ErrMediaURL) {
			t.Fatalf("FetchMedia(%q): err = %v, want ErrMediaURL", u, err)
		}
		if err := c.DeleteMedia(context.Background(), u); !errors.Is(err, twilio.ErrMediaURL) {
			t.Fatalf("DeleteMedia(%q): err = %v, want ErrMediaURL", u, err)
		}
	}
	if len(ms.methods) != 0 {
		t.Fatalf("server saw %d requests, want 0", len(ms.methods))
	}
}

// DeleteMedia sends DELETE to the Media resource's .json; afterwards the media
// URL is 404 and a second delete reports ErrMediaNotFound.
func TestDeleteMedia(t *testing.T) {
	ms := newMediaServer(t, "image/jpeg", []byte("x"))
	c := ms.client()

	if err := c.DeleteMedia(context.Background(), ms.mediaURL()); err != nil {
		t.Fatalf("DeleteMedia: %v", err)
	}
	if ms.methods[0] != http.MethodDelete || ms.paths[0] != mediaPathFor("ME1")+".json" {
		t.Fatalf("request = %s %s", ms.methods[0], ms.paths[0])
	}
	if _, _, err := c.FetchMedia(context.Background(), ms.mediaURL(), 1024); !errors.Is(err, twilio.ErrMediaNotFound) {
		t.Fatalf("fetch after delete: err = %v, want ErrMediaNotFound", err)
	}
	if err := c.DeleteMedia(context.Background(), ms.mediaURL()+".json"); !errors.Is(err, twilio.ErrMediaNotFound) {
		t.Fatalf("second delete: err = %v, want ErrMediaNotFound", err)
	}
}

func TestDeleteMedia_ErrorCarriesStatusNotCredentials(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"code":20003}`)
	}))
	defer srv.Close()
	c := &twilio.RESTClient{AccountSID: testAccount, KeySID: "SKkey", KeySecret: "keysecret", BaseURL: srv.URL}
	err := c.DeleteMedia(context.Background(), srv.URL+mediaPathFor("ME1"))
	if err == nil || !bytes.Contains([]byte(err.Error()), []byte("403")) || bytes.Contains([]byte(err.Error()), []byte("keysecret")) {
		t.Fatalf("err = %v", err)
	}
}
