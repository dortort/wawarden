package admin_test

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/dortort/wawarden/internal/keys"
	"github.com/dortort/wawarden/internal/store/admin"
)

const (
	pruneBatch   = 10_000
	unlockAnchor = "DROP TRIGGER audit_anchor_forward; DROP TRIGGER audit_anchor_keep"
)

func prune(t *testing.T, f *fixture, cutoff time.Time, batch int) admin.PruneResult {
	t.Helper()
	res, err := f.store.Audit().Prune(t.Context(), cutoff, f.clock.Now(), batch)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	return res
}

func ids(rows []admin.AuditRow) []int64 {
	var out []int64
	for _, r := range rows {
		out = append(out, r.ID)
	}
	return out
}

func storedHMAC(t *testing.T, f *fixture, id int64) []byte {
	t.Helper()
	var mac []byte
	if err := rawOpen(t, f.copyArchive(t)).QueryRowContext(t.Context(), "SELECT row_hmac FROM audit WHERE id = ?", id).Scan(&mac); err != nil {
		t.Fatalf("SELECT row_hmac: %v", err)
	}
	return mac
}

func TestPruneRemovesTheRowsBeforeTheCutoffBehindASignedAnchor(t *testing.T) {
	f, heads := chainFixture(t)
	sixth := storedHMAC(t, f, 6)
	res := prune(t, f, start.Add(4500*time.Millisecond), pruneBatch)
	if res.Rows != 6 || res.Through != 6 || res.AnchorHead != hex.EncodeToString(sixth[:16]) {
		t.Fatalf("Prune = %+v, want rows 1 to 6 removed and the anchor at row 6", res)
	}
	path := f.copyArchive(t)
	if got := ids(readRows(t, rawOpen(t, path))); len(got) != 6 || got[0] != 7 || got[5] != 12 {
		t.Fatalf("rows left %v, want 7 to 12", got)
	}
	var through, ts, prunedAt int64
	var keyID, macKeyID string
	var rowHMAC, mac []byte
	if err := rawOpen(t, path).QueryRowContext(t.Context(), "SELECT through, ts, key_id, row_hmac, pruned_at, mac_key_id, mac FROM audit_anchor").
		Scan(&through, &ts, &keyID, &rowHMAC, &prunedAt, &macKeyID, &mac); err != nil {
		t.Fatalf("SELECT audit_anchor: %v", err)
	}
	if through != 6 || ts != start.Add(4*time.Second).UnixMilli() || keyID != f.master.ID() || !bytes.Equal(rowHMAC, sixth) ||
		prunedAt != f.clock.Now().UnixMilli() || macKeyID != f.master.ID() ||
		!hmac.Equal(mac, admin.AnchorMAC(f.master.AuditChainKey(), through, ts, prunedAt, keyID, rowHMAC)) {
		t.Fatal("the anchor does not hold row 6's time, key id and HMAC, the prune time and a MAC under the current audit-chain key")
	}
	rep := verify(t, f, path, heads)
	if !rep.OK() || rep.Rows != 6 || rep.Verified != 6 || rep.PrunedThrough != 6 || rep.AnchorHead != heads[5].Hex ||
		rep.AnchorTS != ts || rep.HeadsPruned != 5 || rep.HeadsMissing != 0 || rep.Heads != 12 || rep.KeyIDs[f.master.ID()] != 6 || len(rep.KeyIDs) != 1 {
		t.Fatalf("Verify after the prune = %+v", rep)
	}
	if again := prune(t, f, start.Add(4500*time.Millisecond), pruneBatch); again != (admin.PruneResult{}) {
		t.Fatalf("a second prune with the same cutoff = %+v, want nothing", again)
	}
	if later := prune(t, f, start.Add(6500*time.Millisecond), pruneBatch); later.Rows != 2 || later.Through != 8 {
		t.Fatalf("a later cutoff = %+v, want rows 7 and 8 and the anchor at row 8", later)
	}
	if rep := verify(t, f, f.copyArchive(t), heads); !rep.OK() || rep.PrunedThrough != 8 || rep.HeadsPruned != 7 || rep.Verified != 4 {
		t.Fatalf("Verify after the anchor moved = %+v", rep)
	}
}

