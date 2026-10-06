package api

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/dortort/wawarden/internal/api/dto"
	"github.com/dortort/wawarden/internal/policy"
)

const (
	actionClientCreate = "client_create"
	actionClientRevoke = "client_revoke"

	maxClientBodyBytes = 64 << 10
	maxChatMatch       = 64
	chatListLimit      = 100

	codeInvalidQuery = "invalid_query"
)

var (
	ErrClientNotFound error = &codedError{status: http.StatusNotFound, code: codeNotFound}
	errBadQuery             = &codedError{status: http.StatusBadRequest, code: codeInvalidQuery}
)

var clientRefusals = map[*policy.SpecError]*codedError{
	policy.ErrNameInvalid:       {status: http.StatusUnprocessableEntity, code: "name_invalid"},
	policy.ErrExpiryOutOfRange:  {status: http.StatusUnprocessableEntity, code: "expiry_out_of_range"},
	policy.ErrReadScopeMissing:  {status: http.StatusUnprocessableEntity, code: "read_scope_missing"},
	policy.ErrReadScopeConflict: {status: http.StatusUnprocessableEntity, code: "read_scope_conflict"},
	policy.ErrAllChatsWithWrite: {status: http.StatusUnprocessableEntity, code: "all_chats_with_write"},
	policy.ErrChatInvalid:       {status: http.StatusUnprocessableEntity, code: "chat_invalid"},
	policy.ErrTooManyChats:      {status: http.StatusUnprocessableEntity, code: "too_many_chats"},
	policy.ErrWriteNotReadable:  {status: http.StatusUnprocessableEntity, code: "write_not_readable"},
	policy.ErrWriteChatUnknown:  {status: http.StatusUnprocessableEntity, code: "write_chat_unknown"},
	policy.ErrNameTaken:         {status: http.StatusConflict, code: "name_taken"},
}

type ClientChatView struct {
	Chat  policy.CanonicalChat
	Known bool
	Name  string
}

type ClientView struct {
	ID                string
	Name              string
	State             string
	CreatedAt         time.Time
	ExpiresAt         time.Time
	RevokedAt         time.Time
	AllChats          bool
	AllowFirstContact bool
	ReadCount         int64
	WriteCount        int64
	Read              []ClientChatView
	Write             []ClientChatView
}

type ChatEntry struct {
	Chat policy.CanonicalChat
	Ref  string
	Name string
}

type ClientService interface {
	CreateClient(ctx context.Context, spec policy.ClientSpec) (view ClientView, credential string, err error)
	Clients(ctx context.Context) ([]ClientView, error)
	Client(ctx context.Context, id string) (ClientView, error)
	RevokeClient(ctx context.Context, id string) (ClientView, error)
	Chats(ctx context.Context, match string, limit int) (chats []ChatEntry, truncated bool, err error)
}

type clientRequest struct {
	Name              string   `json:"name"`
	ReadChats         []string `json:"read_chats"`
	WriteChats        []string `json:"write_chats"`
	AllChats          bool     `json:"all_chats"`
	AllowFirstContact bool     `json:"allow_first_contact"`
	ExpiresInDays     int      `json:"expires_in_days"`
}

