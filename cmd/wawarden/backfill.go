package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/dortort/wawarden/internal/sanitize"
)

const (
	maxBackfillCount = 50
	anchorOldest     = "oldest"
	anchorNewest     = "newest"
)

func adminBackfill(ctx context.Context, args, environ []string, stdin io.Reader, stdout, stderr io.Writer) int {
	flags, src := adminFlags("admin backfill")
	chat := flags.String("chat", "", "the chat whose older messages to ask the phone for: a chat identifier, a +E.164 number or a chat reference")
	count := flags.Int("count", maxBackfillCount, "how many messages before the anchor, from 1 to 50")
	anchor := flags.String("anchor", anchorOldest, "oldest asks for the messages before the chat's oldest archived one, newest for those before its newest")
	if !parseAdminFlags(flags, args, stderr) {
		return usageError(stderr)
	}
	if *chat == "" {
		_, _ = fmt.Fprintln(stderr, "admin: --chat needs a value")
		return usageError(stderr)
	}
	if *count < 1 || *count > maxBackfillCount {
		_, _ = fmt.Fprintln(stderr, "admin: --count must be from 1 to 50")
		return usageError(stderr)
	}
	if *anchor != anchorOldest && *anchor != anchorNewest {
		_, _ = fmt.Fprintln(stderr, "admin: --anchor must be oldest or newest")
		return usageError(stderr)
	}
	session, code := adminConnect(ctx, flags, src, environ, stdin, stderr)
	if session == nil {
		return code
	}
	id, code := session.chatIdentifier(ctx, stderr, "backfill", *chat)
	if code == exitUsage {
		_, _ = fmt.Fprintln(stderr, "admin: --chat is not a chat identifier, a +E.164 number or a chat reference")
		return usageError(stderr)
	}
	if code != 0 {
		return code
	}
	sent := ""
	if *anchor == anchorNewest {
		sent = anchorNewest
	}
	body, err := json.Marshal(struct {
		Chat   string `json:"chat"`
		Count  int    `json:"count"`
		Anchor string `json:"anchor,omitempty"`
	}{id, *count, sent})
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "admin backfill: the request cannot be encoded")
		return exitFailed
	}
	return session.send(ctx, stdout, stderr, adminRequest{label: "backfill", method: http.MethodPost, path: []string{"backfill"}, body: body, print: printBackfill})
}

func printBackfill(answer []byte, stdout, stderr io.Writer) error {
	var a struct {
		RequestID string `json:"request_id"`
	}
	if json.Unmarshal(answer, &a) != nil || a.RequestID == "" {
		return errBadAnswer
	}
	if _, err := fmt.Fprintf(stdout, "backfill requested: %s\n", sanitize.Terminal(a.RequestID)); err != nil {
		return err
	}
	_, _ = fmt.Fprintln(stderr, "The phone sends the messages when it processes the request; they arrive as history and admin status counts today's requests.")
	return nil
}
