package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"

	"github.com/dortort/wawarden/internal/policy"
	"github.com/dortort/wawarden/internal/sanitize"
	"github.com/dortort/wawarden/internal/token"
)

var chatReference = regexp.MustCompile(`^[0-9a-f]{32}$`)

type chatFlag []string

func (c *chatFlag) String() string { return "" }

func (c *chatFlag) Set(v string) error {
	*c = append(*c, v)
	return nil
}

type clientChatAnswer struct {
	ID    *string `json:"id"`
	Kind  *string `json:"kind"`
	Known *bool   `json:"known"`
	Name  *string `json:"name"`
}

type clientAnswer struct {
	ID                *string             `json:"id"`
	Name              *string             `json:"name"`
	State             *string             `json:"state"`
	CreatedAt         *string             `json:"created_at"`
	ExpiresAt         *string             `json:"expires_at"`
	RevokedAt         *string             `json:"revoked_at"`
	AllChats          *bool               `json:"all_chats"`
	AllowFirstContact *bool               `json:"allow_first_contact"`
	ReadChats         *[]clientChatAnswer `json:"read_chats"`
	WriteChats        *[]clientChatAnswer `json:"write_chats"`
	ReadChatCount     *int64              `json:"read_chat_count"`
	WriteChatCount    *int64              `json:"write_chat_count"`
}

type chatAnswer struct {
	ID   *string `json:"id"`
	Kind *string `json:"kind"`
	Ref  *string `json:"ref"`
	Name *string `json:"name"`
}

type chatsAnswer struct {
	Chats     *[]chatAnswer `json:"chats"`
	Truncated *bool         `json:"truncated"`
}

func adminClients(ctx context.Context, args, environ []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return usageError(stderr)
	}
	command, rest := args[0], args[1:]
	flags, src := adminFlags("admin clients " + command)
	var req adminRequest
	switch command {
	case "create":
		return clientCreate(ctx, flags, src, rest, environ, stdin, stdout, stderr)
	case "list":
		if !parseAdminFlags(flags, rest, stderr) {
			return usageError(stderr)
		}
		req = adminRequest{method: http.MethodGet, path: []string{"clients"}, print: printClientList}
	case "show", "revoke":
		id := flags.String("id", "", "the client's id")
		if !parseAdminFlags(flags, rest, stderr) {
			return usageError(stderr)
		}
		if !token.WellFormedClientID(*id) {
			_, _ = fmt.Fprintln(stderr, "admin: --id must be a client id: 8 characters of a to z and 2 to 7")
			return usageError(stderr)
		}
		req = adminRequest{method: http.MethodGet, path: []string{"clients", *id}, print: printClient}
		if command == "revoke" {
			req.method, req.path, req.body = http.MethodPost, append(req.path, "revoke"), []byte("{}")
		}
	default:
		return usageError(stderr)
	}
	req.label = "clients " + command
	return adminSend(ctx, flags, src, environ, stdin, stdout, stderr, req)
}

