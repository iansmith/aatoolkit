package twilio

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// Errors FetchMedia and DeleteMedia return for the cases a caller acts on.
// Each is wrapped, so test with errors.Is.
var (
	// ErrMediaURL means the URL is not a Media resource of this client's
	// account at its BaseURL. The client's credentials are never sent to it.
	ErrMediaURL = errors.New("twilio: not a media URL of this account")
	// ErrMediaNotImage means the response's Content-Type is not image/*.
	// The body is not read.
	ErrMediaNotImage = errors.New("twilio: media is not an image")
	// ErrMediaTooLarge means the body is longer than the caller's cap. What
	// was read is discarded.
	ErrMediaTooLarge = errors.New("twilio: media exceeds size cap")
	// ErrMediaNotFound means the provider answered 404: the media item does
	// not exist, which includes one already deleted.
	ErrMediaNotFound = errors.New("twilio: media not found")
)

// mediaPath matches a Media resource path:
// /2010-04-01/Accounts/{AccountSid}/Messages/{MessageSid}/Media/{MediaSid},
// with or without a trailing .json. Group 1 is the path without the suffix,
// group 2 the account.
var mediaPath = regexp.MustCompile(`^(/2010-04-01/Accounts/([A-Za-z0-9]+)/Messages/[A-Za-z0-9]+/Media/[A-Za-z0-9]+)(?:\.json)?$`)

// mediaResource checks that mediaURL names a Media resource of c's account at
// c's BaseURL, and returns it without any .json suffix. It is what keeps the
// account's credentials from going anywhere but the provider: a URL on another
// host, or under another account, is refused before a request is built.
func (c *RESTClient) mediaResource(mediaURL string) (*url.URL, error) {
	base := c.BaseURL
	if base == "" {
		base = defaultBaseURL
	}
	b, err := url.Parse(base)
	if err != nil {
		return nil, fmt.Errorf("twilio: parse base URL: %w", err)
	}
	u, err := url.Parse(mediaURL)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMediaURL, err)
	}
	if u.Scheme != b.Scheme || u.Host != b.Host || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, ErrMediaURL
	}
	m := mediaPath.FindStringSubmatch(u.Path)
	if m == nil || m[2] != c.AccountSID {
		return nil, ErrMediaURL
	}
	return &url.URL{Scheme: u.Scheme, Host: u.Host, Path: m[1]}, nil
}

func (c *RESTClient) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return http.DefaultClient
}

// FetchMedia reads one media item (an InboundSMS MediaItem.URL) into memory
// and returns its bytes and content type. Auth is the API key pair, as for
// SendSMS. Nothing is written anywhere: the bytes exist only in the returned
// slice.
//
// It refuses, before any request, a URL that is not a Media resource of this
// account (ErrMediaURL). It accepts only an image/* response (ErrMediaNotImage)
// of at most maxBytes bytes (ErrMediaTooLarge); maxBytes must be positive. A
// 404 is ErrMediaNotFound; any other non-2xx is an error carrying the status.
//
// The provider answers the resource URL with a redirect to where the bytes
// live; the client follows it, and net/http drops the Authorization header on
// a redirect to another host, so the credentials go only to the provider.
func (c *RESTClient) FetchMedia(ctx context.Context, mediaURL string, maxBytes int64) (data []byte, contentType string, err error) {
	if maxBytes <= 0 {
		return nil, "", fmt.Errorf("twilio: fetch media: maxBytes must be positive, got %d", maxBytes)
	}
	u, err := c.mediaResource(mediaURL)
	if err != nil {
		return nil, "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, "", fmt.Errorf("twilio: build request: %w", err)
	}
	req.SetBasicAuth(c.KeySID, c.KeySecret)

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("twilio: fetch media: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, "", ErrMediaNotFound
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", fmt.Errorf("twilio: fetch media failed: status %d", resp.StatusCode)
	}

	contentType = resp.Header.Get("Content-Type")
	mt, _, _ := mime.ParseMediaType(contentType)
	if !strings.HasPrefix(mt, "image/") {
		return nil, "", fmt.Errorf("%w: %q", ErrMediaNotImage, contentType)
	}
	if resp.ContentLength > maxBytes {
		return nil, "", fmt.Errorf("%w: %d bytes, cap %d", ErrMediaTooLarge, resp.ContentLength, maxBytes)
	}
	data, err = io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, "", fmt.Errorf("twilio: read media: %w", err)
	}
	if int64(len(data)) > maxBytes {
		return nil, "", fmt.Errorf("%w: over %d bytes", ErrMediaTooLarge, maxBytes)
	}
	return data, contentType, nil
}

// DeleteMedia removes one media item (an InboundSMS MediaItem.URL) at the
// provider: a DELETE of the Media resource under its Message. After it
// succeeds, the media URL answers 404. A URL that is not a Media resource of
// this account is refused before any request (ErrMediaURL). A 404 is
// ErrMediaNotFound, so a caller can treat a repeated delete as done; any other
// non-2xx is an error carrying the status and the provider's response body.
func (c *RESTClient) DeleteMedia(ctx context.Context, mediaURL string) error {
	u, err := c.mediaResource(mediaURL)
	if err != nil {
		return err
	}
	u.Path += ".json"
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, u.String(), nil)
	if err != nil {
		return fmt.Errorf("twilio: build request: %w", err)
	}
	req.SetBasicAuth(c.KeySID, c.KeySecret)

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("twilio: delete media: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))

	if resp.StatusCode == http.StatusNotFound {
		return ErrMediaNotFound
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("twilio: delete media failed: status %d: %s", resp.StatusCode, string(body))
	}
	return nil
}
