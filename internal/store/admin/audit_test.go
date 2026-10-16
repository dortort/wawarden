package admin_test

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dortort/wawarden/internal/cursor"
	"github.com/dortort/wawarden/internal/policy"
	"github.com/dortort/wawarden/internal/store/admin"
	"github.com/dortort/wawarden/internal/token"
)

func chainFixture(t *testing.T) (*fixture, []admin.Head) {
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
	heads := shippedHeads(t, f)
	if len(heads) != 12 {
		t.Fatalf("%d audit lines, want 12", len(heads))
	}
	return f, heads
}

func shippedHeads(t *testing.T, f *fixture) []admin.Head {
	t.Helper()
	var heads []admin.Head
	for _, line := range f.auditLines(t) {
		at, err := time.Parse(time.RFC3339Nano, line["ts"].(string))
		if err != nil {
			t.Fatalf("audit line time %v: %v", line["ts"], err)
		}
		heads = append(heads, admin.Head{Hex: line["chain_head"].(string), TS: at.UnixMilli(), KeyID: line["key_id"].(string)})
	}
	return heads
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
		if got := strings.Join(keys, " "); got != "action bytes chain_head chat_hmac client event key_id message_hmac ok reason ts" || line["event"] != "audit" || line["key_id"] != f.master.ID() {
			t.Fatalf("audit line %d has the keys %s, the event %v and the key id %v, want the audit event under the master key", i, got, line["event"], line["key_id"])
		}
		if heads[i].Hex != hex.EncodeToString(stored[i][:16]) {
			t.Fatalf("line %d ships the head %s, the row's HMAC starts %x", i, heads[i].Hex, stored[i][:16])
		}
	}
	m := hmac.New(sha256.New, chatKey)
	m.Write([]byte(phoneB))
	if lines[1]["chat_hmac"] != hex.EncodeToString(m.Sum(nil)[:16]) || lines[1]["ok"] != false || lines[1]["reason"] != "out_of_scope" {
		t.Fatalf("line %v does not carry the chat's HMAC under the chat-hmac key", lines[1])
	}
	if lines[0]["chat_hmac"] != nil || lines[1]["message_hmac"] != nil || lines[1]["bytes"] != nil || lines[0]["action"] != "client_create" || lines[11]["action"] != "client_revoke" || lines[0]["ts"] != "2026-10-06T12:00:00.000Z" {
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
	g, message := chat(t, groupG), cursor.Ref{Chat: groupG, ID: syntheticID, Sender: phoneA}
	for name, e := range map[string]admin.Event{
		"no time":           {Client: "aaaaaaaa", Action: "messages.read", Reason: "ok"},
		"client not an id":  {At: start, Client: "15550100001", Action: "messages.read", Reason: "ok"},
		"action not a code": {At: start, Client: "aaaaaaaa", Action: "Messages Read", Reason: "ok"},
		"reason not a code": {At: start, Client: "aaaaaaaa", Action: "messages.read", Reason: phoneA},
		"peer too long":     {At: start, Client: "aaaaaaaa", Action: "messages.read", Reason: "ok", Peer: strings.Repeat("1", 65)},
		"negative bytes":    {At: start, Client: "aaaaaaaa", Action: "rest.media", Reason: "ok", Chat: g, Message: message, Bytes: -1},
		"bytes, no message": {At: start, Client: "aaaaaaaa", Action: "rest.media", Reason: "ok", Chat: g, Bytes: 1},
		"message, no chat":  {At: start, Client: "aaaaaaaa", Action: "rest.media", Reason: "not_found", Message: message},
		"another chat":      {At: start, Client: "aaaaaaaa", Action: "rest.media", Reason: "ok", Chat: chat(t, phoneB), Message: message},
		"no sender":         {At: start, Client: "aaaaaaaa", Action: "rest.media", Reason: "ok", Chat: g, Message: cursor.Ref{Chat: groupG, ID: syntheticID}},
		"sender not a chat": {At: start, Client: "aaaaaaaa", Action: "rest.media", Reason: "ok", Chat: g, Message: cursor.Ref{Chat: groupG, ID: syntheticID, Sender: "alice"}},
		"chat not a chat":   {At: start, Client: "aaaaaaaa", Action: "rest.media", Reason: "ok", Chat: g, Message: cursor.Ref{Chat: "group", ID: syntheticID, Sender: phoneA}},
		"no message id":     {At: start, Client: "aaaaaaaa", Action: "rest.media", Reason: "ok", Chat: g, Message: cursor.Ref{Chat: groupG, Sender: phoneA}},
		"message id too long": {At: start, Client: "aaaaaaaa", Action: "rest.media", Reason: "ok", Chat: g,
			Message: cursor.Ref{Chat: groupG, ID: strings.Repeat("A", 257), Sender: phoneA}},
	} {
		if err := f.store.Audit().Record(t.Context(), e); err == nil {
			t.Fatalf("%s: Record succeeded", name)
		}
	}
	if f.audit.String() != "" || len(auditRows(t, f)) != 0 {
		t.Fatal("a refused event left a row or a line")
	}
}

