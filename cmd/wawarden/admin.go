package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/dortort/wawarden/internal/sanitize"
)

const (
	commandStatus    = "status"
	commandPair      = "pair"
	commandReconnect = "reconnect"

	defaultAdminAddr = "http://127.0.0.1:8082"

	exitFailed  = 1
	exitUsage   = 2
	exitToken   = 3
	exitDenied  = 4
	exitRefused = 5

	adminCallTimeout   = 40 * time.Second
	adminDialTimeout   = 5 * time.Second
	adminHeaderTimeout = 35 * time.Second
	maxAnswerBytes     = 64 << 10
)

var errAnswerTooLarge = errors.New("the answer is larger than 65536 bytes")

func adminCall(ctx context.Context, command string, args, environ []string, stdin io.Reader, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("admin "+command, flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() {}
	addr := flags.String("addr", defaultAdminAddr, "the admin listener's URL")
	file := flags.String("token-file", "", "read the admin token from this file")
	fromStdin := flags.Bool("token-stdin", false, "read the admin token from standard input")
	tokenCommand := flags.String("token-command", "", "run this command, without a shell, and read the admin token from its output")
	if err := flags.Parse(args); err != nil || flags.NArg() > 0 {
		return usageError(stderr)
	}
	sources := 0
	flags.Visit(func(f *flag.Flag) {
		if f.Name == "token-file" || f.Name == "token-command" {
			sources++
		}
	})
	if *fromStdin {
		sources++
	}
	if sources != 1 {
		_, _ = fmt.Fprintln(stderr, "admin: give exactly one of --token-file, --token-stdin and --token-command")
		return usageError(stderr)
	}
	base, plain, err := adminURL(*addr)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "admin:", err)
		return usageError(stderr)
	}
	if plain {
		_, _ = fmt.Fprintln(stderr, "admin: warning: --addr sends the admin token over plain HTTP to a host that is not loopback")
	}

	var secret string
	switch {
	case *fromStdin:
		secret, err = tokenFromStdin(stdin)
	case *tokenCommand != "":
		secret, err = tokenFromCommand(ctx, *tokenCommand, environ)
	default:
		secret, err = tokenFromFile(*file)
	}
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "admin:", err)
		return exitToken
	}

	status, answer, err := callAdmin(ctx, newAdminClient(), base, command, secret)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "admin %s: the request failed: %s\n", command, failure(err))
		return exitFailed
	}
	if status != http.StatusOK {
		return adminRefusal(command, status, answer, stderr)
	}
	if err := printAnswer(command, answer, stdout, stderr); err != nil {
		_, _ = fmt.Fprintf(stderr, "admin %s: %v\n", command, err)
		return exitFailed
	}
	return 0
}

func adminURL(raw string) (*url.URL, bool, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Hostname() == "" ||
		u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
		return nil, false, errors.New("--addr must be an http or https URL with a host and without credentials, query or fragment")
	}
	return u, u.Scheme == "http" && !loopbackHost(u.Hostname()), nil
}

func loopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	addr, err := netip.ParseAddr(host)
	return err == nil && addr.Unmap().IsLoopback()
}

func newAdminClient() *http.Client {
	return &http.Client{
		Timeout: adminCallTimeout,
		Transport: &http.Transport{
			Proxy:                 nil,
			DialContext:           (&net.Dialer{Timeout: adminDialTimeout}).DialContext,
			TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
			TLSHandshakeTimeout:   adminDialTimeout,
			ResponseHeaderTimeout: adminHeaderTimeout,
			DisableKeepAlives:     true,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func callAdmin(ctx context.Context, client *http.Client, base *url.URL, command, secret string) (int, []byte, error) {
	method, body := http.MethodPost, io.Reader(strings.NewReader("{}"))
	if command == commandStatus {
		method, body = http.MethodGet, nil
	}
	req, err := http.NewRequestWithContext(ctx, method, base.JoinPath("admin", "v1", command).String(), body) //nolint:gosec // G704: the operator chooses the admin listener's URL
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req) //nolint:gosec // G704: the operator chooses the admin listener's URL
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	answer, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswerBytes+1))
	if err != nil {
		return 0, nil, err
	}
	if len(answer) > maxAnswerBytes {
		return 0, nil, errAnswerTooLarge
	}
	return resp.StatusCode, answer, nil
}

