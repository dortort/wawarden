package wa

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"strconv"
	"strings"
	"sync"
	"testing"

	"go.mau.fi/whatsmeow/socket"

	"github.com/dortort/wawarden/internal/engine"
)

var errNoDial = errors.New("synthetic: the test dialer refuses")

type page struct {
	status   int
	body     []byte
	length   int64
	location string
}

type pages struct {
	mu     sync.Mutex
	byURL  map[string]page
	served []string
}

func (p *pages) RoundTrip(req *http.Request) (*http.Response, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.served = append(p.served, req.URL.String())
	pg, ok := p.byURL[req.URL.String()]
	if !ok {
		return nil, errNoDial
	}
	resp := &http.Response{StatusCode: pg.status, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(pg.body)), ContentLength: pg.length, Request: req}
	if pg.length == 0 {
		resp.ContentLength = -1
	}
	if pg.location != "" {
		resp.Header.Set("Location", pg.location)
	}
	return resp, nil
}

func revisionPage(rev uint32) []byte {
	return []byte(`<html><script>{"client_revision":` + strconv.FormatUint(uint64(rev), 10) + `,"other":1}</script></html>`)
}

func TestTheVersionSourceReadsTheRevisionFromTheWebClient(t *testing.T) {
	p := &pages{byURL: map[string]page{socket.Origin: {status: http.StatusOK, body: revisionPage(1027000123)}}}
	got, err := newVersions(p).Latest(t.Context())
	if err != nil || got != (engine.Version{2, 3000, 1027000123}) {
		t.Fatalf("Latest = %v, %v", got, err)
	}
	if len(p.served) != 1 || p.served[0] != socket.Origin {
		t.Fatalf("requests %v, want one to the web client's origin", p.served)
	}
}

func TestTheVersionSourceFailsClosed(t *testing.T) {
	const canary = "SYNTHETIC-BODY-CANARY"
	other := "https://elsewhere.example/"
	for name, pg := range map[string]page{
		"an error status, whose body the library quotes": {status: http.StatusServiceUnavailable, body: []byte(canary)},
		"no revision":                      {status: http.StatusOK, body: []byte("<html>" + canary + "</html>")},
		"a page over the cap":              {status: http.StatusOK, body: append(bytes.Repeat([]byte(" "), maxVersionPage), revisionPage(1)...)},
		"an announced length over it":      {status: http.StatusOK, body: revisionPage(1), length: maxVersionPage + 1},
		"a redirect to another host":       {status: http.StatusFound, location: other},
		"a redirect to plain HTTP":         {status: http.StatusFound, location: "http://" + strings.TrimPrefix(socket.Origin, "https://") + "/"},
		"a redirect loop on the same host": {status: http.StatusFound, location: socket.Origin},
	} {
		t.Run(name, func(t *testing.T) {
			p := &pages{byURL: map[string]page{socket.Origin: pg, other: {status: http.StatusOK, body: revisionPage(1)}}}
			got, err := newVersions(p).Latest(t.Context())
			if !errors.Is(err, errVersionFetch) || !got.IsZero() {
				t.Fatalf("Latest = %v, %v, want errVersionFetch", got, err)
			}
			if strings.Contains(err.Error(), canary) {
				t.Fatalf("the error %q repeats the response", err)
			}
			for _, u := range p.served {
				if u == other {
					t.Fatal("the redirect to another host was followed")
				}
			}
		})
	}
}

func TestTheVersionSourceFollowsARedirectOnTheSameHost(t *testing.T) {
	moved := socket.Origin + "/moved"
	p := &pages{byURL: map[string]page{
		socket.Origin: {status: http.StatusMovedPermanently, location: moved},
		moved:         {status: http.StatusOK, body: revisionPage(1027000124)},
	}}
	if got, err := newVersions(p).Latest(t.Context()); err != nil || got[2] != 1027000124 {
		t.Fatalf("Latest = %v, %v", got, err)
	}
}

func checkTransport(t *testing.T, name string, rt http.RoundTripper) {
	t.Helper()
	tr, ok := rt.(*http.Transport)
	if !ok {
		t.Fatalf("the %s client's transport is %T", name, rt)
	}
	if tr.Proxy != nil || tr.DialContext == nil || tr.TLSHandshakeTimeout <= 0 || tr.ResponseHeaderTimeout <= 0 || tr.TLSClientConfig.MinVersion < 0x0303 {
		t.Errorf("the %s transport is not hardened: proxy %v, dial %v, TLS timeout %v, header timeout %v", name, tr.Proxy != nil, tr.DialContext != nil, tr.TLSHandshakeTimeout, tr.ResponseHeaderTimeout)
	}
	if _, err := tr.DialContext(t.Context(), "tcp", "127.0.0.1:9"); !errors.Is(err, errOffline) {
		t.Errorf("the %s transport dialled from a test binary: %v", name, err)
	}
}

func TestEveryDialThePackageBuildsRefusesInATestBinary(t *testing.T) {
	dialled := func(context.Context, string, string) (net.Conn, error) {
		t.Error("a dial that a caller handed to the package was used in a test binary")
		return nil, errNoDial
	}
	if _, err := systemDial()(t.Context(), "tcp", "127.0.0.1:9"); !errors.Is(err, errOffline) {
		t.Errorf("the system dialer = %v, want the offline refusal", err)
	}
	if _, err := newTransport(dialled).DialContext(t.Context(), "tcp", "127.0.0.1:9"); !errors.Is(err, errOffline) {
		t.Errorf("a transport built over another dial = %v, want the offline refusal", err)
	}
	if got, err := newVersions(newTransport(dialled)).Latest(t.Context()); !errors.Is(err, errVersionFetch) || !got.IsZero() {
		t.Errorf("a version source over another dial = %v, %v, want errVersionFetch", got, err)
	}
}

func TestAVersionSourceWithoutItsClientFetchesNothing(t *testing.T) {
	ctx := httptrace.WithClientTrace(t.Context(), &httptrace.ClientTrace{GetConn: func(string) {
		t.Fatal("a version source without its client asked for a connection")
	}})
	if got, err := new(Versions).Latest(ctx); !errors.Is(err, errVersionFetch) || !got.IsZero() {
		t.Fatalf("Latest = %v, %v, want errVersionFetch", got, err)
	}
}

func TestTheProductionVersionClientIsBoundedAndIgnoresTheProxyEnvironment(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:9")
	client := NewVersions().client
	c, ok := client.Transport.(capped)
	if !ok || c.max != maxVersionPage || client.Timeout <= 0 || client.CheckRedirect == nil {
		t.Fatalf("the version client is not bounded: transport %T, timeout %v", client.Transport, client.Timeout)
	}
	checkTransport(t, "version", c.next)
}
