package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/dortort/wawarden/internal/policy"
	"github.com/dortort/wawarden/internal/store/ingest"
)

const (
	dropNotPrimary = "history_not_primary"
	queueHistory   = "history"
	historyDir     = "history"
	historyBatch   = 100
	pendingWindow  = 16
	partialSuffix  = ".part"
	blobFileSuffix = ".bin"
)

var (
	errNotConnected = errors.New("engine: the history blob needs a download and the engine is not connected")
	errPaused       = errors.New("engine: ingest is paused")
)

type historian struct {
	p         *pipeline
	client    Client
	decoder   HistoryDecoder
	dir       string
	max       int64
	connected func() bool
	acked     map[string]bool

	fsyncFile  func(*os.File) error
	closeFile  func(*os.File) error
	renameFile func(oldpath, newpath string) error
	fsyncDir   func(dir string) error
}

func newHistorian(o Options, p *pipeline, connected func() bool) *historian {
	return &historian{
		p: p, client: o.Client, decoder: o.Decoder, dir: filepath.Join(o.DataDir, historyDir), max: o.HistoryMaxBytes,
		connected: connected, acked: map[string]bool{},
		fsyncFile: (*os.File).Sync, closeFile: (*os.File).Close, renameFile: os.Rename, fsyncDir: syncDir,
	}
}

func primaryDevice(raw string) bool {
	if _, ok := normalizeUser(raw); !ok {
		return false
	}
	local, _, _ := strings.Cut(raw, "@")
	_, device, ok := strings.Cut(local, ":")
	if !ok {
		return true
	}
	n, err := strconv.Atoi(device)
	return err == nil && n == 0
}

func (h *historian) accept(ev HistoryNotification) bool {
	p := h.p
	if !ev.FromMe || !primaryDevice(ev.Sender) {
		p.counts.dropped.With(dropNotPrimary).Inc()
		return true
	}
	if p.paused.Load() {
		p.counts.refused.With(refusedPaused).Inc()
		return false
	}
	ref, err := json.Marshal(ev.Ref)
	if err != nil {
		p.counts.dropped.With(dropInvalid).Inc()
		return true
	}
	err = p.write(p.ctx, "engine.history_record", func(tx *ingest.Tx) error {
		_, err := tx.RecordBlob(ev.Ref.ID, ref, p.clock.Now())
		return err
	})
	switch {
	case errors.Is(err, ingest.ErrInvalid):
		p.counts.dropped.With(dropInvalid).Inc()
		return true
	case err != nil:
		p.counts.refused.With(refusedStore).Inc()
		return false
	}
	p.wake()
	return true
}

func (h *historian) path(id, suffix string) string {
	return filepath.Join(h.dir, id+suffix)
}

func (h *historian) drain(ctx context.Context) {
	for ctx.Err() == nil && h.p.tick(ctx) {
		var pending []ingest.Blob
		if err := h.p.archive.Read(ctx, "engine.history_pending", func(r *ingest.Reader) error {
			var err error
			pending, err = r.PendingBlobs(pendingWindow)
			return err
		}); err != nil {
			h.p.logger.Warn("reading the pending history blobs failed", slog.String("event", "ingest_failed"), slog.String("queue", queueHistory), slog.String("error_type", fmt.Sprintf("%T", err)))
			_ = wait(ctx, h.p.clock, retryBase)
			return
		}
		ready := false
		for _, b := range pending {
			if h.ready(b) {
				h.process(ctx, b)
				ready = true
				break
			}
		}
		if !ready {
			return
		}
		h.p.drainInbox(ctx)
	}
}

func (h *historian) ready(b ingest.Blob) bool {
	var ref HistoryRef
	if json.Unmarshal(b.Ref, &ref) != nil || len(ref.Inline) > 0 || h.connected() {
		return true
	}
	_, err := os.Lstat(h.path(b.ID, blobFileSuffix))
	return err == nil
}

func (h *historian) process(ctx context.Context, b ingest.Blob) {
	p := h.p
	run := context.WithoutCancel(ctx)
	var attempts int
	if err := p.write(run, "engine.history_attempt", func(tx *ingest.Tx) error {
		var err error
		attempts, err = tx.RecordBlobAttempt(b.ID)
		return err
	}); err != nil {
		p.logger.Warn("recording a history attempt failed", slog.String("event", "ingest_failed"), slog.String("queue", queueHistory), slog.String("error_type", fmt.Sprintf("%T", err)))
		_ = wait(ctx, p.clock, retryBase)
		return
	}
	if attempts > maxAttempts {
		h.quarantine(run, b.ID, attempts-1)
		return
	}
	err := guarded("engine.ingest", func() error { return h.ingest(ctx, b) })
	if err == nil {
		return
	}
	if errors.Is(err, errPaused) || ctx.Err() != nil {
		h.release(run, b.ID)
		return
	}
	p.logger.Warn("processing a history blob failed", slog.String("event", "ingest_failed"), slog.String("queue", queueHistory), slog.Int("attempt", attempts), slog.String("error_type", fmt.Sprintf("%T", err)))
	if attempts >= maxAttempts {
		h.quarantine(run, b.ID, attempts)
		return
	}
	_ = wait(ctx, p.clock, retryBase<<(attempts-1))
}