func TestTheAnchorTimeIsTheNewestTimeItCovers(t *testing.T) {
	f := open(t)
	for _, at := range []time.Duration{5, 1, 2, 3, 4, 6} {
		if err := f.store.Audit().Record(t.Context(), admin.Event{At: start.Add(at * time.Second), Client: "aaaaaaaa", Action: "messages.read", Reason: "ok"}); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	heads := shippedHeads(t, f)
	newest := start.Add(5 * time.Second).UnixMilli()
	for i, want := range []int64{2, 4, 5} {
		if res := prune(t, f, start.Add(4500*time.Millisecond), 2); res.Through != want {
			t.Fatalf("batch %d = %+v, want the anchor at row %d", i, res, want)
		}
		rep := verify(t, f, f.copyArchive(t), heads)
		if !rep.OK() || rep.AnchorTS != newest || rep.HeadsPruned != int(want)-1 || rep.HeadsMissing != 0 {
			t.Fatalf("Verify after batch %d = %+v, want the anchor at the time of row 1, logged after the rows it precedes, and every removed head pruned", i, rep)
		}
	}
}

func TestTheAnchorTimeStaysWithinTheCommitLagOfTheCutoff(t *testing.T) {
	f := open(t)
	for i := range 8 {
		at := start.Add(time.Duration(i) * time.Minute)
		if i == 2 {
			at = time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)
		}
		if err := f.store.Audit().Record(t.Context(), admin.Event{At: at, Client: "aaaaaaaa", Action: "messages.read", Reason: "ok"}); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	heads := shippedHeads(t, f)
	if res := prune(t, f, start.Add(90*time.Second), pruneBatch); res.Through != 2 {
		t.Fatalf("Prune = %+v, want the anchor at row 2", res)
	}
	cutoff := start.Add(210 * time.Second)
	if res := prune(t, f, cutoff, pruneBatch); res.Through != 4 {
		t.Fatalf("Prune over a row written while the clock was ahead = %+v, want the anchor at row 4", res)
	}
	path := f.copyArchive(t)
	exec(t, rawOpen(t, path), unlock, "DELETE FROM audit WHERE id >= 6")
	rep := verify(t, f, path, heads)
	if rep.OK() || rep.AnchorTS != cutoff.Add(10*time.Second).UnixMilli() || rep.HeadsPruned != 2 || rep.HeadsMissing != 4 || rep.FirstMissing != heads[2].Hex {
		t.Fatalf("Verify after rows were cut from the end = %+v, want the anchor dated 10 seconds after the cutoff and rows 3 and 6 to 8 missing", rep)
	}
}

func TestPruneNeverRemovesTheNewestRow(t *testing.T) {
	f, _ := chainFixture(t)
	var total int64
	for {
		res := prune(t, f, start.Add(time.Hour), 4)
		if res.Rows == 0 {
			break
		}
		if res.Rows > 4 {
			t.Fatalf("a batch of 4 removed %d rows", res.Rows)
		}
		total += res.Rows
	}
	if got := ids(auditRows(t, f)); total != 11 || len(got) != 1 || got[0] != 12 {
		t.Fatalf("pruning everything removed %d rows and left %v, want only row 12", total, got)
	}
	f.clock.advance(time.Minute)
	if err := f.store.Audit().Record(t.Context(), admin.Event{At: f.clock.Now(), Client: "aaaaaaaa", Action: "messages.read", Reason: "ok"}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if got := ids(auditRows(t, f)); len(got) != 2 || got[1] != 13 {
		t.Fatalf("the next row after the prune got ids %v, want 13", got)
	}
	rep := verify(t, f, f.copyArchive(t), shippedHeads(t, f))
	if !rep.OK() || rep.Verified != 2 || rep.PrunedThrough != 11 || rep.HeadsPruned != 10 || rep.Heads != 13 {
		t.Fatalf("Verify after pruning all but the newest row and recording one = %+v", rep)
	}
}

func TestPruneLeavesAShortChainAlone(t *testing.T) {
	f := open(t)
	if res := prune(t, f, start.Add(time.Hour), pruneBatch); res != (admin.PruneResult{}) {
		t.Fatalf("Prune of an empty chain = %+v", res)
	}
	if err := f.store.Audit().Record(t.Context(), admin.Event{At: start, Client: "aaaaaaaa", Action: "messages.read", Reason: "ok"}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if res := prune(t, f, start.Add(time.Hour), pruneBatch); res != (admin.PruneResult{}) || len(auditRows(t, f)) != 1 {
		t.Fatalf("Prune of a one-row chain = %+v", res)
	}
	for _, batch := range []int{0, -1} {
		if _, err := f.store.Audit().Prune(t.Context(), start.Add(time.Hour), start, batch); err == nil {
			t.Fatalf("Prune with a batch of %d succeeded", batch)
		}
	}
	var unkeyed *admin.Audit
	if _, err := unkeyed.Prune(t.Context(), start.Add(time.Hour), start, pruneBatch); err == nil {
		t.Fatal("Prune without an audit succeeded")
	}
	if _, err := admin.NewAudit(nil, nil, nil).Prune(t.Context(), start.Add(time.Hour), start, pruneBatch); err == nil {
		t.Fatal("Prune without a key succeeded")
	}
}

func TestVerifyDetectsEveryAnchorTamper(t *testing.T) {
	f, heads := chainFixture(t)
	prune(t, f, start.Add(4500*time.Millisecond), pruneBatch)
	other := syntheticMaster(t, 7)
	forge := func(d *sql.DB, key []byte, keyID string, through int64) {
		var ts int64
		var kid string
		var mac []byte
		if err := d.QueryRowContext(t.Context(), "SELECT ts, key_id, row_hmac FROM audit WHERE id = ?", through).Scan(&ts, &kid, &mac); err != nil {
			t.Fatalf("SELECT: %v", err)
		}
		exec(t, d, unlock, unlockAnchor, "DELETE FROM audit WHERE id <= "+strconv.FormatInt(through, 10))
		if _, err := d.ExecContext(t.Context(), "UPDATE audit_anchor SET through = ?, ts = ?, key_id = ?, row_hmac = ?, pruned_at = ?, mac_key_id = ?, mac = ?",
			through, ts, kid, mac, start.UnixMilli(), keyID, admin.AnchorMAC(key, through, ts, start.UnixMilli(), kid, mac)); err != nil {
			t.Fatalf("UPDATE audit_anchor: %v", err)
		}
	}
	tests := []struct {
		name     string
		tamper   func(*sql.DB)
		firstBad int64
		problem  string
	}{
		{name: "anchor through moved", tamper: func(d *sql.DB) { exec(t, d, unlockAnchor, "UPDATE audit_anchor SET through = 5") }, firstBad: 5, problem: admin.ReasonAnchorInvalid},
		{name: "anchor time edited", tamper: func(d *sql.DB) { exec(t, d, unlockAnchor, "UPDATE audit_anchor SET ts = ts + 1") }, firstBad: 6, problem: admin.ReasonAnchorInvalid},
		{name: "anchor key id edited", tamper: func(d *sql.DB) { exec(t, d, unlockAnchor, "UPDATE audit_anchor SET key_id = '00000000'") }, firstBad: 6, problem: admin.ReasonAnchorInvalid},
		{name: "anchor head edited", tamper: func(d *sql.DB) { exec(t, d, unlockAnchor, "UPDATE audit_anchor SET row_hmac = randomblob(32)") }, firstBad: 6, problem: admin.ReasonAnchorInvalid},
		{name: "anchor prune time edited", tamper: func(d *sql.DB) { exec(t, d, unlockAnchor, "UPDATE audit_anchor SET pruned_at = pruned_at + 1") }, firstBad: 6, problem: admin.ReasonAnchorInvalid},
		{name: "anchor MAC key unknown", tamper: func(d *sql.DB) { exec(t, d, unlockAnchor, "UPDATE audit_anchor SET mac_key_id = '00000000'") }, firstBad: 6, problem: admin.ReasonAnchorInvalid},
		{name: "anchor MAC forged", tamper: func(d *sql.DB) { exec(t, d, unlockAnchor, "UPDATE audit_anchor SET mac = randomblob(32)") }, firstBad: 6, problem: admin.ReasonAnchorInvalid},
		{name: "anchor deleted", tamper: func(d *sql.DB) { exec(t, d, unlockAnchor, "DELETE FROM audit_anchor") }, firstBad: 7, problem: admin.ReasonIDGap},
		{name: "first retained row deleted", tamper: func(d *sql.DB) { exec(t, d, unlock, "DELETE FROM audit WHERE id = 7") }, firstBad: 8, problem: admin.ReasonIDGap},
		{name: "rows deleted behind an anchor MACed with another key", tamper: func(d *sql.DB) { forge(d, other.AuditChainKey(), f.master.ID(), 9) }, firstBad: 9, problem: admin.ReasonAnchorInvalid},
		{name: "rows deleted behind an anchor naming another key", tamper: func(d *sql.DB) { forge(d, other.AuditChainKey(), other.ID(), 9) }, firstBad: 9, problem: admin.ReasonAnchorInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := f.copyArchive(t)
			d := rawOpen(t, path)
			tt.tamper(d)
			if err := d.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
			rep, err := admin.Verify(t.Context(), path, f.master, heads)
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			if rep.OK() || rep.FirstBad != tt.firstBad || rep.Problem != tt.problem {
				t.Fatalf("Verify = %+v, want first bad row %d (%q)", rep, tt.firstBad, tt.problem)
			}
		})
	}
	forged := f.copyArchive(t)
	d := rawOpen(t, forged)
	forge(d, f.master.AuditChainKey(), f.master.ID(), 9)
	if err := d.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if rep := verify(t, f, forged, heads); !rep.OK() || rep.PrunedThrough != 9 {
		t.Fatalf("an anchor MACed with the chain key verifies as %+v: the fixture forges what the key allows", rep)
	}

	previous, current, _ := rotated(t)
	dir := t.TempDir()
	record := func(f *fixture) {
		for range 3 {
			f.clock.advance(time.Second)
			if err := f.store.Audit().Record(t.Context(), admin.Event{At: f.clock.Now(), Client: "aaaaaaaa", Action: "messages.read", Reason: "ok"}); err != nil {
				t.Fatalf("Record: %v", err)
			}
		}
	}
	before := openWith(t, dir, previous)
	record(before)
	if res := prune(t, before, start.Add(1500*time.Millisecond), pruneBatch); res.Through != 1 {
		t.Fatalf("Prune under the retired key = %+v", res)
	}
	if err := before.store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	after := openWith(t, dir, current)
	after.clock.advance(time.Minute)
	record(after)
	rotatedHeads := append(shippedHeads(t, before), shippedHeads(t, after)...)
	for _, tt := range []struct {
		name    string
		through int64
		invalid bool
	}{
		{name: "rows of the retired key cut behind an anchor under it", through: 2},
		{name: "rows of the current key cut behind an anchor under the retired key", through: 4, invalid: true},
		{name: "every row cut behind an anchor under the retired key", through: 6, invalid: true},
	} {
		path := after.copyArchive(t)
		d := rawOpen(t, path)
		forge(d, previous.AuditChainKey(), previous.ID(), tt.through)
		if err := d.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		rep, err := admin.Verify(t.Context(), path, current, rotatedHeads)
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		if tt.invalid && (rep.OK() || rep.FirstBad != tt.through || rep.Problem != admin.ReasonAnchorInvalid || rep.Bad != 1) {
			t.Fatalf("%s: Verify = %+v, want the anchor refused", tt.name, rep)
		}
		if !tt.invalid && (!rep.OK() || rep.PrunedThrough != tt.through || rep.HeadsPruned != 1) {
			t.Fatalf("%s: Verify = %+v, want what the retired key allows to verify", tt.name, rep)
		}
	}
}

func TestShippedHeadsAreClassifiedByTheirTime(t *testing.T) {
	f, heads := chainFixture(t)
	prune(t, f, start.Add(4500*time.Millisecond), pruneBatch)
	path := f.copyArchive(t)
	unknownTime := append([]admin.Head(nil), heads...)
	unknownTime[2].TS = 0
	if rep := verify(t, f, path, unknownTime); rep.OK() || rep.HeadsPruned != 4 || rep.HeadsMissing != 1 || rep.FirstMissing != heads[2].Hex {
		t.Fatalf("a pruned head without a time = %+v, want it missing", rep)
	}
	late := append([]admin.Head(nil), heads...)
	late[3].TS = start.Add(time.Hour).UnixMilli()
	if rep := verify(t, f, path, late); rep.OK() || rep.HeadsMissing != 1 || rep.FirstMissing != heads[3].Hex {
		t.Fatalf("a pruned head logged after the anchor = %+v, want it missing", rep)
	}
	if rep := verify(t, f, path, heads[5:]); !rep.OK() || rep.HeadsPruned != 0 || rep.Heads != 7 {
		t.Fatalf("the anchor's own head = %+v, want it seen", rep)
	}
	twice := append(append([]admin.Head(nil), heads...), heads[0])
	if rep := verify(t, f, path, twice); !rep.OK() || rep.HeadsPruned != 5 || rep.Heads != 12 {
		t.Fatalf("a head shipped twice = %+v, want it counted once", rep)
	}
}

func rotatedChain(t *testing.T) (f *fixture, previous, current *keys.Master, heads []admin.Head) {
	t.Helper()
	previous, current, _ = rotated(t)
	dir := t.TempDir()
	record := func(f *fixture) {
		for range 3 {
			f.clock.advance(time.Second)
			if err := f.store.Audit().Record(t.Context(), admin.Event{At: f.clock.Now(), Client: "aaaaaaaa", Action: "messages.read", Reason: "ok"}); err != nil {
				t.Fatalf("Record: %v", err)
			}
		}
	}
	before := openWith(t, dir, previous)
	record(before)
	if err := before.store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	f = openWith(t, dir, current)
	f.clock.advance(time.Minute)
	record(f)
	return f, previous, current, append(shippedHeads(t, before), shippedHeads(t, f)...)
}

func installAnchor(t *testing.T, d *sql.DB, under *keys.Master, through, ts int64, keyID string, rowHMAC []byte) {
	t.Helper()
	exec(t, d, unlockAnchor, "DELETE FROM audit_anchor")
	if _, err := d.ExecContext(t.Context(), "INSERT INTO audit_anchor (id, through, ts, key_id, row_hmac, pruned_at, mac_key_id, mac) VALUES (1, ?, ?, ?, ?, ?, ?, ?)",
		through, ts, keyID, rowHMAC, start.UnixMilli(), under.ID(), admin.AnchorMAC(under.AuditChainKey(), through, ts, start.UnixMilli(), keyID, rowHMAC)); err != nil {
		t.Fatalf("INSERT audit_anchor: %v", err)
	}
}

func appendRow(t *testing.T, d *sql.DB, under *keys.Master, id int64, prev []byte) {
	t.Helper()
	r := admin.AuditRow{ID: id, TS: start.Add(2 * time.Hour).UnixMilli(), Client: "aaaaaaaa", Action: "messages.read", Reason: "ok", OK: true, Key: under.ID()}
	if _, err := d.ExecContext(t.Context(), "INSERT INTO audit (id, ts, client_id, action, ok, reason, key_id, row_hmac) VALUES (?, ?, ?, ?, 1, ?, ?, ?)",
		r.ID, r.TS, r.Client, r.Action, r.Reason, r.Key, admin.RowMAC(under.AuditChainKey(), r, prev)); err != nil {
		t.Fatalf("INSERT audit: %v", err)
	}
}

func TestARetiredKeyCannotStandInForTheCurrentOne(t *testing.T) {
	f, previous, current, heads := rotatedChain(t)
	far := time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	first, last, forged := storedHMAC(t, f, 1), storedHMAC(t, f, 6), bytes.Repeat([]byte{0x5a}, 32)
	unnamed := append([]admin.Head(nil), heads...)
	for i := range unnamed {
		unnamed[i].KeyID = ""
	}
	cutTheEnd := func(d *sql.DB) {
		exec(t, d, unlock, "DELETE FROM audit WHERE id = 1 OR id >= 4")
		installAnchor(t, d, previous, 1, far, previous.ID(), first)
	}
	for _, tt := range []struct {
		name     string
		tamper   func(*sql.DB)
		heads    []admin.Head
		firstBad int64
		problem  string
		missing  int
	}{
		{name: "every row replaced by one under the retired key behind an anchor under it", tamper: func(d *sql.DB) {
			exec(t, d, unlock, "DELETE FROM audit")
			installAnchor(t, d, previous, 6, far, previous.ID(), forged)
			appendRow(t, d, previous, 7, forged)
		}, heads: heads, missing: 3},
		{name: "the current key's rows cut from the end behind an anchor under the retired key", tamper: cutTheEnd, heads: heads, missing: 3},
		{name: "the same cut against lines that name no key", tamper: cutTheEnd, heads: unnamed, missing: 3},
		{name: "a row under the retired key appended after the current key's rows", tamper: func(d *sql.DB) { appendRow(t, d, previous, 7, last) }, heads: heads, firstBad: 7, problem: admin.ReasonRetiredKey},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := f.copyArchive(t)
			d := rawOpen(t, path)
			tt.tamper(d)
			if err := d.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
			rep, err := admin.Verify(t.Context(), path, current, tt.heads)
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			if rep.OK() || rep.FirstBad != tt.firstBad || rep.Problem != tt.problem || rep.HeadsMissing != tt.missing || (tt.missing > 0 && rep.FirstMissing != heads[3].Hex) {
				t.Fatalf("Verify = %+v, want first bad row %d (%q) and the current key's %d heads missing", rep, tt.firstBad, tt.problem, tt.missing)
			}
		})
	}

	fifth := storedHMAC(t, f, 5)
	if res := prune(t, f, start.Add(62500*time.Millisecond), pruneBatch); res.Through != 5 {
		t.Fatalf("Prune under the current key = %+v, want the anchor at row 5", res)
	}
	path := f.copyArchive(t)
	d := rawOpen(t, path)
	exec(t, d, unlock, "DELETE FROM audit WHERE id = 6")
	appendRow(t, d, previous, 6, fifth)
	if err := d.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if rep := verify(t, f, path, nil); rep.OK() || rep.FirstBad != 6 || rep.Problem != admin.ReasonRetiredKey {
		t.Fatalf("a row under the retired key after an anchor at the current key's row = %+v, want it refused", rep)
	}
}

