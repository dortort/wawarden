package api

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"time"

	"github.com/dortort/wawarden/internal/api/dto"
	"github.com/dortort/wawarden/internal/cursor"
	"github.com/dortort/wawarden/internal/hooks"
	"github.com/dortort/wawarden/internal/policy"
	"github.com/dortort/wawarden/internal/store/scoped"
)

const (
	actionWebhookCreate = "rest.webhook_create"
	actionWebhooks      = "rest.webhooks"
	actionWebhook       = "rest.webhook"
	actionWebhookDelete = "rest.webhook_delete"

	webhooksRoute       = "/v1/webhooks"
	maxWebhookBodyBytes = 64 << 10
	codeWebhookLimit    = "webhook_limit_reached"
)

// ErrWebhookNotFound is the answer for a webhook id the client does not hold.
var ErrWebhookNotFound error = &codedError{status: http.StatusNotFound, code: codeNotFound}

type WebhookView struct {
	ID           string
	Spec         hooks.Spec
	CreatedAt    time.Time
	Delivered    int64
	Failed       int64
	LastDelivery time.Time
	LastFailure  time.Time
	Failure      string
}

// WebhookService stores a client's webhooks; every method acts on the webhooks of the one client it names.
type WebhookService interface {
	Create(ctx context.Context, client string, spec hooks.Spec) (view WebhookView, secret string, err error)
	List(ctx context.Context, client string) ([]WebhookView, error)
	Get(ctx context.Context, client, id string) (WebhookView, error)
	Delete(ctx context.Context, client, id string) error
}

type webhookHeader struct {
	Name  string `json:"name" jsonschema:"the header's name, such as Authorization"`
	Value string `json:"value" jsonschema:"the header's value, printable ASCII; it is stored and never shown again"`
}

type webhookFilterArgs struct {
	Chats     []string `json:"chats,omitempty" jsonschema:"chat ids as list_chats returns them; only messages of these chats, every readable chat when omitted"`
	Direction string   `json:"direction,omitempty" jsonschema:"inbound for messages from others, outbound for the owner's and the gateway's, any when omitted"`
	Kinds     []string `json:"kinds,omitempty" jsonschema:"message kinds: text, media, reaction, poll_update or other; every kind when omitted"`
	Contains  string   `json:"contains,omitempty" jsonschema:"only messages whose text contains this, ignoring case"`
}

type webhookArgs struct {
	URL         string            `json:"url" jsonschema:"the https URL that receives each new message"`
	Method      string            `json:"method,omitempty" jsonschema:"POST, PUT or PATCH; POST when omitted"`
	ContentType string            `json:"content_type,omitempty" jsonschema:"application/json or text/plain; application/json when omitted"`
	Headers     []webhookHeader   `json:"headers,omitempty" jsonschema:"extra request headers, at most 8"`
	Payload     string            `json:"payload,omitempty" jsonschema:"a body template with {{placeholders}} such as {{message.text}}, {{message.chat}} or {{message}}; JSON when content_type is application/json; the default body when omitted"`
	Filter      webhookFilterArgs `json:"filter,omitempty" jsonschema:"which new messages to post; every new live message the client may read when omitted"`
}

type webhookIDArgs struct {
	Webhook string `json:"webhook" jsonschema:"a webhook id as create_webhook or list_webhooks returns it"`
}

type webhooks struct {
	svc WebhookService
	now func() time.Time
}

func (a webhookArgs) spec() hooks.Spec {
	s := hooks.Spec{URL: a.URL, Method: a.Method, ContentType: a.ContentType, Payload: a.Payload, Headers: map[string]string{},
		Filter: hooks.Filter{Chats: a.Filter.Chats, Direction: a.Filter.Direction, Kinds: a.Filter.Kinds, Contains: a.Filter.Contains}}
	for _, h := range a.Headers {
		if _, dup := s.Headers[h.Name]; dup || len(a.Headers) > hooks.MaxHeaders {
			s.Headers = nil
			return s
		}
		s.Headers[h.Name] = h.Value
	}
	return s
}

