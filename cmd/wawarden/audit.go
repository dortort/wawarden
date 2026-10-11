package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/dortort/wawarden/internal/keys"
	adminstore "github.com/dortort/wawarden/internal/store/admin"
)

const (
	exitUnverified = 6
	maxLogLine     = 1 << 20
)

var (
	chainHead     = regexp.MustCompile(`^[0-9a-f]{32}$`)
	keyIDShape    = regexp.MustCompile(`^[0-9a-f]{8}$`)
	errLogNotFile = errors.New("the log is not a regular file")
)

func audit(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "verify" {
		return usageError(stderr)
	}
	flags := flag.NewFlagSet("audit verify", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.Usage = func() {}
	copyPath := flags.String("db", "", "a copy of the archive to verify")
	keyPath := flags.String("master-key-file", "", "the master key the service ran with")
	logPath := flags.String("log", "", "a capture of the service's standard output")
	retiredPath := flags.String("retired-keys-dir", "", "the retired keys kept beside the master key")
	if err := flags.Parse(args[1:]); err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			_, _ = fmt.Fprintln(stderr, "audit verify:", flagProblem(flags, err))
		}
		return usageError(stderr)
	}
	if flags.NArg() > 0 || *copyPath == "" || *keyPath == "" {
		_, _ = fmt.Fprintln(stderr, "audit verify: give --db and --master-key-file, each with a value, and no other argument")
		return usageError(stderr)
	}
	retired := *retiredPath
	if _, err := os.Stat(keys.RetiredDirOf(*keyPath)); retired == "" && err == nil {
		retired = keys.RetiredDirOf(*keyPath)
	}
	master, refusal := keys.LoadFiles(*keyPath, retired)
	if refusal != nil {
		_, _ = fmt.Fprintln(stderr, "audit verify:", refusal.Error())
		return exitFailed
	}
	var heads []adminstore.Head
	if *logPath != "" {
		var err error
		if heads, err = shippedHeads(*logPath); err != nil {
			_, _ = fmt.Fprintln(stderr, "audit verify: the log cannot be read:", withoutPath(err))
			return exitFailed
		}
	}
	rep, err := adminstore.Verify(ctx, *copyPath, master, heads)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "audit verify:", err)
		return exitFailed
	}
	var out bytes.Buffer
	first, problem, head, missing, pruned, anchor, ids := "none", "none", "none", "none", "none", "none", "none"
	if rep.FirstBad != 0 {
		first, problem = strconv.FormatInt(rep.FirstBad, 10), rep.Problem
	}
	if rep.Head != "" {
		head = rep.Head
	}
	if rep.FirstMissing != "" {
		missing = rep.FirstMissing
	}
	if rep.PrunedThrough != 0 {
		pruned, anchor = strconv.FormatInt(rep.PrunedThrough, 10), rep.AnchorHead
	}
	if len(rep.KeyIDs) > 0 {
		var counts []string
		for _, id := range slices.Sorted(maps.Keys(rep.KeyIDs)) {
			counts = append(counts, id+"="+strconv.FormatInt(rep.KeyIDs[id], 10))
		}
		ids = strings.Join(counts, " ")
	}
	for _, line := range [][2]string{
		{"rows", strconv.FormatInt(rep.Rows, 10)},
		{"verified", strconv.FormatInt(rep.Verified, 10)},
		{"rows failing", strconv.FormatInt(rep.Bad, 10)},
		{"first failing row", first},
		{"problem", problem},
		{"chain head", head},
		{"pruned through", pruned},
		{"anchor head", anchor},
		{"key ids", ids},
		{"shipped heads pruned", strconv.Itoa(rep.HeadsPruned)},
		{"shipped heads", strconv.Itoa(rep.Heads)},
		{"shipped heads missing", strconv.Itoa(rep.HeadsMissing)},
		{"first missing head", missing},
	} {
		fmt.Fprintf(&out, "%s: %s\n", line[0], line[1])
	}
	if _, err := stdout.Write(out.Bytes()); err != nil {
		return exitFailed
	}
	switch {
	case *logPath == "":
		_, _ = fmt.Fprintln(stderr, "audit verify: without --log no shipped chain head is compared, so rows removed from the end of the chain go unnoticed")
	case len(heads) == 0:
		_, _ = fmt.Fprintln(stderr, "audit verify: the log holds no line with a chain_head, so no shipped chain head is compared and rows removed from the end of the chain go unnoticed")
		return exitUnverified
	}
	if !rep.OK() {
		return exitUnverified
	}
	return 0
}

func withoutPath(err error) error {
	if pe, ok := errors.AsType[*fs.PathError](err); ok {
		return pe.Err
	}
	return err
}

func shippedHeads(path string) ([]adminstore.Head, error) {
	f, err := os.OpenFile(filepath.Clean(path), os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return nil, errLogNotFile
	}
	var heads []adminstore.Head
	lines := bufio.NewScanner(f)
	lines.Buffer(make([]byte, 64<<10), maxLogLine)
	for lines.Scan() {
		var line struct {
			ChainHead *string         `json:"chain_head"`
			TS        json.RawMessage `json:"ts"`
			KeyID     json.RawMessage `json:"key_id"`
		}
		if json.Unmarshal(lines.Bytes(), &line) == nil && line.ChainHead != nil && chainHead.MatchString(*line.ChainHead) {
			h := adminstore.Head{Hex: *line.ChainHead}
			if json.Unmarshal(line.KeyID, &h.KeyID) != nil || !keyIDShape.MatchString(h.KeyID) {
				h.KeyID = ""
			}
			var ts string
			if json.Unmarshal(line.TS, &ts) == nil {
				if at, err := time.Parse(time.RFC3339Nano, ts); err == nil {
					h.TS = at.UnixMilli()
				}
			}
			heads = append(heads, h)
		}
	}
	return heads, lines.Err()
}
