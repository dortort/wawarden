package wa

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waSyncAction"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"github.com/dortort/wawarden/internal/engine"
	"github.com/dortort/wawarden/internal/logx"
	"github.com/dortort/wawarden/internal/metrics"
	"github.com/dortort/wawarden/internal/notify"
	"github.com/dortort/wawarden/internal/policy"
	"github.com/dortort/wawarden/internal/store/ingest"
	"github.com/dortort/wawarden/internal/store/scoped"
)

type noVersions struct{ t *testing.T }

func (v noVersions) Latest(context.Context) (engine.Version, error) {
	v.t.Error("the engine fetched a version beyond its restart budget")
	return engine.Version{}, errVersionFetch
}

type wired struct {
	c       *Client
	archive *ingest.Store
	reg     *metrics.Registry
	logs    *logBuffer
}

func wireEngine(t *testing.T) *wired {
	t.Helper()
	dir := t.TempDir()
	w, logs := newWriter()
	logger := logx.New(w, slog.LevelInfo)
	archive, err := ingest.Open(t.Context(), ingest.Options{DataDir: dir, UID: os.Geteuid(), Profile: ingest.ProfileLocal, Logger: logger})
	if err != nil {
		t.Fatalf("ingest.Open: %v", err)
	}
	t.Cleanup(func() { _ = archive.Close() })
	c, err := newClient(Options{Device: pairedDevice(), Devices: &freshDevices{}, OwnerPhone: ownerPhone, HistoryMaxBytes: 1 << 20, Writer: w, LogLevel: slog.LevelInfo, Alerts: logger}, refusingDial(t), noDownload, time.Now)
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}
	reg := metrics.NewRegistry()
	notifier, err := notify.New(notify.Options{Writer: w})
	if err != nil {
		t.Fatalf("notify.New: %v", err)
	}
	e, err := engine.New(engine.Options{
		Client: c, Versions: noVersions{t}, Decoder: c, Archive: archive, DataDir: dir, OwnerPhone: ownerPhone,
		HistoryMaxBytes: 1 << 20, Logger: logger, Notify: notifier, Metrics: reg,
	})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	e.Start(t.Context(), engine.MaxRecentStarts+1)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 10*time.Second)
		defer cancel()
		if err := e.Stop(ctx); err != nil {
			t.Errorf("Stop: %v", err)
		}
	})
	if st := e.Status(); st.State != engine.StateDisconnected || st.Reason != engine.ReasonRestartBudget {
		t.Fatalf("engine status %+v, want it held by its restart budget so that nothing connects", st)
	}
	return &wired{c: c, archive: archive, reg: reg, logs: logs}
}

func (w *wired) send(t *testing.T, evt any) {
	t.Helper()
	w.c.mu.Lock()
	gen := w.c.gen
	w.c.mu.Unlock()
	if !w.c.handlerFor(gen)(evt) {
		t.Fatalf("the engine refused %T", evt)
	}
}

func canonical(t *testing.T, jid types.JID) policy.CanonicalChat {
	t.Helper()
	c, ok := policy.Normalize(jid.String())
	if !ok {
		t.Fatalf("Normalize(%s) failed", jid)
	}
	return c
}

func (w *wired) find(t *testing.T, chat types.JID, id string, sender types.JID) (ingest.Found, bool) {
	t.Helper()
	var found ingest.Found
	var ok bool
	if err := w.archive.Read(t.Context(), "test.find", func(r *ingest.Reader) error {
		var err error
		found, ok, err = r.ResolveInChat(canonical(t, chat), id, canonical(t, sender))
		return err
	}); err != nil {
		t.Fatalf("Read: %v", err)
	}
	return found, ok
}

