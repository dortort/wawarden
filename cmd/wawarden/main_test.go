package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/dortort/wawarden/internal/app"
)

const mainHelper = "GO_TEST_RUN_MAIN_WITH_A_STUCK_DRAIN"

func TestMain(m *testing.M) {
	if os.Getenv(mainHelper) == "1" {
		runApp = func(_ *app.App, ctx context.Context) error {
			_, _ = fmt.Fprintln(os.Stderr, "serving")
			<-ctx.Done()
			_, _ = fmt.Fprintln(os.Stderr, "draining")
			select {}
		}
		os.Args = []string{"wawarden", "serve"}
		main()
	}
	os.Exit(m.Run())
}

func lines(t *testing.T, r io.Reader) <-chan string {
	t.Helper()
	out := make(chan string, 64)
	go func() {
		defer close(out)
		s := bufio.NewScanner(r)
		for s.Scan() {
			out <- s.Text()
		}
	}()
	return out
}

func awaitLine(t *testing.T, from <-chan string, want string) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case line, ok := <-from:
			if !ok {
				t.Fatalf("the process closed its output before printing %q", want)
			}
			if strings.Contains(line, want) {
				return
			}
		case <-deadline:
			t.Fatalf("no %q from the process after 10 s", want)
		}
	}
}

func TestLaterSignalsEndAStuckShutdown(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("Executable: %v", err)
	}
	cmd := exec.CommandContext(t.Context(), executable, "-test.run=^$") //nolint:gosec // G204: the command is the running test binary, from os.Executable
	cmd.Env = []string{
		mainHelper + "=1",
		"WAWARDEN_DATA_DIR=" + filepath.Join(t.TempDir(), "data"),
		"WAWARDEN_LISTEN=" + freeAddr(t),
		"WAWARDEN_HEALTH_LISTEN=" + freeAddr(t),
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatalf("StderrPipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	diagnostics := lines(t, stderr)
	awaitLine(t, diagnostics, "serving")

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("first SIGTERM: %v", err)
	}
	awaitLine(t, diagnostics, "draining")
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	deadline := time.After(10 * time.Second)
	for {
		if err := cmd.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
			t.Fatalf("another SIGTERM: %v", err)
		}
		select {
		case err := <-exited:
			var exit *exec.ExitError
			if !errors.As(err, &exit) {
				t.Fatalf("Wait = %v, want the process killed by a later SIGTERM", err)
			}
			if status, ok := exit.Sys().(syscall.WaitStatus); !ok || !status.Signaled() || status.Signal() != syscall.SIGTERM {
				t.Fatalf("the process ended with %v, want it killed by a later SIGTERM", exit)
			}
			return
		case <-time.After(100 * time.Millisecond):
		case <-deadline:
			t.Fatal("further SIGTERMs did not end a process whose shutdown is stuck")
		}
	}
}
