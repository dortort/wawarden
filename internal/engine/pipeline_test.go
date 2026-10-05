package engine

import (
	"bytes"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dortort/wawarden/internal/store/ingest"
)

func TestPayloadRoundTrip(t *testing.T) {
	for _, ev := range []Event{
		Message{Chat: group, ID: "M1", Sender: alice, SenderAlt: aliceLID, Addressing: "pn", FromMe: true, Timestamp: epoch, PushName: "Alice Example",
			Kind: KindEdit, Text: "body", MediaType: "image/jpeg", Target: &Key{RemoteJID: group, FromMe: true, ID: "M0", Participant: bob},
			Reply: &Reply{ID: "M0", Participant: bob, RemoteJID: group, Text: "quoted"}, Expiration: time.Hour},
		Group{Chat: group, Subject: "Synthetic Group", Members: []Participant{{User: alice, Admin: true}}, Joined: []Participant{{User: bob}}, Left: []string{carol}, Timestamp: epoch},
	} {
		b, err := encodePayload(ev)
		if err != nil {
			t.Fatalf("encode %T: %v", ev, err)
		}
		got, err := decodePayload(b)
		if err != nil || !reflect.DeepEqual(got, ev) {
			t.Fatalf("round trip of %T = %#v, %v", ev, got, err)
		}
	}
	if _, err := encodePayload(Connected{}); err == nil {
		t.Fatal("a connection event was encoded as an inbox payload")
	}
}

func TestPayloadKeepsMarkupAsIs(t *testing.T) {
	if b := mustPayload(t, dm("M1", alice, "<b>&amp;</b>")); !bytes.Contains(b, []byte(`"text":"<b>&amp;</b>"`)) {
		t.Fatalf("the payload escapes markup, which multiplies its size: %s", b)
	}
}

const messageMemoryBound = 48 << 20

func TestMessageMemoryBoundIsTheDocumentedOne(t *testing.T) {
	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "configuration.md"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	flat := strings.Join(strings.Fields(string(doc)), " ")
	for _, want := range []string{
		fmt.Sprintf("add up to more than %d KiB", maxMessageBytes>>10),
		fmt.Sprintf("allocates at most %d MiB", messageMemoryBound>>20),
	} {
		if !strings.Contains(flat, want) {
			t.Errorf("docs/configuration.md does not state %q", want)
		}
	}
}

func TestAMessageAtTheCapStaysWithinTheMemoryBound(t *testing.T) {
	for _, unit := range []string{"a", "<", "\x01", "\xff", " "} {
		r := newPipeRig(t)
		m := dm("M1", alice, "")
		m.Text = strings.Repeat(unit, (maxMessageBytes-m.size())/len(unit))
		alloc := allocated(t, func() {
			if !r.p.accept(m) {
				t.Errorf("%q: a message at the cap was refused", unit)
			}
			r.drain()
		})
		r.must(alice, "M1", alice)
		if alloc > messageMemoryBound {
			t.Errorf("%q: accepting and applying a message at the cap allocated %d KiB, over the documented bound", unit, alloc>>10)
		}
	}
}

func TestAMessageOverTheCapIsDropped(t *testing.T) {
	r := newPipeRig(t)
	long := strings.Repeat("x", maxMessageBytes)
	quoting := dm("M1", alice, "short")
	quoting.Reply = &Reply{ID: "M0", Text: long}
	named := dm("M2", alice, "short")
	named.PushName = long
	for _, m := range []Message{dm("M3", alice, long), quoting, named} {
		if !r.p.accept(m) {
			t.Fatal("a message over the cap was refused instead of dropped")
		}
	}
	if r.dropped(dropTooLarge) != 3 {
		t.Fatalf("too_large drops %v, want 3", r.dropped(dropTooLarge))
	}
	db := r.inspect()
	if n := query[int](t, db, "SELECT count(*) FROM inbox"); n != 0 {
		t.Fatalf("%d inbox rows for messages over the cap", n)
	}
}

func TestPayloadDecodingIsStrict(t *testing.T) {
	for _, in := range []string{
		``, `{}`, `{"v":1}`, `{"v":2,"message":{"chat":"x"}}`, `{"v":1,"message":{},"group":{}}`,
		`{"v":1,"message":{"chat":"x"}} {}`, `{"v":1,"message":{"chat":"x","unknown":1}}`, `{"v":1,"other":{}}`, `[1]`, "\xff",
	} {
		if _, err := decodePayload([]byte(in)); err == nil {
			t.Errorf("decodePayload(%q) accepted", in)
		}
	}
}