func verify(t *testing.T, f *fixture, path string, heads []admin.Head) admin.Report {
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
	if !intact.OK() || intact.Rows != 12 || intact.Verified != 12 || intact.Heads != 12 || intact.HeadsMissing != 0 || intact.Head != heads[11].Hex {
		t.Fatalf("the intact chain reports %+v", intact)
	}
	other := syntheticMaster(t, 7)
	type tamperCase struct {
		name         string
		tamper       func(*sql.DB)
		master       bool
		firstBad     int64
		problem      string
		headsMissing int
	}
	var tests []tamperCase
	for _, edit := range []struct{ column, value string }{
		{"ts", "ts + 1"},
		{"client_id", "'aaaaaaaa'"},
		{"action", "'messages.list'"},
		{"chat", "'15550100003@s.whatsapp.net'"},
		{"chat_hmac", "'00000000000000000000000000000000'"},
		{"ok", "1 - ok"},
		{"reason", "'ok'"},
		{"peer", "'127.0.0.1:50001'"},
	} {
		tests = append(tests, tamperCase{name: "edit " + edit.column, tamper: func(d *sql.DB) {
			exec(t, d, unlock, "UPDATE audit SET "+edit.column+" = "+edit.value+" WHERE id = 5")
		}, firstBad: 5, problem: admin.ReasonHMACMismatch})
	}
	tests = append(tests, []tamperCase{
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
	}...)
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
				if rep.FirstMissing != heads[8].Hex {
					t.Fatalf("first missing head %s, want %s", rep.FirstMissing, heads[8].Hex)
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

func TestRowMACCoversTheKeyID(t *testing.T) {
	f, _ := chainFixture(t)
	r := readRows(t, rawOpen(t, f.copyArchive(t)))[4]
	key, prev := f.master.AuditChainKey(), make([]byte, 32)
	moved := r
	moved.Key = "00000000"
	if hmac.Equal(admin.RowMAC(key, r, prev), admin.RowMAC(key, moved, prev)) {
		t.Fatal("the row HMAC ignores key_id")
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

func TestAuditEventsCarryingATokenAreRefused(t *testing.T) {
	f := open(t)
	admin1 := token.NewAdmin()
	id, client := token.NewClient()
	for _, tok := range []string{admin1, client} {
		for name, e := range map[string]admin.Event{
			"client": {At: start, Client: tok, Action: "messages.read", Reason: "ok"},
			"action": {At: start, Client: id, Action: tok, Reason: "ok"},
			"reason": {At: start, Client: id, Action: "messages.read", Reason: tok},
		} {
			if err := f.store.Audit().Record(t.Context(), e); err == nil {
				t.Fatalf("a token as the %s was recorded", name)
			}
		}
	}
	if f.audit.String() != "" || len(auditRows(t, f)) != 0 {
		t.Fatal("a refused event left a row or a line")
	}
}

const syntheticID = "SYNTHETIC1"

func mediaChainFixture(t *testing.T) (*fixture, []admin.Head) {
	t.Helper()
	f := open(t)
	c, _ := f.create(t, policy.ClientSpec{Name: "agent", Read: []string{phoneA, groupG}})
	g, message := chat(t, groupG), cursor.Ref{Chat: groupG, ID: syntheticID, Sender: phoneA}
	device := cursor.Ref{Chat: groupG, ID: syntheticID, Sender: "15550100001:7@s.whatsapp.net"}
	for i, e := range []admin.Event{
		{Client: c.ID, Action: "messages.read", OK: true, Reason: "ok", Chat: g},
		{Client: c.ID, Action: "rest.media", OK: true, Reason: "ok", Chat: g, Message: message, Bytes: 4096},
		{Client: c.ID, Action: "rest.media", OK: true, Reason: "ok", Chat: g, Message: message},
		{Client: c.ID, Action: "rest.media.sent", Reason: "client_gone", Chat: g, Message: device, Bytes: 1000, Peer: "127.0.0.1:50000"},
		{Client: c.ID, Action: "rest.media", Reason: "not_found", Peer: "127.0.0.1:50000"},
	} {
		e.At = start.Add(time.Duration(i+1) * time.Second)
		if err := f.store.Audit().Record(t.Context(), e); err != nil {
			t.Fatalf("Record %d: %v", i, err)
		}
	}
	heads := shippedHeads(t, f)
	if len(heads) != 6 {
		t.Fatalf("%d audit lines, want 6", len(heads))
	}
	return f, heads
}

func messageHMAC(key []byte, chat, id, sender string) []byte {
	b := []byte("wawarden/audit-message/v1\x00")
	for _, s := range []string{chat, id, sender} {
		b = append(b, 1)
		b = binary.AppendUvarint(b, uint64(len(s)))
		b = append(b, s...)
	}
	m := hmac.New(sha256.New, key)
	m.Write(b)
	return m.Sum(nil)[:16]
}

func readMessageRows(t *testing.T, d *sql.DB) []admin.AuditRow {
	t.Helper()
	rows, err := d.QueryContext(t.Context(), "SELECT id, ts, client_id, action, chat, chat_hmac, ok, reason, peer, key_id, message_hmac, bytes FROM audit ORDER BY id")
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []admin.AuditRow
	for rows.Next() {
		var r admin.AuditRow
		if err := rows.Scan(&r.ID, &r.TS, &r.Client, &r.Action, &r.Chat, &r.ChatHMAC, &r.OK, &r.Reason, &r.Peer, &r.Key, &r.MessageHMAC, &r.Bytes); err != nil {
			t.Fatalf("Scan: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

func TestMessageRowsCarryAKeyedHMACAndTheBytes(t *testing.T) {
	f, _ := mediaChainFixture(t)
	want := messageHMAC(f.master.ChatHMACKey(), groupG, syntheticID, phoneA)
	counts := map[int]float64{2: 4096, 3: 0, 4: 1000}
	for i, line := range f.auditLines(t) {
		n, carries := counts[i]
		switch {
		case carries && (line["message_hmac"] != hex.EncodeToString(want) || line["bytes"] != n):
			t.Fatalf("line %d carries the message %v and %v bytes, want %x and %v", i, line["message_hmac"], line["bytes"], want, n)
		case !carries && (line["message_hmac"] != nil || line["bytes"] != nil):
			t.Fatalf("line %d names the message %v and %v bytes, want neither", i, line["message_hmac"], line["bytes"])
		}
		if _, ok := line["message_hmac"]; !ok {
			t.Fatalf("line %d has no message_hmac key", i)
		}
		if _, ok := line["bytes"]; !ok {
			t.Fatalf("line %d has no bytes key", i)
		}
	}
	text := f.audit.String()
	for _, s := range []string{syntheticID, phoneA, groupG, "15550100001", "120363000000000001", "127.0.0.1"} {
		if strings.Contains(text, s) {
			t.Fatalf("an audit line carries %q", s)
		}
	}
	rows := readMessageRows(t, rawOpen(t, f.copyArchive(t)))
	for i, r := range rows {
		n, carries := counts[i]
		if carries != (r.MessageHMAC != nil) || carries && (!hmac.Equal(r.MessageHMAC, want) || r.Bytes != sql.NullInt64{Int64: int64(n), Valid: true}) || !carries && r.Bytes.Valid {
			t.Fatalf("row %d holds the message %x and %+v bytes", r.ID, r.MessageHMAC, r.Bytes)
		}
	}
	if rows[4].Chat.String != groupG || rows[5].Chat.Valid {
		t.Fatalf("rows %+v and %+v", rows[4], rows[5])
	}
}

func TestRowMACKeepsTheFirstLayoutAndBindsTheMessage(t *testing.T) {
	key, prev := bytes.Repeat([]byte{0x33}, 32), bytes.Repeat([]byte{0x44}, 32)
	valid := func(s string) sql.NullString { return sql.NullString{String: s, Valid: true} }
	r := admin.AuditRow{
		ID: 7, TS: start.UnixMilli(), Client: "aaaaaaaa", Action: "messages.read", Reason: "ok", Key: "0a1b2c3d", OK: true,
		Chat: valid(groupG), ChatHMAC: valid("00112233445566778899aabbccddeeff"), Peer: valid("127.0.0.1:50000"),
	}
	first := admin.AuditRow{ID: 1, TS: start.UnixMilli(), Client: "aaaaaaaa", Action: "client_create", Reason: "ok", Key: "0a1b2c3d"}
	for name, tt := range map[string]struct {
		r    admin.AuditRow
		prev []byte
		want string
	}{
		"a read":        {r, prev, "e1eb9f3aa84c6fe116cba14fd7c21795a66894dd4b0b14f3f4127f9921b0488e"},
		"the first row": {first, make([]byte, 32), "a8bbc0eca5eec196dfda119c484ad6dfd83209202e1655539d3d039148552981"},
	} {
		if got := hex.EncodeToString(admin.RowMAC(key, tt.r, tt.prev)); got != tt.want {
			t.Fatalf("%s: the row HMAC is %s, want the version 1 value %s", name, got, tt.want)
		}
	}
	v2 := r
	v2.MessageHMAC, v2.Bytes = bytes.Repeat([]byte{0x55}, 16), sql.NullInt64{Int64: 4096, Valid: true}
	b, _ := binary.Append([]byte("wawarden/audit-row/v2\x00"), binary.BigEndian, [2]int64{v2.ID, v2.TS})
	for _, f := range []struct {
		present bool
		s       string
	}{{true, "aaaaaaaa"}, {true, "messages.read"}, {true, groupG}, {true, "00112233445566778899aabbccddeeff"}, {true, ""}, {true, "ok"}, {true, "127.0.0.1:50000"}, {true, string(v2.MessageHMAC)}, {true, "4096"}, {true, "0a1b2c3d"}} {
		b = append(b, 1)
		b = binary.AppendUvarint(b, uint64(len(f.s)))
		b = append(b, f.s...)
	}
	m := hmac.New(sha256.New, key)
	m.Write(append(b, prev...))
	if !hmac.Equal(admin.RowMAC(key, v2, prev), m.Sum(nil)) {
		t.Fatal("a row naming a message is not MACed under the version 2 layout")
	}
	seen := map[string]string{hex.EncodeToString(admin.RowMAC(key, r, prev)): "no message", hex.EncodeToString(admin.RowMAC(key, v2, prev)): "the message"}
	for name, change := range map[string]func(*admin.AuditRow){
		"bytes alone":       func(r *admin.AuditRow) { r.MessageHMAC = nil },
		"zero bytes alone":  func(r *admin.AuditRow) { r.MessageHMAC, r.Bytes.Int64 = nil, 0 },
		"the message alone": func(r *admin.AuditRow) { r.Bytes = sql.NullInt64{} },
		"zero bytes":        func(r *admin.AuditRow) { r.Bytes.Int64 = 0 },
		"one byte more":     func(r *admin.AuditRow) { r.Bytes.Int64++ },
		"another message":   func(r *admin.AuditRow) { r.MessageHMAC = bytes.Repeat([]byte{0x56}, 16) },
	} {
		changed := v2
		change(&changed)
		mac := hex.EncodeToString(admin.RowMAC(key, changed, prev))
		if other, dup := seen[mac]; dup {
			t.Fatalf("%s has the row HMAC of %s", name, other)
		}
		seen[mac] = name
	}
}

func TestVerifyDetectsMessageTampers(t *testing.T) {
	f, heads := mediaChainFixture(t)
	if intact := verify(t, f, f.copyArchive(t), heads); !intact.OK() || intact.Rows != 6 || intact.Verified != 6 || intact.HeadsMissing != 0 {
		t.Fatalf("the intact chain reports %+v", intact)
	}
	for name, tt := range map[string]struct {
		update string
		row    int64
	}{
		"message HMAC edited":          {"message_hmac = randomblob(16)", 3},
		"message HMAC widened":         {"message_hmac = CAST(message_hmac || x'00' AS BLOB)", 5},
		"bytes edited":                 {"bytes = bytes - 1", 3},
		"zero bytes edited":            {"bytes = 1", 4},
		"message HMAC removed":         {"message_hmac = NULL", 3},
		"bytes removed":                {"bytes = NULL", 4},
		"both removed":                 {"message_hmac = NULL, bytes = NULL", 5},
		"message HMAC added to a read": {"message_hmac = randomblob(16)", 2},
		"bytes added to a read":        {"bytes = 0", 2},
		"bytes added to a refusal":     {"bytes = 0", 6},
		"message swapped from a row":   {"message_hmac = (SELECT message_hmac FROM audit WHERE id = 3), bytes = (SELECT bytes FROM audit WHERE id = 3)", 2},
	} {
		t.Run(name, func(t *testing.T) {
			path := f.copyArchive(t)
			d := rawOpen(t, path)
			exec(t, d, unlock, "UPDATE audit SET "+tt.update+" WHERE id = "+strconv.FormatInt(tt.row, 10))
			if err := d.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
			rep := verify(t, f, path, heads)
			if rep.OK() || rep.Bad != 1 || rep.FirstBad != tt.row || rep.Problem != admin.ReasonHMACMismatch || rep.HeadsMissing != 0 {
				t.Fatalf("Verify = %+v, want row %d alone refused for its HMAC", rep, tt.row)
			}
		})
	}
}

func TestVerifyReadsACopyFromBeforeTheMessageColumns(t *testing.T) {
	f, heads := chainFixture(t)
	path := f.copyArchive(t)
	d := rawOpen(t, path)
	exec(t, d, "ALTER TABLE audit DROP COLUMN message_hmac", "ALTER TABLE audit DROP COLUMN bytes")
	if err := d.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if rep := verify(t, f, path, heads); !rep.OK() || rep.Rows != 12 || rep.Verified != 12 || rep.HeadsMissing != 0 {
		t.Fatalf("Verify of a copy without the message columns = %+v", rep)
	}
}
