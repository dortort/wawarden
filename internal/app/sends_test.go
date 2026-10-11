package app

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dortort/wawarden/internal/api"
	"github.com/dortort/wawarden/internal/engine"
	"github.com/dortort/wawarden/internal/policy"
	"github.com/dortort/wawarden/internal/sender"
	"github.com/dortort/wawarden/internal/store/ingest"
	"github.com/dortort/wawarden/internal/token"
)

func TestTheAPIKeyRuleIsTheStoresKeyRule(t *testing.T) {
	cases := []string{"", "1234567", "12345678", "synthetic key", "synthetic-ké", "tab\tkey-0001", string(make([]byte, 129)),
		"~!@#$%^&*()_+{}|:<>?", "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}
	for n := range 140 {
		for _, c := range []byte{0x00, 0x20, 0x21, 0x41, 0x7e, 0x7f, 0x80, 0xc3} {
			cases = append(cases, strings.Repeat("k", n)+string([]byte{c}))
		}
	}
	for _, k := range cases {
		_, store := ingest.ParseKey(k)
		if api.WellFormedKey(k) != store {
			t.Fatalf("the api and the store disagree on %q: %v and %v", k, api.WellFormedKey(k), store)
		}
	}
}

func TestEngineOutcomesBecomeSenderClasses(t *testing.T) {
	for _, tt := range []struct {
		err  error
		want error
		code int
	}{
		{err: engine.ErrNotConnected, want: sender.ErrNotConnected},
		{err: engine.ErrSendBusy, want: sender.ErrEngineBusy},
		{err: engine.ErrRecipientUnavailable, want: sender.ErrRejected},
		{err: &engine.RejectedError{Code: 463}, want: sender.ErrRejected, code: 463},
		{err: engine.ErrRejected, want: sender.ErrRejected},
		{err: engine.ErrOutcomeUnknown, want: sender.ErrOutcomeUnknown},
		{err: errors.New("synthetic unclassified"), want: sender.ErrOutcomeUnknown},
	} {
		got := senderError(tt.err)
		rejection, rejected := errors.AsType[*sender.Rejection](got)
		if !errors.Is(got, tt.want) || rejected != errors.Is(tt.want, sender.ErrRejected) || (rejected && rejection.Code != tt.code) {
			t.Fatalf("senderError(%v) = %#v, want %v with code %d", tt.err, got, tt.want, tt.code)
		}
	}
}

func TestEngineRequestsCarryTheCanonicalForms(t *testing.T) {
	alice, _ := policy.Normalize("15550100001@s.whatsapp.net")
	lid, _ := policy.Normalize("100000000000001@lid")
	req := engineRequest(sender.Message{ID: "3EB0SYNTHETIC", Chat: lid, Text: "synthetic", Expiration: 24 * time.Hour,
		Quote: &sender.Quote{ID: "Q1", Participant: alice, Text: "quoted"}})
	want := engine.SendRequest{ID: "3EB0SYNTHETIC", Chat: "100000000000001@lid", Text: "synthetic", Expiration: 24 * time.Hour,
		Quote: &engine.SendQuote{ID: "Q1", Participant: "15550100001@s.whatsapp.net", Text: "quoted"}}
	if req.ID != want.ID || req.Chat != want.Chat || req.Text != want.Text || req.Expiration != want.Expiration || *req.Quote != *want.Quote {
		t.Fatalf("engineRequest = %+v %+v", req, req.Quote)
	}
	if req := engineRequest(sender.Message{ID: "X", Chat: alice, Text: "t"}); req.Quote != nil || req.Expiration != 0 {
		t.Fatalf("a plain message became %+v", req)
	}
	at := time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC)
	if ack := ackOf(engine.Sent{ID: "X", Chat: "15550100001@s.whatsapp.net", Sender: "100000000000009@lid", Timestamp: at}); ack.Sender.JID() != "100000000000009@lid" || !ack.At.Equal(at) {
		t.Fatalf("ackOf = %+v", ack)
	}
}

func TestSenderRefusalsBecomeFixedAnswersAndCountReasons(t *testing.T) {
	for _, tt := range []struct {
		err    error
		want   error
		reason string
	}{
		{err: &ingest.RateLimitError{Reason: ingest.ReasonMinute, RetryAfter: 42 * time.Second}, want: api.RateLimited(42 * time.Second), reason: "budget_minute"},
		{err: &ingest.RateLimitError{Reason: ingest.ReasonHour, RetryAfter: time.Hour}, want: api.RateLimited(time.Hour), reason: "budget_hour"},
		{err: &ingest.RateLimitError{Reason: ingest.ReasonGlobal, RetryAfter: 9 * time.Second}, want: api.RateLimited(9 * time.Second), reason: "budget_global"},
		{err: &ingest.RateLimitError{Reason: ingest.ReasonPacer, RetryAfter: 3 * time.Second}, want: api.RateLimited(3 * time.Second), reason: "pacer"},
		{err: sender.ErrEngineBusy, want: api.RateLimited(time.Second), reason: "pacer"},
		{err: ingest.ErrFirstContact, want: api.ErrFirstContact, reason: "first_contact"},
		{err: ingest.ErrKeyReused, want: api.ErrKeyReused},
		{err: ingest.ErrNotReady, want: api.ErrNotConnected},
		{err: sender.ErrNotConnected, want: api.ErrNotConnected},
		{err: ingest.ErrNotAllowed, want: api.ErrNotFound},
		{err: &sender.Failure{Err: sender.ErrInProgress, RetryAfter: 4 * time.Second}, want: api.InProgress(4 * time.Second)},
		{err: &sender.Failure{Err: sender.ErrRejected, Replayed: true}, want: api.ErrSendRejected},
		{err: &sender.Failure{Err: sender.ErrOutcomeUnknown}, want: api.ErrSendOutcomeUnknown},
		{err: context.DeadlineExceeded, want: api.ErrBusy},
		{err: sender.ErrUnrecorded, want: sender.ErrUnrecorded},
	} {
		var counted []string
		s := sendService{rejected: func(reason string) { counted = append(counted, reason) }}
		got := s.answer(tt.err)
		if !reflect.DeepEqual(got, tt.want) || (tt.reason == "" && len(counted) != 0) || (tt.reason != "" && !slices.Equal(counted, []string{tt.reason})) {
			t.Fatalf("answer(%v) = %v counting %q, want %v counting %q", tt.err, got, counted, tt.want, tt.reason)
		}
	}
}