func tamperStopped(t *testing.T, f *fixture, master *keys.Master, tamper func(*sql.DB)) *fixture {
	t.Helper()
	if err := f.store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	d := rawOpen(t, filepath.Join(f.dir, "archive.db"))
	tamper(d)
	if err := d.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return openWith(t, f.dir, master)
}

func execArgs(t *testing.T, d *sql.DB, statement string, args ...any) {
	t.Helper()
	if _, err := d.ExecContext(t.Context(), statement, args...); err != nil {
		t.Fatalf("%s: %v", statement, err)
	}
}

func rowHMACOf(t *testing.T, d *sql.DB, id int64) []byte {
	t.Helper()
	var mac []byte
	if err := d.QueryRowContext(t.Context(), "SELECT row_hmac FROM audit WHERE id = ?", id).Scan(&mac); err != nil {
		t.Fatalf("SELECT row_hmac: %v", err)
	}
	return mac
}

func rekeyRow(t *testing.T, d *sql.DB, under *keys.Master, id int64, prev []byte) []byte {
	t.Helper()
	for _, r := range readRows(t, d) {
		if r.ID == id {
			r.Key = under.ID()
			mac := admin.RowMAC(under.AuditChainKey(), r, prev)
			execArgs(t, d, "UPDATE audit SET key_id = ?, row_hmac = ? WHERE id = ?", r.Key, mac, id)
			return mac
		}
	}
	t.Fatalf("no row %d", id)
	return nil
}

