// Package dto declares every response body the API can send, so nothing else can reach a caller.
package dto

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"

	"github.com/dortort/wawarden/internal/metrics"
)

const jsonContentType = "application/json; charset=utf-8"

var (
	errNoResponse      = errors.New("dto: no response to encode")
	errNoRegistry      = errors.New("dto: no metrics registry to encode")
	errForeignResponse = errors.New("dto: the response type is declared outside package dto")
)

var pkgPath = reflect.TypeFor[Error]().PkgPath()

type Response interface {
	encode() (contentType string, body []byte, err error)
}

func Encode(r Response) (contentType string, body []byte, err error) {
	if r == nil {
		return "", nil, errNoResponse
	}
	t := reflect.TypeOf(r)
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.PkgPath() != pkgPath {
		return "", nil, errForeignResponse
	}
	return r.encode()
}

type Error struct {
	Code string `json:"error"`
}

func (e Error) encode() (string, []byte, error) { return encodeJSON(e) }

type Health struct {
	Status string `json:"status"`
}

func (h Health) encode() (string, []byte, error) { return encodeJSON(h) }

type Status struct {
	State        string        `json:"state"`
	Reason       string        `json:"reason"`
	Paired       bool          `json:"paired"`
	Counts       StatusCounts  `json:"counts"`
	Clients      StatusClients `json:"clients"`
	Warnings     []string      `json:"warnings"`
	LastIngestAt *string       `json:"last_ingest_at"`
	Version      string        `json:"version"`
}

type StatusClients struct {
	Active         int64 `json:"active"`
	Expired        int64 `json:"expired"`
	Revoked        int64 `json:"revoked"`
	AllChatsActive int64 `json:"all_chats_active"`
}

type StatusCounts struct {
	Chats            int64 `json:"chats"`
	Messages         int64 `json:"messages"`
	BlobsPending     int64 `json:"history_blobs_pending"`
	BlobsQuarantined int64 `json:"history_blobs_quarantined"`
	InboxBacklog     int64 `json:"inbox_backlog"`
	InboxQuarantined int64 `json:"inbox_quarantined"`
}

func (s Status) encode() (string, []byte, error) { return encodeJSON(s) }

type Pairing struct {
	Code string `json:"code"`
}

func (p Pairing) encode() (string, []byte, error) { return encodeJSON(p) }

type Accepted struct {
	Status string `json:"status"`
}

func (a Accepted) encode() (string, []byte, error) { return encodeJSON(a) }

type ClientChat struct {
	ID    string  `json:"id"`
	Kind  string  `json:"kind"`
	Known bool    `json:"known"`
	Name  *string `json:"name"`
}

type Client struct {
	ID                string       `json:"id"`
	Name              string       `json:"name"`
	State             string       `json:"state"`
	CreatedAt         string       `json:"created_at"`
	ExpiresAt         string       `json:"expires_at"`
	RevokedAt         *string      `json:"revoked_at"`
	AllChats          bool         `json:"all_chats"`
	AllowFirstContact bool         `json:"allow_first_contact"`
	ReadChats         []ClientChat `json:"read_chats"`
	WriteChats        []ClientChat `json:"write_chats"`
}

func (c Client) encode() (string, []byte, error) { return encodeJSON(c) }

type ClientCreated struct {
	Client     Client `json:"client"`
	Credential string `json:"credential"`
}

func (c ClientCreated) encode() (string, []byte, error) { return encodeJSON(c) }

type ClientSummary struct {
	ID                string  `json:"id"`
	Name              string  `json:"name"`
	State             string  `json:"state"`
	CreatedAt         string  `json:"created_at"`
	ExpiresAt         string  `json:"expires_at"`
	RevokedAt         *string `json:"revoked_at"`
	AllChats          bool    `json:"all_chats"`
	AllowFirstContact bool    `json:"allow_first_contact"`
	ReadChatCount     int64   `json:"read_chat_count"`
	WriteChatCount    int64   `json:"write_chat_count"`
}

type ClientList struct {
	Clients []ClientSummary `json:"clients"`
}

func (l ClientList) encode() (string, []byte, error) { return encodeJSON(l) }