func FuzzDecodePayload(f *testing.F) {
	for _, ev := range []Event{text(alice, "M1", alice, "body"), Group{Chat: group, Timestamp: epoch}} {
		b, err := encodePayload(ev)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		ev, err := decodePayload(b)
		if err != nil {
			return
		}
		again, err := encodePayload(ev)
		if err != nil {
			t.Fatalf("a decoded payload does not encode: %v", err)
		}
		back, err := decodePayload(again)
		if err != nil {
			t.Fatalf("a re-encoded payload does not decode: %v", err)
		}
		if third, err := encodePayload(back); err != nil || !bytes.Equal(third, again) {
			t.Fatalf("re-encoding is not stable: %q then %q, %v", again, third, err)
		}
	})
}

func (r *pipeRig) appendRaw(payload []byte) {
	r.t.Helper()
	r.write(func(tx *ingest.Tx) error {
		_, err := tx.AppendInbox(payload, epoch)
		return err
	})
}

func (r *pipeRig) refused(reason string) float64 {
	return r.counter("wawarden_ingest_refused_total", "reason", reason)
}

func TestBacklogBoundRefusesSoWhatsAppRedelivers(t *testing.T) {
	r := newPipeRig(t)
	r.bulkInbox(ingest.MaxInboxBacklog - 1)
	if !r.p.accept(dm("M0", alice, "the last one that fits")) {
		t.Fatal("a message within the backlog was refused")
	}
	if r.p.accept(dm("M1", alice, "one too many")) {
		t.Fatal("a message beyond the backlog was acknowledged")
	}
	if r.refused(refusedBacklog) != 1 {
		t.Fatal("the refusal was not counted")
	}
	if !r.p.accept(text("status@broadcast", "S1", alice, "dropped, not refused")) {
		t.Fatal("a dropped chat was refused instead of acknowledged")
	}
}

func TestPausedIngestRefuses(t *testing.T) {
	r := newPipeRig(t)
	r.ingestOpts.MinFreeBytes = math.MaxUint64
	r.reopen()
	r.p.checkSpace()
	if a := r.alerts("ingest_paused"); len(a) != 1 || a[0]["floor_bytes"] != float64(math.MaxUint64) {
		t.Fatalf("ingest_paused alerts %v", a)
	}
	if r.p.accept(dm("M1", alice, "while paused")) || r.refused(refusedPaused) != 1 {
		t.Fatal("a message was acknowledged while ingest is paused")
	}
	r.appendRaw(mustPayload(t, dm("M2", alice, "already in the inbox")))
	r.drain()
	if _, ok := r.find(alice, "M2", alice); ok {
		t.Fatal("the worker applied the inbox while ingest is paused")
	}
	r.ingestOpts.MinFreeBytes = 1
	r.reopen()
	r.p.checkSpace()
	r.ingest(dm("M3", alice, "resumed"))
	r.must(alice, "M2", alice)
}

func mustPayload(t *testing.T, ev Event) []byte {
	t.Helper()
	b, err := encodePayload(ev)
	if err != nil {
		t.Fatalf("encodePayload: %v", err)
	}
	return b
}

func TestPoisonRowIsQuarantinedAfterThreeAttempts(t *testing.T) {
	r := newPipeRig(t)
	r.appendRaw([]byte(`{"v":1,"message":{"chat":"` + alice + `","ts":"not a time"}}`))
	r.ingest(dm("M1", alice, "after the poison"))
	if !slices.Equal(r.clock.slept(), []time.Duration{time.Second, 2 * time.Second}) {
		t.Fatalf("waits %v, want 1s and 2s between attempts", r.clock.slept())
	}
	if a := r.alerts("quarantine"); len(a) != 1 || a[0]["queue"] != "inbox" || a[0]["attempts"] != float64(3) {
		t.Fatalf("quarantine alerts %v", a)
	}
	if r.counter("wawarden_ingest_quarantined_total", "queue", "inbox") != 1 {
		t.Fatal("the quarantine was not counted")
	}
	if len(r.logs.events("ingest_failed")) != 3 {
		t.Fatalf("ingest_failed events: %s", r.logs)
	}
	r.must(alice, "M1", alice)
	db := r.inspect()
	if got := query[string](t, db, "SELECT attempts || ' ' || quarantined || ' ' || (payload IS NULL) FROM inbox"); got != "3 1 1" {
		t.Fatalf("quarantined row = %q, want three attempts, quarantined and no payload", got)
	}
}

