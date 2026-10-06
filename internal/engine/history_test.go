package engine

import (
	"bytes"
	"compress/zlib"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dortort/wawarden/internal/store/ingest"
)

type fakeDecoder struct {
	mu      sync.Mutex
	decoded map[string]History
	err     error
	inputs  [][]byte
}

func (d *fakeDecoder) Decode(blob []byte) (History, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.inputs = append(d.inputs, blob)
	if d.err != nil {
		return History{}, d.err
	}
	h, ok := d.decoded[string(blob)]
	if !ok {
		return History{}, errors.New("synthetic: undecodable blob")
	}
	return h, nil
}

type histRig struct {
	*pipeRig
	decoder *fakeDecoder
	online  atomic.Bool
	h       *historian
}

func withHistoryMax(n int64) option { return func(o *Options) { o.HistoryMaxBytes = n } }

func newHistRig(t *testing.T, options ...option) *histRig {
	t.Helper()
	r := &histRig{pipeRig: newPipeRig(t, options...), decoder: &fakeDecoder{decoded: map[string]History{}}}
	r.opts.Decoder = r.decoder
	r.h = newHistorian(r.opts, r.p, r.online.Load)
	return r
}

func (r *histRig) restart() {
	r.t.Helper()
	r.pipeRig.restart()
	r.h = newHistorian(r.opts, r.p, r.online.Load)
}

func (r *histRig) notify(ref HistoryRef) {
	r.t.Helper()
	if !r.h.accept(HistoryNotification{Sender: owner, FromMe: true, Ref: ref}) {
		r.t.Fatal("the history notification was not acknowledged")
	}
}

func (r *histRig) drainHistory() {
	r.t.Helper()
	r.h.drain(r.t.Context())
}

func (r *histRig) file(id string) string { return filepath.Join(r.opts.DataDir, "history", id+".bin") }

