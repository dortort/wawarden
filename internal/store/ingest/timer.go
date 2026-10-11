package ingest

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/dortort/wawarden/internal/policy"
)

const selectChatTimer = "SELECT expires_at - ts FROM messages WHERE chat_jid = ?1 AND expires_at IS NOT NULL ORDER BY ts DESC, seq DESC LIMIT 1"

func (s *Store) ChatTimer(ctx context.Context, chat policy.CanonicalChat) (time.Duration, error) {
	var timer time.Duration
	err := s.Read(ctx, "ingest.chat_timer", func(r *Reader) error {
		c, err := r.Canonical(chat)
		if err != nil {
			return err
		}
		var span int64
		switch err := r.q.QueryRowContext(r.ctx, selectChatTimer, c.JID()).Scan(&span); {
		case errors.Is(err, sql.ErrNoRows):
			return nil
		case err != nil:
			return err
		}
		timer = wholeSeconds(time.Duration(span) * time.Millisecond)
		return nil
	})
	if err != nil {
		return 0, err
	}
	return timer, nil
}