func TestPruneRefusesRowsItCannotAuthenticate(t *testing.T) {
	cutoff := start.Add(3 * time.Hour)
	forgeTail := func(t *testing.T, d *sql.DB, previous *keys.Master, fourth []byte) {
		exec(t, d, unlock, "DELETE FROM audit WHERE id = 6")
		appendRow(t, d, previous, 6, rekeyRow(t, d, previous, 5, fourth))
	}
	for _, tt := range []struct {
		name     string
		anchorAt time.Time
		tamper   func(t *testing.T, d *sql.DB, previous *keys.Master, fourth []byte)
		through  int64
		firstBad int64
		problem  string
	}{
		{name: "the newest row forged under the retired key behind the second newest renamed to it", tamper: func(t *testing.T, d *sql.DB, previous *keys.Master, _ []byte) {
			fifth := rowHMACOf(t, d, 5)
			exec(t, d, unlock)
			execArgs(t, d, "UPDATE audit SET key_id = ?, ts = (SELECT ts FROM audit WHERE id = 6) WHERE id = 5", previous.ID())
			exec(t, d, "DELETE FROM audit WHERE id = 6")
			appendRow(t, d, previous, 6, fifth)
		}, firstBad: 5, problem: admin.ReasonRetiredKey},
		{name: "the second newest row rewritten under the retired key", tamper: forgeTail, firstBad: 5, problem: admin.ReasonRetiredKey},
		{name: "a row edited without a key", tamper: func(t *testing.T, d *sql.DB, _ *keys.Master, _ []byte) {
			exec(t, d, unlock, "UPDATE audit SET reason = 'edited' WHERE id = 2")
		}, firstBad: 2, problem: admin.ReasonHMACMismatch},
		{name: "a row under the retired key after an anchor at the current key's row", anchorAt: start.Add(61500 * time.Millisecond), tamper: forgeTail,
			through: 4, firstBad: 5, problem: admin.ReasonRetiredKey},
		{name: "the anchor renamed to the retired key without a key", anchorAt: start.Add(61500 * time.Millisecond), tamper: func(t *testing.T, d *sql.DB, previous *keys.Master, fourth []byte) {
			exec(t, d, unlockAnchor)
			execArgs(t, d, "UPDATE audit_anchor SET key_id = ?", previous.ID())
			forgeTail(t, d, previous, fourth)
		}, through: 4, firstBad: 4, problem: admin.ReasonAnchorInvalid},
		{name: "a removed row's time and the anchor's moved forward without a key", anchorAt: start.Add(61500 * time.Millisecond), tamper: func(t *testing.T, d *sql.DB, _ *keys.Master, _ []byte) {
			later, far := strconv.FormatInt(start.Add(2*time.Hour).UnixMilli(), 10), strconv.FormatInt(time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli(), 10)
			exec(t, d, unlock, unlockAnchor, "UPDATE audit SET ts = "+later+" WHERE id = 5", "UPDATE audit_anchor SET ts = "+far)
		}, through: 4, firstBad: 4, problem: admin.ReasonAnchorInvalid},
		{name: "the retired key's rows cut behind an anchor under it", tamper: func(t *testing.T, d *sql.DB, previous *keys.Master, _ []byte) {
			third := rowHMACOf(t, d, 3)
			exec(t, d, unlock, "DELETE FROM audit WHERE id <= 3")
			installAnchor(t, d, previous, 3, start.Add(3*time.Second).UnixMilli(), previous.ID(), third)
		}, through: 3, firstBad: 3, problem: admin.ReasonAnchorInvalid},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f, previous, current, heads := rotatedChain(t)
			fourth := storedHMAC(t, f, 4)
			if !tt.anchorAt.IsZero() {
				if res := prune(t, f, tt.anchorAt, pruneBatch); res.Through != 4 {
					t.Fatalf("Prune before the tamper = %+v, want the anchor at row 4", res)
				}
			}
			f = tamperStopped(t, f, current, func(d *sql.DB) { tt.tamper(t, d, previous, fourth) })
			if res, err := f.store.Audit().Prune(t.Context(), cutoff, f.clock.Now(), pruneBatch); !errors.Is(err, admin.ErrChainMismatch) {
				t.Fatalf("Prune over the tampered rows = %+v, %v; want %v", res, err, admin.ErrChainMismatch)
			}
			rep := verify(t, f, f.copyArchive(t), heads)
			if rep.OK() || rep.PrunedThrough != tt.through || rep.Rows != 6-tt.through || rep.FirstBad != tt.firstBad || rep.Problem != tt.problem {
				t.Fatalf("Verify after the refused prune = %+v, want the anchor at row %d, nothing removed and first bad row %d (%q)", rep, tt.through, tt.firstBad, tt.problem)
			}
		})
	}
}

