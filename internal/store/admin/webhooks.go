package admin

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/dortort/wawarden/internal/hooks"
	"github.com/dortort/wawarden/internal/store/internal/db"
	"github.com/dortort/wawarden/internal/token"
)

const (
	webhookPrefix  = "wh_"
	webhookIDBytes = 8
	webhookSecret  = 32
)

var (
	ErrWebhookNotFound = errors.New("admin: no such webhook")
	errCorruptWebhook  = errors.New("admin: a stored webhook row is not one the service writes")
	webhookID          = regexp.MustCompile(`^wh_[0-9a-f]{16}$`)
)

// Webhook is one client webhook as the client sees it, without its header values or its secret.
type Webhook struct {
	ID           string
	Client       string
	Spec         hooks.Spec
	CreatedAt    time.Time
	Delivered    int64
	Failed       int64
	LastDelivery time.Time
	LastFailure  time.Time
	Failure      string
}

type Webhooks struct {
	db  *db.DB
	now func() time.Time
}

func NewWebhooks(d *db.DB, now func() time.Time) *Webhooks {
	if now == nil {
		now = time.Now
	}
	return &Webhooks{db: d, now: now}
}

func WellFormedWebhookID(id string) bool { return webhookID.MatchString(id) }

const (
	selectWebhookCount = `SELECT count(*) FROM webhooks WHERE client_id = ?`
	selectWebhookTaken = `SELECT EXISTS (SELECT 1 FROM webhooks WHERE id = ?)`
	selectFeedTop      = `SELECT coalesce(max(change_seq), 0), coalesce(max(seq), 0) FROM messages`
	insertWebhook      = `INSERT INTO webhooks (id, client_id, url, method, content_type, headers, payload, filter, secret, created_at, change_seq, high_seq)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	webhookColumns = `id, client_id, url, method, content_type, headers, coalesce(payload, ''), filter, created_at, delivered, failed,
		last_delivery_at, last_failure_at, coalesce(last_failure, '')`
	selectClientWebhooks = `SELECT ` + webhookColumns + ` FROM webhooks WHERE client_id = ? ORDER BY created_at, id`
	selectClientWebhook  = `SELECT ` + webhookColumns + ` FROM webhooks WHERE client_id = ? AND id = ?`
	deleteWebhook        = `DELETE FROM webhooks WHERE client_id = ? AND id = ?`
	selectDeliveryHooks  = `SELECT id, client_id, url, method, content_type, headers, coalesce(payload, ''), filter, secret, change_seq, high_seq FROM webhooks ORDER BY id`
	advanceWebhook       = `UPDATE webhooks SET change_seq = max(change_seq, ?2), high_seq = max(high_seq, ?3), delivered = delivered + ?4, failed = failed + ?5,
		last_delivery_at = coalesce(?6, last_delivery_at), last_failure_at = coalesce(?7, last_failure_at), last_failure = coalesce(?8, last_failure) WHERE id = ?1`
	selectChangeTop = `SELECT coalesce(max(change_seq), 0) FROM messages`
)

// Create stores a webhook that starts after the newest message of the archive, and returns its signing secret once.
func (w *Webhooks) Create(ctx context.Context, client string, spec hooks.Spec) (Webhook, []byte, error) {
	if !token.WellFormedClientID(client) {
		return Webhook{}, nil, ErrNotFound
	}
	spec, err := hooks.Normalize(spec)
	if err != nil {
		return Webhook{}, nil, err
	}
	filter, err := hooks.EncodeFilter(spec.Filter)
	if err != nil {
		return Webhook{}, nil, err
	}
	headers, err := hooks.EncodeHeaders(spec.Headers)
	if err != nil {
		return Webhook{}, nil, err
	}
	secret := make([]byte, webhookSecret)
	_, _ = rand.Read(secret)
	now := w.now()
	var view Webhook
	err = w.db.Write(ctx, "admin.webhook_create", func(ctx context.Context, q db.Querier) error {
		var n int
		if err := q.QueryRowContext(ctx, selectWebhookCount, client).Scan(&n); err != nil {
			return err
		}
		if n >= hooks.MaxPerClient {
			return hooks.ErrTooMany
		}
		id, err := freshWebhookID(ctx, q)
		if err != nil {
			return err
		}
		var change, high int64
		if err := q.QueryRowContext(ctx, selectFeedTop).Scan(&change, &high); err != nil {
			return err
		}
		var payload sql.NullString
		if spec.Payload != "" {
			payload = sql.NullString{String: spec.Payload, Valid: true}
		}
		if _, err := q.ExecContext(ctx, insertWebhook, id, client, spec.URL, spec.Method, spec.ContentType, headers, payload, filter, secret,
			now.UnixMilli(), change, high); err != nil {
			return err
		}
		view, err = scanWebhook(q.QueryRowContext(ctx, selectClientWebhook, client, id))
		return err
	})
	if err != nil {
		return Webhook{}, nil, err
	}
	return view, secret, nil
}

func freshWebhookID(ctx context.Context, q db.Querier) (string, error) {
	for range idAttempts {
		var raw [webhookIDBytes]byte
		_, _ = rand.Read(raw[:])
		id := webhookPrefix + hex.EncodeToString(raw[:])
		var taken bool
		if err := q.QueryRowContext(ctx, selectWebhookTaken, id).Scan(&taken); err != nil {
			return "", err
		}
		if !taken {
			return id, nil
		}
	}
	return "", errIDExhausted
}

func (w *Webhooks) List(ctx context.Context, client string) ([]Webhook, error) {
	out := []Webhook{}
	err := w.db.Read(ctx, "admin.webhook_list", func(ctx context.Context, q db.Querier) error {
		rows, err := q.QueryContext(ctx, selectClientWebhooks, client)
		if err != nil {
			return err
		}
		for rows.Next() {
			v, err := scanWebhook(rows)
			if err != nil {
				return errors.Join(err, rows.Close())
			}
			out = append(out, v)
		}
		return errors.Join(rows.Err(), rows.Close())
	})
	if err != nil {
		return nil, fmt.Errorf("admin: list the webhooks: %w", err)
	}
	return out, nil
}

func (w *Webhooks) Get(ctx context.Context, client, id string) (Webhook, error) {
	if !WellFormedWebhookID(id) {
		return Webhook{}, ErrWebhookNotFound
	}
	var view Webhook
	err := w.db.Read(ctx, "admin.webhook_get", func(ctx context.Context, q db.Querier) error {
		var err error
		view, err = scanWebhook(q.QueryRowContext(ctx, selectClientWebhook, client, id))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrWebhookNotFound
		}
		return err
	})
	return view, err
}

func (w *Webhooks) Delete(ctx context.Context, client, id string) error {
	if !WellFormedWebhookID(id) {
		return ErrWebhookNotFound
	}
	return w.db.Write(ctx, "admin.webhook_delete", func(ctx context.Context, q db.Querier) error {
		res, err := q.ExecContext(ctx, deleteWebhook, client, id)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrWebhookNotFound
		}
		return nil
	})
}

// Hooks returns every stored webhook with its secret and position, for the dispatcher.
func (w *Webhooks) Hooks(ctx context.Context) ([]hooks.Hook, error) {
	var out []hooks.Hook
	err := w.db.Read(ctx, "admin.webhook_hooks", func(ctx context.Context, q db.Querier) error {
		rows, err := q.QueryContext(ctx, selectDeliveryHooks)
		if err != nil {
			return err
		}
		for rows.Next() {
			var h hooks.Hook
			var headers, filter string
			if err := rows.Scan(&h.ID, &h.Client, &h.Spec.URL, &h.Spec.Method, &h.Spec.ContentType, &headers, &h.Spec.Payload, &filter,
				&h.Secret, &h.Change, &h.High); err != nil {
				return errors.Join(err, rows.Close())
			}
			if h.Spec.Headers, err = hooks.DecodeHeaders(headers); err != nil {
				return errors.Join(errCorruptWebhook, rows.Close())
			}
			if h.Spec.Filter, err = hooks.DecodeFilter(filter); err != nil || len(h.Secret) != webhookSecret {
				return errors.Join(errCorruptWebhook, rows.Close())
			}
			out = append(out, h)
		}
		return errors.Join(rows.Err(), rows.Close())
	})
	if err != nil {
		return nil, fmt.Errorf("admin: load the webhooks: %w", err)
	}
	return out, nil
}

// Advance stores a webhook's new position and adds its outcome counts; a deleted webhook is left deleted.
func (w *Webhooks) Advance(ctx context.Context, id string, p hooks.Progress) error {
	return w.db.Write(ctx, "admin.webhook_advance", func(ctx context.Context, q db.Querier) error {
		_, err := q.ExecContext(ctx, advanceWebhook, id, p.Change, p.High, p.Delivered, p.Failed, optionalMS(p.LastDelivery), optionalMS(p.LastFailure),
			sql.NullString{String: p.Failure, Valid: p.Failure != ""})
		return err
	})
}

// Top is the archive's newest change number, which tells the dispatcher whether anything is new without reading a message.
func (w *Webhooks) Top(ctx context.Context) (int64, error) {
	var top int64
	err := w.db.Read(ctx, "admin.webhook_top", func(ctx context.Context, q db.Querier) error {
		return q.QueryRowContext(ctx, selectChangeTop).Scan(&top)
	})
	return top, err
}

func scanWebhook(s scanner) (Webhook, error) {
	var v Webhook
	var headers, filter string
	var created int64
	var delivered, failed sql.NullInt64
	if err := s.Scan(&v.ID, &v.Client, &v.Spec.URL, &v.Spec.Method, &v.Spec.ContentType, &headers, &v.Spec.Payload, &filter, &created,
		&v.Delivered, &v.Failed, &delivered, &failed, &v.Failure); err != nil {
		return Webhook{}, err
	}
	var err error
	if v.Spec.Headers, err = hooks.DecodeHeaders(headers); err != nil {
		return Webhook{}, errCorruptWebhook
	}
	if v.Spec.Filter, err = hooks.DecodeFilter(filter); err != nil {
		return Webhook{}, errCorruptWebhook
	}
	v.CreatedAt = fromMS(created)
	if delivered.Valid {
		v.LastDelivery = fromMS(delivered.Int64)
	}
	if failed.Valid {
		v.LastFailure = fromMS(failed.Int64)
	}
	return v, nil
}

func optionalMS(t time.Time) sql.NullInt64 {
	if t.IsZero() {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: t.UnixMilli(), Valid: true}
}