func TestInvalidContentIsDroppedWithoutRetries(t *testing.T) {
	r := newPipeRig(t)
	r.ingest(dm("an id with spaces", alice, "invalid id"), Message{Chat: alice, ID: "M2", Sender: alice, Kind: KindText, Text: "no time"})
	if r.dropped(dropInvalid) != 2 || len(r.clock.slept()) != 0 {
		t.Fatalf("invalid drops %v, waits %v", r.dropped(dropInvalid), r.clock.slept())
	}
	db := r.inspect()
	if n := query[int](t, db, "SELECT (SELECT count(*) FROM inbox) + (SELECT count(*) FROM messages) + (SELECT count(*) FROM chats)"); n != 0 {
		t.Fatalf("%d rows left behind by invalid content", n)
	}
}

func (r *pipeRig) crashBeforeApply() {
	r.t.Helper()
	r.p.beforeApply = func(int64) { runtime.Goexit() }
	crashes(r.t, r.drain)
}

func TestAPanicWhileApplyingIsAFailedAttempt(t *testing.T) {
	r := newPipeRig(t)
	r.reportPanics()
	canary := "canary panic value 15550100001@s.whatsapp.net"
	r.deliver(dm("M1", alice, "panics on every attempt"), dm("M2", alice, "applied after the quarantine"))
	var poison int64
	r.p.beforeApply = func(seq int64) {
		if poison == 0 {
			poison = seq
		}
		if seq == poison {
			panic(canary)
		}
	}
	r.drain()
	r.must(alice, "M2", alice)
	if _, ok := r.find(alice, "M1", alice); ok {
		t.Fatal("the row that panicked was applied")
	}
	if !slices.Equal(r.clock.slept(), []time.Duration{time.Second, 2 * time.Second}) {
		t.Fatalf("waits %v, want 1s and 2s between attempts", r.clock.slept())
	}
	if a := r.alerts("quarantine"); len(a) != 1 || a[0]["queue"] != "inbox" || a[0]["attempts"] != float64(3) {
		t.Fatalf("quarantine alerts %v", a)
	}
	if r.counter("wawarden_panics_total", "name", "engine.ingest") != 3 || len(r.logs.events("panic")) != 3 {
		t.Fatalf("panics counted %v, reported %d, want 3", r.counter("wawarden_panics_total", "name", "engine.ingest"), len(r.logs.events("panic")))
	}
	if strings.Contains(r.logs.String(), "canary") {
		t.Fatalf("the panic value reached the log: %s", r.logs)
	}
}

func (r *pipeRig) inboxAttempts() int {
	r.t.Helper()
	var attempts int
	if err := r.archive.Read(r.t.Context(), "test.inbox_attempts", func(rd *ingest.Reader) error {
		item, ok, err := rd.NextInbox()
		if err == nil && !ok {
			r.t.Error("the inbox is empty")
		}
		attempts = item.Attempts
		return err
	}); err != nil {
		r.t.Fatalf("Read: %v", err)
	}
	return attempts
}

func TestTheAttemptIsCommittedBeforeTheApply(t *testing.T) {
	r := newPipeRig(t)
	r.deliver(dm("M1", alice, "applied after its attempt is recorded"))
	var seen []int
	r.p.beforeApply = func(int64) { seen = append(seen, r.inboxAttempts()) }
	r.drain()
	if !slices.Equal(seen, []int{1}) {
		t.Fatalf("attempts recorded before the apply: %v, want [1]", seen)
	}
	r.must(alice, "M1", alice)
}

func TestCrashBetweenAttemptAndApplyReplaysOnce(t *testing.T) {
	r := newPipeRig(t)
	orig := dm("M1", alice, "before the crash")
	edit := change(KindEdit, alice, "M2", alice, Key{FromMe: true, ID: "M1"})
	edit.Text = "edited once"
	r.deliver(orig, edit)
	for crash := 1; crash <= 2; crash++ {
		r.crashBeforeApply()
		r.restart()
		if got := r.inboxAttempts(); got != crash {
			t.Fatalf("after crash %d the row records %d attempts", crash, got)
		}
	}
	r.drain()
	if f := r.must(alice, "M1", alice); f.Text != "edited once" {
		t.Fatalf("after the replay %+v", f)
	}
	r.deliver(orig, edit)
	r.drain()
	db := r.inspect()
	if got := query[string](t, db, "SELECT count(*) || ' ' || (SELECT count(*) FROM inbox) || ' ' || max(text) FROM messages"); got != "1 0 edited once" {
		t.Fatalf("messages, inbox rows and text = %q, want one row, an empty inbox and the edit", got)
	}
}