func TestVerifyReadsACopyWithoutAnAnchorTable(t *testing.T) {
	f, heads := chainFixture(t)
	path := f.copyArchive(t)
	d := rawOpen(t, path)
	exec(t, d, "DROP TRIGGER audit_no_delete; DROP TRIGGER audit_anchor_forward; DROP TRIGGER audit_anchor_keep; DROP TABLE audit_anchor")
	if err := d.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if rep := verify(t, f, path, heads); !rep.OK() || rep.Verified != 12 || rep.PrunedThrough != 0 {
		t.Fatalf("Verify of a copy without audit_anchor = %+v", rep)
	}
}

func rotated(t *testing.T) (previous, current, currentOnly *keys.Master) {
	t.Helper()
	dir := t.TempDir()
	previous, r := keys.Load(dir, os.Geteuid())
	if r != nil {
		t.Fatalf("Load: %v", r)
	}
	if _, _, r := keys.Rotate(dir, os.Geteuid(), func() error { return nil }); r != nil {
		t.Fatalf("Rotate: %v", r)
	}
	if current, r = keys.Load(dir, os.Geteuid()); r != nil {
		t.Fatalf("Load: %v", r)
	}
	if currentOnly, r = keys.LoadFile(filepath.Join(dir, "keys", "master")); r != nil {
		t.Fatalf("LoadFile: %v", r)
	}
	return previous, current, currentOnly
}