func registerWebhooks(rt *router, h *webhooks) {
	rt.read("POST "+webhooksRoute, notedBody(actionWebhookCreate, func(ctx context.Context, g policy.ReadGrant, r *Request) (dto.Response, policy.CanonicalChat, error) {
		var body webhookArgs
		if err := r.decodeJSON(&body, maxWebhookBodyBytes); err != nil {
			return nil, policy.CanonicalChat{}, err
		}
		return response(h.create(ctx, g, body))
	}))
	rt.read("GET "+webhooksRoute, noted(actionWebhooks, func(ctx context.Context, g policy.ReadGrant, r *Request) (dto.Response, policy.CanonicalChat, error) {
		if _, err := params(r); err != nil {
			return nil, policy.CanonicalChat{}, err
		}
		return response(h.list(ctx, g))
	}))
	rt.read("GET "+webhooksRoute+"/{id}", noted(actionWebhook, func(ctx context.Context, g policy.ReadGrant, r *Request) (dto.Response, policy.CanonicalChat, error) {
		if _, err := params(r); err != nil {
			return nil, policy.CanonicalChat{}, err
		}
		return response(h.get(ctx, g, r.req.PathValue("id")))
	}))
	rt.read("DELETE "+webhooksRoute+"/{id}", noted(actionWebhookDelete, func(ctx context.Context, g policy.ReadGrant, r *Request) (dto.Response, policy.CanonicalChat, error) {
		if _, err := params(r); err != nil {
			return nil, policy.CanonicalChat{}, err
		}
		return response(h.remove(ctx, g, r.req.PathValue("id")))
	}))
}

// notedBody is noted for a route that reads its own JSON body.
func notedBody(action string, h readHandler) func(context.Context, policy.ReadGrant, *Request) (dto.Response, error) {
	return func(ctx context.Context, g policy.ReadGrant, r *Request) (dto.Response, error) {
		n := noteFrom(ctx)
		if n != nil {
			n.action = action
		}
		resp, _, err := h(ctx, g, r)
		if errors.Is(err, scoped.ErrBusy) {
			err = errBusy
		}
		if refusal, ok := errors.AsType[*codedError](err); ok && n != nil {
			n.reason = refusal.code
		}
		return resp, err
	}
}

// holder is the client behind a grant when it may hold webhooks; any other client is answered as if the routes did not exist.
func (h *webhooks) holder(ctx context.Context, g policy.ReadGrant) (string, error) {
	c := clientFrom(ctx)
	if c == nil || !c.AllowWebhooks || c.ID != g.Client() {
		return "", errNotFound
	}
	return c.ID, nil
}

func (h *webhooks) create(ctx context.Context, g policy.ReadGrant, body webhookArgs) (dto.WebhookCreated, policy.CanonicalChat, error) {
	client, err := h.holder(ctx, g)
	if err != nil {
		return dto.WebhookCreated{}, policy.CanonicalChat{}, err
	}
	spec := body.spec()
	if spec.Headers == nil {
		return dto.WebhookCreated{}, policy.CanonicalChat{}, webhookRefusal(hooks.ErrHeaderInvalid)
	}
	view, secret, err := h.svc.Create(ctx, client, spec)
	if err != nil {
		return dto.WebhookCreated{}, policy.CanonicalChat{}, webhookRefusal(err)
	}
	item := webhookItem(view)
	return dto.WebhookCreated{Webhook: &item, Secret: secret}, policy.CanonicalChat{}, nil
}

func (h *webhooks) list(ctx context.Context, g policy.ReadGrant) (dto.WebhookList, policy.CanonicalChat, error) {
	client, err := h.holder(ctx, g)
	if err != nil {
		return dto.WebhookList{}, policy.CanonicalChat{}, err
	}
	views, err := h.svc.List(ctx, client)
	if err != nil {
		return dto.WebhookList{}, policy.CanonicalChat{}, err
	}
	out := dto.WebhookList{Webhooks: make([]dto.Webhook, 0, len(views))}
	for _, v := range views {
		out.Webhooks = append(out.Webhooks, webhookItem(v))
	}
	return out, policy.CanonicalChat{}, nil
}