func TestThreeCrashesQuarantine(t *testing.T) {
	r := newPipeRig(t)
	r.deliver(dm("M1", alice, "crashes the process"))
	for range maxAttempts {
		r.crashBeforeApply()
		r.restart()
	}
	r.drain()
	if _, ok := r.find(alice, "M1", alice); ok {
		t.Fatal("a row that crashed three times was applied a fourth time")
	}
	if a := r.alerts("quarantine"); len(a) != 1 || a[0]["attempts"] != float64(3) {
		t.Fatalf("quarantine alerts %v", a)
	}
	db := r.inspect()
	if got := query[string](t, db, "SELECT quarantined || ' ' || (payload IS NULL) FROM inbox"); got != "1 1" {
		t.Fatalf("quarantined row = %q, want quarantined and no payload", got)
	}
}

func TestConcurrentDeliveryKeepsEveryMessageOnce(t *testing.T) {
	r := newPipeRig(t)
	const senders, each = 4, 25
	var wg sync.WaitGroup
	for s := range senders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range each {
				m := dm("M"+strconv.Itoa(s)+"x"+strconv.Itoa(i), alice, "concurrent")
				for !r.p.accept(m) {
					time.Sleep(time.Millisecond)
				}
				if i%5 == 0 {
					r.p.accept(m)
				}
			}
		}()
	}
	done := make(chan struct{})
	var drainer sync.WaitGroup
	drainer.Add(1)
	go func() {
		defer drainer.Done()
		for {
			select {
			case <-done:
				r.p.drainInbox(t.Context())
				return
			case <-r.p.kick:
				r.p.drainInbox(t.Context())
			}
		}
	}()
	wg.Wait()
	close(done)
	drainer.Wait()
	if got := r.counter("wawarden_messages_ingested_total"); got != senders*each {
		t.Fatalf("ingested %v, want %d", got, senders*each)
	}
	db := r.inspect()
	if n := query[int](t, db, "SELECT count(*) FROM messages"); n != senders*each {
		t.Fatalf("%d rows, want %d", n, senders*each)
	}
}

func TestInboxNeverHoldsDroppedTraffic(t *testing.T) {
	r := newPipeRig(t)
	canary := "status text that must never reach the disk"
	r.deliver(text("status@broadcast", "S1", alice, canary))
	if err := r.archive.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	raw, err := os.ReadFile(filepath.Clean(filepath.Join(r.opts.DataDir, "archive.db")))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if bytes.Contains(raw, []byte(canary)) {
		t.Fatal("a dropped status update was written to the archive")
	}
}

func TestLogsAndMetricsCarryNoContent(t *testing.T) {
	r := newPipeRig(t)
	canary, name := "canary text that must stay in the archive", "Canary Push Name"
	first := dm("M1", aliceLID, canary)
	first.PushName, first.SenderAlt = name, alice
	conflict := dm("M2", aliceLID, canary)
	conflict.PushName, conflict.SenderAlt = name, bob
	r.appendRaw([]byte(`{"v":1,"message":{"chat":"` + alice + `","text":"` + canary + `","ts":"poison"}}`))
	r.ingest(first, conflict, text("status@broadcast", "S1", carol, canary), dm("bad id", alice, canary))
	r.p.sweep(t.Context())
	var metrics bytes.Buffer
	if err := r.reg.WriteText(&metrics); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	if len(r.alerts("quarantine")) != 1 || len(r.alerts("rekey_conflict")) != 1 {
		t.Fatalf("the scenario did not raise its alerts: %s", r.logs)
	}
	for _, out := range []string{r.logs.String(), metrics.String()} {
		for _, secret := range []string{canary, name, "15550100001", "15550100002", "15550100003", "100000000000001"} {
			if bytes.Contains([]byte(out), []byte(secret)) {
				t.Fatalf("%q reached the logs or the metrics:\n%s", secret, out)
			}
		}
	}
}