func TestVerifyPicksEachRowsKeyFromTheSet(t *testing.T) {
	previous, current, currentOnly := rotated(t)
	dir := t.TempDir()
	first := openWith(t, dir, previous)
	record := func(f *fixture, n int) {
		for range n {
			f.clock.advance(time.Second)
			if err := f.store.Audit().Record(t.Context(), admin.Event{At: f.clock.Now(), Client: "aaaaaaaa", Action: "messages.read", Reason: "ok"}); err != nil {
				t.Fatalf("Record: %v", err)
			}
		}
	}
	record(first, 4)
	if res := prune(t, first, start.Add(2500*time.Millisecond), pruneBatch); res.Through != 2 {
		t.Fatalf("Prune under the previous key = %+v", res)
	}
	if err := first.store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	second := openWith(t, dir, current)
	second.clock.advance(time.Minute)
	record(second, 3)
	path := second.copyArchive(t)
	rep := verify(t, second, path, nil)
	if !rep.OK() || rep.Verified != 5 || rep.KeyIDs[previous.ID()] != 2 || rep.KeyIDs[current.ID()] != 3 || len(rep.KeyIDs) != 2 {
		t.Fatalf("Verify with both keys = %+v", rep)
	}
	only, err := admin.Verify(t.Context(), path, currentOnly, nil)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if only.OK() || only.Problem != admin.ReasonAnchorInvalid || only.Bad != 3 || only.Verified != 3 {
		t.Fatalf("Verify with the current key alone = %+v, want the anchor and the two older rows refused", only)
	}
	if res := prune(t, second, start.Add(3500*time.Millisecond), pruneBatch); res.Through != 3 {
		t.Fatalf("Prune under the current key = %+v", res)
	}
	moved := second.copyArchive(t)
	var macKeyID string
	if err := rawOpen(t, moved).QueryRowContext(t.Context(), "SELECT mac_key_id FROM audit_anchor").Scan(&macKeyID); err != nil || macKeyID != current.ID() {
		t.Fatalf("the moved anchor is MACed under %q, %v; want the current key", macKeyID, err)
	}
	if rep := verify(t, second, moved, nil); !rep.OK() || rep.Verified != 4 {
		t.Fatalf("Verify after the anchor moved under the current key = %+v", rep)
	}
}