func clientCreate(ctx context.Context, flags *flag.FlagSet, src adminSource, args, environ []string, stdin io.Reader, stdout, stderr io.Writer) int {
	name := flags.String("name", "", "the client's name")
	var read, write chatFlag
	flags.Var(&read, "read", "a chat the client may read; repeat for more")
	flags.Var(&write, "write", "a chat the client may write to; repeat for more")
	allChats := flags.Bool("all-chats", false, "let the client read every chat")
	firstContact := flags.Bool("allow-first-contact", false, "let the client write to a direct chat the archive has not seen")
	days := flags.Int("expires-days", 0, "days until the client expires, 90 by default")
	if !parseAdminFlags(flags, args, stderr) {
		return usageError(stderr)
	}
	if *name == "" {
		_, _ = fmt.Fprintln(stderr, "admin: --name needs a value")
		return usageError(stderr)
	}
	session, code := adminConnect(ctx, flags, src, environ, stdin, stderr)
	if session == nil {
		return code
	}
	resolved := map[string][]string{}
	for _, set := range []struct {
		flag   string
		values []string
	}{{"read", read}, {"write", write}} {
		for i, v := range set.values {
			chat, code := session.chatIdentifier(ctx, stderr, v)
			if code != 0 {
				if code == exitUsage {
					_, _ = fmt.Fprintf(stderr, "admin: --%s value %d is not a chat identifier, a +E.164 number or a chat reference\n", set.flag, i+1)
					return usageError(stderr)
				}
				return code
			}
			resolved[set.flag] = append(resolved[set.flag], chat)
		}
	}
	body, err := json.Marshal(struct {
		Name              string   `json:"name"`
		ReadChats         []string `json:"read_chats"`
		WriteChats        []string `json:"write_chats"`
		AllChats          bool     `json:"all_chats"`
		AllowFirstContact bool     `json:"allow_first_contact"`
		ExpiresInDays     int      `json:"expires_in_days"`
	}{*name, nonNil(resolved["read"]), nonNil(resolved["write"]), *allChats, *firstContact, *days})
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "admin clients create: the request cannot be encoded")
		return exitFailed
	}
	if *allChats {
		_, _ = fmt.Fprintln(stderr, "admin clients create: warning: --all-chats lets this client read every chat, including chats that appear later")
	}
	return session.send(ctx, stdout, stderr, adminRequest{label: "clients create", method: http.MethodPost, path: []string{"clients"}, body: body, print: printCreated})
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func (s *adminSession) chatIdentifier(ctx context.Context, stderr io.Writer, v string) (string, int) {
	if digits, ok := policy.OwnerDigits(v); ok {
		return digits + "@s.whatsapp.net", 0
	}
	if chat, ok := policy.Normalize(v); ok {
		return chat.JID(), 0
	}
	if !chatReference.MatchString(v) {
		return "", exitUsage
	}
	answer, code := s.exchange(ctx, stderr, adminRequest{label: "clients create", method: http.MethodGet, path: []string{"chats"}, query: url.Values{"match": {v}}})
	if code != 0 {
		return "", code
	}
	var a chatsAnswer
	if json.Unmarshal(answer, &a) != nil || a.Chats == nil {
		_, _ = fmt.Fprintf(stderr, "admin clients create: %v\n", errBadAnswer)
		return "", exitFailed
	}
	for _, c := range *a.Chats {
		if c.Ref != nil && *c.Ref == v && c.ID != nil {
			if chat, ok := policy.Normalize(*c.ID); ok && chat.JID() == *c.ID {
				return chat.JID(), 0
			}
		}
	}
	_, _ = fmt.Fprintln(stderr, "admin clients create: no chat of the archive has that reference")
	return "", exitRefused
}

func printCreated(answer []byte, stdout, stderr io.Writer) error {
	var a struct {
		Client     *clientAnswer `json:"client"`
		Credential *string       `json:"credential"`
	}
	if json.Unmarshal(answer, &a) != nil || a.Client == nil || a.Credential == nil || *a.Credential == "" {
		return errBadAnswer
	}
	var out bytes.Buffer
	if err := writeClient(&out, stderr, *a.Client); err != nil {
		return err
	}
	fmt.Fprintf(&out, "token: %s\n", sanitize.Terminal(*a.Credential))
	if _, err := stdout.Write(out.Bytes()); err != nil {
		return err
	}
	_, _ = fmt.Fprintln(stderr, "Give the token to the client now and store it in its secret store, it is not shown again; revoke the client to withdraw it.")
	return nil
}

func printClient(answer []byte, stdout, stderr io.Writer) error {
	var a clientAnswer
	if json.Unmarshal(answer, &a) != nil {
		return errBadAnswer
	}
	var out bytes.Buffer
	if err := writeClient(&out, stderr, a); err != nil {
		return err
	}
	_, err := stdout.Write(out.Bytes())
	return err
}

