// Package engine is the WhatsApp engine without the protocol library: the client boundary, plain-data events, the supervisor, the durable ingest pipeline and the history worker.
package engine

import (
	"context"
	"io"
	"log/slog"
	"time"

	"github.com/dortort/wawarden/internal/metrics"
	"github.com/dortort/wawarden/internal/store/ingest"
)

const (
	DefaultHistoryMaxBytes = 32 << 20
	MaxHistoryMaxBytes     = 256 << 20
)

type Options struct {
	Client          Client
	Versions        VersionSource
	Decoder         HistoryDecoder
	Archive         *ingest.Store
	DataDir         string
	OwnerPhone      string
	HistoryMaxBytes int64
	Logger          *slog.Logger
	Notify          Notifier
	Metrics         *metrics.Registry
	Clock           Clock
	Jitter          func(time.Duration) time.Duration
}

type Notifier interface {
	Unpaired()
	Disconnected(reason string)
	PairRejected(stage string)
	LogoutFailed(attempt int, errorType string)
	Quarantine(queue string, attempts int)
	RekeyConflict(conflict string)
	IngestPaused(freeBytes, floorBytes uint64)
}

type Version [3]uint32

func (v Version) IsZero() bool { return v == Version{} }

func (v Version) Less(o Version) bool {
	for i := range v {
		if v[i] != o[i] {
			return v[i] < o[i]
		}
	}
	return false
}

type HistoryRef struct {
	ID            string `json:"id"`
	DirectPath    string `json:"direct_path,omitempty"`
	MediaKey      []byte `json:"media_key,omitempty"`
	FileSHA256    []byte `json:"file_sha256,omitempty"`
	FileEncSHA256 []byte `json:"file_enc_sha256,omitempty"`
	FileLength    uint64 `json:"file_length,omitempty"`
	Inline        []byte `json:"inline,omitempty"`
}

type Client interface {
	Connect(ctx context.Context) error
	Disconnect()
	Paired() bool
	Account() string
	PairPhone(ctx context.Context, digits string) (code string, err error)
	Logout(ctx context.Context) error
	Version() Version
	SetVersion(Version)
	DownloadHistory(ctx context.Context, ref HistoryRef, dst io.Writer) error
	AckHistory(ctx context.Context, ref HistoryRef) error
	OnEvent(handler func(Event) bool) (cancel func())
}

type VersionSource interface {
	Latest(ctx context.Context) (Version, error)
}

type HistoryDecoder interface {
	Decode(blob []byte) (History, error)
}

type History struct {
	Conversations []Conversation
	LIDMappings   []LIDMapping
	Contacts      []Contact
}

type Conversation struct {
	Chat     string
	Subject  string
	Members  []Participant
	Messages []Message
}

type LIDMapping struct {
	PN  string
	LID string
}

type Contact struct {
	User     string
	PushName string
}

type Event interface{ engineEvent() }

type Kind string

const (
	KindText       Kind = "text"
	KindMedia      Kind = "media"
	KindReaction   Kind = "reaction"
	KindPollUpdate Kind = "poll_update"
	KindEdit       Kind = "edit"
	KindRevoke     Kind = "revoke"
	KindOther      Kind = "other"
)

type Key struct {
	RemoteJID   string `json:"remote_jid,omitempty"`
	FromMe      bool   `json:"from_me,omitempty"`
	ID          string `json:"id"`
	Participant string `json:"participant,omitempty"`
}

type Reply struct {
	ID          string `json:"id"`
	Participant string `json:"participant,omitempty"`
	RemoteJID   string `json:"remote_jid,omitempty"`
	Text        string `json:"text,omitempty"`
}

type Message struct {
	Chat         string        `json:"chat"`
	ID           string        `json:"id"`
	Sender       string        `json:"sender"`
	SenderAlt    string        `json:"sender_alt,omitempty"`
	RecipientAlt string        `json:"recipient_alt,omitempty"`
	Addressing   string        `json:"addressing,omitempty"`
	FromMe       bool          `json:"from_me,omitempty"`
	Timestamp    time.Time     `json:"ts"`
	PushName     string        `json:"push_name,omitempty"`
	Kind         Kind          `json:"kind,omitempty"`
	Text         string        `json:"text,omitempty"`
	MediaType    string        `json:"media_type,omitempty"`
	Target       *Key          `json:"target,omitempty"`
	Reply        *Reply        `json:"reply,omitempty"`
	Expiration   time.Duration `json:"expiration,omitempty"`
}

type Participant struct {
	User  string `json:"user"`
	Admin bool   `json:"admin,omitempty"`
}

type Group struct {
	Chat      string        `json:"chat"`
	Subject   string        `json:"subject,omitempty"`
	Members   []Participant `json:"members,omitempty"`
	Joined    []Participant `json:"joined,omitempty"`
	Left      []string      `json:"left,omitempty"`
	Timestamp time.Time     `json:"ts"`
}

type HistoryNotification struct {
	Sender string
	FromMe bool
	Ref    HistoryRef
}

type (
	Connected        struct{}
	Disconnected     struct{}
	ClientOutdated   struct{}
	StreamReplaced   struct{}
	LoggedOut        struct{}
	CATRefreshFailed struct{}
	TemporaryBan     struct{ Expire time.Duration }
	ConnectFailure   struct{ Code int }
	Paired           struct{ JID string }
	PairRejected     struct{}
)

func (Message) engineEvent()             {}
func (Group) engineEvent()               {}
func (HistoryNotification) engineEvent() {}
func (Connected) engineEvent()           {}
func (Disconnected) engineEvent()        {}
func (ClientOutdated) engineEvent()      {}
func (StreamReplaced) engineEvent()      {}
func (LoggedOut) engineEvent()           {}
func (CATRefreshFailed) engineEvent()    {}
func (TemporaryBan) engineEvent()        {}
func (ConnectFailure) engineEvent()      {}
func (Paired) engineEvent()              {}
func (PairRejected) engineEvent()        {}
