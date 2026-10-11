package db

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"

	"modernc.org/sqlite"
)

func Unlocked(ctx context.Context, dataDir string, name Name) error {
	if name.label() == "" {
		return errors.New("db: unknown database")
	}
	dir, err := filepath.Abs(dataDir)
	if err != nil {
		return errors.New("db: the data directory's absolute path cannot be determined")
	}
	path := filepath.Join(dir, name.file())
	fi, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return errors.New("db: " + name.file() + " cannot be inspected: " + cause(err))
	case !fi.Mode().IsRegular():
		return errors.New("db: " + name.file() + " is not a regular file")
	}
	q := url.Values{}
	q.Set("mode", "ro")
	uri := (&url.URL{Scheme: "file", Path: filepath.ToSlash(path), RawQuery: q.Encode()}).String()
	h := sql.OpenDB(newConnector(uri, func(sqlite.ExecQuerierContext) error { return nil }, false, func() {}))
	defer func() { _ = h.Close() }()
	h.SetMaxOpenConns(1)
	var tables int64
	switch err := h.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_schema").Scan(&tables); {
	case busy(err):
		return errors.New("db: " + name.file() + " is locked by another process")
	case err != nil:
		return errors.New("db: " + name.file() + " cannot be read to check its lock")
	}
	return nil
}