func writeClient(out *bytes.Buffer, stderr io.Writer, a clientAnswer) error {
	if a.ID == nil || a.Name == nil || a.State == nil || a.CreatedAt == nil || a.ExpiresAt == nil || a.AllChats == nil ||
		a.AllowFirstContact == nil || a.ReadChats == nil || a.WriteChats == nil {
		return errBadAnswer
	}
	writeClientHead(out, a)
	for _, set := range []struct {
		key   string
		chats []clientChatAnswer
	}{{"read chat", *a.ReadChats}, {"write chat", *a.WriteChats}} {
		for _, c := range set.chats {
			if c.ID == nil || c.Kind == nil || c.Known == nil {
				return errBadAnswer
			}
			seen := "seen"
			if !*c.Known {
				seen = "not yet seen"
				_, _ = fmt.Fprintf(stderr, "warning: the archive has not seen the %s %s yet\n", set.key, sanitize.Terminal(*c.ID))
			}
			name := ""
			if c.Name != nil {
				name = " " + *c.Name
			}
			fmt.Fprintf(out, "%s: %s\n", set.key, sanitize.Terminal(*c.ID+" ("+*c.Kind+", "+seen+")"+name))
		}
	}
	return nil
}

func writeClientHead(out *bytes.Buffer, a clientAnswer) {
	revoked := "never"
	if a.RevokedAt != nil {
		revoked = *a.RevokedAt
	}
	for _, line := range [][2]string{
		{"id", *a.ID},
		{"name", *a.Name},
		{"state", *a.State},
		{"created", *a.CreatedAt},
		{"expires", *a.ExpiresAt},
		{"revoked", revoked},
		{"all chats", strconv.FormatBool(*a.AllChats)},
		{"allow first contact", strconv.FormatBool(*a.AllowFirstContact)},
	} {
		fmt.Fprintf(out, "%s: %s\n", line[0], sanitize.Terminal(line[1]))
	}
}

func printClientList(answer []byte, stdout, _ io.Writer) error {
	var a struct {
		Clients *[]clientAnswer `json:"clients"`
	}
	if json.Unmarshal(answer, &a) != nil || a.Clients == nil {
		return errBadAnswer
	}
	var out bytes.Buffer
	for i, c := range *a.Clients {
		if c.ID == nil || c.Name == nil || c.State == nil || c.CreatedAt == nil || c.ExpiresAt == nil || c.AllChats == nil ||
			c.AllowFirstContact == nil || c.ReadChatCount == nil || c.WriteChatCount == nil {
			return errBadAnswer
		}
		if i > 0 {
			out.WriteString("\n")
		}
		writeClientHead(&out, c)
		fmt.Fprintf(&out, "read chats: %d\nwrite chats: %d\n", *c.ReadChatCount, *c.WriteChatCount)
	}
	_, err := stdout.Write(out.Bytes())
	return err
}

func adminChats(ctx context.Context, args, environ []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "list" {
		return usageError(stderr)
	}
	flags, src := adminFlags("admin chats list")
	match := flags.String("match", "", "list only chats whose name, identifier or reference contains this text")
	if !parseAdminFlags(flags, args[1:], stderr) {
		return usageError(stderr)
	}
	req := adminRequest{label: "chats list", method: http.MethodGet, path: []string{"chats"}, print: printChats}
	if *match != "" {
		req.query = url.Values{"match": {*match}}
	}
	return adminSend(ctx, flags, src, environ, stdin, stdout, stderr, req)
}

func printChats(answer []byte, stdout, stderr io.Writer) error {
	var a chatsAnswer
	if json.Unmarshal(answer, &a) != nil || a.Chats == nil || a.Truncated == nil {
		return errBadAnswer
	}
	var out bytes.Buffer
	for i, c := range *a.Chats {
		if c.ID == nil || c.Kind == nil || c.Ref == nil {
			return errBadAnswer
		}
		if i > 0 {
			out.WriteString("\n")
		}
		name := "none"
		if c.Name != nil {
			name = *c.Name
		}
		for _, line := range [][2]string{{"id", *c.ID}, {"kind", *c.Kind}, {"ref", *c.Ref}, {"name", name}} {
			fmt.Fprintf(&out, "%s: %s\n", line[0], sanitize.Terminal(line[1]))
		}
	}
	if _, err := stdout.Write(out.Bytes()); err != nil {
		return err
	}
	if *a.Truncated {
		_, _ = fmt.Fprintln(stderr, "admin chats list: more chats match than are listed; narrow them with --match")
	}
	return nil
}
