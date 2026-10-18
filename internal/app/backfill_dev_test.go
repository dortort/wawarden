//go:build dev

package app

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/dortort/wawarden/internal/config"
	"github.com/dortort/wawarden/internal/engine"
	"github.com/dortort/wawarden/internal/logx"
	"github.com/dortort/wawarden/internal/store/ingest"
	"github.com/dortort/wawarden/internal/token"
)

func TestANewestAnchoredBackfillGivesAttachmentsArchivedWithoutAReferenceOne(t *testing.T) {
	adminToken := token.NewAdmin()
	cfg := testConfig(t, adminToken)
	cfg.OwnerPhone, cfg.Dev.FakeEngine = "+15550100009", true
	cfg.MediaMaxBytes, cfg.MediaRefRetentionDays = 16<<20, config.DefaultMediaRefRetentionDays
	cfg.MediaPerMinute, cfg.MediaGlobalPerHour, cfg.MediaGlobalBytesPerHour = 60000, config.MaxMediaGlobalPerHour, config.DefaultMediaGlobalBytesPerHour
	logs := &syncBuffer{}
	a, err := New(t.Context(), cfg, logx.NewWriter(logs), testMaster(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	stop := run(t, a)
	logs.waitFor(t, "ready")
	f := &fakeDownloads{t: t, to: addr(t, a, listenerClient), admin: addr(t, a, listenerAdmin), token: adminToken, served: map[string]int64{}}
	if r := post(t, f.admin, "/admin/v1/pair", bearer(adminToken), "{}"); r.status != http.StatusOK {
		t.Fatalf("pair = %d %s", r.status, r.body)
	}
	waitUntil(t, "every scripted attachment reference", func() bool { counts, _ := f.status(); return counts["media_refs_live"] == fakeAttachment })
	if err := a.archive.Write(t.Context(), "test.downloads_off", func(tx *ingest.Tx) error {
		_, err := tx.PurgeMediaRefs(time.Now(), engine.DefaultMediaRefRetention, false, 2*fakeAttachment)
		return err
	}); err != nil {
		t.Fatalf("the purge of a downloads-off sweep: %v", err)
	}
	created := post(t, f.admin, "/admin/v1/clients", bearer(adminToken), `{"name":"synthetic attachment reader","read_chats":["`+fakeChat+`"],"allow_media":true}`)
	var client struct {
		Credential string `json:"credential"`
	}
	if created.status != http.StatusOK || json.Unmarshal([]byte(created.body), &client) != nil || client.Credential == "" {
		t.Fatalf("create = %d %s", created.status, created.body)
	}
	f.auth = bearer(client.Credential)
	f.mrefs = fakeMediaRefs(t, f.to, f.auth)
	const photo, newest = 0, fakeAttachment - 1
	unavailable := func(i int, when string) {
		t.Helper()
		if r := mustDo(t, http.MethodPost, f.to, "/v1/messages/"+f.mrefs[i]+"/media", f.auth); r.status != http.StatusNotFound || r.body != `{"error":"media_unavailable"}` {
			t.Fatalf("attachment %d %s = %d %s, want 404 media_unavailable", i, when, r.status, r.body)
		}
	}
	if counts, _ := f.status(); counts["media_refs_live"] != 0 {
		t.Fatalf("status counts %v after the purge, want no reference", counts)
	}
	unavailable(photo, "archived without a reference")

	if r := post(t, f.admin, "/admin/v1/backfill", bearer(adminToken), `{"chat":"`+fakeChat+`","anchor":"newest"}`); r.status != http.StatusAccepted {
		t.Fatalf("backfill = %d %s", r.status, r.body)
	}
	waitUntil(t, "the re-delivered attachments' references", func() bool { counts, _ := f.status(); return counts["media_refs_live"] == fakeAttachment-1 })
	if r := f.rest(photo); r.status != http.StatusOK || r.header.Get("Content-Type") != "image/jpeg" {
		t.Fatalf("the photo after the backfill = %d %v, want it served", r.status, r.header)
	}
	unavailable(newest, "that anchored the backfill")
	if counts, _ := f.status(); counts["backfills_today"] != 1 || counts["media_refs_live"] != fakeAttachment-1 {
		t.Fatalf("status counts %v after the backfill", counts)
	}
	if err := stop(); err != nil {
		t.Fatalf("Run = %v", err)
	}
}