func deflate(t testing.TB, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw, err := zlib.NewWriterLevel(&buf, zlib.BestSpeed)
	if err != nil {
		t.Fatal(err)
	}
	for chunk := range slices.Chunk(data, 1<<20) {
		if _, err := zw.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func (r *histRig) blob(raw string, h History) []byte {
	r.decoder.decoded[raw] = h
	return deflate(r.t, []byte(raw))
}

func sampleHistory() History {
	return History{
		Conversations: []Conversation{
			{
				Chat: group, Subject: "From History", Members: []Participant{{User: alice, Admin: true}, {User: bob}},
				Messages: []Message{
					{Chat: "120363000000000009@g.us", ID: "H1", Sender: alice, Timestamp: epoch, Kind: KindText, Text: "old group message"},
					{ID: "H2", Sender: "15550100009@s.whatsapp.net", FromMe: true, Timestamp: epoch, Kind: KindText, Text: "old own message"},
				},
			},
			{Chat: carolLIDChat, Messages: []Message{{ID: "H3", Sender: carolLIDChat, Timestamp: epoch, Kind: KindText, Text: "from a LID chat"}}},
			{Chat: "status@broadcast", Messages: []Message{{ID: "H4", Sender: alice, Timestamp: epoch, Kind: KindText, Text: "status"}}},
		},
		LIDMappings: []LIDMapping{{PN: carol, LID: carolLIDChat}, {PN: "garbage", LID: carolLIDChat}},
		Contacts:    []Contact{{User: bob, PushName: "Bob Example"}},
	}
}

const carolLIDChat = "100000000000003@lid"

func TestHistoryIsPersistedAckedThenIngested(t *testing.T) {
	r := newHistRig(t)
	r.online.Store(true)
	compressed := r.blob("synthetic history one", sampleHistory())
	r.client.blobs["HS1"] = compressed
	var fileAtAck []byte
	var modeAtAck fs.FileMode
	r.client.onAck = func(HistoryRef) {
		fileAtAck, _ = os.ReadFile(filepath.Clean(r.file("HS1")))
		if fi, err := os.Stat(r.file("HS1")); err == nil {
			modeAtAck = fi.Mode()
		}
	}
	r.notify(HistoryRef{ID: "HS1", DirectPath: "/v/t62.synthetic", FileLength: uint64(len(compressed))})
	r.drainHistory()
	if !bytes.Equal(fileAtAck, compressed) || modeAtAck != 0o600 {
		t.Fatalf("at the receipt the file held %d bytes with mode %v, want the whole blob with mode 0600", len(fileAtAck), modeAtAck)
	}
	if calls := r.client.history(); !slices.Equal(calls, []string{"download:HS1", "ack:HS1"}) {
		t.Fatalf("calls %v, want the download then the receipt", calls)
	}
	if _, err := os.Stat(r.file("HS1")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the processed blob's file is still there: %v", err)
	}
	if fi, err := os.Stat(filepath.Join(r.opts.DataDir, "history")); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("history directory: %v, %v", fi, err)
	}
	if f := r.must(group, "H1", alice); f.Text != "old group message" {
		t.Fatalf("H1 %+v", f)
	}
	if f := r.must(group, "H2", owner); !f.FromMe {
		t.Fatalf("H2 %+v", f)
	}
	r.must(carolLIDChat, "H3", carolLIDChat)
	if r.dropped(dropChat) != 1 || r.dropped(dropInvalid) != 1 {
		t.Fatalf("drops: chat %v invalid %v", r.dropped(dropChat), r.dropped(dropInvalid))
	}
	db := r.inspect()
	if got := query[string](t, db, "SELECT group_concat(DISTINCT origin) FROM messages"); got != "history" {
		t.Fatalf("origins %q", got)
	}
	if got := query[string](t, db, "SELECT pn || ' ' || source FROM lid_map"); got != carol+" history" {
		t.Fatalf("lid_map %q", got)
	}
	if got := query[string](t, db, "SELECT push_name FROM contacts WHERE jid = ?", bob); got != "Bob Example" {
		t.Fatalf("contact %q", got)
	}
	if got := query[string](t, db, "SELECT name || ' ' || name_source FROM chats WHERE jid = ?", group); got != "From History group_subject" {
		t.Fatalf("group %q", got)
	}
	if got := query[string](t, db, "SELECT group_concat(user_jid || ' ' || is_admin, ',') FROM (SELECT user_jid, is_admin FROM group_participants WHERE group_jid = ? ORDER BY user_jid)", group); got != alice+" 1,"+bob+" 0" {
		t.Fatalf("group members %q, want alice as an admin and bob as a member", got)
	}
	if got := query[string](t, db, "SELECT (processed_at IS NOT NULL) || ' ' || (ref IS NULL) || ' ' || attempts FROM history_blobs"); got != "1 1 1" {
		t.Fatalf("blob row %q", got)
	}
}

func (r *histRig) applyBlob(id string, h History) {
	r.t.Helper()
	r.notify(HistoryRef{ID: id, Inline: r.blob("synthetic "+id, h)})
	r.drainHistory()
}

func groupSnapshot(subject string, members ...Participant) History {
	return History{Conversations: []Conversation{{Chat: group, Subject: subject, Members: members}}}
}

func (r *histRig) revokeM1(id, sender string) {
	r.t.Helper()
	r.ingest(change(KindRevoke, group, id, sender, Key{RemoteJID: group, Participant: alice, ID: "M1"}))
}

func TestAStaleHistoryBlobKeepsALiveDemotion(t *testing.T) {
	r := newHistRig(t)
	r.ingest(members(carol), inGroup("M1", alice, "alice secret"))
	r.ingest(Group{Chat: group, Joined: []Participant{{User: carol}}, Timestamp: epoch})
	r.revokeM1("M2", carol)
	r.applyBlob("HS1", groupSnapshot("Stale Subject", Participant{User: alice}, Participant{User: bob}, Participant{User: carol, Admin: true}))
	r.revokeM1("M3", carol)
	if f := r.must(group, "M1", alice); f.Revoked || f.Text != "alice secret" || r.dropped(dropNotAdmin) != 2 {
		t.Fatalf("M1 revoked %v with text %q, not-admin drops %v: a stale history blob restored a demoted admin", f.Revoked, f.Text, r.dropped(dropNotAdmin))
	}
	db := r.inspect()
	if got := query[string](t, db, "SELECT name FROM chats WHERE jid = ?", group); got != "Synthetic Group" {
		t.Fatalf("subject %q, want the live one", got)
	}
}

func TestAStaleHistoryBlobKeepsLiveChangesToAGroupKnownFromHistory(t *testing.T) {
	r := newHistRig(t)
	dave := "15550100004@s.whatsapp.net"
	snapshot := groupSnapshot("From History", Participant{User: alice, Admin: true}, Participant{User: bob}, Participant{User: carol, Admin: true}, Participant{User: dave, Admin: true})
	r.applyBlob("HS1", snapshot)
	r.ingest(inGroup("M1", alice, "alice secret"))
	r.ingest(Group{Chat: group, Subject: "Live Subject", Joined: []Participant{{User: carol}, {User: bob, Admin: true}}, Left: []string{dave}, Timestamp: epoch})
	r.applyBlob("HS2", snapshot)
	r.revokeM1("M2", carol)
	r.revokeM1("M3", dave)
	if f := r.must(group, "M1", alice); f.Revoked || r.dropped(dropNotAdmin) != 1 || r.dropped(dropAdminUnknown) != 1 {
		t.Fatalf("M1 revoked %v, drops: not admin %v, admin unknown %v: a stale history blob undid a live demotion or removal", f.Revoked, r.dropped(dropNotAdmin), r.dropped(dropAdminUnknown))
	}
	r.revokeM1("M4", bob)
	if f := r.must(group, "M1", alice); !f.Revoked {
		t.Fatal("a stale history blob undid a live promotion")
	}
	db := r.inspect()
	if got := query[string](t, db, "SELECT name FROM chats WHERE jid = ?", group); got != "Live Subject" {
		t.Fatalf("subject %q, want the live one", got)
	}
}

func TestALiveChangeThatArrivesAfterTheNotificationOutlivesItsBlob(t *testing.T) {
	r := newHistRig(t)
	r.applyBlob("HS1", groupSnapshot("From History", Participant{User: alice}, Participant{User: carol, Admin: true}))
	r.ingest(inGroup("M1", alice, "alice secret"))
	r.notify(HistoryRef{ID: "HS2", Inline: r.blob("synthetic HS2", groupSnapshot("From History", Participant{User: alice}, Participant{User: carol, Admin: true}))})
	r.deliver(Group{Chat: group, Joined: []Participant{{User: carol}}, Timestamp: epoch})
	r.drain()
	r.drainHistory()
	r.revokeM1("M2", carol)
	if f := r.must(group, "M1", alice); f.Revoked || r.dropped(dropNotAdmin) != 1 {
		t.Fatal("a blob announced before a live demotion and processed after it restored the admin")
	}
}

func TestHistoryKeepsLivePushNames(t *testing.T) {
	r := newHistRig(t)
	live := text(bob, "D1", bob, "hello")
	live.PushName = "Bob Live"
	r.ingest(live)
	old := text(bob, "D0", bob, "older")
	old.PushName = "Bob Old"
	r.applyBlob("HS1", History{Conversations: []Conversation{{Chat: bob, Messages: []Message{old}}}, Contacts: []Contact{{User: bob, PushName: "Bob Example"}}})
	r.must(bob, "D0", bob)
	db := r.inspect()
	if got := query[string](t, db, "SELECT k.push_name || ', ' || c.name FROM contacts k, chats c WHERE k.jid = ?1 AND c.jid = ?1", bob); got != "Bob Live, Bob Live" {
		t.Fatalf("push name and chat name %q, want the live ones", got)
	}
}

func (r *histRig) trace() *[]string {
	var steps []string
	step := func(s string) {
		if len(steps) == 0 || steps[len(steps)-1] != s {
			steps = append(steps, s)
		}
	}
	r.client.onWrite = func() { step("write") }
	r.client.onAck = func(HistoryRef) { step("ack") }
	r.h.fsyncFile = func(f *os.File) error { step("sync_file"); return f.Sync() }
	r.h.closeFile = func(f *os.File) error { step("close"); return f.Close() }
	r.h.renameFile = func(from, to string) error {
		step("rename " + filepath.Base(from) + " " + filepath.Base(to))
		return os.Rename(from, to)
	}
	r.h.fsyncDir = func(dir string) error { step("sync_dir"); return syncDir(dir) }
	return &steps
}

func incompressible(n int) string {
	b := make([]byte, n)
	_, _ = rand.NewChaCha8([32]byte{5, 8}).Read(b)
	return string(b)
}

func TestHistoryIsDurableBeforeItsReceipt(t *testing.T) {
	r := newHistRig(t)
	r.online.Store(true)
	r.client.blobs["HS1"] = r.blob(incompressible(3*4096), History{Conversations: []Conversation{{Chat: alice, Messages: []Message{{ID: "D1", Sender: alice, Timestamp: epoch, Kind: KindText}}}}})
	steps := r.trace()
	write := r.client.onWrite
	r.client.onWrite = func() {
		write()
		if _, err := os.Lstat(r.file("HS1")); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("the blob has its final name while it downloads: %v", err)
		}
	}
	r.notify(HistoryRef{ID: "HS1"})
	r.drainHistory()
	r.must(alice, "D1", alice)
	if want := []string{"write", "sync_file", "close", "rename HS1.part HS1.bin", "sync_dir", "ack", "sync_dir"}; !slices.Equal(*steps, want) {
		t.Fatalf("steps %v, want %v", *steps, want)
	}
	if names := r.historyFiles(); len(names) != 0 {
		t.Fatalf("history files after processing: %v", names)
	}
}

