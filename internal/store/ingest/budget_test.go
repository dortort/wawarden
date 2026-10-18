package ingest

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dortort/wawarden/internal/policy"
)

func budgeted(t *testing.T, k string, at time.Time, limits SendLimits, gap time.Duration) SendReservation {
	t.Helper()
	in := request(t, k, alice, "synthetic "+k, at)
	in.Limits, in.PacerGap = limits, gap
	return in
}

func refused(t *testing.T, s *Store, g policy.WriteGrant, in SendReservation, reason RateReason, retry time.Duration) {
	t.Helper()
	_, err := s.ReserveSend(g, t.Context(), in)
	var rl *RateLimitError
	if !errors.As(err, &rl) || rl.Reason != reason || rl.RetryAfter != retry {
		t.Fatalf("ReserveSend at %v = %v, want refused by %v, retry after %v", in.Clock().Sub(epoch), err, reason, retry)
	}
	if got := ledger(t, s, g.Client(), in.Key.Value()); got != "none" {
		t.Fatalf("a refused send left a ledger row: %q", got)
	}
}

func TestTheMinuteBudget(t *testing.T) {
	s := inboundStore(t)
	g := grantFor(t, writerID, false, alice)
	limits := SendLimits{PerMinute: 3, GlobalPerHour: 60, Holders: 1}
	for i, at := range []time.Duration{0, 10 * time.Second, 20 * time.Second} {
		reserved(t, s, g, budgeted(t, "minute-0"+string(rune('1'+i)), epoch.Add(at), limits, 0))
	}
	refused(t, s, g, budgeted(t, "minute-04", epoch.Add(30*time.Second), limits, 0), ReasonMinute, 30*time.Second)
	refused(t, s, g, budgeted(t, "minute-04", epoch.Add(time.Minute-time.Millisecond), limits, 0), ReasonMinute, time.Second)
	reserved(t, s, g, budgeted(t, "minute-04", epoch.Add(time.Minute), limits, 0))
	limits.PerMinute = 1
	refused(t, s, g, budgeted(t, "minute-05", epoch.Add(61*time.Second), limits, 0), ReasonMinute, 59*time.Second)
	if _, err := s.ReserveSend(g, t.Context(), budgeted(t, "minute-01", epoch.Add(61*time.Second), limits, 0)); err != nil {
		t.Fatalf("a replay with the budget spent = %v, want it answered from the ledger", err)
	}
}

func TestTheHourShareFollowsTheWriteHolders(t *testing.T) {
	s := inboundStore(t)
	g := grantFor(t, writerID, false, alice)
	three := SendLimits{PerMinute: 10, GlobalPerHour: 6, Holders: 3}
	reserved(t, s, g, budgeted(t, "hour-key-01", epoch, three, 0))
	reserved(t, s, g, budgeted(t, "hour-key-02", epoch.Add(time.Minute), three, 0))
	refused(t, s, g, budgeted(t, "hour-key-03", epoch.Add(2*time.Minute), three, 0), ReasonHour, 58*time.Minute)
	two := three
	two.Holders = 2
	reserved(t, s, g, budgeted(t, "hour-key-03", epoch.Add(2*time.Minute), two, 0))
	refused(t, s, g, budgeted(t, "hour-key-04", epoch.Add(3*time.Minute), three, 0), ReasonHour, 58*time.Minute)
	refused(t, s, g, budgeted(t, "hour-key-04", epoch.Add(3*time.Minute), two, 0), ReasonHour, 57*time.Minute)
	none := three
	none.Holders = 0
	reserved(t, s, g, budgeted(t, "hour-key-04", epoch.Add(4*time.Minute), none, 0))
	many := three
	many.Holders = 100
	refused(t, s, g, budgeted(t, "hour-key-05", epoch.Add(time.Hour), many, 0), ReasonHour, 4*time.Minute)
	reserved(t, s, g, budgeted(t, "hour-key-05", epoch.Add(64*time.Minute), many, 0))
}

func TestTheGlobalBudgetSpansClients(t *testing.T) {
	s := inboundStore(t)
	g, other := grantFor(t, writerID, false, alice), grantFor(t, otherID, false, alice)
	limits := SendLimits{PerMinute: 10, GlobalPerHour: 3, Holders: 1}
	reserved(t, s, g, budgeted(t, "global-k01", epoch, limits, 0))
	reserved(t, s, g, budgeted(t, "global-k02", epoch.Add(time.Minute), limits, 0))
	reserved(t, s, other, budgeted(t, "global-k03", epoch.Add(2*time.Minute), limits, 0))
	refused(t, s, other, budgeted(t, "global-k04", epoch.Add(3*time.Minute), limits, 0), ReasonGlobal, 57*time.Minute)
	refused(t, s, other, budgeted(t, "global-k04", epoch.Add(time.Hour-time.Millisecond), limits, 0), ReasonGlobal, time.Second)
	reserved(t, s, other, budgeted(t, "global-k04", epoch.Add(time.Hour), limits, 0))
}

