package admin_test

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dortort/wawarden/internal/keys"
	"github.com/dortort/wawarden/internal/logx"
	"github.com/dortort/wawarden/internal/policy"
	"github.com/dortort/wawarden/internal/store/admin"
	"github.com/dortort/wawarden/internal/store/ingest"
	"github.com/dortort/wawarden/internal/token"

	_ "modernc.org/sqlite"
)

const (
	phoneA = "15550100001@s.whatsapp.net"
	lidA   = "100000000000001@lid"
	phoneB = "15550100002@s.whatsapp.net"
	groupG = "120363000000000001@g.us"
	groupH = "120363000000000002@g.us"
)

var start = time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

type clock struct {
	mu sync.Mutex
	at time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

type fixture struct {
	store   *ingest.Store
	clients *admin.Clients
	master  *keys.Master
	clock   *clock
	audit   *lockedBuffer
	logs    *lockedBuffer
	dir     string
}

func syntheticMaster(t *testing.T, first byte) *keys.Master {
	t.Helper()
	path := filepath.Join(t.TempDir(), "master")
	secret := make([]byte, 32)
	for i := range secret {
		secret[i] = first + byte(i)
	}
	if err := os.WriteFile(path, secret, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	m, r := keys.LoadFile(path)
	if r != nil {
		t.Fatalf("LoadFile: %v", r)
	}
	return m
}

func openWith(t *testing.T, dir string, master *keys.Master) *fixture {
	t.Helper()
	return openTimed(t, dir, master, 0)
}

func openTimed(t *testing.T, dir string, master *keys.Master, readTimeout time.Duration) *fixture {
	t.Helper()
	f := &fixture{master: master, clock: &clock{at: start}, audit: &lockedBuffer{}, logs: &lockedBuffer{}, dir: dir}
	logs := logx.NewWriter(f.logs)
	logs.SetKey(make([]byte, 32))
	out := logx.NewWriter(f.audit)
	out.SetKey(make([]byte, 32))
	s, err := ingest.Open(t.Context(), ingest.Options{
		DataDir: dir, UID: os.Geteuid(), Profile: ingest.ProfileLocal, Logger: logx.New(logs, slog.LevelDebug),
		Master: master, AuditOut: out, Now: f.clock.Now, ReadTimeout: readTimeout,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	f.store, f.clients = s, s.Clients()
	return f
}

func open(t *testing.T) *fixture {
	t.Helper()
	return openWith(t, t.TempDir(), syntheticMaster(t, 100))
}

func (f *fixture) seed(t *testing.T, chats ...string) {
	t.Helper()
	err := f.store.Write(t.Context(), "test.seed", func(tx *ingest.Tx) error {
		for i, jid := range chats {
			c := chat(t, jid)
			sender := c
			if c.Kind() == policy.GroupChat {
				sender = chat(t, phoneB)
			}
			if _, _, err := tx.InsertMessage(ingest.Message{
				Chat: c, ID: "M" + string(rune('A'+i)), Sender: sender, Origin: ingest.OriginLive,
				Timestamp: start.Add(-time.Hour), Kind: ingest.KindText, Text: "synthetic", Ingested: start,
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func (f *fixture) create(t *testing.T, spec policy.ClientSpec) (admin.Client, string) {
	t.Helper()
	c, full, err := f.clients.Create(t.Context(), spec)
	if err != nil {
		t.Fatalf("Create(%+v): %v", spec, err)
	}
	return c, full
}

func (f *fixture) authenticate(t *testing.T, presented string) (*policy.Client, bool) {
	t.Helper()
	c, ok, err := f.clients.Authenticate(t.Context(), presented)
	if err != nil {
		t.Fatalf("Authenticate: %v, want an answer from a loaded client list", err)
	}
	return c, ok
}

func (f *fixture) auditLines(t *testing.T) []map[string]any {
	t.Helper()
	var out []map[string]any
	for line := range strings.Lines(f.audit.String()) {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("audit line %q is not JSON: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

func TestCreateValidatesAgainstTheArchive(t *testing.T) {
	f := open(t)
	f.seed(t, phoneB, groupG)
	err := f.store.Write(t.Context(), "test.map", func(tx *ingest.Tx) error {
		_, err := tx.LearnLID(chat(t, lidA), chat(t, phoneA), ingest.MappingSenderAlt, start)
		return err
	})
	if err != nil {
		t.Fatalf("LearnLID: %v", err)
	}
	created, full := f.create(t, policy.ClientSpec{Name: "Reader", Read: []string{phoneA, phoneB, groupG, groupH}, Write: []string{phoneB, groupG}})
	if created.State != admin.StateActive || !created.CreatedAt.Equal(start) || !created.ExpiresAt.Equal(start.Add(90*24*time.Hour)) || created.AllChats {
		t.Fatalf("created %+v", created)
	}
	if id, ok := token.ParseClient(full); !ok || id != created.ID {
		t.Fatalf("credential %q does not carry the id %q", full, created.ID)
	}
	var read []string
	for _, c := range created.Read {
		read = append(read, c.Chat.JID()+map[bool]string{true: "+", false: "-"}[c.Known])
	}
	if got, want := strings.Join(read, " "), lidA+"- "+groupG+"+ "+groupH+"- "+phoneB+"+"; got != want {
		t.Fatalf("read chats %s, want %s: a phone number mapped to a LID is stored as the LID and unseen chats are known:false", got, want)
	}
	if len(created.Write) != 2 || created.ReadCount != 4 || created.WriteCount != 2 {
		t.Fatalf("write chats %+v, counts %d/%d", created.Write, created.ReadCount, created.WriteCount)
	}

	tests := []struct {
		name string
		spec policy.ClientSpec
		want error
	}{
		{name: "name taken in another case", spec: policy.ClientSpec{Name: "READER", Read: []string{phoneB}}, want: policy.ErrNameTaken},
		{name: "unseen group to write", spec: policy.ClientSpec{Name: "w1", Read: []string{groupH}, Write: []string{groupH}, AllowFirstContact: true}, want: policy.ErrWriteChatUnknown},
		{name: "unseen direct chat without first contact", spec: policy.ClientSpec{Name: "w2", Read: []string{"15550100007@s.whatsapp.net"}, Write: []string{"15550100007@s.whatsapp.net"}}, want: policy.ErrWriteChatUnknown},
		{name: "a spec the policy refuses", spec: policy.ClientSpec{Name: "w3", AllChats: true, Write: []string{phoneB}}, want: policy.ErrAllChatsWithWrite},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, full, err := f.clients.Create(t.Context(), tt.spec); !errors.Is(err, tt.want) || full != "" {
				t.Fatalf("Create = %q, %v, want %v", full, err, tt.want)
			}
		})
	}
	first, _ := f.create(t, policy.ClientSpec{Name: "first contact", Read: []string{"15550100007@s.whatsapp.net"}, Write: []string{"15550100007@s.whatsapp.net"}, AllowFirstContact: true})
	if len(first.Write) != 1 || first.Write[0].Known || !first.AllowFirstContact {
		t.Fatalf("first-contact client %+v", first)
	}
	all, _ := f.create(t, policy.ClientSpec{Name: "everything", AllChats: true, ExpiresInDays: 365})
	if !all.AllChats || len(all.Read) != 0 || len(all.Write) != 0 || !all.ExpiresAt.Equal(start.Add(365*24*time.Hour)) {
		t.Fatalf("all-chats client %+v", all)
	}
	list, err := f.clients.List(t.Context())
	if err != nil || len(list) != 3 || list[0].ID != created.ID || list[0].Read != nil || list[0].ReadCount != 4 {
		t.Fatalf("List = %+v, %v", list, err)
	}
	if got, err := f.clients.Get(t.Context(), created.ID); err != nil || got.Name != "Reader" || len(got.Read) != 4 {
		t.Fatalf("Get = %+v, %v", got, err)
	}
	for _, id := range []string{"", "aaaaaaaa", "AAAAAAAA", "x"} {
		if _, err := f.clients.Get(t.Context(), id); !errors.Is(err, admin.ErrNotFound) {
			t.Fatalf("Get(%q) = %v, want ErrNotFound", id, err)
		}
		if _, err := f.clients.Revoke(t.Context(), id); !errors.Is(err, admin.ErrNotFound) {
			t.Fatalf("Revoke(%q) = %v, want ErrNotFound", id, err)
		}
	}
}

func TestAuthenticationHashesAndComparesOncePerOutcome(t *testing.T) {
	f := open(t)
	live, liveToken := f.create(t, policy.ClientSpec{Name: "live", Read: []string{phoneA}})
	revoked, revokedToken := f.create(t, policy.ClientSpec{Name: "revoked", Read: []string{phoneA}})
	_, expiringToken := f.create(t, policy.ClientSpec{Name: "expiring", Read: []string{phoneA}, ExpiresInDays: 1})
	if _, err := f.clients.Revoke(t.Context(), revoked.ID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	_, unknown := token.NewClient()
	wrongSecret := liveToken[:12] + unknown[12:55]
	wrongSecret += "_" + checksumOf(wrongSecret)
	ops := f.clients.CountOperations()
	tests := []struct {
		name, presented string
		advance         time.Duration
		ok, dummy       bool
	}{
		{name: "valid", presented: liveToken, ok: true},
		{name: "empty", presented: "", dummy: true},
		{name: "malformed", presented: "ww_not_a_token", dummy: true},
		{name: "admin token shape", presented: "wwadm_" + strings.Repeat("A", 43) + "_00000000", dummy: true},
		{name: "unknown id", presented: unknown, dummy: true},
		{name: "wrong secret", presented: wrongSecret},
		{name: "bad checksum on a known id", presented: liveToken[:56] + "00000000", dummy: true},
		{name: "revoked", presented: revokedToken},
		{name: "expired", presented: expiringToken, advance: 24 * time.Hour},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f.clock.advance(tt.advance)
			*ops = admin.Operations{}
			c, ok := f.authenticate(t, tt.presented)
			if ok != tt.ok || (c != nil) != tt.ok {
				t.Fatalf("Authenticate = %v, %v, want %v", c, ok, tt.ok)
			}
			if ok && c.ID != live.ID {
				t.Fatalf("authenticated as %s, want %s", c.ID, live.ID)
			}
			if ops.Hashes != 1 || ops.Compares != 1 {
				t.Fatalf("%d hashes and %d compares, want exactly one of each in every outcome", ops.Hashes, ops.Compares)
			}
			if wantDummy := map[bool]int{true: 1, false: 0}[tt.dummy]; ops.AgainstDummy != wantDummy {
				t.Fatalf("%d compares against the dummy digest, want %d", ops.AgainstDummy, wantDummy)
			}
		})
	}
}

func checksumOf(body string) string {
	return fmt.Sprintf("%08x", crc32.ChecksumIEEE([]byte(body)))
}

func TestRevocationAndReKeysReachTheNextAuthentication(t *testing.T) {
	f := open(t)
	c, full := f.create(t, policy.ClientSpec{Name: "agent", Read: []string{phoneA}})
	got, ok := f.authenticate(t, full)
	if !ok || len(got.Read) != 1 {
		t.Fatalf("Authenticate = %+v, %v", got, ok)
	}
	if _, has := got.Read[chat(t, phoneA)]; !has {
		t.Fatalf("the client reads %v, want %s", got.Read, phoneA)
	}
	later, laterToken := f.create(t, policy.ClientSpec{Name: "later", Read: []string{phoneB}})
	if got, ok := f.authenticate(t, laterToken); !ok || got.ID != later.ID {
		t.Fatalf("a client created after the cache loaded = %+v, %v: the create did not invalidate the cache", got, ok)
	}
	err := f.store.Write(t.Context(), "test.rekey", func(tx *ingest.Tx) error {
		res, err := tx.LearnLID(chat(t, lidA), chat(t, phoneA), ingest.MappingSenderAlt, start)
		if err == nil && !res.Rescoped {
			err = errors.New("the mapping moved no client chat")
		}
		return err
	})
	if err != nil {
		t.Fatalf("LearnLID: %v", err)
	}
	got, ok = f.authenticate(t, full)
	if !ok {
		t.Fatal("the re-keyed client no longer authenticates")
	}
	if _, has := got.Read[chat(t, lidA)]; !has || len(got.Read) != 1 {
		t.Fatalf("after the re-key the client reads %v, want only %s: the cache kept the old scope", got.Read, lidA)
	}
	if _, err := f.clients.Revoke(t.Context(), c.ID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if got, ok := f.authenticate(t, full); ok || got != nil {
		t.Fatal("a revoked client authenticated on the next request")
	}
	again, err := f.clients.Revoke(t.Context(), c.ID)
	if err != nil || again.State != admin.StateRevoked || !again.RevokedAt.Equal(start) {
		t.Fatalf("a second revoke = %+v, %v, want the same revoked view", again, err)
	}
	if lines := f.auditLines(t); len(lines) != 3 || lines[2]["action"] != "client_revoke" {
		t.Fatalf("audit lines %v, want two creates and one revoke: a repeated revoke changes nothing", lines)
	}
}

func TestCountsFollowTheClock(t *testing.T) {
	f := open(t)
	f.create(t, policy.ClientSpec{Name: "a", Read: []string{phoneA}, ExpiresInDays: 1})
	f.create(t, policy.ClientSpec{Name: "b", AllChats: true})
	r, _ := f.create(t, policy.ClientSpec{Name: "c", Read: []string{phoneA}})
	if _, err := f.clients.Revoke(t.Context(), r.ID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if n, err := f.clients.Counts(t.Context()); err != nil || n != (admin.ClientCounts{Active: 2, Revoked: 1, AllChatsActive: 1}) {
		t.Fatalf("Counts = %+v, %v", n, err)
	}
	f.clock.advance(24 * time.Hour)
	if n, err := f.clients.Counts(t.Context()); err != nil || n != (admin.ClientCounts{Active: 1, Expired: 1, Revoked: 1, AllChatsActive: 1}) {
		t.Fatalf("Counts a day later = %+v, %v", n, err)
	}
	list, err := f.clients.List(t.Context())
	if err != nil || list[0].State != admin.StateExpired || list[1].State != admin.StateActive || list[2].State != admin.StateRevoked {
		t.Fatalf("List = %+v, %v", list, err)
	}
}

func TestTheCredentialAppearsOnlyInTheCreateResult(t *testing.T) {
	f := open(t)
	c, full := f.create(t, policy.ClientSpec{Name: "canary", Read: []string{phoneA}})
	if _, ok := f.authenticate(t, full); !ok {
		t.Fatal("Authenticate refused the credential")
	}
	f.authenticate(t, full[:63]+"x")
	if _, err := f.clients.Revoke(t.Context(), c.ID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if err := f.store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	secret := full[12:55]
	files, err := filepath.Glob(filepath.Join(f.dir, "*"))
	if err != nil {
		t.Fatalf("Glob: %v", err)
	}
	for _, name := range files {
		b, err := os.ReadFile(name) //nolint:gosec // G304: the test's own data directory
		if err != nil {
			continue
		}
		if bytes.Contains(b, []byte(secret)) {
			t.Fatalf("%s holds the credential's secret: the store keeps only its SHA-256", filepath.Base(name))
		}
	}
	for name, text := range map[string]string{"audit lines": f.audit.String(), "logs": f.logs.String()} {
		if strings.Contains(text, secret) {
			t.Fatalf("the %s hold the credential's secret", name)
		}
	}
}

func TestFailedAuthenticationsWriteNoAuditRow(t *testing.T) {
	f := open(t)
	_, full := f.create(t, policy.ClientSpec{Name: "agent", Read: []string{phoneA}})
	before := f.audit.String()
	_, other := token.NewClient()
	for _, presented := range []string{"", "garbage", full[:56] + other[56:], full[:12] + other[12:]} {
		if _, ok := f.authenticate(t, presented); ok {
			t.Fatalf("Authenticate(%q) succeeded", presented)
		}
	}
	if _, ok := f.authenticate(t, full); !ok {
		t.Fatal("Authenticate refused the credential")
	}
	if f.audit.String() != before {
		t.Fatal("an authentication wrote an audit line: authentication alone records nothing")
	}
	if rows := auditRows(t, f); len(rows) != 1 {
		t.Fatalf("%d audit rows, want the create's only", len(rows))
	}
}

func TestAnUnkeyedAuditRefusesEveryChange(t *testing.T) {
	f := openWith(t, t.TempDir(), nil)
	if _, full, err := f.clients.Create(t.Context(), policy.ClientSpec{Name: "agent", Read: []string{phoneA}}); err == nil || full != "" {
		t.Fatalf("Create without an audit key = %q, %v, want a refusal", full, err)
	}
	if list, err := f.clients.List(t.Context()); err != nil || len(list) != 0 {
		t.Fatalf("List = %+v, %v: the refused create left a client", list, err)
	}
	if err := f.store.Audit().Record(t.Context(), admin.Event{At: start, Client: "aaaaaaaa", Action: "messages.read", OK: true, Reason: "ok"}); err == nil {
		t.Fatal("Record without an audit key succeeded")
	}

	dir := t.TempDir()
	keyed := openWith(t, dir, syntheticMaster(t, 100))
	c, full := keyed.create(t, policy.ClientSpec{Name: "agent", Read: []string{phoneA}})
	if err := keyed.store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	unkeyed := openWith(t, dir, nil)
	if _, err := unkeyed.clients.Revoke(t.Context(), c.ID); err == nil {
		t.Fatal("Revoke without an audit key succeeded")
	}
	if got, err := unkeyed.clients.Get(t.Context(), c.ID); err != nil || got.State != admin.StateActive || !got.RevokedAt.IsZero() {
		t.Fatalf("Get = %+v, %v: the refused revoke committed without its audit row", got, err)
	}
	if text := unkeyed.audit.String(); text != "" {
		t.Fatalf("the refused revoke wrote the audit line %q", text)
	}
	if _, ok := unkeyed.authenticate(t, full); !ok {
		t.Fatal("the client no longer authenticates after a refused revoke")
	}
}

func (f *fixture) copyArchive(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "copy")
	err := f.store.Backup(t.Context(), filepath.Join(t.TempDir(), "staging"), func(_ string, _ int64, r io.Reader) error {
		out, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // G304: a file in the test's own directory
		if err != nil {
			return err
		}
		_, err = io.Copy(out, r)
		return errors.Join(err, out.Close())
	})
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	return path
}

func rawOpen(t *testing.T, path string) *sql.DB {
	t.Helper()
	d, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	d.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func readRows(t *testing.T, d *sql.DB) []admin.AuditRow {
	t.Helper()
	rows, err := d.QueryContext(t.Context(), "SELECT id, ts, client_id, action, chat, chat_hmac, ok, reason, peer, key_id FROM audit ORDER BY id")
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []admin.AuditRow
	for rows.Next() {
		var r admin.AuditRow
		if err := rows.Scan(&r.ID, &r.TS, &r.Client, &r.Action, &r.Chat, &r.ChatHMAC, &r.OK, &r.Reason, &r.Peer, &r.Key); err != nil {
			t.Fatalf("Scan: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

func auditRows(t *testing.T, f *fixture) []admin.AuditRow {
	t.Helper()
	return readRows(t, rawOpen(t, f.copyArchive(t)))
}