func (r *histRig) historyFiles() []string {
	r.t.Helper()
	entries, err := os.ReadDir(filepath.Join(r.opts.DataDir, "history"))
	if err != nil {
		r.t.Fatalf("ReadDir: %v", err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func (r *histRig) downloaded(id string) {
	r.t.Helper()
	r.client.blobs[id] = []byte("synthetic compressed " + id)
	r.notify(HistoryRef{ID: id})
	if _, err := r.h.load(r.t.Context(), id, HistoryRef{ID: id}); err != nil {
		r.t.Fatalf("download %s: %v", id, err)
	}
}

func TestCleanKeepsOnlyTheFilesOfPendingBlobs(t *testing.T) {
	r := newHistRig(t)
	r.online.Store(true)
	for _, id := range []string{"HS1", "HS2", "HS3"} {
		r.downloaded(id)
	}
	r.write(func(tx *ingest.Tx) error {
		if err := tx.MarkBlobProcessed("HS1", epoch); err != nil {
			return err
		}
		return tx.QuarantineBlob("HS2")
	})
	for _, name := range []string{"HS3.part", "HS9.part", "bad id!.bin", "notes.txt"} {
		if err := os.WriteFile(filepath.Join(r.opts.DataDir, "history", name), []byte("synthetic"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	steps := r.trace()
	r.h.clean(t.Context())
	if names := r.historyFiles(); !slices.Equal(names, []string{"HS3.bin", "HS3.part", "notes.txt"}) {
		t.Fatalf("history files %v, want only the pending blob's and the unrelated file", names)
	}
	if !slices.Equal(*steps, []string{"sync_dir"}) {
		t.Fatalf("steps %v, want one directory sync", *steps)
	}
}

func TestAStopDuringThePendingBlobsReadIsNotLoggedAsAFailure(t *testing.T) {
	r := newHistRig(t)
	r.online.Store(true)
	r.client.blobs["HS1"] = r.blob("synthetic left for the next start", History{})
	r.notify(HistoryRef{ID: "HS1"})
	ctx, stop := context.WithCancel(t.Context())
	r.p.clock = stoppingClock{Clock: r.clock, stop: stop}
	r.h.drain(ctx)
	if e := r.logs.events("ingest_failed"); len(e) != 0 {
		t.Fatalf("the stop was logged as %v", e)
	}
	if n := r.client.count("download:HS1"); n != 0 {
		t.Fatalf("%d downloads after the stop", n)
	}
}

func TestAStopDuringCleanKeepsTheFilesWithoutAWarning(t *testing.T) {
	r := newHistRig(t)
	dir := filepath.Join(r.opts.DataDir, "history")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "HS9.bin"), []byte("synthetic"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(t.Context())
	stop()
	r.h.clean(ctx)
	if e := r.logs.events("history_file_kept"); len(e) != 0 {
		t.Fatalf("the stop was logged as %v", e)
	}
	if names := r.historyFiles(); !slices.Equal(names, []string{"HS9.bin"}) {
		t.Fatalf("history files %v, want the file kept for the next start", names)
	}
}

func TestQuarantineDeletesTheDownloadedBlob(t *testing.T) {
	r := newHistRig(t)
	r.online.Store(true)
	r.client.blobs["HS1"] = r.blob("synthetic undecodable", History{})
	r.decoder.err = errors.New("synthetic: malformed history")
	r.notify(HistoryRef{ID: "HS1"})
	r.drainHistory()
	if r.counter("wawarden_ingest_quarantined_total", "queue", "history") != 1 || r.client.count("download:HS1") != 1 {
		t.Fatalf("calls %v: want one download and a quarantine", r.client.history())
	}
	if names := r.historyFiles(); len(names) != 0 {
		t.Fatalf("history files after the quarantine: %v", names)
	}
}

func TestADownloadCutShortIsDownloadedAgain(t *testing.T) {
	r := newHistRig(t)
	r.online.Store(true)
	compressed := r.blob(incompressible(3*4096), History{Conversations: []Conversation{{Chat: alice, Messages: []Message{{ID: "C1", Sender: alice, Timestamp: epoch, Kind: KindText}}}}})
	r.client.blobs["HS1"] = compressed
	writes := 0
	r.client.onWrite = func() {
		if writes++; writes == 2 {
			runtime.Goexit()
		}
	}
	r.notify(HistoryRef{ID: "HS1"})
	crashes(t, r.drainHistory)
	if len(r.client.acks) != 0 {
		t.Fatal("a receipt was sent for a blob cut short")
	}
	r.restart()
	steps := r.trace()
	var atAck []byte
	r.client.onAck = func(HistoryRef) {
		atAck, _ = os.ReadFile(filepath.Clean(r.file("HS1")))
		(*steps) = append(*steps, "ack")
	}
	r.drainHistory()
	if r.client.count("download:HS1") != 2 || len(r.client.acks) != 1 || !bytes.Equal(atAck, compressed) {
		t.Fatalf("calls %v; the receipt saw %d of %d bytes", r.client.history(), len(atAck), len(compressed))
	}
	if i := slices.Index(*steps, "ack"); i < 1 || (*steps)[i-1] != "sync_dir" || !slices.Contains((*steps)[:i], "rename HS1.part HS1.bin") {
		t.Fatalf("steps %v, want the receipt only after the rename", *steps)
	}
	r.must(alice, "C1", alice)
}

func TestHistoryFromAnotherDeviceIsDropped(t *testing.T) {
	r := newHistRig(t)
	notifications := []HistoryNotification{
		{Sender: owner, FromMe: false, Ref: HistoryRef{ID: "HS3"}},
		{Sender: "15550100009@bot", FromMe: true, Ref: HistoryRef{ID: "HS4"}},
	}
	for i, device := range []string{"1", "2", "4", "5", "99"} {
		for j, sender := range []string{"15550100009:%s@s.whatsapp.net", "15550100009.0:%s@s.whatsapp.net", "100000000000009:%s@lid"} {
			ref := HistoryRef{ID: fmt.Sprintf("HC%d%d", i, j), Inline: []byte("x")}
			notifications = append(notifications, HistoryNotification{Sender: fmt.Sprintf(sender, device), FromMe: true, Ref: ref})
		}
	}
	for _, n := range notifications {
		if !r.h.accept(n) {
			t.Fatal("a dropped notification was refused instead of acknowledged")
		}
	}
	if r.dropped(dropNotPrimary) != float64(len(notifications)) {
		t.Fatalf("history drops %v", r.dropped(dropNotPrimary))
	}
	r.notify(HistoryRef{ID: "bad id!"})
	if r.dropped(dropInvalid) != 1 {
		t.Fatal("a notification with an unusable id was not dropped")
	}
	if r.h.accept(HistoryNotification{Sender: "15550100009:0@s.whatsapp.net", FromMe: true, Ref: HistoryRef{ID: "HS5", Inline: []byte("x")}}) != true {
		t.Fatal("a notification from device 0 was refused")
	}
	db := r.inspect()
	if got := query[string](t, db, "SELECT group_concat(id) FROM history_blobs"); got != "HS5" {
		t.Fatalf("recorded blobs %q, want only the primary device's", got)
	}
}

func TestInlineBootstrapPayloadNeedsNoDownload(t *testing.T) {
	r := newHistRig(t)
	compressed := r.blob("synthetic inline bootstrap", History{Conversations: []Conversation{{Chat: alice, Messages: []Message{{ID: "B1", Sender: alice, Timestamp: epoch, Kind: KindText, Text: "bootstrap"}}}}})
	r.notify(HistoryRef{ID: "HS1", Inline: compressed})
	r.drainHistory()
	r.must(alice, "B1", alice)
	if calls := r.client.history(); !slices.Equal(calls, []string{"ack:HS1"}) {
		t.Fatalf("calls %v, want only the receipt", calls)
	}
}

func TestHistoryDownloadsWaitForAConnection(t *testing.T) {
	r := newHistRig(t)
	r.client.blobs["HS1"] = r.blob("synthetic waits", History{Conversations: []Conversation{{Chat: alice, Messages: []Message{{ID: "W1", Sender: alice, Timestamp: epoch, Kind: KindText}}}}})
	r.notify(HistoryRef{ID: "HS1"})
	r.drainHistory()
	if len(r.client.history()) != 0 {
		t.Fatalf("calls %v while disconnected", r.client.history())
	}
	r.online.Store(true)
	r.drainHistory()
	r.must(alice, "W1", alice)
	db := r.inspect()
	if got := query[int](t, db, "SELECT attempts FROM history_blobs"); got != 1 {
		t.Fatalf("attempts %d: waiting for a connection used an attempt", got)
	}
}

func TestDownloadIsCapped(t *testing.T) {
	r := newHistRig(t, withHistoryMax(1<<20))
	r.online.Store(true)
	r.client.blobs["HS1"] = bytes.Repeat([]byte{0xA5}, 1<<20+1)
	r.notify(HistoryRef{ID: "HS1"})
	r.notify(HistoryRef{ID: "HS2", FileLength: 1<<20 + 1})
	r.drainHistory()
	if r.client.count("download:HS1") != 3 || r.client.count("download:HS2") != 0 || len(r.client.acks) != 0 {
		t.Fatalf("calls %v: want three capped downloads of HS1 and none of HS2, and no receipt", r.client.history())
	}
	entries, err := os.ReadDir(filepath.Join(r.opts.DataDir, "history"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("history directory holds %v, %v", entries, err)
	}
	if r.counter("wawarden_ingest_quarantined_total", "queue", "history") != 2 {
		t.Fatal("the oversized blobs were not quarantined")
	}
}

const (
	historyMemoryFactor = 2
	historyMemorySlack  = 1 << 20
)

func allocated(t *testing.T, fn func()) uint64 {
	t.Helper()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	fn()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

func TestHistoryMemoryBoundIsTheDocumentedOne(t *testing.T) {
	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "configuration.md"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	want := fmt.Sprintf("at most %d times `WAWARDEN_HISTORY_MAX_BYTES` plus %d MiB", historyMemoryFactor, historyMemorySlack>>20)
	if !strings.Contains(strings.Join(strings.Fields(string(doc)), " "), want) {
		t.Fatalf("docs/configuration.md does not state %q", want)
	}
}

func TestReadingAndInflatingABlobStaysWithinTheMemoryBound(t *testing.T) {
	const limit = 8 << 20
	raw := []byte(incompressible(limit - 4096))
	path := filepath.Join(t.TempDir(), "HS1.bin")
	if err := os.WriteFile(path, deflate(t, raw), 0o600); err != nil {
		t.Fatal(err)
	}
	var out []byte
	alloc := allocated(t, func() {
		data, err := readCapped(path, limit)
		if err == nil {
			out, err = inflate(data, limit)
		}
		if err != nil {
			t.Errorf("read and inflate: %v", err)
		}
	})
	if !bytes.Equal(out, raw) {
		t.Fatal("the blob did not round-trip")
	}
	if alloc > historyMemoryFactor*limit+historyMemorySlack {
		t.Fatalf("reading and inflating a blob at an 8 MiB cap allocated %d KiB, over the documented bound", alloc>>10)
	}
}

func TestReadCappedRefusesAFileOverTheCap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "HS1.bin")
	if err := os.WriteFile(path, make([]byte, 101), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readCapped(path, 100); !errors.Is(err, errTooLarge) {
		t.Fatalf("a file one byte over the cap = %v, want errTooLarge", err)
	}
	if data, err := readCapped(path, 101); err != nil || len(data) != 101 {
		t.Fatalf("a file at the cap = %d bytes, %v", len(data), err)
	}
}

func TestZipBombIsRefusedWithBoundedMemory(t *testing.T) {
	const limit = 8 << 20
	bomb := deflate(t, make([]byte, 64<<20))
	var err error
	alloc := allocated(t, func() { _, err = inflate(bomb, limit) })
	if !errors.Is(err, errTooLarge) {
		t.Fatalf("inflate = %v, want errTooLarge", err)
	}
	if alloc > historyMemorySlack {
		t.Fatalf("refusing a bomb at an 8 MiB cap allocated %d KiB", alloc>>10)
	}

	r := newHistRig(t, withHistoryMax(limit))
	r.notify(HistoryRef{ID: "BOMB", Inline: bomb})
	r.drainHistory()
	if a := r.alerts("quarantine"); len(a) != 1 || a[0]["queue"] != "history" || a[0]["attempts"] != float64(3) {
		t.Fatalf("quarantine alerts %v", a)
	}
	if len(r.decoder.inputs) != 0 {
		t.Fatal("the bomb reached the decoder")
	}
	db := r.inspect()
	if got := query[string](t, db, "SELECT attempts || ' ' || quarantined || ' ' || (ref IS NULL) FROM history_blobs"); got != "3 1 1" {
		t.Fatalf("blob row %q", got)
	}
}

func stored(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw, err := zlib.NewWriterLevel(&buf, zlib.NoCompression)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := zw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestAnInlinePayloadOverTheCapIsRefusedBeforeItIsInflated(t *testing.T) {
	const limit = 1024
	r := newHistRig(t, withHistoryMax(limit))
	one := func(id string) History {
		return History{Conversations: []Conversation{{Chat: alice, Messages: []Message{{ID: id, Sender: alice, Timestamp: epoch, Kind: KindText, Text: "inline"}}}}}
	}
	const overhead = 13
	over, fit := strings.Repeat("o", limit+1-overhead), strings.Repeat("f", limit-overhead)
	r.decoder.decoded[over], r.decoder.decoded[fit] = one("O1"), one("F1")
	overBlob, fitBlob := stored(t, []byte(over)), stored(t, []byte(fit))
	if len(overBlob) != limit+1 || len(fitBlob) != limit {
		t.Fatalf("stored streams of %d and %d bytes, want %d and %d", len(overBlob), len(fitBlob), limit+1, limit)
	}
	r.notify(HistoryRef{ID: "HS1", Inline: overBlob})
	r.notify(HistoryRef{ID: "HS2", Inline: fitBlob})
	r.drainHistory()
	if a := r.alerts("quarantine"); len(a) != 1 || a[0]["attempts"] != float64(3) {
		t.Fatalf("quarantine alerts %v", a)
	}
	if len(r.decoder.inputs) != 1 || string(r.decoder.inputs[0]) != fit {
		t.Fatalf("the decoder saw %d inputs, want only the payload at the cap", len(r.decoder.inputs))
	}
	r.must(alice, "F1", alice)
	if _, ok := r.find(alice, "O1", alice); ok {
		t.Fatal("the payload over the cap was applied")
	}
	if calls := r.client.history(); !slices.Equal(calls, []string{"ack:HS2"}) {
		t.Fatalf("calls %v, want a receipt only for the payload at the cap", calls)
	}
}

func TestASwappedHistoryMappingIsDroppedAndTheBlobStillApplies(t *testing.T) {
	r := newHistRig(t)
	r.notify(HistoryRef{ID: "HS1", Inline: r.blob("synthetic swapped mapping", History{
		Conversations: []Conversation{{Chat: alice, Messages: []Message{{ID: "L1", Sender: alice, Timestamp: epoch, Kind: KindText, Text: "kept"}}}},
		LIDMappings:   []LIDMapping{{PN: aliceLID, LID: alice}},
	})})
	r.drainHistory()
	r.must(alice, "L1", alice)
	if r.dropped(dropInvalid) != 1 || len(r.alerts("quarantine")) != 0 {
		t.Fatalf("invalid drops %v, quarantine alerts %v", r.dropped(dropInvalid), r.alerts("quarantine"))
	}
	db := r.inspect()
	if got := query[string](t, db, "SELECT (SELECT count(*) FROM lid_map) || ' ' || (SELECT processed_at IS NOT NULL FROM history_blobs)"); got != "0 1" {
		t.Fatalf("lid_map rows and processed blob = %q", got)
	}
}

func TestInflateBoundaryAndDamage(t *testing.T) {
	data := bytes.Repeat([]byte("history "), 125)
	z := deflate(t, data)
	if got, err := inflate(z, int64(len(data))); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("exactly at the cap: %d bytes, %v", len(got), err)
	}
	if _, err := inflate(z, int64(len(data))-1); !errors.Is(err, errTooLarge) {
		t.Fatalf("one byte over the cap: %v", err)
	}
	for name, in := range map[string][]byte{
		"empty":     nil,
		"truncated": z[:len(z)-6],
		"bad sum":   append(slices.Clone(z[:len(z)-1]), z[len(z)-1]^0xff),
		"not zlib":  []byte("plain protobuf bytes"),
	} {
		if _, err := inflate(in, 1<<20); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	w := &cappedWriter{w: &bytes.Buffer{}, limit: 10}
	if _, err := w.Write(make([]byte, 10)); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte{1}); !errors.Is(err, errTooLarge) {
		t.Fatalf("a write past the cap: %v", err)
	}
}

func FuzzInflate(f *testing.F) {
	f.Add(deflate(f, []byte("history")), uint16(16))
	f.Add(deflate(f, make([]byte, 4096)), uint16(100))
	f.Add([]byte("not zlib"), uint16(1))
	f.Fuzz(func(t *testing.T, in []byte, limit uint16) {
		out, err := inflate(in, int64(limit)+1)
		if err != nil {
			return
		}
		if int64(len(out)) > int64(limit)+1 {
			t.Fatalf("inflate returned %d bytes over a cap of %d", len(out), int64(limit)+1)
		}
		zr, err := zlib.NewReader(bytes.NewReader(in))
		if err != nil {
			t.Fatalf("an accepted input is not zlib: %v", err)
		}
		full := new(bytes.Buffer)
		if _, err := full.ReadFrom(zr); err != nil || !bytes.Equal(full.Bytes(), out) {
			t.Fatalf("inflate disagrees with an uncapped read: %v", err)
		}
	})
}

func TestUnprocessedBlobsReplayAtStartup(t *testing.T) {
	r := newHistRig(t)
	r.online.Store(true)
	compressed := r.blob("synthetic replay", History{Conversations: []Conversation{{Chat: alice, Messages: []Message{{ID: "R1", Sender: alice, Timestamp: epoch, Kind: KindText}}}}})
	r.client.blobs["HS1"] = compressed
	r.notify(HistoryRef{ID: "HS1"})
	if _, err := r.h.load(t.Context(), "HS1", HistoryRef{ID: "HS1"}); err != nil {
		t.Fatalf("download: %v", err)
	}
	r.online.Store(false)
	r.restart()
	r.drainHistory()
	r.must(alice, "R1", alice)
	if r.client.count("download:HS1") != 1 || r.client.count("ack:HS1") != 2 {
		t.Fatalf("calls %v: want one download, and a receipt in each process", r.client.history())
	}
}

func TestBlobQuarantineAfterThreeFailures(t *testing.T) {
	r := newHistRig(t)
	compressed := r.blob("synthetic poison", History{})
	r.decoder.err = errors.New("synthetic: malformed history")
	r.notify(HistoryRef{ID: "HS1", Inline: compressed})
	r.notify(HistoryRef{ID: "HS2", Inline: []byte("not zlib at all")})
	r.drainHistory()
	if r.counter("wawarden_ingest_quarantined_total", "queue", "history") != 2 {
		t.Fatal("the failing blobs were not quarantined")
	}
	if n := len(r.logs.events("ingest_failed")); n != 6 {
		t.Fatalf("%d ingest_failed events, want three per blob", n)
	}
	r.restart()
	r.drainHistory()
	db := r.inspect()
	if got := query[string](t, db, "SELECT group_concat(attempts || quarantined, ',') FROM history_blobs"); got != "31,31" {
		t.Fatalf("blob rows %q", got)
	}
}

type tickingClock struct{ *fakeClock }

func (c tickingClock) Now() time.Time {
	c.advance(spaceInterval)
	return c.fakeClock.Now()
}

type crashingDecoder struct{}

func (crashingDecoder) Decode([]byte) (History, error) {
	runtime.Goexit()
	return History{}, nil
}

func TestAStopGivesItsHistoryAttemptBackAndACrashDoesNot(t *testing.T) {
	r := newHistRig(t)
	msgs := make([]Message, 450)
	for i := range msgs {
		msgs[i] = Message{ID: fmt.Sprintf("S%03d", i), Sender: alice, Timestamp: epoch, Kind: KindText, Text: "history"}
	}
	r.notify(HistoryRef{ID: "HS1", Inline: r.blob("synthetic stopped blob", History{Conversations: []Conversation{{Chat: alice, Messages: msgs}}})})
	r.p.clock = tickingClock{r.clock}
	blob := func() string {
		var pending []ingest.Blob
		if err := r.archive.Read(t.Context(), "test.pending", func(rd *ingest.Reader) error {
			var err error
			pending, err = rd.PendingBlobs(pendingWindow)
			return err
		}); err != nil {
			t.Fatalf("Read: %v", err)
		}
		if len(pending) != 1 {
			return "not pending"
		}
		return fmt.Sprintf("attempts %d", pending[0].Attempts)
	}
	for stop := range 3 {
		ctx, cancel := context.WithCancel(t.Context())
		checks := 0
		r.p.space = func() (ingest.Space, error) {
			if checks++; checks == 4 {
				cancel()
			}
			return ingest.Space{}, nil
		}
		r.h.drain(ctx)
		cancel()
		if got := blob(); got != "attempts 0" {
			t.Fatalf("after stop %d the blob is %s, want pending with attempts 0", stop+1, got)
		}
	}
	if _, ok := r.find(alice, "S199", alice); !ok {
		t.Fatal("the batches committed before the stop are not stored")
	}
	if _, ok := r.find(alice, "S200", alice); ok {
		t.Fatal("a batch after the stop was applied")
	}
	r.h.decoder = crashingDecoder{}
	crashes(t, r.drainHistory)
	if got := blob(); got != "attempts 1" {
		t.Fatalf("after a crash the blob is %s, want pending with attempts 1", got)
	}
	r.h.decoder = r.decoder
	r.drainHistory()
	for _, m := range msgs {
		r.must(alice, m.ID, alice)
	}
	if len(r.alerts("quarantine")) != 0 {
		t.Fatalf("quarantine alerts %v", r.alerts("quarantine"))
	}
	db := r.inspect()
	if got := query[string](t, db, "SELECT attempts || ' ' || quarantined || ' ' || (processed_at IS NOT NULL) FROM history_blobs"); got != "2 0 1" {
		t.Fatalf("blob row %q, want processed after the crash's attempt and its own", got)
	}
}

func TestHistoryBatchesSkipInvalidRows(t *testing.T) {
	r := newHistRig(t)
	msgs := []Message{
		{ID: "ok-1", Sender: alice, Timestamp: epoch, Kind: KindText, Text: "kept"},
		{ID: "not valid", Sender: alice, Timestamp: epoch, Kind: KindText, Text: "skipped"},
		{ID: "ok-2", Sender: alice, Timestamp: epoch, Kind: KindText, Text: "kept"},
	}
	r.notify(HistoryRef{ID: "HS1", Inline: r.blob("synthetic batch", History{Conversations: []Conversation{{Chat: alice, Messages: msgs}}})})
	r.drainHistory()
	r.must(alice, "ok-1", alice)
	r.must(alice, "ok-2", alice)
	if r.dropped(dropInvalid) != 1 || r.counter("wawarden_messages_ingested_total") != 2 {
		t.Fatalf("invalid drops %v, ingested %v", r.dropped(dropInvalid), r.counter("wawarden_messages_ingested_total"))
	}
}

func TestAFailedWriteRefusesSoWhatsAppRedelivers(t *testing.T) {
	r := newHistRig(t)
	if err := r.archive.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if r.p.accept(dm("M1", alice, "never written")) {
		t.Fatal("a message was acknowledged although the inbox write failed")
	}
	if r.h.accept(HistoryNotification{Sender: owner, FromMe: true, Ref: HistoryRef{ID: "HS1", Inline: []byte("x")}}) {
		t.Fatal("a history notification was acknowledged although its record failed")
	}
	if r.counter("wawarden_ingest_refused_total", "reason", "store_error") != 2 {
		t.Fatalf("store_error refusals %v, want 2", r.counter("wawarden_ingest_refused_total", "reason", "store_error"))
	}
}

func TestHistoryWaitsWhileIngestIsPaused(t *testing.T) {
	r := newHistRig(t)
	r.online.Store(true)
	one := func(id string) History {
		return History{Conversations: []Conversation{{Chat: alice, Messages: []Message{{ID: id, Sender: alice, Timestamp: epoch, Kind: KindText, Text: "history"}}}}}
	}
	r.client.blobs["HS2"] = r.blob("synthetic download", one("P2"))
	r.notify(HistoryRef{ID: "HS1", Inline: r.blob("synthetic inline", one("P1"))})
	r.notify(HistoryRef{ID: "HS2"})
	var freed atomic.Bool
	r.p.space = func() (ingest.Space, error) { return ingest.Space{Free: 1, Floor: 2, Paused: !freed.Load()}, nil }
	r.drainHistory()
	if calls := r.client.history(); len(calls) != 0 {
		t.Fatalf("calls %v while ingest is paused", calls)
	}
	var pending []ingest.Blob
	if err := r.archive.Read(t.Context(), "test.pending", func(rd *ingest.Reader) error {
		var err error
		pending, err = rd.PendingBlobs(pendingWindow)
		return err
	}); err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(pending) != 2 || pending[0].Attempts != 0 || pending[1].Attempts != 0 {
		t.Fatalf("pending blobs %+v, want both without an attempt", pending)
	}
	if _, ok := r.find(alice, "P1", alice); ok {
		t.Fatal("history was applied while ingest is paused")
	}
	freed.Store(true)
	r.clock.advance(spaceInterval)
	r.drainHistory()
	r.must(alice, "P1", alice)
	r.must(alice, "P2", alice)
}

func TestHistoryRecordingRespectsThePause(t *testing.T) {
	r := newHistRig(t)
	r.p.paused.Store(true)
	if r.h.accept(HistoryNotification{Sender: owner, FromMe: true, Ref: HistoryRef{ID: "HS1", Inline: []byte("x")}}) {
		t.Fatal("a history notification was acknowledged while ingest is paused")
	}
	if r.counter("wawarden_ingest_refused_total", "reason", "paused") != 1 {
		t.Fatal("the refusal was not counted")
	}
}