func TestTheLongestWaitIsReported(t *testing.T) {
	s := inboundStore(t)
	g := grantFor(t, writerID, false, alice)
	reserved(t, s, g, budgeted(t, "longest-01", epoch, SendLimits{PerMinute: 1, GlobalPerHour: 60, Holders: 1}, 4999*time.Millisecond))
	refused(t, s, g, budgeted(t, "longest-02", epoch.Add(time.Second), SendLimits{PerMinute: 1, GlobalPerHour: 60, Holders: 1}, 0), ReasonMinute, 59*time.Second)
	refused(t, s, g, budgeted(t, "longest-02", epoch.Add(time.Second), SendLimits{PerMinute: 1, GlobalPerHour: 2, Holders: 2}, 0), ReasonHour, time.Hour-time.Second)
	refused(t, s, g, budgeted(t, "longest-02", epoch.Add(time.Second), SendLimits{PerMinute: 10, GlobalPerHour: 1, Holders: 1}, 0), ReasonHour, time.Hour-time.Second)
	refused(t, s, g, budgeted(t, "longest-02", epoch.Add(time.Second), SendLimits{PerMinute: 10, GlobalPerHour: 60, Holders: 1}, 0), ReasonPacer, 4*time.Second)

	s = inboundStore(t)
	two := SendLimits{PerMinute: 2, GlobalPerHour: 60, Holders: 1}
	reserved(t, s, g, budgeted(t, "longest-03", epoch, two, 0))
	reserved(t, s, g, budgeted(t, "longest-04", epoch.Add(58*time.Second), two, 5*time.Second))
	refused(t, s, g, budgeted(t, "longest-05", epoch.Add(59*time.Second), two, 0), ReasonPacer, 4*time.Second)
}

func TestThePacerSpacesEverySend(t *testing.T) {
	s := inboundStore(t)
	g, other := grantFor(t, writerID, false, alice), grantFor(t, otherID, false, alice)
	reserved(t, s, g, budgeted(t, "pacer-key1", epoch, defaultLimits, 3*time.Second))
	refused(t, s, other, budgeted(t, "pacer-key2", epoch.Add(3*time.Second-time.Millisecond), defaultLimits, 0), ReasonPacer, time.Second)
	reserved(t, s, other, budgeted(t, "pacer-key2", epoch.Add(3*time.Second), defaultLimits, 4999*time.Millisecond))
	refused(t, s, g, budgeted(t, "pacer-key3", epoch.Add(4*time.Second), defaultLimits, 0), ReasonPacer, 4*time.Second)
	refused(t, s, g, budgeted(t, "pacer-key3", epoch.Add(7998*time.Millisecond), defaultLimits, 0), ReasonPacer, time.Second)
	reserved(t, s, g, budgeted(t, "pacer-key3", epoch.Add(7999*time.Millisecond), defaultLimits, 0))
	reserved(t, s, other, budgeted(t, "pacer-key4", epoch.Add(7999*time.Millisecond), defaultLimits, 0))
}

func TestReleasingASendRefundsItsBudget(t *testing.T) {
	s := inboundStore(t)
	g := grantFor(t, writerID, false, alice)
	one := SendLimits{PerMinute: 1, GlobalPerHour: 60, Holders: 1}
	first := budgeted(t, "refund-k01", epoch, one, 3*time.Second)
	reserved(t, s, g, first)
	refused(t, s, g, budgeted(t, "refund-k02", epoch.Add(10*time.Second), one, 0), ReasonMinute, 50*time.Second)
	if err := s.ReleaseSend(g, t.Context(), first.Key); err != nil {
		t.Fatalf("ReleaseSend: %v", err)
	}
	reserved(t, s, g, budgeted(t, "refund-k02", epoch.Add(10*time.Second), one, 0))
}

func uncounted(t *testing.T, s *Store, client, k string, at time.Time) {
	t.Helper()
	write(t, s, func(tx *Tx) error {
		_, err := tx.q.ExecContext(tx.ctx, `INSERT INTO idempotency (client_id, key, state, fingerprint, msg_id, reserved_at, next_ok_at, counted, expires_at)
			VALUES (?1, ?2, 'failed', zeroblob(32), 'MSG-' || ?2, ?3, ?4, 0, ?5)`, client, k, ms(at), ms(at.Add(time.Hour)), ms(at.Add(SendTTL)))
		return err
	})
}