func (w *wired) metric(t *testing.T, sample string) bool {
	t.Helper()
	var b strings.Builder
	if err := w.reg.WriteText(&b); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	return strings.Contains(b.String(), sample+"\n")
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting until %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestFixturesTakeEffectThroughTheEngine(t *testing.T) {
	w := wireEngine(t)
	w.send(t, &events.JoinedGroup{GroupInfo: types.GroupInfo{
		JID: group, GroupName: types.GroupName{Name: "Synthetic group"},
		Participants: []types.GroupParticipant{{JID: admin, IsAdmin: true}, {JID: victim}, {JID: peer}},
	}})
	sender := types.JID{User: peer.User, Device: 3, Server: types.DefaultUserServer}
	w.send(t, textMessage(t, peer, sender, "3EB0A1", "synthetic original"))
	w.send(t, textMessage(t, group, victim, "3EB0G9", "synthetic group text"))
	w.send(t, textMessage(t, group, peer, "3EB0G8", "synthetic other group text"))
	w.send(t, editMessage(t, peer, peer, "3EB0E1", msgKey(owner, true, "3EB0A1", nil), "synthetic edited"))
	w.send(t, revokeMessage(t, group, admin, "3EB0R3", msgKey(peer, false, "3EB0A1", &peer), types.EditAttributeAdminRevoke))
	w.send(t, revokeMessage(t, group, admin, "3EB0R2", msgKey(group, false, "3EB0G9", &victim), types.EditAttributeAdminRevoke))
	w.send(t, revokeMessage(t, group, victim, "3EB0R4", msgKey(group, false, "3EB0G8", &peer), types.EditAttributeAdminRevoke))
	w.send(t, live(t, peer, peer, "3EB0Z1", &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{Key: msgKey(peer, false, "3EB0A1", nil)}}))
	w.send(t, live(t, types.StatusBroadcastJID, peer, "3EB0S1", &waE2E.Message{Conversation: proto.String("synthetic status")}))
	w.send(t, live(t, channel, channel, "3EB0N1", &waE2E.Message{Conversation: proto.String("synthetic post")}))
	w.send(t, historyNotification(t, companion, "3EB0H2", nil))
	w.send(t, textMessage(t, peer, peer, "3EB0LAST", "synthetic marker"))

	eventually(t, "the last message is stored", func() bool { _, ok := w.find(t, peer, "3EB0LAST", peer); return ok })
	original, ok := w.find(t, peer, "3EB0A1", peer)
	if !ok || original.Text != "synthetic edited" || original.Revoked || original.EditedAt.IsZero() {
		t.Fatalf("the direct message = %+v, %v: the edit applies, while the revoke from another chat and the untyped protocol message do not", original, ok)
	}
	revoked, ok := w.find(t, group, "3EB0G9", victim)
	if !ok || !revoked.Revoked || revoked.Text != "" {
		t.Fatalf("the group message = %+v, %v: an admin's revoke applies", revoked, ok)
	}
	kept, ok := w.find(t, group, "3EB0G8", peer)
	if !ok || kept.Revoked {
		t.Fatalf("the other group message = %+v, %v: a member who is no admin cannot revoke it", kept, ok)
	}
	for _, sample := range []string{
		`wawarden_ingest_dropped_total{reason="chat_rejected"} 2`,
		`wawarden_ingest_dropped_total{reason="foreign_reference"} 1`,
		`wawarden_ingest_dropped_total{reason="not_admin"} 1`,
		`wawarden_ingest_dropped_total{reason="history_not_primary"} 1`,
	} {
		if !w.metric(t, sample) {
			t.Errorf("no %s", sample)
		}
	}
	for _, leak := range []string{"synthetic", "15550100001", "120363000000000001"} {
		if strings.Contains(w.logs.String(), leak) {
			t.Fatalf("%q reached the logs:\n%s", leak, w.logs.String())
		}
	}
}

