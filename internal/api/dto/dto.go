// Package dto declares every response body the API can send, so nothing else can reach a caller.
package dto

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"iter"
	"math"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/dortort/wawarden/internal/metrics"
)

const (
	jsonContentType = "application/json; charset=utf-8"
	statusOK        = 200
	statusCreated   = 201
	statusAccepted  = 202
)

var (
	errNoResponse      = errors.New("dto: no response to encode")
	errNoRegistry      = errors.New("dto: no metrics registry to encode")
	errForeignResponse = errors.New("dto: the response type is declared outside package dto")
	errStreamOnly      = errors.New("dto: an attachment streams through its delivery plan and is never encoded")
	errAttachment      = errors.New("dto: the attachment's kind, variant, served type, size, tag, body or expiry is invalid")
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
	Downloads    string        `json:"downloads"`
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
	BackfillsToday   int64 `json:"backfills_today"`
	MediaRefsLive    int64 `json:"media_refs_live"`
	MediaRefsExpired int64 `json:"media_refs_expired"`
	RevokeTombstones int64 `json:"revoke_tombstones"`
}

func (s Status) encode() (string, []byte, error) { return encodeJSON(s) }

type MediaPause struct {
	Paused bool `json:"paused"`
}

func (p MediaPause) encode() (string, []byte, error) { return encodeJSON(p) }

type Pairing struct {
	Code string `json:"code"`
}

func (p Pairing) encode() (string, []byte, error) { return encodeJSON(p) }

type Accepted struct {
	Status string `json:"status"`
}

func (a Accepted) encode() (string, []byte, error) { return encodeJSON(a) }

type BackfillRequested struct {
	RequestID string `json:"request_id"`
}

func (BackfillRequested) Status() int { return statusAccepted }

func (b BackfillRequested) encode() (string, []byte, error) { return encodeJSON(b) }

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
	AllowMedia        bool         `json:"allow_media"`
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
	AllowMedia        bool    `json:"allow_media"`
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

type ChatResult struct {
	Chat    *Chat   `json:"chat"`
	Session Session `json:"session"`
}

func (c ChatResult) encode() (string, []byte, error) { return encodeJSON(c) }

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

type SendResult struct {
	Message           *Message `json:"message"`
	Replayed          bool     `json:"replayed"`
	Session           Session  `json:"session"`
	RetryAfterSeconds *int     `json:"retry_after_seconds,omitempty"`
}

func (SendResult) Status() int { return statusCreated }

func (r SendResult) encode() (string, []byte, error) { return encodeJSON(r) }

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

const (
	MediaFetching = "fetching"
	MediaReady    = "ready"

	statusPartialContent      = 206
	statusRangeNotSatisfiable = 416
	mediaRetryAfter           = 2 * time.Second
	typeOctetStream           = "application/octet-stream"
	attachmentPolicy          = "default-src 'none'; sandbox; frame-ancestors 'none'"
)

var servedExtensions = map[string]string{
	"image/jpeg":      "jpg",
	"image/png":       "png",
	"audio/ogg":       "ogg",
	"audio/mpeg":      "mp3",
	"audio/mp4":       "m4a",
	"video/mp4":       "mp4",
	"application/pdf": "pdf",
	typeOctetStream:   "bin",
}

var (
	imageTypes = []string{"image/jpeg", "image/png"}
	videoTypes = []string{"video/mp4", typeOctetStream}
	audioTypes = []string{"audio/ogg", "audio/mpeg", "audio/mp4", typeOctetStream}
)

var servedTypes = map[string][]string{
	"image/":           imageTypes,
	"video/":           videoTypes,
	"video/gif":        videoTypes,
	"video/video_note": videoTypes,
	"audio/":           audioTypes,
	"audio/voice_note": audioTypes,
	"document/":        {"application/pdf", typeOctetStream, "image/jpeg", "image/png"},
	"sticker/":         imageTypes,
	"sticker/animated": imageTypes,
	"sticker/lottie":   {typeOctetStream},
}

var deliveryHeaders = []string{
	"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges", "ETag", "Content-Disposition", "Content-Security-Policy",
	"Cross-Origin-Resource-Policy",
}

func DeliveryHeaders() []string { return slices.Clone(deliveryHeaders) }

type Attachment struct {
	kind       string
	servedType string
	size       int64
	etag       string
	body       io.ReaderAt
	expires    time.Time
	finished   func(sent int64, complete bool)
}

func NewAttachment(kind, variant, servedType string, size int64, tag [16]byte, body io.ReaderAt, expires time.Time,
	finished func(sent int64, complete bool)) (Attachment, error) {
	if !slices.Contains(servedTypes[kind+"/"+variant], servedType) || size < 0 || tag == [16]byte{} || body == nil || expires.IsZero() {
		return Attachment{}, errAttachment
	}
	return Attachment{
		kind: kind, servedType: servedType, size: size, etag: `"` + hex.EncodeToString(tag[:]) + `"`, body: body, expires: expires, finished: finished,
	}, nil
}

func (Attachment) encode() (string, []byte, error) { return "", nil, errStreamOnly }

func (a Attachment) Expires() time.Time { return a.expires }

func (a Attachment) Finished(sent int64, complete bool) {
	if a.finished != nil {
		a.finished(sent, complete)
	}
}