func (h *historian) release(ctx context.Context, id string) {
	if err := h.p.write(ctx, "engine.history_release", func(tx *ingest.Tx) error { return tx.ReleaseBlobAttempt(id) }); err != nil {
		h.p.logger.Warn("giving back an interrupted history attempt failed", slog.String("event", "ingest_failed"), slog.String("queue", queueHistory), slog.String("error_type", fmt.Sprintf("%T", err)))
	}
}

func (h *historian) ingest(ctx context.Context, b ingest.Blob) error {
	var ref HistoryRef
	if err := json.Unmarshal(b.Ref, &ref); err != nil {
		return err
	}
	compressed, err := h.load(ctx, b.ID, ref)
	if err != nil {
		return err
	}
	raw, err := inflate(compressed, h.max)
	if err != nil {
		return err
	}
	hist, err := h.decoder.Decode(raw)
	if err != nil {
		return err
	}
	if err := h.apply(ctx, hist, b.ReceivedAt); err != nil {
		return err
	}
	if err := h.p.write(context.WithoutCancel(ctx), "engine.history_processed", func(tx *ingest.Tx) error {
		return tx.MarkBlobProcessed(b.ID, h.p.clock.Now())
	}); err != nil {
		return err
	}
	h.remove(b.ID)
	return nil
}

func (h *historian) load(ctx context.Context, id string, ref HistoryRef) ([]byte, error) {
	if len(ref.Inline) > 0 {
		if int64(len(ref.Inline)) > h.max {
			return nil, errTooLarge
		}
		h.ack(ctx, id, ref)
		return ref.Inline, nil
	}
	data, err := readCapped(h.path(id, blobFileSuffix), h.max)
	if !errors.Is(err, fs.ErrNotExist) {
		if err == nil {
			h.ack(ctx, id, ref)
		}
		return data, err
	}
	if !h.connected() {
		return nil, errNotConnected
	}
	if h.max > 0 && ref.FileLength > uint64(h.max) {
		return nil, errTooLarge
	}
	if err := h.download(ctx, id, ref); err != nil {
		return nil, err
	}
	h.ack(ctx, id, ref)
	return readCapped(h.path(id, blobFileSuffix), h.max)
}

func (h *historian) download(ctx context.Context, id string, ref HistoryRef) error {
	if err := os.Mkdir(h.dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	partial := h.path(id, partialSuffix)
	f, err := os.OpenFile(filepath.Clean(partial), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	err = h.client.DownloadHistory(ctx, ref, &cappedWriter{w: f, limit: h.max})
	if err == nil {
		err = h.fsyncFile(f)
	}
	if err = errors.Join(err, h.closeFile(f)); err != nil {
		_ = os.Remove(partial)
		return err
	}
	if err := h.renameFile(partial, h.path(id, blobFileSuffix)); err != nil {
		return err
	}
	return h.fsyncDir(h.dir)
}

func syncDir(dir string) error {
	d, err := os.Open(filepath.Clean(dir))
	if err != nil {
		return err
	}
	return errors.Join(d.Sync(), d.Close())
}

func (h *historian) ack(ctx context.Context, id string, ref HistoryRef) {
	if h.acked[id] {
		return
	}
	if err := h.client.AckHistory(ctx, ref); err != nil {
		h.p.logger.Warn("sending the history receipt failed", slog.String("event", "history_ack_failed"), slog.String("error_type", fmt.Sprintf("%T", err)))
		return
	}
	h.acked[id] = true
}

func (h *historian) kept(err error) {
	h.p.logger.Warn("removing a history file failed", slog.String("event", "history_file_kept"), slog.String("error_type", fmt.Sprintf("%T", err)))
}

func (h *historian) remove(id string) {
	removed := false
	for _, suffix := range []string{blobFileSuffix, partialSuffix} {
		switch err := os.Remove(h.path(id, suffix)); {
		case err == nil:
			removed = true
		case !errors.Is(err, fs.ErrNotExist):
			h.kept(err)
		}
	}
	if removed {
		if err := h.fsyncDir(h.dir); err != nil {
			h.kept(err)
		}
	}
	delete(h.acked, id)
}

func (h *historian) clean(ctx context.Context) {
	entries, err := os.ReadDir(h.dir)
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	if err != nil {
		h.kept(err)
		return
	}
	removed := false
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), blobFileSuffix)
		if !ok {
			id, ok = strings.CutSuffix(e.Name(), partialSuffix)
		}
		if !ok {
			continue
		}
		var pending bool
		err := h.p.archive.Read(ctx, "engine.history_clean", func(r *ingest.Reader) error {
			var err error
			pending, err = r.BlobPending(id)
			return err
		})
		if err != nil && !errors.Is(err, ingest.ErrInvalid) {
			h.kept(err)
			continue
		}
		if pending {
			continue
		}
		if err := os.Remove(filepath.Join(h.dir, e.Name())); err != nil {
			h.kept(err)
			continue
		}
		removed = true
	}
	if removed {
		if err := h.fsyncDir(h.dir); err != nil {
			h.kept(err)
		}
	}
}

