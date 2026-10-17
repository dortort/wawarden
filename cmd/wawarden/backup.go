package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"regexp"
	"strconv"
	"time"

	"github.com/dortort/wawarden/internal/backup"
	"github.com/dortort/wawarden/internal/sanitize"
	"github.com/dortort/wawarden/internal/store/inspect"
)

var versionShape = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.+_-]{0,63}$`)

func backupCommand(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "verify" {
		return usageError(stderr)
	}
	flags := flag.NewFlagSet("backup verify", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.Usage = func() {}
	workDir := flags.String("work-dir", "", "an existing private directory to extract into")
	extractTo := flags.String("extract-to", "", "a new directory to keep the verified databases in")
	if err := flags.Parse(args[1:]); err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			_, _ = fmt.Fprintln(stderr, "backup verify:", flagProblem(flags, err))
		}
		return usageError(stderr)
	}
	if *workDir == "" || flags.NArg() != 1 || flags.Arg(0) != "-" {
		_, _ = fmt.Fprintln(stderr, "backup verify: give --work-dir with a value and - to read the decrypted backup from standard input")
		return usageError(stderr)
	}
	if *extractTo != "" {
		if _, err := os.Lstat(*extractTo); !errors.Is(err, fs.ErrNotExist) {
			_, _ = fmt.Fprintln(stderr, "backup verify: --extract-to must name a directory that does not exist yet")
			return exitFailed
		}
	}
	archiveName, sessionName := inspect.Files()
	names := [2]string{archiveName, sessionName}
	ext, err := backup.Extract(ctx, stdin, backup.ExtractOptions{WorkDir: *workDir, UID: os.Geteuid(), Names: names, Free: inspect.FreeBytes})
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "backup verify:", verifyProblem(err))
		if errors.Is(err, backup.ErrRejected) {
			return exitUnverified
		}
		return exitFailed
	}
	defer func() { _ = os.RemoveAll(ext.Dir) }()
	archive, err := inspect.CheckArchive(ctx, ext.Members[0].Path)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "backup verify: the extracted archive could not be checked")
		return exitFailed
	}
	device, err := inspect.CheckDevice(ctx, ext.Members[1].Path)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "backup verify: the extracted device store could not be checked")
		return exitFailed
	}
	var out bytes.Buffer
	verified := report(&out, ext, archive, device)
	if _, err := stdout.Write(out.Bytes()); err != nil {
		return exitFailed
	}
	if !verified {
		return exitUnverified
	}
	if *extractTo != "" {
		if err := backup.Keep(ext, *extractTo, os.Geteuid(), names); err != nil {
			_, _ = fmt.Fprintln(stderr, "backup verify: the verified databases could not be moved to --extract-to, which must be on the filesystem of --work-dir")
			return exitFailed
		}
	}
	return 0
}

func verifyProblem(err error) string {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "stopped before the backup was read"
	}
	if errors.Is(err, backup.ErrWorkDir) {
		return "--work-dir must be an existing directory of this user with mode 0700"
	}
	return err.Error()
}

func report(out *bytes.Buffer, ext backup.Extracted, archive inspect.Archive, device inspect.Device) bool {
	version := ext.Version
	if !versionShape.MatchString(version) {
		version = "unrecognised"
	}
	line := func(key, value string) { fmt.Fprintf(out, "%s: %s\n", key, value) }
	count := func(key string, n int64) { line(key, strconv.FormatInt(n, 10)) }
	line("format", strconv.Itoa(ext.Format))
	line("written by", version)
	line("created", ext.Created.Format(time.RFC3339))
	ok := integrity(line, "archive", archive.Integrity)
	if archive.Counted {
		ok = schema(line, "archive", archive.SchemaVersion, ext.Members[0].SchemaVersion, inspect.LatestArchiveVersion()) && ok
		count("archive chats", archive.Chats)
		count("archive messages", archive.Messages)
		count("archive history blobs pending", archive.BlobsPending)
		count("archive history blobs quarantined", archive.BlobsQuarantined)
		count("archive inbox backlog", archive.InboxBacklog)
		count("archive inbox quarantined", archive.InboxQuarantined)
		count("archive active clients", archive.ClientsActive)
		count("archive audit rows", archive.AuditRows)
		count("archive media references live", archive.MediaRefsLive)
		count("archive media references expired", archive.MediaRefsExpired)
		count("archive revoke tombstones", archive.RevokeTombstones)
	}
	ok = integrity(line, "device store", device.Integrity) && ok
	if device.Counted {
		ok = schema(line, "device store", device.SchemaVersion, ext.Members[1].SchemaVersion, 0) && ok
		paired := "no"
		if device.Paired {
			paired = "yes"
		}
		line("device store paired", paired)
		count("device store pre-keys", device.PreKeys)
		count("device store sessions", device.Sessions)
	}
	verified := "no"
	if ok {
		verified = "yes"
	}
	line("verified", verified)
	return ok
}

func integrity(line func(key, value string), name string, in inspect.Integrity) bool {
	switch {
	case !in.OK:
		line(name+" integrity", "failed")
		for _, p := range in.Problems {
			line(name+" integrity problem", sanitize.Terminal(p))
		}
		return false
	case !in.Counted:
		line(name+" integrity", "ok")
		line(name+" counts", "unreadable")
		return false
	}
	line(name+" integrity", "ok")
	return true
}

func schema(line func(key, value string), name string, got, manifest, latest int) bool {
	line(name+" schema version", strconv.Itoa(got))
	switch {
	case got != manifest:
		line(name+" schema", "differs from the manifest's "+strconv.Itoa(manifest))
		return false
	case latest > 0 && got > latest:
		line(name+" schema", "newer than this build, which reads up to "+strconv.Itoa(latest))
		return false
	}
	line(name+" schema", "ok")
	return true
}
