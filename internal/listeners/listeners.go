// Package listeners is the only package that opens sockets: named plain-HTTP listeners served by hardened servers.
package listeners

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"time"

	"github.com/dortort/wawarden/internal/safego"
)

const (
	readHeaderTimeout = 5 * time.Second
	readTimeout       = 15 * time.Second
	writeTimeout      = 30 * time.Second
	idleTimeout       = 60 * time.Second
	maxHeaderBytes    = 16 << 10
)

type Spec struct {
	Name    string
	Addr    netip.AddrPort
	Handler http.Handler
}

type Bound struct {
	Name string
	Addr netip.AddrPort
}

type Set struct {
	servers []*server
	errs    chan error
}

type server struct {
	name  string
	bound netip.AddrPort
	ln    net.Listener
	http  *http.Server
}

func Open(ctx context.Context, logger *slog.Logger, specs []Spec) (*Set, error) {
	s := &Set{errs: make(chan error, len(specs))}
	for _, spec := range specs {
		srv, err := open(ctx, logger, spec)
		if err != nil {
			s.closeListeners()
			return nil, err
		}
		s.servers = append(s.servers, srv)
	}
	return s, nil
}

func open(ctx context.Context, logger *slog.Logger, spec Spec) (*server, error) {
	if spec.Name == "" || !spec.Addr.IsValid() || spec.Handler == nil {
		return nil, fmt.Errorf("listeners: spec %q needs a name, an address and a handler", spec.Name)
	}
	network := "tcp6"
	if spec.Addr.Addr().Is4() {
		network = "tcp4"
	}
	ln, err := (&net.ListenConfig{}).Listen(ctx, network, spec.Addr.String())
	if err != nil {
		return nil, fmt.Errorf("listeners: %s: %w", spec.Name, err)
	}
	errorLog := logger.With(slog.String("event", "http_server_error"), slog.String("listener", spec.Name))
	return &server{
		name:  spec.Name,
		bound: ln.Addr().(*net.TCPAddr).AddrPort(),
		ln:    ln,
		http: &http.Server{
			Handler:           spec.Handler,
			ReadHeaderTimeout: readHeaderTimeout,
			ReadTimeout:       readTimeout,
			WriteTimeout:      writeTimeout,
			IdleTimeout:       idleTimeout,
			MaxHeaderBytes:    maxHeaderBytes,
			ErrorLog:          slog.NewLogLogger(errorLog.Handler(), slog.LevelWarn),
		},
	}, nil
}

func (s *Set) Inventory() []Bound {
	out := make([]Bound, 0, len(s.servers))
	for _, srv := range s.servers {
		out = append(out, Bound{Name: srv.name, Addr: srv.bound})
	}
	return out
}

func (s *Set) Serve() {
	for _, srv := range s.servers {
		safego.Go("listeners."+srv.name, func() {
			if err := srv.http.Serve(srv.ln); !errors.Is(err, http.ErrServerClosed) {
				s.errs <- fmt.Errorf("listeners: %s: %w", srv.name, err)
			}
		})
	}
}

func (s *Set) Err() <-chan error { return s.errs }

func (s *Set) Shutdown(ctx context.Context) error {
	errs := make([]error, len(s.servers))
	var wg sync.WaitGroup
	for i, srv := range s.servers {
		wg.Add(1)
		safego.Go("listeners.shutdown", func() {
			defer wg.Done()
			errs[i] = srv.shutdown(ctx)
		})
	}
	wg.Wait()
	return errors.Join(errs...)
}

func (srv *server) shutdown(ctx context.Context) error {
	var errs []error
	if err := srv.http.Shutdown(ctx); err != nil && !errors.Is(err, net.ErrClosed) {
		errs = append(errs, err, srv.http.Close())
	}
	if err := srv.ln.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		errs = append(errs, err)
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("listeners: %s: %w", srv.name, err)
	}
	return nil
}

func (s *Set) closeListeners() {
	for _, srv := range s.servers {
		_ = srv.ln.Close()
	}
}