func registerClients(rt *router, svc ClientService, events AdminEvents) {
	rt.admin("POST /admin/v1/clients", decodedMutation(events, actionClientCreate, maxClientBodyBytes, func(ctx context.Context, _ *Request, body clientRequest) (dto.Response, error) {
		view, credential, err := svc.CreateClient(ctx, policy.ClientSpec{
			Name: body.Name, Read: body.ReadChats, Write: body.WriteChats,
			AllChats: body.AllChats, AllowFirstContact: body.AllowFirstContact, ExpiresInDays: body.ExpiresInDays,
		})
		if err != nil {
			return nil, clientRefusal(err)
		}
		return dto.ClientCreated{Client: clientResponse(view), Credential: credential}, nil
	}))
	rt.admin("GET /admin/v1/clients", func(ctx context.Context, _ policy.AdminGrant, _ *Request) (dto.Response, error) {
		views, err := svc.Clients(ctx)
		if err != nil {
			return nil, err
		}
		list := dto.ClientList{Clients: make([]dto.ClientSummary, 0, len(views))}
		for _, v := range views {
			list.Clients = append(list.Clients, dto.ClientSummary{
				ID: v.ID, Name: v.Name, State: v.State, CreatedAt: timeText(v.CreatedAt), ExpiresAt: timeText(v.ExpiresAt),
				RevokedAt: optionalTime(v.RevokedAt), AllChats: v.AllChats, AllowFirstContact: v.AllowFirstContact,
				ReadChatCount: v.ReadCount, WriteChatCount: v.WriteCount,
			})
		}
		return list, nil
	})
	rt.admin("GET /admin/v1/clients/{id}", func(ctx context.Context, _ policy.AdminGrant, r *Request) (dto.Response, error) {
		view, err := svc.Client(ctx, r.req.PathValue("id"))
		if err != nil {
			return nil, err
		}
		return clientResponse(view), nil
	})
	rt.admin("POST /admin/v1/clients/{id}/revoke", decodedMutation(events, actionClientRevoke, maxBodyBytes, func(ctx context.Context, r *Request, _ struct{}) (dto.Response, error) {
		view, err := svc.RevokeClient(ctx, r.req.PathValue("id"))
		if err != nil {
			return nil, err
		}
		return clientResponse(view), nil
	}))
	rt.admin("GET /admin/v1/chats", func(ctx context.Context, _ policy.AdminGrant, r *Request) (dto.Response, error) {
		match, err := chatMatch(r.req.URL.RawQuery)
		if err != nil {
			return nil, err
		}
		chats, truncated, err := svc.Chats(ctx, match, chatListLimit)
		if err != nil {
			return nil, err
		}
		out := dto.AdminChats{Chats: make([]dto.AdminChat, 0, len(chats)), Truncated: truncated}
		for _, c := range chats {
			out.Chats = append(out.Chats, dto.AdminChat{ID: c.Chat.JID(), Kind: chatKind(c.Chat.Kind()), Ref: c.Ref, Name: optional(c.Name)})
		}
		return out, nil
	})
}

func clientRefusal(err error) error {
	spec, ok := errors.AsType[*policy.SpecError](err)
	if !ok {
		return err
	}
	if refusal, known := clientRefusals[spec]; known {
		return refusal
	}
	return errors.New("api: a client refusal without a code")
}

func chatMatch(rawQuery string) (string, error) {
	values, err := url.ParseQuery(rawQuery)
	if err != nil {
		return "", errBadQuery
	}
	var match string
	for key, v := range values {
		if key != "match" || len(v) != 1 {
			return "", errBadQuery
		}
		match = v[0]
	}
	if !utf8.ValidString(match) || utf8.RuneCountInString(match) > maxChatMatch {
		return "", errBadQuery
	}
	for _, r := range match {
		if unicode.IsControl(r) {
			return "", errBadQuery
		}
	}
	return match, nil
}

func clientResponse(v ClientView) dto.Client {
	return dto.Client{
		ID: v.ID, Name: v.Name, State: v.State, CreatedAt: timeText(v.CreatedAt), ExpiresAt: timeText(v.ExpiresAt),
		RevokedAt: optionalTime(v.RevokedAt), AllChats: v.AllChats, AllowFirstContact: v.AllowFirstContact,
		ReadChats: clientChats(v.Read), WriteChats: clientChats(v.Write),
	}
}

func clientChats(chats []ClientChatView) []dto.ClientChat {
	out := make([]dto.ClientChat, 0, len(chats))
	for _, c := range chats {
		out = append(out, dto.ClientChat{ID: c.Chat.JID(), Kind: chatKind(c.Chat.Kind()), Known: c.Known, Name: optional(c.Name)})
	}
	return out
}

func chatKind(k policy.ChatKind) string {
	switch k {
	case policy.PhoneChat:
		return "phone"
	case policy.LIDChat:
		return "lid"
	case policy.GroupChat:
		return "group"
	}
	return "invalid"
}

func timeText(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func optionalTime(t time.Time) *string {
	if t.IsZero() {
		return nil
	}
	s := timeText(t)
	return &s
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
