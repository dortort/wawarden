package db

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"modernc.org/sqlite"
)

var errCopyNotRegular = errors.New("db: the copy is not a regular file")

func OpenCopy(ctx context.Context, path string, timeout time.Duration) (*DB, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, errors.New("db: the copy's absolute path cannot be determined")
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return nil, errors.New("db: the copy cannot be inspected: " + cause(err))
	}
	if !fi.Mode().IsRegular() {
		return nil, errCopyNotRegular
	}
	q := url.Values{}
	q.Set("mode", "ro")
	q.Add("_pragma", "query_only(1)")
	uri := (&url.URL{Scheme: "file", Path: filepath.ToSlash(abs), RawQuery: q.Encode()}).String()
	d := &DB{
		name:           Archive,
		path:           abs,
		dir:            filepath.Dir(abs),
		logger:         slog.New(slog.DiscardHandler),
		readTimeout:    timeout,
		writeTimeout:   timeout,
		rewriteTimeout: timeout,
		closeWait:      defaultCloseWait,
	}
	d.connector = newConnector(uri, func(sqlite.ExecQuerierContext) error { return nil }, false, func() {})
	d.sql = sql.OpenDB(d.connector)
	d.sql.SetMaxOpenConns(1)
	if err := d.sql.PingContext(ctx); err != nil {
		return nil, errors.Join(err, d.sql.Close())
	}
	return d, nil
}
