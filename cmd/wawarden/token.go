package main

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/dortort/wawarden/internal/policy"
)

const maxTokenSourceBytes = 4096

var (
	errNotAdminToken  = errors.New("the credential is not an admin token: it must have the form that wawarden admin init prints")
	errTokenTooLarge  = errors.New("the token source holds more than 4096 bytes")
	errStdinTerminal  = errors.New("standard input is a terminal: pipe the token in instead of typing it")
	errStdinUnread    = errors.New("standard input cannot be read")
	errFileUnreadable = errors.New("the token file cannot be read")
	errFileNotRegular = errors.New("the token file is not a regular file")
)

func adminToken(b []byte) (string, error) {
	s := strings.TrimSpace(string(b))
	if !policy.WellFormedAdminToken(s) {
		return "", errNotAdminToken
	}
	return s, nil
}

func readCapped(r io.Reader) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, maxTokenSourceBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxTokenSourceBytes {
		return nil, errTokenTooLarge
	}
	return b, nil
}

func tokenFromFile(path string) (string, error) {
	f, err := os.OpenFile(filepath.Clean(path), os.O_RDONLY|syscall.O_NONBLOCK, 0) //nolint:gosec // G703: the operator names the token file to read
	if err != nil {
		return "", errFileUnreadable
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return "", errFileUnreadable
	}
	if !fi.Mode().IsRegular() {
		return "", errFileNotRegular
	}
	b, err := readCapped(f)
	if errors.Is(err, errTokenTooLarge) {
		return "", err
	}
	if err != nil {
		return "", errFileUnreadable
	}
	return adminToken(b)
}

func tokenFromStdin(stdin io.Reader) (string, error) {
	if stdin == nil {
		return "", errStdinUnread
	}
	if f, ok := stdin.(*os.File); ok {
		if fi, err := f.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
			return "", errStdinTerminal
		}
	}
	b, err := readCapped(stdin)
	if errors.Is(err, errTokenTooLarge) {
		return "", err
	}
	if err != nil {
		return "", errStdinUnread
	}
	return adminToken(b)
}