func TestOnlyCountedRowsSpendTheBudget(t *testing.T) {
	s := inboundStore(t)
	g, other := grantFor(t, writerID, false, alice), grantFor(t, otherID, false, alice)
	uncounted(t, s, writerID, "uncounted-1", epoch)
	uncounted(t, s, otherID, "uncounted-2", epoch)
	at := epoch.Add(30 * time.Second)
	reserved(t, s, g, budgeted(t, "counted-k01", at, SendLimits{PerMinute: 1, GlobalPerHour: 1, Holders: 1}, 0))
	refused(t, s, g, budgeted(t, "counted-k02", at.Add(10*time.Second), SendLimits{PerMinute: 1, GlobalPerHour: 60, Holders: 1}, 0), ReasonMinute, 50*time.Second)
	refused(t, s, other, budgeted(t, "counted-k03", at.Add(10*time.Second), SendLimits{PerMinute: 10, GlobalPerHour: 1, Holders: 1}, 0), ReasonGlobal, time.Hour-10*time.Second)
}

func TestTheBudgetQueriesUseThePartialIndexes(t *testing.T) {
	s := openStore(t)
	for name, tt := range map[string]struct {
		query                string
		args                 []any
		clientWindow, window int
	}{
		"the window counts":     {selectSendWindows, []any{writerID, 0, 0, "budget-key"}, 2, 2},
		"the client admit time": {selectClientAdmit, []any{writerID, 0, "budget-key", 0}, 1, 0},
		"the global admit time": {selectGlobalAdmit, []any{writerID, 0, "budget-key", 0}, 0, 1},
	} {
		plan := queryPlan(t, s, tt.query, tt.args...)
		if strings.Count(plan, "INDEX idempotency_client_window (") != tt.clientWindow || strings.Count(plan, "INDEX idempotency_window (") != tt.window || strings.Contains(plan, "SCAN idempotency") {
			t.Errorf("%s plans as %q, want idempotency_client_window %d and idempotency_window %d times and no scan", name, plan, tt.clientWindow, tt.window)
		}
	}
}

func TestTheSendBackfillAndBlobQueriesUseTheirIndexes(t *testing.T) {
	s := openStore(t)
	backfills := "SEARCH backfills USING COVERING INDEX backfills_requested (requested_at>?)"
	for name, tt := range map[string]struct {
		query, want string
		args        []any
	}{
		"the chat timer":        {selectChatTimer, "SEARCH messages USING INDEX messages_chat_expiring (chat_jid=?)", []any{alice}},
		"the idempotency sweep": {sweepSends, "SEARCH idempotency USING INDEX idempotency_expiry (expires_at<?)", []any{0}},
		"the backfill count":    {countBackfills, backfills, []any{0}},
		"the backfill quota":    {selectQuota, "SCALAR SUBQUERY 1; " + backfills + "; SCALAR SUBQUERY 2; " + backfills, []any{0, 0}},
		"the backfill anchor":   {selectAnchor, "SEARCH messages USING INDEX messages_chat_ts (chat_jid=?)", []any{alice}},
		"the newest anchor":     {selectNewestAnchor, "SEARCH messages USING INDEX messages_chat_ts (chat_jid=?)", []any{alice}},
		"the sent row":          {selectSentBy, "SEARCH messages USING INDEX sqlite_autoindex_messages_1 (chat_jid=? AND id=?)", []any{alice, "3EB0SENT", writerID}},
		"the blob prune":        {pruneBlobs, "SEARCH history_blobs USING INTEGER PRIMARY KEY (rowid<?)", []any{0, 10}},
	} {
		plan := queryPlan(t, s, tt.query, tt.args...)
		scan := strings.Contains(plan, "SCAN messages") || strings.Contains(plan, "SCAN idempotency") || strings.Contains(plan, "SCAN backfills")
		if !strings.Contains(plan, tt.want) || scan || strings.Contains(plan, "TEMP B-TREE") {
			t.Errorf("%s plans as %q, want %q without a scan of the table or a temporary b-tree", name, plan, tt.want)
		}
	}
}

func TestTheBudgetSurvivesReopening(t *testing.T) {
	opts := testOptions(t)
	s := openWith(t, opts)
	insert(t, s, textMessage(t, alice, "IN1", alice, "synthetic inbound"))
	g := grantFor(t, writerID, false, alice)
	one := SendLimits{PerMinute: 1, GlobalPerHour: 60, Holders: 1}
	reserved(t, s, g, budgeted(t, "reopen-k01", epoch, one, 0))
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	again := openWith(t, opts)
	refused(t, again, g, budgeted(t, "reopen-k02", epoch.Add(time.Second), one, 0), ReasonMinute, 59*time.Second)
	if res := replay(t, again, g, budgeted(t, "reopen-k01", epoch.Add(2*time.Second), one, 0)); res.Kind != InProgress {
		t.Fatalf("replay after reopening = %+v", res)
	}
}

