// Command wawarden runs the WaWarden gateway and its operator subcommands.
package main

import (
	"context"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
)

func main() {
	syscall.Umask(0o077)
	debug.SetTraceback("single")
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Environ(), os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}