func (h *webhooks) get(ctx context.Context, g policy.ReadGrant, id string) (dto.Webhook, policy.CanonicalChat, error) {
	client, err := h.holder(ctx, g)
	if err != nil {
		return dto.Webhook{}, policy.CanonicalChat{}, err
	}
	view, err := h.svc.Get(ctx, client, id)
	if err != nil {
		return dto.Webhook{}, policy.CanonicalChat{}, err
	}
	return webhookItem(view), policy.CanonicalChat{}, nil
}

func (h *webhooks) remove(ctx context.Context, g policy.ReadGrant, id string) (dto.WebhookDeleted, policy.CanonicalChat, error) {
	client, err := h.holder(ctx, g)
	if err != nil {
		return dto.WebhookDeleted{}, policy.CanonicalChat{}, err
	}
	if err := h.svc.Delete(ctx, client, id); err != nil {
		return dto.WebhookDeleted{}, policy.CanonicalChat{}, err
	}
	return dto.WebhookDeleted{ID: id, Deleted: true}, policy.CanonicalChat{}, nil
}

func webhookRefusal(err error) error {
	spec, ok := errors.AsType[*hooks.SpecError](err)
	switch {
	case !ok:
		return err
	case errors.Is(err, hooks.ErrTooMany):
		return &codedError{status: http.StatusConflict, code: codeWebhookLimit}
	}
	return &codedError{status: http.StatusBadRequest, code: spec.Code()}
}

func webhookItem(v WebhookView) dto.Webhook {
	names := make([]string, 0, len(v.Spec.Headers))
	for name := range v.Spec.Headers {
		names = append(names, name)
	}
	slices.Sort(names)
	out := dto.Webhook{
		ID: v.ID, URL: v.Spec.URL, Method: v.Spec.Method, ContentType: v.Spec.ContentType, HeaderNames: names, Payload: optional(v.Spec.Payload),
		Filter: dto.WebhookFilter{
			Chats: nonNil(v.Spec.Filter.Chats), Direction: v.Spec.Filter.Direction, Kinds: nonNil(v.Spec.Filter.Kinds), Contains: optional(v.Spec.Filter.Contains),
		},
		CreatedAt: readTime(v.CreatedAt), Delivered: v.Delivered, Failed: v.Failed,
		LastDeliveryAt: optionalReadTime(v.LastDelivery), LastFailureAt: optionalReadTime(v.LastFailure), LastFailure: optional(v.Failure),
	}
	return out
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

type FeedDeps struct {
	Archive ReadArchive
	Refs    *cursor.Sealer
	Now     func() time.Time
}

// WebhookFeed reads a client's change feed for its webhooks, under a read grant decided for that client at each call.
type WebhookFeed struct {
	reads *reads
	now   func() time.Time
}

func NewWebhookFeed(d FeedDeps) *WebhookFeed {
	if d.Archive == nil || d.Refs == nil {
		panic("api: NewWebhookFeed needs an archive and a message reference sealer")
	}
	now := clockOrSystem(d.Now)
	return &WebhookFeed{reads: &reads{archive: d.Archive, refs: d.Refs, now: now}, now: now}
}

func (f *WebhookFeed) Next(ctx context.Context, c *policy.Client, after int64, limit int) (hooks.Page, bool, error) {
	if c == nil || !c.AllowWebhooks || after < 0 {
		return hooks.Page{}, false, nil
	}
	g, ok := policy.DecideRead(c, f.now())
	if !ok {
		return hooks.Page{}, false, nil
	}
	page, _, err := f.reads.archive.Changes(g, ctx, "", scoped.ChangePosition{ChangeSeq: after}, limit)
	if err != nil {
		return hooks.Page{}, true, err
	}
	out := hooks.Page{Items: make([]hooks.Item, 0, len(page.Messages)), Next: page.Next.ChangeSeq, More: page.More}
	for _, m := range page.Messages {
		item, err := f.reads.messageItem(g.Client(), m)
		if err != nil {
			return hooks.Page{}, true, err
		}
		out.Items = append(out.Items, hooks.Item{Seq: m.Position.Seq, Change: m.Change.ChangeSeq, Live: m.Live, Message: item})
	}
	return out, true, nil
}