func TestASavedContactNameTakesEffectThroughTheEngine(t *testing.T) {
	w := wireEngine(t)
	w.send(t, textMessage(t, group, peer, "3EB0G1", "synthetic group text"))
	w.send(t, contactEvent(t, peer, &waSyncAction.ContactAction{FullName: proto.String("Synthetic Saved Peer"), FirstName: proto.String("Synthetic")}))
	w.send(t, contactEvent(t, group, &waSyncAction.ContactAction{FullName: proto.String("Synthetic Saved Group")}))
	w.send(t, textMessage(t, other, other, "3EB0LAST", "synthetic marker"))
	eventually(t, "the last message is stored", func() bool { _, ok := w.find(t, other, "3EB0LAST", other); return ok })
	if !w.metric(t, `wawarden_ingest_dropped_total{reason="chat_rejected"} 1`) {
		t.Error("the saved name of a group was not dropped")
	}
	now := time.Now()
	grant := func(jids ...types.JID) policy.ReadGrant {
		read := map[policy.CanonicalChat]struct{}{}
		for _, jid := range jids {
			read[canonical(t, jid)] = struct{}{}
		}
		g, ok := policy.DecideRead(&policy.Client{ID: "client01", Read: read, ExpiresAt: now.Add(time.Hour)}, now)
		if !ok {
			t.Fatal("DecideRead refused a live client")
		}
		return g
	}
	chats, err := w.archive.Scoped().Chats(grant(group), t.Context(), scoped.ChatPosition{}, 5)
	if err != nil || len(chats.Chats) != 1 {
		t.Fatalf("Chats = %+v, %v", chats, err)
	}
	for _, tt := range []struct {
		g    policy.ReadGrant
		want string
	}{{grant(group), ""}, {grant(group, other), ""}, {grant(group, peer), "Synthetic Saved Peer"}} {
		page, _, err := w.archive.Scoped().Messages(tt.g, t.Context(), chats.Chats[0].Ref, scoped.MessagePosition{}, scoped.Older, 5)
		if err != nil || len(page.Messages) != 1 || page.Messages[0].PushName != "Synthetic Peer" || page.Messages[0].SavedName != tt.want {
			t.Fatalf("Messages = %+v, %v, want the saved name %q", page, err, tt.want)
		}
	}
	for _, leak := range []string{"Saved", "15550100001", "120363000000000001"} {
		if strings.Contains(w.logs.String(), leak) {
			t.Fatalf("%q reached the logs:\n%s", leak, w.logs.String())
		}
	}
}

func TestAnEditResentByThePhoneTakesEffectThroughTheEngine(t *testing.T) {
	w := wireEngine(t)
	w.send(t, textMessage(t, peer, peer, "3EB0A1", "synthetic original"))
	w.send(t, resent(t, w.c.current(), peer, false, "3EB0E9", editContent(msgKey(owner, true, "3EB0A1", nil), "synthetic edited via resend")))
	w.send(t, resent(t, w.c.current(), peer, false, "3EB0E8", editContent(msgKey(owner, true, "3EB0A2", nil), "synthetic edit of an unknown message")))
	w.send(t, textMessage(t, peer, peer, "3EB0LAST", "synthetic marker"))
	eventually(t, "the last message is stored", func() bool { _, ok := w.find(t, peer, "3EB0LAST", peer); return ok })
	edited, ok := w.find(t, peer, "3EB0A1", peer)
	if !ok || edited.Text != "synthetic edited via resend" || edited.EditedAt.IsZero() {
		t.Fatalf("the edited message = %+v, %v: an edit the phone resends must apply to its target", edited, ok)
	}
	if stray, ok := w.find(t, peer, "3EB0A2", peer); ok {
		t.Fatalf("an edit of an unknown message was stored as its target: %+v", stray)
	}
	for _, id := range []string{"3EB0E9", "3EB0E8"} {
		if row, ok := w.find(t, peer, id, peer); ok && row.Text != "" {
			t.Fatalf("the edit %s was stored with text %+v", id, row)
		}
	}
}

func TestAHistoryBlobTakesEffectThroughTheEngine(t *testing.T) {
	w := wireEngine(t)
	blob := compress(t, historyBlob(t, &waHistorySync.HistorySync{
		SyncType:      waHistorySync.HistorySync_INITIAL_BOOTSTRAP.Enum(),
		Conversations: []*waHistorySync.Conversation{conv(peer.String(), webMessage(peer, false, "3EB0HA", &waE2E.Message{Conversation: proto.String("synthetic old text")}, nil))},
	}))
	w.send(t, historyNotification(t, owner, "3EB0H9", blob))
	eventually(t, "the history row is stored", func() bool { _, ok := w.find(t, peer, "3EB0HA", peer); return ok })
	if !w.metric(t, "wawarden_messages_ingested_total 1") {
		t.Fatal("the history row was not counted as ingested")
	}
}
