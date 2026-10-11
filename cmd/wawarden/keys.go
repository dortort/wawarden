package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/dortort/wawarden/internal/keys"
	adminstore "github.com/dortort/wawarden/internal/store/admin"
)

func keysCommand(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "rotate" {
		return usageError(stderr)
	}
	flags := flag.NewFlagSet("keys rotate", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.Usage = func() {}
	dataDir := flags.String("data-dir", "", "the data directory of the stopped service")
	if err := flags.Parse(args[1:]); err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			_, _ = fmt.Fprintln(stderr, "keys rotate:", flagProblem(flags, err))
		}
		return usageError(stderr)
	}
	if flags.NArg() > 0 || *dataDir == "" {
		_, _ = fmt.Fprintln(stderr, "keys rotate: give --data-dir with a value, and no other argument")
		return usageError(stderr)
	}
	previous, current, refusal := keys.Rotate(*dataDir, os.Geteuid(), func() error { return adminstore.ArchiveUnlocked(ctx, *dataDir) })
	if refusal != nil {
		_, _ = fmt.Fprintln(stderr, "keys rotate:", refusal.Error())
		return exitFailed
	}
	if _, err := fmt.Fprintf(stdout, "previous key id: %s\nnew key id: %s\n", previous, current); err != nil {
		return exitFailed
	}
	return 0
}
