package admin_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dortort/wawarden/internal/policy"
	"github.com/dortort/wawarden/internal/store/admin"
)

func chainFixture(t *testing.T) (*fixture, []string) {
	t.Helper()
	f := open(t)
	c, _ := f.create(t, policy.ClientSpec{Name: "agent", Read: []string{phoneA, groupG}})
	for i := range 10 {
		e := admin.Event{At: start.Add(time.Duration(i) * time.Second), Client: c.ID, Action: "messages.read", OK: i%3 != 0, Reason: "ok", Peer: "127.0.0.1:50000"}
		switch {
		case !e.OK:
			e.Reason, e.Chat = "out_of_scope", chat(t, phoneB)
		case i%2 == 0:
			e.Chat = chat(t, groupG)
		}
		if err := f.store.Audit().Record(t.Context(), e); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	if _, err := f.clients.Revoke(t.Context(), c.ID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	var heads []string
	for _, line := range f.auditLines(t) {
		heads = append(heads, line["chain_head"].(string))
	}
	if len(heads) != 12 {
		t.Fatalf("%d audit lines, want 12", len(heads))
	}
	return f, heads
}

func TestAuditLinesCarryTheShapeAndTheHead(t *testing.T) {
	f, heads := chainFixture(t)
	path := f.copyArchive(t)
	d := rawOpen(t, path)
	var stored [][]byte
	rows, err := d.QueryContext(t.Context(), "SELECT row_hmac FROM audit ORDER BY id")
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	for rows.Next() {
		var b []byte
		if err := rows.Scan(&b); err != nil {
			t.Fatalf("Scan: %v", err)
		}
		stored = append(stored, b)
	}
	_ = rows.Close()
	lines := f.auditLines(t)
	chatKey := f.master.ChatHMACKey()
	for i, line := range lines {
		var keys []string
		for k := range line {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		if got := strings.Join(keys, " "); got != "action chain_head chat_hmac client ok reason ts" {
			t.Fatalf("audit line %d has the keys %s", i, got)
		}
		if heads[i] != hex.EncodeToString(stored[i][:16]) {
			t.Fatalf("line %d ships the head %s, the row's HMAC starts %x", i, heads[i], stored[i][:16])
		}
	}
	m := hmac.New(sha256.New, chatKey)
	m.Write([]byte(phoneB))
	if lines[1]["chat_hmac"] != hex.EncodeToString(m.Sum(nil)[:16]) || lines[1]["ok"] != false || lines[1]["reason"] != "out_of_scope" {
		t.Fatalf("line %v does not carry the chat's HMAC under the chat-hmac key", lines[1])
	}
	if lines[0]["chat_hmac"] != nil || lines[0]["action"] != "client_create" || lines[11]["action"] != "client_revoke" || lines[0]["ts"] != "2026-10-06T12:00:00.000Z" {
		t.Fatalf("lines %v and %v", lines[0], lines[11])
	}
	text := f.audit.String()
	for _, jid := range []string{phoneA, phoneB, groupG, "15550100002", "127.0.0.1"} {
		if strings.Contains(text, jid) {
			t.Fatalf("an audit line carries %q", jid)
		}
	}
	rowsRead := readRows(t, d)
	if rowsRead[1].Chat.String != phoneB || rowsRead[1].Peer.String != "127.0.0.1:50000" || rowsRead[0].Chat.Valid || rowsRead[0].Key != f.master.ID() {
		t.Fatalf("rows %+v and %+v", rowsRead[0], rowsRead[1])
	}
}

func TestAppendRefusesEventsTheChainDoesNotRecord(t *testing.T) {
	f := open(t)
	for name, e := range map[string]admin.Event{
		"no time":           {Client: "aaaaaaaa", Action: "messages.read", Reason: "ok"},
		"client not an id":  {At: start, Client: "15550100001", Action: "messages.read", Reason: "ok"},
		"action not a code": {At: start, Client: "aaaaaaaa", Action: "Messages Read", Reason: "ok"},
		"reason not a code": {At: start, Client: "aaaaaaaa", Action: "messages.read", Reason: phoneA},
		"peer too long":     {At: start, Client: "aaaaaaaa", Action: "messages.read", Reason: "ok", Peer: strings.Repeat("1", 65)},
	} {
		if err := f.store.Audit().Record(t.Context(), e); err == nil {
			t.Fatalf("%s: Record succeeded", name)
		}
	}
	if f.audit.String() != "" || len(auditRows(t, f)) != 0 {
		t.Fatal("a refused event left a row or a line")
	}
}

func verify(t *testing.T, f *fixture, path string, heads []string) admin.Report {
	t.Helper()
	rep, err := admin.Verify(t.Context(), path, f.master, heads)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	return rep
}

func exec(t *testing.T, d *sql.DB, statements ...string) {
	t.Helper()
	for _, s := range statements {
		if _, err := d.ExecContext(t.Context(), s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
}

func rechain(t *testing.T, d *sql.DB, key []byte, from int64) {
	t.Helper()
	rows := readRows(t, d)
	prev := make([]byte, 32)
	for _, r := range rows {
		if r.ID >= from {
			mac := admin.RowMAC(key, r, prev)
			if _, err := d.ExecContext(t.Context(), "UPDATE audit SET row_hmac = ? WHERE id = ?", mac, r.ID); err != nil {
				t.Fatalf("UPDATE: %v", err)
			}
		}
		if err := d.QueryRowContext(t.Context(), "SELECT row_hmac FROM audit WHERE id = ?", r.ID).Scan(&prev); err != nil {
			t.Fatalf("SELECT: %v", err)
		}
	}
}

const unlock = "DROP TRIGGER audit_no_update; DROP TRIGGER audit_no_delete"

func TestVerifyDetectsEveryTamper(t *testing.T) {
	f, heads := chainFixture(t)
	intact := verify(t, f, f.copyArchive(t), heads)
	if !intact.OK() || intact.Rows != 12 || intact.Verified != 12 || intact.Heads != 12 || intact.HeadsMissing != 0 || intact.Head != heads[11] {
		t.Fatalf("the intact chain reports %+v", intact)
	}
	other := syntheticMaster(t, 7)
	tests := []struct {
		name         string
		tamper       func(*sql.DB)
		master       bool
		firstBad     int64
		problem      string
		headsMissing int
	}{
		{name: "field edit", tamper: func(d *sql.DB) { exec(t, d, unlock, "UPDATE audit SET reason = 'ok' WHERE id = 2") }, firstBad: 2, problem: admin.ReasonHMACMismatch},
		{name: "plaintext chat edit", tamper: func(d *sql.DB) {
			exec(t, d, unlock, "UPDATE audit SET chat = '15550100003@s.whatsapp.net' WHERE id = 5")
		}, firstBad: 5, problem: admin.ReasonHMACMismatch},
		{name: "ok flipped", tamper: func(d *sql.DB) { exec(t, d, unlock, "UPDATE audit SET ok = 1 WHERE id = 2") }, firstBad: 2, problem: admin.ReasonHMACMismatch},
		{name: "middle delete", tamper: func(d *sql.DB) { exec(t, d, unlock, "DELETE FROM audit WHERE id = 7") }, firstBad: 8, problem: admin.ReasonIDGap, headsMissing: 1},
		{name: "first rows deleted", tamper: func(d *sql.DB) { exec(t, d, unlock, "DELETE FROM audit WHERE id <= 2") }, firstBad: 3, problem: admin.ReasonIDGap, headsMissing: 2},
		{name: "forged HMAC", tamper: func(d *sql.DB) { exec(t, d, unlock, "UPDATE audit SET row_hmac = randomblob(32) WHERE id = 4") }, firstBad: 4, problem: admin.ReasonHMACMismatch, headsMissing: 1},
		{name: "re-chained with another key", tamper: func(d *sql.DB) { exec(t, d, unlock); rechain(t, d, other.AuditChainKey(), 1) }, firstBad: 1, problem: admin.ReasonHMACMismatch, headsMissing: 12},
		{name: "verified with another master key", master: true, firstBad: 1, problem: admin.ReasonUnknownKey},
		{name: "key id rewritten", tamper: func(d *sql.DB) { exec(t, d, unlock, "UPDATE audit SET key_id = '00000000' WHERE id = 9") }, firstBad: 9, problem: admin.ReasonUnknownKey},
		{name: "tail truncated", tamper: func(d *sql.DB) { exec(t, d, unlock, "DELETE FROM audit WHERE id > 8") }, headsMissing: 4},
		{name: "edited and re-chained with the right key", tamper: func(d *sql.DB) {
			exec(t, d, unlock, "UPDATE audit SET reason = 'ok', ok = 1 WHERE id = 2")
			rechain(t, d, f.master.AuditChainKey(), 2)
		}, headsMissing: 11},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := f.copyArchive(t)
			if tt.tamper != nil {
				d := rawOpen(t, path)
				tt.tamper(d)
				if err := d.Close(); err != nil {
					t.Fatalf("Close: %v", err)
				}
			}
			m := f.master
			if tt.master {
				m = other
			}
			rep, err := admin.Verify(t.Context(), path, m, heads)
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			if rep.OK() || rep.FirstBad != tt.firstBad || rep.Problem != tt.problem || rep.HeadsMissing != tt.headsMissing {
				t.Fatalf("Verify = %+v, want first bad row %d (%q) and %d shipped heads missing", rep, tt.firstBad, tt.problem, tt.headsMissing)
			}
			if tt.name == "tail truncated" {
				chainOnly, err := admin.Verify(t.Context(), path, f.master, nil)
				if err != nil || !chainOnly.OK() || chainOnly.Rows != 8 {
					t.Fatalf("without the shipped heads the truncated chain reports %+v, %v: truncation is visible only through the heads", chainOnly, err)
				}
				if rep.FirstMissing != heads[8] {
					t.Fatalf("first missing head %s, want %s", rep.FirstMissing, heads[8])
				}
			}
		})
	}
}

func TestTheTriggersRefuseUpdatesAndDeletes(t *testing.T) {
	f, _ := chainFixture(t)
	d := rawOpen(t, f.copyArchive(t))
	for _, s := range []string{"UPDATE audit SET reason = 'ok' WHERE id = 2", "DELETE FROM audit WHERE id = 12"} {
		if _, err := d.ExecContext(t.Context(), s); err == nil || !strings.Contains(err.Error(), "append-only") {
			t.Fatalf("%s: %v, want the append-only refusal", s, err)
		}
	}
}

func TestVerifyRefusesWithoutAKeyOrACopy(t *testing.T) {
	f, _ := chainFixture(t)
	if _, err := admin.Verify(t.Context(), f.copyArchive(t), nil, nil); err == nil {
		t.Fatal("Verify without a master key succeeded")
	}
	if _, err := admin.Verify(t.Context(), t.TempDir(), f.master, nil); err == nil {
		t.Fatal("Verify of a directory succeeded")
	}
}