func failure(err error) string {
	var timeout interface{ Timeout() bool }
	switch {
	case errors.Is(err, errAnswerTooLarge):
		return err.Error()
	case errors.As(err, &timeout) && timeout.Timeout():
		return "no answer in time"
	}
	var op *net.OpError
	if errors.As(err, &op) {
		return "cannot connect to the admin listener"
	}
	return "the connection failed"
}

func adminRefusal(command string, status int, answer []byte, stderr io.Writer) int {
	var refusal struct {
		Error string `json:"error"`
	}
	code := "unknown"
	if json.Unmarshal(answer, &refusal) == nil && refusal.Error != "" {
		code = sanitize.Terminal(refusal.Error)
	}
	_, _ = fmt.Fprintf(stderr, "admin %s: refused with %s (HTTP %d)\n", command, code, status)
	switch {
	case status == http.StatusUnauthorized, status == http.StatusTooManyRequests && refusal.Error == "too_many_requests":
		return exitDenied
	case status == http.StatusConflict, status == http.StatusTooManyRequests:
		return exitRefused
	}
	return exitFailed
}

type statusAnswer struct {
	State  *string `json:"state"`
	Reason *string `json:"reason"`
	Paired *bool   `json:"paired"`
	Counts *struct {
		Chats            int64 `json:"chats"`
		Messages         int64 `json:"messages"`
		BlobsPending     int64 `json:"history_blobs_pending"`
		BlobsQuarantined int64 `json:"history_blobs_quarantined"`
		InboxBacklog     int64 `json:"inbox_backlog"`
		InboxQuarantined int64 `json:"inbox_quarantined"`
	} `json:"counts"`
	LastIngestAt *string `json:"last_ingest_at"`
	Version      *string `json:"version"`
}

var errBadAnswer = errors.New("the answer is not what the admin listener sends")

func printAnswer(command string, answer []byte, stdout, stderr io.Writer) error {
	var out bytes.Buffer
	switch command {
	case commandStatus:
		var s statusAnswer
		if json.Unmarshal(answer, &s) != nil || s.State == nil || s.Reason == nil || s.Paired == nil || s.Counts == nil || s.Version == nil {
			return errBadAnswer
		}
		last := "never"
		if s.LastIngestAt != nil {
			last = *s.LastIngestAt
		}
		for _, line := range [][2]string{
			{"state", *s.State},
			{"reason", orNone(*s.Reason)},
			{"paired", strconv.FormatBool(*s.Paired)},
			{"chats", strconv.FormatInt(s.Counts.Chats, 10)},
			{"messages", strconv.FormatInt(s.Counts.Messages, 10)},
			{"history blobs pending", strconv.FormatInt(s.Counts.BlobsPending, 10)},
			{"history blobs quarantined", strconv.FormatInt(s.Counts.BlobsQuarantined, 10)},
			{"inbox backlog", strconv.FormatInt(s.Counts.InboxBacklog, 10)},
			{"inbox quarantined", strconv.FormatInt(s.Counts.InboxQuarantined, 10)},
			{"last ingest", last},
			{"version", *s.Version},
		} {
			fmt.Fprintf(&out, "%s: %s\n", line[0], sanitize.Terminal(line[1]))
		}
	case commandPair:
		var p struct {
			Code string `json:"code"`
		}
		if json.Unmarshal(answer, &p) != nil || p.Code == "" {
			return errBadAnswer
		}
		fmt.Fprintf(&out, "pairing code: %s\n", sanitize.Terminal(p.Code))
		_, _ = fmt.Fprintln(stderr, "On the phone, open Linked devices, choose Link a device, then Link with phone number instead, and enter the code.")
	case commandReconnect:
		var a struct {
			Status string `json:"status"`
		}
		if json.Unmarshal(answer, &a) != nil || a.Status != "accepted" {
			return errBadAnswer
		}
		out.WriteString("reconnect requested\n")
	}
	_, err := stdout.Write(out.Bytes())
	return err
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}