func TestEveryRefusalReasonIsCreatedAtStart(t *testing.T) {
	for _, reason := range []ingest.RateReason{ingest.ReasonMinute, ingest.ReasonHour, ingest.ReasonGlobal, ingest.ReasonPacer} {
		if !slices.Contains(sendRejectReasons, reason.String()) {
			t.Fatalf("%s is counted but not created at start", reason)
		}
	}
	for _, reason := range []string{reasonFirstContact, reasonPacer, "scope"} {
		if !slices.Contains(sendRejectReasons, reason) {
			t.Fatalf("%s is counted but not created at start", reason)
		}
	}
}

func TestLiftedSendCapsAreAnnounced(t *testing.T) {
	cfg := testConfig(t, token.NewAdmin())
	cfg.UnsafeRateCaps = true
	a, logs := open(t, cfg, noClients{})
	_ = a.closeStores()
	if got := logs.find("unsafe_send_caps"); len(got) != 0 {
		t.Fatalf("unsafe_send_caps logged with the caps in place: %v", got)
	}
	cfg = testConfig(t, token.NewAdmin())
	cfg.UnsafeSendCaps, cfg.SendGlobal, cfg.SendPerMinute = true, 600, 60
	a, logs = open(t, cfg, noClients{})
	_ = a.closeStores()
	got := logs.find("unsafe_send_caps")
	if len(got) != 1 || got[0]["level"] != "WARN" || got[0]["send_global_per_hour"] != float64(600) || got[0]["send_per_client_per_minute"] != float64(60) {
		t.Fatalf("unsafe_send_caps events = %v", got)
	}
	if len(logs.find("unsafe_rate_caps")) != 0 {
		t.Fatal("lifting the send caps announced lifted read caps")
	}
}

func TestPendingSendsFailAtStartAndAreReported(t *testing.T) {
	cfg := testConfig(t, token.NewAdmin())
	a, _ := open(t, cfg, noClients{})
	alice, _ := policy.Normalize("15550100001@s.whatsapp.net")
	if err := a.archive.Write(t.Context(), "test.inbound", func(tx *ingest.Tx) error {
		_, _, err := tx.InsertMessage(ingest.Message{Chat: alice, ID: "M1", Sender: alice, Origin: ingest.OriginLive, Timestamp: time.Now().Add(-time.Minute),
			Kind: ingest.KindText, Text: "synthetic", Ingested: time.Now()})
		return err
	}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	g, _ := policy.DecideWrite(&policy.Client{ID: "aaaaaaaa", Write: map[policy.CanonicalChat]struct{}{alice: {}}, ExpiresAt: time.Now().Add(time.Hour)}, time.Now())
	for _, k := range []string{"pending-key-01", "pending-key-02"} {
		key, _ := ingest.ParseKey(k)
		if _, err := a.archive.ReserveSend(g, t.Context(), ingest.SendReservation{Key: key, Chat: alice, MsgID: "3EB0" + k, Now: time.Now(),
			Limits: ingest.SendLimits{PerMinute: 10, GlobalPerHour: 30, Holders: 1}, Ready: func() bool { return true }}); err != nil {
			t.Fatalf("ReserveSend: %v", err)
		}
	}
	if err := a.closeStores(); err != nil {
		t.Fatalf("closeStores: %v", err)
	}
	a, logs := open(t, cfg, noClients{})
	stop := run(t, a)
	logs.waitFor(t, "ready")
	if err := stop(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := logs.find("send_outcome_unknown")
	if len(got) != 2 || got[0]["reason"] != "restart" || got[1]["reason"] != "restart" {
		t.Fatalf("send_outcome_unknown events = %v", got)
	}
	key, _ := ingest.ParseKey("pending-key-01")
	reopened, _ := open(t, cfg, noClients{})
	defer func() { _ = reopened.closeStores() }()
	res, err := reopened.archive.ReserveSend(g, t.Context(), ingest.SendReservation{Key: key, Chat: alice, MsgID: "3EB0X", Now: time.Now(),
		Limits: ingest.SendLimits{PerMinute: 10, GlobalPerHour: 30, Holders: 1}, Ready: func() bool { return true }})
	if err != nil || res.Kind != ingest.ReplayedFailed || res.Code != ingest.CodeUnknown {
		t.Fatalf("the recovered key replays %+v, %v, want the outcome unknown", res, err)
	}
}
