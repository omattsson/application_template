package auth

import (
	"crypto/tls"
	"errors"
	"io"
	"net/http"
	"time"
)

// maxResponseBytes limits every response of the identity provider (discovery
// document, key set, token response). A real one is a few kilobytes; the
// limit stops a hostile or broken endpoint from filling the memory.
const maxResponseBytes = 1 << 20

var errResponseTooLarge = errors.New("response too large")

// newHTTPClient returns the client for every request to the identity
// provider: certificate and host name verification on (the Go defaults, TLS
// 1.2 or later), a total timeout, a limit on the response size, and no
// redirects. A redirect is refused, not followed: the token request carries
// the authorization code and the client secret, and must not be sent to a
// host that the discovery document did not name. The answer of a refused
// redirect is the 3xx response itself, which the callers treat as an error.
func newHTTPClient(timeout time.Duration) *http.Client {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		base = &http.Transport{}
	}
	tr := base.Clone()
	tr.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	tr.ResponseHeaderTimeout = timeout
	tr.MaxResponseHeaderBytes = 64 << 10

	return &http.Client{
		Transport: &limitTransport{base: tr, max: maxResponseBytes},
		Timeout:   timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// limitTransport limits the size of the (decompressed) body of every
// response.
type limitTransport struct {
	base http.RoundTripper
	max  int64
}

func (t *limitTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if resp.ContentLength > t.max {
		_ = resp.Body.Close()
		return nil, errResponseTooLarge
	}
	resp.Body = &limitedBody{rc: resp.Body, left: t.max}
	return resp, nil
}

// limitedBody fails with errResponseTooLarge when the body is longer than the
// limit (unlike io.LimitReader, which cuts it without a sign).
type limitedBody struct {
	rc   io.ReadCloser
	left int64
}

func (b *limitedBody) Read(p []byte) (int, error) {
	if b.left <= 0 {
		// The limit is used up: more data is an error, the end is fine.
		var one [1]byte
		n, err := b.rc.Read(one[:])
		if n > 0 {
			return 0, errResponseTooLarge
		}
		return 0, err
	}
	if int64(len(p)) > b.left {
		p = p[:b.left]
	}
	n, err := b.rc.Read(p)
	b.left -= int64(n)
	return n, err
}

func (b *limitedBody) Close() error { return b.rc.Close() }