type AdminChat struct {
	ID   string  `json:"id"`
	Kind string  `json:"kind"`
	Ref  string  `json:"ref"`
	Name *string `json:"name"`
}

type AdminChats struct {
	Chats     []AdminChat `json:"chats"`
	Truncated bool        `json:"truncated"`
}

func (c AdminChats) encode() (string, []byte, error) { return encodeJSON(c) }

type Session struct {
	State string `json:"state"`
}

type ScopeChat struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
}

type ReadScope struct {
	All   bool        `json:"all"`
	Chats []ScopeChat `json:"chats"`
}

type WriteScope struct {
	Chats []ScopeChat `json:"chats"`
}

type MeClient struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	ExpiresAt string     `json:"expires_at"`
	Read      ReadScope  `json:"read"`
	Write     WriteScope `json:"write"`
}

type Me struct {
	Client  MeClient `json:"client"`
	Session Session  `json:"session"`
}

func (m Me) encode() (string, []byte, error) { return encodeJSON(m) }

type Chat struct {
	ID            string  `json:"id"`
	Kind          string  `json:"kind"`
	Name          *string `json:"name"`
	NameSource    *string `json:"name_source"`
	LastMessageAt *string `json:"last_message_at"`
}

func (c Chat) encode() (string, []byte, error) { return encodeJSON(c) }

type ChatPage struct {
	Chats     []Chat  `json:"chats"`
	Next      *string `json:"next"`
	Truncated bool    `json:"truncated"`
	Session   Session `json:"session"`
}

func (p ChatPage) encode() (string, []byte, error) { return encodeJSON(p) }

type Sender struct {
	ID   string  `json:"id"`
	Name *string `json:"name"`
}

type Message struct {
	Ref           string  `json:"mref"`
	Chat          string  `json:"chat"`
	Sender        Sender  `json:"sender"`
	FromMe        bool    `json:"from_me"`
	TS            string  `json:"ts"`
	Kind          string  `json:"kind"`
	Text          *string `json:"text"`
	TextDisplay   *string `json:"text_display"`
	TextTruncated bool    `json:"text_truncated"`
	MediaType     *string `json:"media_type"`
	ReplyTo       *string `json:"reply_to"`
	QuoteVerified bool    `json:"quote_verified"`
	EditedAt      *string `json:"edited_at"`
	Revoked       bool    `json:"revoked"`
	Origin        string  `json:"origin"`
	Untrusted     bool    `json:"untrusted"`
}

func (m Message) encode() (string, []byte, error) { return encodeJSON(m) }

type MessagePage struct {
	Messages  []Message `json:"messages"`
	Next      *string   `json:"next"`
	Truncated bool      `json:"truncated"`
	Session   Session   `json:"session"`
}

func (p MessagePage) encode() (string, []byte, error) { return encodeJSON(p) }

type SearchPage struct {
	Messages  []Message `json:"messages"`
	Next      *string   `json:"next"`
	More      bool      `json:"more"`
	Truncated bool      `json:"truncated"`
	Session   Session   `json:"session"`
}

func (p SearchPage) encode() (string, []byte, error) { return encodeJSON(p) }

type ChangePage struct {
	Messages  []Message `json:"messages"`
	Next      *string   `json:"next"`
	More      bool      `json:"more"`
	Truncated bool      `json:"truncated"`
	Session   Session   `json:"session"`
}

func (p ChangePage) encode() (string, []byte, error) { return encodeJSON(p) }

func Size[T Chat | Message](item T) (int, error) {
	b, err := json.Marshal(item)
	return len(b), err
}

type Prometheus struct {
	reg *metrics.Registry
}

func Metrics(reg *metrics.Registry) Prometheus { return Prometheus{reg: reg} }

func (p Prometheus) encode() (string, []byte, error) {
	if p.reg == nil {
		return "", nil, errNoRegistry
	}
	var b bytes.Buffer
	if err := p.reg.WriteText(&b); err != nil {
		return "", nil, err
	}
	return metrics.ContentType, b.Bytes(), nil
}

func encodeJSON(v any) (string, []byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", nil, err
	}
	return jsonContentType, b, nil
}