func TestAnchorMACMatchesItsLayout(t *testing.T) {
	key := bytes.Repeat([]byte{0x01}, 32)
	rowHMAC := bytes.Repeat([]byte{0x02}, 32)
	got := admin.AnchorMAC(key, 6, 1_791_000_000_000, 1_791_086_400_000, "6a53922c", rowHMAC)
	b := []byte("wawarden/audit-anchor/v1\x00")
	b = binary.BigEndian.AppendUint64(b, 6)
	b = binary.BigEndian.AppendUint64(b, 1_791_000_000_000)
	b = binary.BigEndian.AppendUint64(b, 1_791_086_400_000)
	b = append(b, 1, 8)
	b = append(b, "6a53922c"...)
	b = append(b, rowHMAC...)
	m := hmac.New(sha256.New, key)
	m.Write(b)
	if !hmac.Equal(got, m.Sum(nil)) {
		t.Fatal("the anchor MAC is not HMAC-SHA256 over the documented layout")
	}
	const vector = "57d268977493e2046ec8b14d17ae8751ecb19b87c43028ecb097bfdae919dc63"
	if hex.EncodeToString(got) != vector {
		t.Fatalf("the anchor MAC is %x, the recorded vector is %s: a change breaks every anchor already written", got, vector)
	}
}
