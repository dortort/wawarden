package policy

import (
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	maxClientName     = 64
	maxClientChats    = 256
	defaultClientDays = 90
	maxClientDays     = 365
	clientDay         = 24 * time.Hour
)

type ClientSpec struct {
	Name              string
	Read              []string
	Write             []string
	AllChats          bool
	AllowFirstContact bool
	ExpiresInDays     int
}

type SpecError struct{ code string }

func (e *SpecError) Error() string { return "policy: the client is refused: " + e.code }

func (e *SpecError) Code() string { return e.code }

var (
	ErrNameInvalid       = &SpecError{code: "name_invalid"}
	ErrNameTaken         = &SpecError{code: "name_taken"}
	ErrExpiryOutOfRange  = &SpecError{code: "expiry_out_of_range"}
	ErrReadScopeMissing  = &SpecError{code: "read_scope_missing"}
	ErrReadScopeConflict = &SpecError{code: "read_scope_conflict"}
	ErrAllChatsWithWrite = &SpecError{code: "all_chats_with_write"}
	ErrChatInvalid       = &SpecError{code: "chat_invalid"}
	ErrTooManyChats      = &SpecError{code: "too_many_chats"}
	ErrWriteNotReadable  = &SpecError{code: "write_not_readable"}
	ErrWriteChatUnknown  = &SpecError{code: "write_chat_unknown"}
)

func ValidateClientSpec(spec ClientSpec, now time.Time) (Client, error) {
	if !validClientName(spec.Name) {
		return Client{}, ErrNameInvalid
	}
	days := spec.ExpiresInDays
	if days == 0 {
		days = defaultClientDays
	}
	if days < 1 || days > maxClientDays || now.IsZero() {
		return Client{}, ErrExpiryOutOfRange
	}
	if len(spec.Read) > maxClientChats || len(spec.Write) > maxClientChats {
		return Client{}, ErrTooManyChats
	}
	read, readOK := normalizeChats(spec.Read)
	write, writeOK := normalizeChats(spec.Write)
	switch {
	case !readOK || !writeOK:
		return Client{}, ErrChatInvalid
	case spec.AllChats && len(write) > 0:
		return Client{}, ErrAllChatsWithWrite
	case spec.AllChats && len(read) > 0:
		return Client{}, ErrReadScopeConflict
	case !spec.AllChats && len(read) == 0:
		return Client{}, ErrReadScopeMissing
	}
	for chat := range write {
		if _, ok := read[chat]; !ok {
			return Client{}, ErrWriteNotReadable
		}
	}
	return Client{
		Name:              spec.Name,
		ReadAll:           spec.AllChats,
		Read:              read,
		Write:             write,
		ExpiresAt:         now.Add(time.Duration(days) * clientDay),
		AllowFirstContact: spec.AllowFirstContact,
	}, nil
}

func WritableChat(chat CanonicalChat, known, allowFirstContact bool) bool {
	direct := chat.Kind() == PhoneChat || chat.Kind() == LIDChat
	return chat.Valid() && (known || allowFirstContact && direct)
}

func MayContact(chat CanonicalChat, inbound, allowFirstContact bool) bool {
	direct := chat.Kind() == PhoneChat || chat.Kind() == LIDChat
	return chat.Valid() && (!direct || inbound || allowFirstContact)
}

func validClientName(name string) bool {
	if name == "" || !utf8.ValidString(name) || utf8.RuneCountInString(name) > maxClientName {
		return false
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func normalizeChats(in []string) (map[CanonicalChat]struct{}, bool) {
	out := make(map[CanonicalChat]struct{}, len(in))
	for _, s := range in {
		chat, ok := Normalize(s)
		if !ok {
			return nil, false
		}
		out[chat] = struct{}{}
	}
	return out, true
}
