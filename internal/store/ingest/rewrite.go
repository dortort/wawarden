package ingest

import (
	"context"
	"errors"
	"strings"

	"github.com/dortort/wawarden/internal/store/internal/db"
)

const (
	rewriteDueKey        = "index_rewrite_due"
	createForgotten      = "CREATE VIRTUAL TABLE IF NOT EXISTS temp.forgotten USING fts5 (text, content = '', tokenize = 'trigram', detail = 'none')"
	createForgottenTerms = "CREATE VIRTUAL TABLE IF NOT EXISTS temp.forgotten_terms USING fts5vocab (temp, forgotten, 'row')"
	insertForgotten      = "INSERT INTO temp.forgotten (text) VALUES (?)"
	selectForgottenTerms = "SELECT term FROM temp.forgotten_terms"
	clearForgotten       = "INSERT INTO temp.forgotten (forgotten) VALUES ('delete-all')"
	selectPageKeys       = "SELECT term FROM messages_fts_idx"
	countIndexed         = "SELECT count(*) FROM (SELECT 1 FROM messages_fts WHERE messages_fts MATCH ? LIMIT 1)"
	markRewriteDue       = "INSERT INTO sync_state (key, value) VALUES (?, '1') ON CONFLICT (key) DO NOTHING"
	insertFiller         = "INSERT INTO messages_fts (rowid, text) VALUES (0, 'index rewrite')"
	optimizeIndex        = "INSERT INTO messages_fts (messages_fts) VALUES ('optimize')"
	deleteFiller         = "INSERT INTO messages_fts (messages_fts, rowid, text) VALUES ('delete', 0, 'index rewrite')"
	clearRewriteDue      = "DELETE FROM sync_state WHERE key = ?"
)

func (tx *Tx) noteForgotten(text string) error {
	if !tx.forgotten {
		if _, err := tx.q.ExecContext(tx.ctx, createForgotten); err != nil {
			return err
		}
		if _, err := tx.q.ExecContext(tx.ctx, createForgottenTerms); err != nil {
			return err
		}
		tx.forgotten = true
	}
	_, err := tx.q.ExecContext(tx.ctx, insertForgotten, text)
	return err
}

func (tx *Tx) markStaleKeys() (bool, error) {
	if !tx.forgotten {
		return false, nil
	}
	terms, err := tx.forgottenTerms()
	if err != nil {
		return false, err
	}
	keys, err := tx.pageKeysAmong(terms)
	if err != nil {
		return false, err
	}
	for _, key := range keys {
		var n int
		if err := tx.q.QueryRowContext(tx.ctx, countIndexed, `"`+strings.ReplaceAll(key, `"`, `""`)+`"`).Scan(&n); err != nil {
			return false, err
		}
		if n == 0 {
			_, err := tx.q.ExecContext(tx.ctx, markRewriteDue, rewriteDueKey)
			return err == nil, err
		}
	}
	return false, nil
}

func (tx *Tx) forgottenTerms() (map[string]bool, error) {
	rows, err := tx.q.QueryContext(tx.ctx, selectForgottenTerms)
	if err != nil {
		return nil, err
	}
	terms := map[string]bool{}
	for rows.Next() {
		var term string
		if err := rows.Scan(&term); err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		terms[term] = true
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	_, err = tx.q.ExecContext(tx.ctx, clearForgotten)
	return terms, err
}

func (tx *Tx) pageKeysAmong(terms map[string]bool) ([]string, error) {
	rows, err := tx.q.QueryContext(tx.ctx, selectPageKeys)
	if err != nil {
		return nil, err
	}
	var keys []string
	for rows.Next() {
		var key []byte
		if err := rows.Scan(&key); err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		if len(key) > 1 && key[0] == '0' && terms[string(key[1:])] {
			keys = append(keys, string(key[1:]))
		}
	}
	return keys, errors.Join(rows.Err(), rows.Close())
}

func (s *Store) rewriteIndex(ctx context.Context) error {
	return s.db.Rewrite(ctx, "ingest.index_rewrite", func(ctx context.Context, q db.Querier) error {
		if _, err := q.ExecContext(ctx, insertFiller); err != nil {
			return err
		}
		if _, err := q.ExecContext(ctx, optimizeIndex); err != nil {
			return err
		}
		if _, err := q.ExecContext(ctx, deleteFiller); err != nil {
			return err
		}
		_, err := q.ExecContext(ctx, clearRewriteDue, rewriteDueKey)
		return err
	})
}

func (s *Store) finishRewrite(ctx context.Context) error {
	var due bool
	if err := s.Read(ctx, "ingest.index_rewrite_due", func(r *Reader) error {
		var err error
		_, due, err = r.SyncValue(rewriteDueKey)
		return err
	}); err != nil || !due {
		return err
	}
	return s.rewriteIndex(ctx)
}
