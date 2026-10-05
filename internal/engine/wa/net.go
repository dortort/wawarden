package wa

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

const (
	dialTimeout           = 10 * time.Second
	tlsTimeout            = 10 * time.Second
	responseHeaderTimeout = 20 * time.Second
	idleTimeout           = 90 * time.Second
	versionTimeout        = 20 * time.Second
	maxVersionPage        = 8 << 20
	maxRedirects          = 3
)

var (
	errOffline          = errors.New("wa: a test binary opens no network connection")
	errResponseTooLarge = errors.New("wa: the response is larger than its cap")
	errRedirect         = errors.New("wa: a redirect to another host or scheme is refused")
)

type dialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

func guarded(dial dialFunc) dialFunc {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		if testing.Testing() {
			return nil, errOffline
		}
		return dial(ctx, network, addr)
	}
}

func systemDial() dialFunc {
	return (&net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second}).DialContext
}

func newTransport(dial dialFunc) *http.Transport {
	return &http.Transport{
		Proxy:                 nil,
		DialContext:           dial,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:   tlsTimeout,
		ResponseHeaderTimeout: responseHeaderTimeout,
		IdleConnTimeout:       idleTimeout,
		MaxIdleConns:          4,
	}
}

type capped struct {
	next http.RoundTripper
	max  int64
}

func (c capped) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := c.next.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if resp.ContentLength > c.max {
		_ = resp.Body.Close()
		return nil, errResponseTooLarge
	}
	resp.Body = &cappedBody{body: resp.Body, left: c.max}
	return resp, nil
}

type cappedBody struct {
	body io.ReadCloser
	left int64
}

func (b *cappedBody) Read(p []byte) (int, error) {
	if b.left <= 0 {
		var probe [1]byte
		n, err := b.body.Read(probe[:])
		if n > 0 {
			return 0, errResponseTooLarge
		}
		return 0, err
	}
	if int64(len(p)) > b.left {
		p = p[:b.left]
	}
	n, err := b.body.Read(p)
	b.left -= int64(n)
	return n, err
}

func (b *cappedBody) Close() error { return b.body.Close() }

func sameHostRedirects(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects || req.URL.Scheme != "https" || req.URL.Host != via[0].URL.Host {
		return errRedirect
	}
	return nil
}