func (a Attachment) Plan(rangeHeader, ifRange string) (Delivery, error) {
	if a.body == nil {
		return Delivery{}, errAttachment
	}
	first, last, code := int64(0), a.size-1, statusOK
	if rangeHeader != "" && (ifRange == "" || ifRange == a.etag) {
		first, last, code = singleRange(rangeHeader, a.size)
	}
	if code == statusRangeNotSatisfiable {
		return Delivery{
			code:   code,
			header: [][2]string{{"Content-Range", "bytes */" + strconv.FormatInt(a.size, 10)}},
			body:   io.NewSectionReader(a.body, 0, 0),
		}, nil
	}
	length := last - first + 1
	header := [][2]string{{"Content-Type", a.servedType}, {"Content-Length", strconv.FormatInt(length, 10)}}
	if code == statusPartialContent {
		header = append(header, [2]string{"Content-Range", "bytes " + strconv.FormatInt(first, 10) + "-" + strconv.FormatInt(last, 10) + "/" + strconv.FormatInt(a.size, 10)})
	}
	header = append(header,
		[2]string{"Accept-Ranges", "bytes"},
		[2]string{"ETag", a.etag},
		[2]string{"Content-Disposition", `attachment; filename="wawarden-` + a.kind + "." + servedExtensions[a.servedType] + `"`},
		[2]string{"Content-Security-Policy", attachmentPolicy},
		[2]string{"Cross-Origin-Resource-Policy", "same-origin"},
	)
	return Delivery{code: code, header: header, body: io.NewSectionReader(a.body, first, length), whole: length == a.size}, nil
}

func singleRange(header string, size int64) (first, last int64, code int) {
	whole := size - 1
	spec, ok := cutPrefixFold(header, "bytes=")
	if !ok {
		return 0, whole, statusOK
	}
	firstText, lastText, ok := strings.Cut(strings.Trim(spec, " \t"), "-")
	if !ok {
		return 0, whole, statusOK
	}
	if firstText == "" {
		suffix, ok := decimal(lastText)
		switch {
		case !ok:
			return 0, whole, statusOK
		case suffix == 0 || size == 0:
			return 0, 0, statusRangeNotSatisfiable
		}
		return size - min(suffix, size), whole, statusPartialContent
	}
	first, ok = decimal(firstText)
	if !ok {
		return 0, whole, statusOK
	}
	last = math.MaxInt64
	if lastText != "" {
		if last, ok = decimal(lastText); !ok || last < first {
			return 0, whole, statusOK
		}
	}
	if first >= size {
		return 0, 0, statusRangeNotSatisfiable
	}
	return first, min(last, whole), statusPartialContent
}

func cutPrefixFold(s, prefix string) (string, bool) {
	if len(s) < len(prefix) || !strings.EqualFold(s[:len(prefix)], prefix) {
		return "", false
	}
	return s[len(prefix):], true
}

func decimal(s string) (int64, bool) {
	if s == "" {
		return 0, false
	}
	var n int64
	for i := range len(s) {
		d := int64(s[i]) - '0'
		switch {
		case d < 0 || d > 9:
			return 0, false
		case n > (math.MaxInt64-d)/10:
			n = math.MaxInt64
		default:
			n = n*10 + d
		}
	}
	return n, true
}

type Delivery struct {
	code   int
	header [][2]string
	body   *io.SectionReader
	whole  bool
}

func (d Delivery) Code() int { return d.code }

func (d Delivery) Header() iter.Seq2[string, string] {
	return func(yield func(string, string) bool) {
		for _, h := range d.header {
			if !yield(h[0], h[1]) {
				return
			}
		}
	}
}

func (d Delivery) Body() *io.SectionReader { return d.body }

func (d Delivery) Offset() int64 {
	if d.body == nil {
		return 0
	}
	_, off, _ := d.body.Outer()
	return off
}

func (d Delivery) Length() int64 {
	if d.body == nil {
		return 0
	}
	return d.body.Size()
}

func (d Delivery) Whole() bool { return d.whole }

type AttachmentInfo struct {
	Kind              string  `json:"kind"`
	Variant           string  `json:"variant"`
	State             string  `json:"state"`
	ServedType        string  `json:"served_type"`
	Format            string  `json:"format"`
	Normalised        bool    `json:"normalised"`
	Bytes             int64   `json:"bytes"`
	SizeDeclared      int64   `json:"size_declared"`
	Delivery          string  `json:"delivery"`
	Reason            *string `json:"reason"`
	Parts             int     `json:"parts"`
	Offset            int64   `json:"offset"`
	Length            int64   `json:"length"`
	RestRoute         string  `json:"rest_route"`
	ExpiresAt         *string `json:"expires_at"`
	RetryAfterSeconds *int    `json:"retry_after_seconds,omitempty"`
	Untrusted         bool    `json:"untrusted"`
	Notice            string  `json:"notice"`
}

type MediaStatus struct {
	AttachmentInfo
}

func (m MediaStatus) Status() int {
	if m.State == MediaFetching {
		return statusAccepted
	}
	return statusOK
}

func (m MediaStatus) RetryAfter() time.Duration {
	if m.State == MediaFetching {
		return mediaRetryAfter
	}
	return 0
}

func (m MediaStatus) encode() (string, []byte, error) { return encodeJSON(m) }
