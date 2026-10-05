package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

const tokenCommandWaitDelay = 2 * time.Second

var tokenCommandTimeout = 30 * time.Second

var (
	errCommandSyntax   = errors.New("the token command is empty or has an unterminated quote or escape")
	errCommandTimeout  = errors.New("the token command did not finish in time")
	errCommandLingered = errors.New("the token command left a process holding its output open")
	errCommandStart    = errors.New("the token command could not be started")
	errCommandFailed   = errors.New("the token command failed")
)

func tokenFromCommand(ctx context.Context, command string, environ []string) (string, error) {
	argv, err := splitCommand(command)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, tokenCommandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // G204: the operator's own token command, run without a shell
	cmd.Env = withoutServiceVariables(environ)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = tokenCommandWaitDelay
	out := &cappedWriter{left: maxTokenSourceBytes}
	cmd.Stdout = out
	err = cmd.Run()
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	if out.exceeded {
		return "", errTokenTooLarge
	}
	if err != nil {
		return "", commandError(ctx, err)
	}
	return adminToken(out.buf.Bytes())
}

func commandError(ctx context.Context, err error) error {
	var exit *exec.ExitError
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return errCommandTimeout
	case errors.Is(err, exec.ErrWaitDelay):
		return errCommandLingered
	case errors.As(err, &exit):
		return fmt.Errorf("%w: exit status %d", errCommandFailed, exit.ExitCode())
	}
	return errCommandStart
}

func withoutServiceVariables(environ []string) []string {
	out := make([]string, 0, len(environ))
	for _, kv := range environ {
		if !strings.HasPrefix(kv, "WAWARDEN_") {
			out = append(out, kv)
		}
	}
	return out
}

type cappedWriter struct {
	buf      bytes.Buffer
	left     int
	exceeded bool
}

func (c *cappedWriter) Write(p []byte) (int, error) {
	if len(p) > c.left {
		c.exceeded = true
		return 0, errTokenTooLarge
	}
	c.left -= len(p)
	return c.buf.Write(p)
}

func splitCommand(s string) ([]string, error) {
	var out []string
	var word strings.Builder
	inWord, escaped := false, false
	var quote rune
	for _, r := range s {
		switch {
		case escaped:
			word.WriteRune(r)
			escaped = false
		case r == '\\' && quote != '\'':
			escaped, inWord = true, true
		case quote != 0 && r == quote:
			quote = 0
		case quote != 0:
			word.WriteRune(r)
		case r == '"' || r == '\'':
			quote, inWord = r, true
		case r == ' ' || r == '\t':
			if inWord {
				out = append(out, word.String())
				word.Reset()
				inWord = false
			}
		default:
			word.WriteRune(r)
			inWord = true
		}
	}
	if escaped || quote != 0 {
		return nil, errCommandSyntax
	}
	if inWord {
		out = append(out, word.String())
	}
	if len(out) == 0 {
		return nil, errCommandSyntax
	}
	return out, nil
}
