package engine

import (
	"bytes"
	"compress/zlib"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
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
	if got := query[string](t, db, "SELECT (processed_at IS NOT NULL) || ' ' || (ref IS NULL) || ' ' || attempts FROM history_blobs"); got != "1 1 1" {
		t.Fatalf("blob row %q", got)
	}
}

func TestHistoryFromAnotherDeviceIsDropped(t *testing.T) {
	r := newHistRig(t)
	for _, n := range []HistoryNotification{
		{Sender: "15550100009:5@s.whatsapp.net", FromMe: true, Ref: HistoryRef{ID: "HS1"}},
		{Sender: "15550100009.0:5@s.whatsapp.net", FromMe: true, Ref: HistoryRef{ID: "HS2"}},
		{Sender: owner, FromMe: false, Ref: HistoryRef{ID: "HS3"}},
		{Sender: "15550100009@bot", FromMe: true, Ref: HistoryRef{ID: "HS4"}},
	} {
		if !r.h.accept(n) {
			t.Fatal("a dropped notification was refused instead of acknowledged")
		}
	}
	if r.dropped(dropNotPrimary) != 4 {
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

func TestZipBombIsRefusedWithBoundedMemory(t *testing.T) {
	const limit = 8 << 20
	bomb := deflate(t, make([]byte, 64<<20))
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	_, err := inflate(bomb, limit)
	runtime.ReadMemStats(&after)
	if !errors.Is(err, errTooLarge) {
		t.Fatalf("inflate = %v, want errTooLarge", err)
	}
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 48<<20 {
		t.Fatalf("inflating with an 8 MiB cap allocated %d MiB", alloc>>20)
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