func (h *historian) quarantine(ctx context.Context, id string, attempts int) {
	p := h.p
	if err := p.write(ctx, "engine.history_quarantine", func(tx *ingest.Tx) error { return tx.QuarantineBlob(id) }); err != nil {
		p.logger.Warn("quarantining a history blob failed", slog.String("event", "ingest_failed"), slog.String("queue", queueHistory), slog.String("error_type", fmt.Sprintf("%T", err)))
		return
	}
	h.remove(id)
	p.counts.quarantined.With(queueHistory).Inc()
	p.notify.Quarantine(queueHistory, attempts)
}

func (h *historian) apply(ctx context.Context, hist History, received time.Time) error {
	if err := h.batch(ctx, func(a *applier) error { return a.identities(hist, received) }); err != nil {
		return err
	}
	for _, conv := range hist.Conversations {
		if c, ok := policy.Normalize(conv.Chat); ok && c.Kind() == policy.GroupChat && (conv.Subject != "" || len(conv.Members) > 0) {
			g := Group{Chat: conv.Chat, Subject: conv.Subject, Members: conv.Members}
			if err := h.batch(ctx, func(a *applier) error { return a.group(g) }); err != nil {
				return err
			}
		}
		for start := 0; start < len(conv.Messages); start += historyBatch {
			msgs := conv.Messages[start:min(start+historyBatch, len(conv.Messages))]
			if err := h.messages(ctx, conv.Chat, msgs); err != nil {
				return err
			}
		}
	}
	return nil
}

func (h *historian) messages(ctx context.Context, chat string, msgs []Message) error {
	one := func(m Message) func(*applier) error {
		return func(a *applier) error {
			m.Chat = chat
			return a.message(m, ingest.OriginHistory)
		}
	}
	err := h.batch(ctx, func(a *applier) error {
		for _, m := range msgs {
			if err := one(m)(a); err != nil {
				return err
			}
		}
		return nil
	})
	if !errors.Is(err, ingest.ErrInvalid) {
		return err
	}
	for _, m := range msgs {
		err := h.batch(ctx, one(m))
		switch {
		case errors.Is(err, ingest.ErrInvalid):
			h.p.counts.dropped.With(dropInvalid).Inc()
		case err != nil:
			return err
		}
	}
	return nil
}

func (h *historian) batch(ctx context.Context, fn func(*applier) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !h.p.tick(ctx) {
		return errPaused
	}
	var out outcome
	err := h.p.write(context.WithoutCancel(ctx), "engine.history_ingest", func(tx *ingest.Tx) error {
		a := &applier{tx: tx, owner: h.p.owner, now: h.p.clock.Now()}
		if err := fn(a); err != nil {
			return err
		}
		out = a.out
		return nil
	})
	if err == nil {
		h.p.record(out)
	}
	return err
}

func (a *applier) identities(hist History, received time.Time) error {
	for _, m := range hist.LIDMappings {
		lid, ok := policy.Normalize(m.LID)
		pn, okPN := policy.Normalize(m.PN)
		if !ok || !okPN || lid.Kind() != policy.LIDChat || pn.Kind() != policy.PhoneChat {
			a.drop(dropInvalid)
			continue
		}
		if err := a.mapping(lid, pn, ingest.MappingHistory); err != nil {
			return err
		}
	}
	for _, c := range hist.Contacts {
		if u, ok := normalizeUser(c.User); ok && c.PushName != "" {
			if err := a.tx.SetPushName(u, c.PushName, received); err != nil {
				return err
			}
		}
	}
	return nil
}
