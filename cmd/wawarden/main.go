// Command wawarden runs the WaWarden gateway and its operator subcommands.
package main

import (
	"context"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"

	"github.com/dortort/wawarden/internal/safego"
)

func main() {
	syscall.Umask(0o077)
	debug.SetTraceback("single")
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	safego.Go("signals", func() {
		<-ctx.Done()
		stop()
	})
	code := run(ctx, os.Args[1:], os.Environ(), os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}