func TestClockStepsKeepTheBudgetConservative(t *testing.T) {
	s := inboundStore(t)
	g := grantFor(t, writerID, false, alice)
	reserved(t, s, g, budgeted(t, "clock-key1", epoch, defaultLimits, 0))
	refused(t, s, g, budgeted(t, "clock-key2", epoch.Add(-10*time.Minute), SendLimits{PerMinute: 1, GlobalPerHour: 60, Holders: 1}, 0), ReasonMinute, 11*time.Minute)
	refused(t, s, g, budgeted(t, "clock-key2", epoch.Add(-2*time.Hour), SendLimits{PerMinute: 10, GlobalPerHour: 1, Holders: 1}, 0), ReasonHour, time.Hour)
	refused(t, s, g, budgeted(t, "clock-key2", epoch.Add(-2*time.Hour), defaultLimits, 0), ReasonPacer, time.Hour)
	reserved(t, s, g, budgeted(t, "clock-key2", epoch.Add(time.Hour), SendLimits{PerMinute: 1, GlobalPerHour: 1, Holders: 1}, 0))
}

func TestInvalidReservationsAreRefused(t *testing.T) {
	s := inboundStore(t)
	g := grantFor(t, writerID, false, alice)
	base := request(t, "invalid-k01", alice, "synthetic", epoch)
	for name, tt := range map[string]struct {
		g    policy.WriteGrant
		edit func(*SendReservation)
		want error
	}{
		"a zero grant":                 {policy.WriteGrant{}, func(*SendReservation) {}, ErrNotAllowed},
		"a chat outside the grant":     {g, func(in *SendReservation) { in.Chat = chat(t, bob) }, ErrNotAllowed},
		"the zero chat":                {g, func(in *SendReservation) { in.Chat = policy.CanonicalChat{} }, ErrNotAllowed},
		"the zero key":                 {g, func(in *SendReservation) { in.Key = Key{} }, ErrInvalid},
		"no message id":                {g, func(in *SendReservation) { in.MsgID = "" }, ErrInvalid},
		"a message id with a space":    {g, func(in *SendReservation) { in.MsgID = "MSG 1" }, ErrInvalid},
		"no clock":                     {g, func(in *SendReservation) { in.Clock = nil }, ErrInvalid},
		"a clock at the zero time":     {g, func(in *SendReservation) { in.Clock = clockAt(time.Time{}) }, ErrInvalid},
		"no minute budget":             {g, func(in *SendReservation) { in.Limits.PerMinute = 0 }, ErrInvalid},
		"no global budget":             {g, func(in *SendReservation) { in.Limits.GlobalPerHour = 0 }, ErrInvalid},
		"a negative global budget":     {g, func(in *SendReservation) { in.Limits.GlobalPerHour = -1 }, ErrInvalid},
		"a negative number of holders": {g, func(in *SendReservation) { in.Limits.Holders = -1 }, ErrInvalid},
		"a negative pacer gap":         {g, func(in *SendReservation) { in.PacerGap = -time.Millisecond }, ErrInvalid},
		"no readiness probe":           {g, func(in *SendReservation) { in.Ready = nil }, ErrInvalid},
	} {
		in := base
		tt.edit(&in)
		if _, err := s.ReserveSend(tt.g, t.Context(), in); !errors.Is(err, tt.want) {
			t.Errorf("%s: ReserveSend = %v, want %v", name, err, tt.want)
		}
	}
	if n := scalar[int](t, s, "SELECT count(*) FROM idempotency"); n != 0 {
		t.Fatalf("refused reservations left %d rows", n)
	}
}

func TestAnEngineThatCannotSendRefusesAfterTheFirstContactRule(t *testing.T) {
	s := inboundStore(t)
	g := grantFor(t, writerID, false, alice, bob)
	var asked int
	notReady := func() bool { asked++; return false }
	in := request(t, "ready-key01", bob, "synthetic", epoch)
	in.Ready = notReady
	if _, err := s.ReserveSend(g, t.Context(), in); !errors.Is(err, ErrFirstContact) || asked != 0 {
		t.Fatalf("ReserveSend = %v after %d readiness probes, want the first-contact refusal before any", err, asked)
	}
	in = request(t, "ready-key01", alice, "synthetic", epoch)
	in.Ready = notReady
	if _, err := s.ReserveSend(g, t.Context(), in); !errors.Is(err, ErrNotReady) || asked != 1 {
		t.Fatalf("ReserveSend = %v after %d readiness probes, want ErrNotReady after one", err, asked)
	}
	if got := ledger(t, s, writerID, "ready-key01"); got != "none" {
		t.Fatalf("a refused send left a ledger row: %q", got)
	}
}
