package engine

import (
	"bytes"
	"compress/zlib"
	"errors"
	"io"
	"os"
	"path/filepath"
)

var errTooLarge = errors.New("engine: the history blob is larger than WAWARDEN_HISTORY_MAX_BYTES")

func inflate(compressed []byte, limit int64) ([]byte, error) {
	zr, err := zlib.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return nil, err
	}
	n, err := io.Copy(io.Discard, io.LimitReader(zr, limit+1))
	if err != nil {
		return nil, err
	}
	if n > limit {
		return nil, errTooLarge
	}
	if zr, err = zlib.NewReader(bytes.NewReader(compressed)); err != nil {
		return nil, err
	}
	out := make([]byte, n)
	if _, err := io.ReadFull(zr, out); err != nil {
		return nil, err
	}
	return out, nil
}

type cappedWriter struct {
	w     io.Writer
	limit int64
	n     int64
}

func (c *cappedWriter) Write(p []byte) (int, error) {
	if c.n+int64(len(p)) > c.limit {
		return 0, errTooLarge
	}
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

func readCapped(path string, limit int64) ([]byte, error) {
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if fi.Size() > limit {
		return nil, errTooLarge
	}
	data := make([]byte, fi.Size())
	if _, err := io.ReadFull(f, data); err != nil {
		return nil, err
	}
	return data, nil
}
