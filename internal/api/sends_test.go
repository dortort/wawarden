package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dortort/wawarden/internal/metrics"
	"github.com/dortort/wawarden/internal/policy"
	"github.com/dortort/wawarden/internal/store/scoped"
)

const (
	sendKey     = "synthetic-key-0001"
	keyCanary   = "canary-key-7d41c0"
	textCanary  = "canary-text-93be12"
	sendTarget  = "/v1/chats/" + aliceRef + "/messages"
	sendAction  = "mcp." + toolSendMessage
	gatewayFrom = "gateway:client-alice"
)

const (
	sendText     = "synthetic hello\n\twith a \U0001F469\u200d\U0001F4BB"
	sendBodyText = `{"text":"synthetic hello\n\twith a \ud83d\udc69\u200d\ud83d\udcbb"}`
)

var ownChat = syntheticChat("15550100009@s.whatsapp.net")

func (f *readFixture) post(t *testing.T, key, target, body string, header http.Header) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, target, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+key)
	for name, values := range header {
		r.Header[name] = values
	}
	return serve(f.handler, r)
}

func idempotency(keys ...string) http.Header { return http.Header{headerIdempotencyKey: keys} }

func (f *readFixture) acceptSends() {
	f.sender.script(func(_ context.Context, client string, req SendRequest) (Sent, error) {
		f.archive.mu.Lock()
		defer f.archive.mu.Unlock()
		id := "GATEWAY-" + req.Key
		for _, m := range f.archive.messages {
			if m.ID == id {
				return Sent{Chat: m.Chat, ID: id, Sender: ownChat, Replayed: true}, nil
			}
		}
		at := testNow.Add(time.Hour)
		m := scoped.Message{Chat: req.Chat, ChatRef: req.ChatRef, ID: id, Sender: ownChat, FromMe: true, At: at, Kind: "text", Text: req.Text,
			TextDisplay: req.Text, SentBy: client, Position: scoped.MessagePosition{TS: at.UnixMilli(), Seq: 100}, Change: scoped.ChangePosition{ChangeSeq: 100}}
		if req.Reply != nil {
			m.ReplyID, m.ReplySender, m.QuoteVerified = req.Reply.ID, req.Reply.Sender, true
		}
		f.archive.messages = append(f.archive.messages, m)
		return Sent{Chat: req.Chat, ID: id, Sender: ownChat}, nil
	})
}

func decodeSend(t *testing.T, rec *httptest.ResponseRecorder) sendView {
	t.Helper()
	var v sendView
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d %s, want 201", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.String(), err)
	}
	return v
}

func (f *readFixture) failSends(err error, replayed bool) {
	f.sender.script(func(context.Context, string, SendRequest) (Sent, error) { return Sent{Replayed: replayed}, err })
}

type sendView struct {
	Message           *messageView `json:"message"`
	Replayed          bool         `json:"replayed"`
	Session           sessionView  `json:"session"`
	RetryAfterSeconds *int         `json:"retry_after_seconds"`
}

func TestASendAnswers201WithTheStoredRowAndReplaysItEqually(t *testing.T) {
	f := newMCPFixture(t)
	f.acceptSends()
	first := f.post(t, keyAlice, sendTarget, sendBodyText, idempotency(sendKey))
	if first.Code != http.StatusCreated || first.Header().Get(headerReplayed) != "" {
		t.Fatalf("first send = %d %s %v", first.Code, first.Body.String(), first.Header())
	}
	got := decodeSend(t, first)
	if got.Message == nil || got.Replayed || got.Session.State != "connected" || got.RetryAfterSeconds != nil ||
		got.Message.Origin != gatewayFrom || !got.Message.FromMe || got.Message.Chat != aliceRef || *got.Message.Text != sendText {
		t.Fatalf("first answer = %+v %+v", got, got.Message)
	}
	calls := f.sender.requests()
	want := SendRequest{Key: sendKey, Chat: aliceChat, ChatRef: aliceRef, Text: sendText}
	if len(calls) != 1 || calls[0] != want || f.sender.grants[0] != "client-alice" {
		t.Fatalf("sender calls = %+v by %q, want %+v", calls, f.sender.grants, want)
	}
	replay := f.post(t, keyAlice, sendTarget, sendBodyText, idempotency(sendKey))
	if replay.Code != http.StatusCreated || replay.Header().Get(headerReplayed) != "true" {
		t.Fatalf("replay = %d %v", replay.Code, replay.Header())
	}
	if !bytes.Equal(bytes.Replace(replay.Body.Bytes(), []byte(`"replayed":true`), []byte(`"replayed":false`), 1), first.Body.Bytes()) {
		t.Fatalf("replay %s differs from the first answer %s beyond the marker", replay.Body.String(), first.Body.String())
	}
	f.archive.mu.Lock()
	f.archive.messages[len(f.archive.messages)-1].Revoked = true
	f.archive.mu.Unlock()
	revoked := decodeSend(t, f.post(t, keyAlice, sendTarget, sendBodyText, idempotency(sendKey)))
	if revoked.Message == nil || !revoked.Message.Revoked || revoked.Message.Text != nil || !revoked.Replayed {
		t.Fatalf("a replay after a revoke = %+v", revoked.Message)
	}
	events := f.audit.recorded()
	if len(events) != 3 {
		t.Fatalf("%d audit rows, want 3", len(events))
	}
	for _, e := range events {
		if e.Action != actionSend || e.Chat != aliceChat || !e.OK || e.Reason != outcomeOK || e.Client != "client-alice" {
			t.Fatalf("audit row = %+v", e)
		}
	}
}

func TestAReplyCarriesTheQuotedRowFromTheSameChat(t *testing.T) {
	f := newMCPFixture(t)
	f.clients[keyAlice].Read[groupChat] = struct{}{}
	f.clients[keyAlice].Write[groupChat] = struct{}{}
	f.acceptSends()
	page := decodeBody[pageView](t, f.get(t, keyAlice, "/v1/chats/"+groupRef+"/messages?limit=4"))
	aliceRow := decodeBody[pageView](t, f.get(t, keyAlice, "/v1/chats/"+aliceRef+"/messages")).Messages[0].Ref
	f.archive.messages[1].Revoked = true
	f.archive.messages[2].Text = ""
	groupTarget := "/v1/chats/" + groupRef + "/messages"
	for name, replyTo := range map[string]string{
		"garbage":            "m1_garbage",
		"another chat's row": aliceRow,
		"a revoked row":      page.Messages[1].Ref,
		"a row without text": page.Messages[2].Ref,
		"a number":           "7",
	} {
		body := `{"text":"synthetic","reply_to":"` + replyTo + `"}`
		if name == "a number" {
			body = `{"text":"synthetic","reply_to":7}`
			requireError(t, f.post(t, keyAlice, groupTarget, body, idempotency(sendKey)), http.StatusBadRequest, codeInvalidBody)
			continue
		}
		requireError(t, f.post(t, keyAlice, groupTarget, body, idempotency(sendKey)), http.StatusNotFound, codeNotFound)
	}
	if calls := f.sender.requests(); len(calls) != 0 {
		t.Fatalf("a refused reply reached the sender: %+v", calls)
	}
	rec := f.post(t, keyAlice, groupTarget, `{"text":"synthetic answer","reply_to":"`+page.Messages[0].Ref+`"}`, idempotency(sendKey))
	if rec.Code != http.StatusCreated {
		t.Fatalf("a reply = %d %s", rec.Code, rec.Body.String())
	}
	calls := f.sender.requests()
	if len(calls) != 1 || calls[0].Reply == nil || *calls[0].Reply != (SendReply{ID: "SYNTHETIC10", Sender: aliceChat, Seq: 10, Text: "synthetic text 10"}) || calls[0].ChatRef != groupRef {
		t.Fatalf("sender calls = %+v", calls)
	}
	if got := decodeSend(t, rec); got.Message.ReplyTo == nil || !got.Message.QuoteVerified {
		t.Fatalf("the stored reply = %+v", got.Message)
	}
	rec = f.post(t, keyAlice, groupTarget, `{"text":"synthetic plain","reply_to":null}`, idempotency("synthetic-key-0002"))
	if calls := f.sender.requests(); rec.Code != http.StatusCreated || calls[1].Reply != nil {
		t.Fatalf("a null reply_to = %d, %+v", rec.Code, calls[1])
	}
}

func TestSendRefusalsFollowTheFixedOrder(t *testing.T) {
	f := newMCPFixture(t)
	f.clients[keyAlice].Read[groupChat] = struct{}{}
	f.acceptSends()
	long := strings.Repeat("a", 4097)
	wide := strings.Repeat("\U0001F600", 2049)
	for _, tt := range []struct {
		name   string
		target string
		body   string
		header http.Header
		status int
		code   string
	}{
		{"unknown field", sendTarget, `{"text":"a","x":1}`, idempotency(sendKey), 400, codeInvalidBody},
		{"no text", sendTarget, `{"reply_to":null}`, idempotency(sendKey), 400, codeInvalidBody},
		{"null text", sendTarget, `{"text":null}`, idempotency(sendKey), 400, codeInvalidBody},
		{"numeric text", sendTarget, `{"text":5}`, idempotency(sendKey), 400, codeInvalidBody},
		{"duplicate text", sendTarget, `{"text":"a","text":"b"}`, idempotency(sendKey), 400, codeInvalidBody},
		{"trailing token", sendTarget, `{"text":"a"} {}`, idempotency(sendKey), 400, codeInvalidBody},
		{"bad body before a bad key", sendTarget, `{"text":`, nil, 400, codeInvalidBody},
		{"no key", sendTarget, `{"text":"a"}`, nil, 400, codeInvalidKey},
		{"two keys", sendTarget, `{"text":"a"}`, idempotency(sendKey, sendKey), 400, codeInvalidKey},
		{"short key", sendTarget, `{"text":"a"}`, idempotency("1234567"), 400, codeInvalidKey},
		{"long key", sendTarget, `{"text":"a"}`, idempotency(strings.Repeat("k", 129)), 400, codeInvalidKey},
		{"key with a space", sendTarget, `{"text":"a"}`, idempotency("synthetic key"), 400, codeInvalidKey},
		{"key outside ASCII", sendTarget, `{"text":"a"}`, idempotency("synthetic-k\u00e9"), 400, codeInvalidKey},
		{"bad key before bad text", sendTarget, `{"text":""}`, idempotency("short"), 400, codeInvalidKey},
		{"empty text", sendTarget, `{"text":""}`, idempotency(sendKey), 400, codeInvalidText},
		{"text over 4096 characters", sendTarget, `{"text":"` + long + `"}`, idempotency(sendKey), 400, codeInvalidText},
		{"text over 8192 bytes", sendTarget, `{"text":"` + wide + `"}`, idempotency(sendKey), 400, codeInvalidText},
		{"a control character", sendTarget, `{"text":"a\u0007b"}`, idempotency(sendKey), 400, codeInvalidText},
		{"a bidi override", sendTarget, `{"text":"a\u202eb"}`, idempotency(sendKey), 400, codeInvalidText},
		{"a byte order mark", sendTarget, `{"text":"a\ufeffb"}`, idempotency(sendKey), 400, codeInvalidText},
		{"a line separator", sendTarget, `{"text":"a\u2028b"}`, idempotency(sendKey), 400, codeInvalidText},
		{"a tag character", sendTarget, `{"text":"a\udb40\udc41b"}`, idempotency(sendKey), 400, codeInvalidText},
		{"bad text before an unknown chat", "/v1/chats/" + noRef + "/messages", `{"text":""}`, idempotency(sendKey), 400, codeInvalidText},
		{"unknown chat", "/v1/chats/" + noRef + "/messages", `{"text":"a"}`, idempotency(sendKey), 404, codeNotFound},
		{"unreadable chat", "/v1/chats/" + bobRef + "/messages", `{"text":"a"}`, idempotency(sendKey), 404, codeNotFound},
		{"readable chat outside the write set", "/v1/chats/" + groupRef + "/messages", `{"text":"a"}`, idempotency(sendKey), 404, codeNotFound},
		{"malformed chat", "/v1/chats/x/messages", `{"text":"a"}`, idempotency(sendKey), 404, codeNotFound},
	} {
		t.Run(tt.name, func(t *testing.T) {
			requireError(t, f.post(t, keyAlice, tt.target, tt.body, tt.header), tt.status, tt.code)
		})
	}
	if calls := f.sender.requests(); len(calls) != 0 {
		t.Fatalf("a refused request reached the sender: %+v", calls)
	}
	if got := f.scope.Load(); got != 1 {
		t.Fatalf("%d scope rejections counted, want the one chat that was found outside the write set", got)
	}
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, sendTarget, strings.NewReader(`{"text":"a"}`))
	r.Header.Set("Content-Type", "text/plain")
	r.Header.Set("Authorization", "Bearer "+keyAlice)
	requireError(t, serve(f.handler, r), http.StatusUnsupportedMediaType, codeUnsupportedMediaType)
	requireError(t, f.post(t, keyAlice, sendTarget, `{"text":"`+strings.Repeat("a", maxSendBodyBytes)+`"}`, idempotency(sendKey)), http.StatusRequestEntityTooLarge, codeBodyTooLarge)
	for _, text := range []string{`a\u200db`, `a\ufe0fb`, `a\u200cb`, "   "} {
		if rec := f.post(t, keyAlice, sendTarget, `{"text":"`+text+`"}`, idempotency("kept-"+fmt.Sprintf("%x", text))); rec.Code != http.StatusCreated {
			t.Fatalf("text %q = %d %s, want it sent", text, rec.Code, rec.Body.String())
		}
	}
}

func escaped(s string) string {
	var b strings.Builder
	for _, u := range utf16.Encode([]rune(s)) {
		fmt.Fprintf(&b, `\u%04x`, u)
	}
	return b.String()
}

func TestAnEscapedTextWithinThePolicyIsSentOverRESTAndMCP(t *testing.T) {
	f := newMCPFixture(t)
	f.acceptSends()
	ref := decodeBody[pageView](t, f.get(t, keyAlice, "/v1/chats/"+aliceRef+"/messages")).Messages[0].Ref
	angles := strings.Repeat("<", maxOutboundChars)
	encoded, err := json.Marshal(sendBody{Text: &angles})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	widest := strings.Repeat("a", 2731) + strings.Repeat("\U0001F600", 1365)
	for i, body := range []string{
		`{"text":"` + escaped(strings.Repeat("\u0436", maxOutboundChars)) + `"}`,
		`{"text":"` + escaped(strings.Repeat("\U0001F600", maxOutboundChars/2)) + `"}`,
		string(encoded),
		`{"text":"` + escaped(widest) + `","reply_to":"` + escaped(ref) + `"}`,
	} {
		if rec := f.post(t, keyAlice, sendTarget, body, idempotency(fmt.Sprintf("escaped-key-%04d", i))); rec.Code != http.StatusCreated {
			t.Fatalf("an escaped text within the policy in a %d-byte body = %d %s", len(body), rec.Code, rec.Body.String())
		}
	}
	requireError(t, f.post(t, keyAlice, sendTarget, `{"text":"`+escaped(strings.Repeat("a", maxOutboundChars+1))+`"}`, idempotency("escaped-key-over")),
		http.StatusBadRequest, codeInvalidText)
	cs := connect(t, f.handler, keyAlice, protocols[0])
	if res := call(t, cs, toolSendMessage, map[string]any{"chat": aliceRef, "text": angles, "idempotency_key": "escaped-key-mcp1"}); res.IsError {
		t.Fatalf("send_message with the text REST took = %+v", res)
	}
	if calls := f.sender.requests(); len(calls) != 5 || calls[3].Text != widest || calls[3].Reply == nil || calls[4].Text != angles {
		t.Fatalf("%d sender calls, want five with the decoded texts", len(calls))
	}
}

func TestAWriteRouteAnswersAReadOnlyClientLikeAnUnknownRoute(t *testing.T) {
	f := newMCPFixture(t)
	f.acceptSends()
	srv := httptest.NewServer(f.handler)
	defer srv.Close()
	wantStatus, wantHeader, wantBody := fetch(t, srv, http.MethodPost, "/v1/no-such-route", bearer(keyGroup))
	for _, key := range []string{keyGroup, keyAll} {
		status, header, body := fetch(t, srv, http.MethodPost, "/v1/chats/"+groupRef+"/messages", bearer(key))
		if status != wantStatus || !equalHeaders(header, wantHeader) || !bytes.Equal(body, wantBody) || status != http.StatusNotFound {
			t.Fatalf("%s: %d %v %q, want %d %v %q", key, status, header, body, wantStatus, wantHeader, wantBody)
		}
	}
	if calls := f.sender.requests(); len(calls) != 0 {
		t.Fatalf("a read-only client reached the sender: %+v", calls)
	}
	for _, e := range f.audit.recorded() {
		if e.Action != actionUnrouted || e.Reason != codeNotFound || e.OK {
			t.Fatalf("audit row = %+v, want the unrouted 404", e)
		}
	}
}

func equalHeaders(a, b http.Header) bool {
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return bytes.Equal(ja, jb)
}

func TestSenderErrorsMapToTheFixedCodes(t *testing.T) {
	for _, tt := range []struct {
		err        error
		replayed   bool
		status     int
		code       string
		retryAfter string
	}{
		{err: ErrKeyReused, status: 422, code: codeKeyReused},
		{err: ErrFirstContact, status: 403, code: codeFirstContact},
		{err: ErrNotConnected, status: 503, code: codeNotConnected, retryAfter: "5"},
		{err: ErrBusy, status: 503, code: codeBusy, retryAfter: "1"},
		{err: fmt.Errorf("%w: synthetic", scoped.ErrBusy), status: 503, code: codeBusy, retryAfter: "1"},
		{err: RateLimited(7 * time.Second), status: 429, code: codeRateLimited, retryAfter: "7"},
		{err: InProgress(1500 * time.Millisecond), status: 409, code: codeInProgress, retryAfter: "2"},
		{err: ErrSendRejected, status: 502, code: codeSendRejected},
		{err: ErrSendRejected, replayed: true, status: 502, code: codeSendRejected},
		{err: ErrSendOutcomeUnknown, status: 504, code: codeSendOutcomeUnknown},
		{err: ErrSendOutcomeUnknown, replayed: true, status: 504, code: codeSendOutcomeUnknown},
		{err: ErrNotFound, status: 404, code: codeNotFound},
		{err: errors.New("synthetic failure naming " + aliceChat.JID() + " " + keyCanary), status: 500, code: codeInternal},
	} {
		t.Run(fmt.Sprintf("%s/%v", tt.code, tt.replayed), func(t *testing.T) {
			f := newMCPFixture(t)
			f.failSends(tt.err, tt.replayed)
			rec := f.post(t, keyAlice, sendTarget, `{"text":"`+textCanary+`"}`, idempotency(keyCanary))
			requireError(t, rec, tt.status, tt.code)
			if got := rec.Header().Get("Retry-After"); got != tt.retryAfter {
				t.Fatalf("Retry-After = %q, want %q", got, tt.retryAfter)
			}
			if got, want := rec.Header().Get(headerReplayed), map[bool]string{true: "true"}[tt.replayed]; got != want {
				t.Fatalf("%s = %q, want %q", headerReplayed, got, want)
			}
			events := f.audit.recorded()
			if len(events) != 1 || events[0].Action != actionSend || events[0].Reason != tt.code || events[0].OK {
				t.Fatalf("audit = %+v", events)
			}
			if wire := fmt.Sprintf("%v %+v", rec.Header(), events); strings.Contains(wire, keyCanary) || strings.Contains(wire, textCanary) {
				t.Fatalf("a key or a text reached the answer or the audit: %s", wire)
			}
		})
	}
}

func TestAnAcknowledgedSendMissingFromTheArchiveIsAnInternalError(t *testing.T) {
	f := newMCPFixture(t)
	f.sender.script(func(context.Context, string, SendRequest) (Sent, error) {
		return Sent{Chat: aliceChat, ID: "NOT-STORED", Sender: ownChat}, nil
	})
	requireError(t, f.post(t, keyAlice, sendTarget, `{"text":"a"}`, idempotency(sendKey)), http.StatusInternalServerError, codeInternal)
}

func TestTheSendHasItsOwnDeadline(t *testing.T) {
	f := newMCPFixture(t)
	var left []time.Duration
	f.sender.script(func(ctx context.Context, _ string, _ SendRequest) (Sent, error) {
		deadline, ok := ctx.Deadline()
		if !ok {
			return Sent{}, errors.New("synthetic: no deadline")
		}
		left = append(left, time.Until(deadline))
		return Sent{}, ErrSendOutcomeUnknown
	})
	f.post(t, keyAlice, sendTarget, `{"text":"a"}`, idempotency(sendKey))
	cs := connect(t, f.handler, keyAlice, protocols[0])
	requireToolError(t, call(t, cs, toolSendMessage, map[string]any{"chat": aliceRef, "text": "a", "idempotency_key": sendKey}), codeSendOutcomeUnknown)
	if len(left) != 2 {
		t.Fatalf("%d sends, want 2", len(left))
	}
	for _, d := range left {
		if d <= sendDeadline-5*time.Second || d > sendDeadline {
			t.Fatalf("the send had %v left, want just under %v", d, sendDeadline)
		}
	}
}

func TestSendMessageAnswersLikeRESTOnBothProtocols(t *testing.T) {
	for _, p := range protocols {
		t.Run(p.name, func(t *testing.T) {
			f := newMCPFixture(t)
			f.acceptSends()
			cs := connect(t, f.handler, keyAlice, p)
			res := call(t, cs, toolSendMessage, map[string]any{"chat": aliceRef, "text": "synthetic via mcp", "idempotency_key": sendKey})
			if res.IsError {
				t.Fatalf("send_message = %+v", res)
			}
			rest := f.post(t, keyAlice, sendTarget, `{"text":"synthetic via mcp"}`, idempotency(sendKey))
			if rest.Header().Get(headerReplayed) != "true" {
				t.Fatalf("REST did not replay the MCP send: %v", rest.Header())
			}
			want := bytes.Replace(rest.Body.Bytes(), []byte(`"replayed":true`), []byte(`"replayed":false`), 1)
			if got := structured(t, res); normalised(t, got) != normalised(t, want) {
				t.Fatalf("send_message = %s, REST answers %s", got, want)
			}
			again := call(t, cs, toolSendMessage, map[string]any{"chat": aliceRef, "text": "synthetic via mcp", "idempotency_key": sendKey})
			var replay sendView
			if err := json.Unmarshal(structured(t, again), &replay); err != nil || !replay.Replayed || again.IsError {
				t.Fatalf("the replay = %s", structured(t, again))
			}
			if n := len(f.sender.requests()); n != 3 {
				t.Fatalf("%d sender calls, want 3", n)
			}
			for _, e := range f.audit.recorded() {
				if (e.Action != sendAction && e.Action != actionSend && e.Action != actionMCP) || (e.Action != actionMCP && (e.Chat != aliceChat || !e.OK)) {
					t.Fatalf("audit row = %+v", e)
				}
			}
		})
	}
}

func TestSendMessageErrorsCarryTheRESTCodes(t *testing.T) {
	long := strings.Repeat("a", 4097)
	for _, p := range protocols {
		t.Run(p.name, func(t *testing.T) {
			f := newMCPFixture(t)
			cs := connect(t, f.handler, keyAlice, p)
			args := func(over map[string]any) map[string]any {
				a := map[string]any{"chat": aliceRef, "text": textCanary, "idempotency_key": keyCanary}
				for k, v := range over {
					if v == nil {
						delete(a, k)
						continue
					}
					a[k] = v
				}
				return a
			}
			for _, tt := range []struct {
				args map[string]any
				code string
			}{
				{args(map[string]any{"text": ""}), codeInvalidArguments},
				{args(map[string]any{"text": long}), codeInvalidArguments},
				{args(map[string]any{"text": nil}), codeInvalidArguments},
				{args(map[string]any{"idempotency_key": "short"}), codeInvalidArguments},
				{args(map[string]any{"idempotency_key": "synthetic key"}), codeInvalidArguments},
				{args(map[string]any{"idempotency_key": nil}), codeInvalidArguments},
				{args(map[string]any{"reply_to": ""}), codeInvalidArguments},
				{args(map[string]any{argCanary: 1}), codeInvalidArguments},
				{args(map[string]any{"text": "a\u0007b"}), codeInvalidText},
				{args(map[string]any{"text": strings.Repeat("\U0001F600", 2049)}), codeInvalidText},
				{args(map[string]any{"chat": noRef}), codeNotFound},
				{args(map[string]any{"chat": groupRef}), codeNotFound},
				{args(map[string]any{"reply_to": "m1_garbage"}), codeNotFound},
			} {
				res := call(t, cs, toolSendMessage, tt.args)
				requireToolError(t, res, tt.code)
				if wire, _ := json.Marshal(res); bytes.Contains(wire, []byte(argCanary)) || bytes.Contains(wire, []byte(keyCanary)) || bytes.Contains(wire, []byte(textCanary)) {
					t.Fatalf("%v echoed an argument: %s", tt.args, wire)
				}
			}
			if calls := f.sender.requests(); len(calls) != 0 {
				t.Fatalf("a refused call reached the sender: %+v", calls)
			}
			for _, tt := range []struct {
				err      error
				replayed bool
				code     string
				retry    *int
			}{
				{err: InProgress(3 * time.Second), code: codeInProgress, retry: new(3)},
				{err: RateLimited(42 * time.Second), code: codeRateLimited, retry: new(42)},
				{err: ErrNotConnected, code: codeNotConnected, retry: new(5)},
				{err: ErrBusy, code: codeBusy, retry: new(1)},
				{err: ErrKeyReused, code: codeKeyReused},
				{err: ErrFirstContact, code: codeFirstContact},
				{err: ErrSendRejected, replayed: true, code: codeSendRejected},
				{err: ErrSendOutcomeUnknown, code: codeSendOutcomeUnknown},
				{err: errors.New("synthetic " + keyCanary), code: codeInternal},
			} {
				f.failSends(tt.err, tt.replayed)
				res := call(t, cs, toolSendMessage, args(nil))
				requireToolError(t, res, tt.code)
				var got sendView
				if err := json.Unmarshal(structured(t, res), &got); err != nil || got.Message != nil || got.Replayed != tt.replayed ||
					(got.RetryAfterSeconds == nil) != (tt.retry == nil) || (tt.retry != nil && *got.RetryAfterSeconds != *tt.retry) {
					t.Fatalf("%s: structured content %s", tt.code, structured(t, res))
				}
				if wire, _ := json.Marshal(res); bytes.Contains(wire, []byte(keyCanary)) || bytes.Contains(wire, []byte(textCanary)) {
					t.Fatalf("%s echoed an argument: %s", tt.code, wire)
				}
			}
			for _, e := range f.audit.recorded() {
				if strings.Contains(fmt.Sprintf("%+v", e), keyCanary) || strings.Contains(fmt.Sprintf("%+v", e), textCanary) {
					t.Fatalf("the audit holds an argument: %+v", e)
				}
			}
		})
	}
}

func TestAHiddenSendIsAnUnknownToolOnBothProtocols(t *testing.T) {
	for _, p := range protocols {
		t.Run(p.name, func(t *testing.T) {
			f := newMCPFixture(t)
			f.acceptSends()
			f.clients[keyRevoked] = &policy.Client{ID: "client-revoked", Write: map[policy.CanonicalChat]struct{}{aliceChat: {}}, Revoked: true, ExpiresAt: mcpExpiry}
			answer := func(key, tool string) []byte {
				rec := &recordingTransport{handlerTransport: handlerTransport{handler: f.handler, key: key}}
				cs := connectThrough(t, rec, p)
				_, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: tool, Arguments: map[string]any{"chat": aliceRef, "text": "a", "idempotency_key": sendKey}})
				if err == nil || !strings.Contains(err.Error(), "unknown tool") {
					t.Fatalf("%s calling %s = %v, want unknown tool", key, tool, err)
				}
				return rec.last()
			}
			unknown := answer(keyGroup, "no_such_tool")
			for _, c := range []struct{ key, tool string }{{keyGroup, toolSendMessage}, {keyAll, toolSendMessage}, {keyRevoked, toolSendMessage}, {keyAlice, "no_such_tool"}} {
				if got := answer(c.key, c.tool); !bytes.Equal(got, unknown) {
					t.Fatalf("%s calling %s = %s, an unknown tool = %s", c.key, c.tool, got, unknown)
				}
			}
			if calls := f.sender.requests(); len(calls) != 0 {
				t.Fatalf("a hidden call reached the sender: %+v", calls)
			}
			for _, e := range f.audit.recorded() {
				if e.Action == actionMCP && e.Reason != "" && e.Reason != outcomeOK && e.Reason != codeUnknownTool && e.Reason != codeNotFound {
					t.Fatalf("audit row = %+v", e)
				}
				if e.Action == sendAction {
					t.Fatalf("a hidden call was audited as a send: %+v", e)
				}
			}
		})
	}
}

func TestOnlyScopeRefusalsAreCountedByTheAPI(t *testing.T) {
	reg := metrics.NewRegistry()
	vec := reg.CounterVec("wawarden_sends_rejected_total", "x", "reason")
	f := newMCPFixture(t)
	f.clients[keyAlice].Read[groupChat] = struct{}{}
	deps := testClientDeps(t, f.clients, f.clock.now)
	deps.Archive, deps.Audit, deps.Sender = f.archive, f.audit, f.sender
	deps.SendRejected = func(reason string) { vec.With(reason).Inc() }
	h := NewClientHandler(deps)
	f.handler = h
	f.failSends(ErrFirstContact, false)
	f.post(t, keyAlice, "/v1/chats/"+groupRef+"/messages", `{"text":"a"}`, idempotency(sendKey))
	f.post(t, keyAlice, sendTarget, `{"text":"a"}`, idempotency(sendKey))
	if got := metricValue(t, reg, `wawarden_sends_rejected_total{reason="scope"}`); got != "1" {
		t.Fatalf("scope rejections = %q, want 1", got)
	}
	if strings.Contains(exposition(t, reg), "first_contact") {
		t.Fatal("the api counted a refusal that the sender's adapter counts")
	}
}

func FuzzDecodeSend(f *testing.F) {
	for _, seed := range []struct{ contentType, key, body string }{
		{"application/json", sendKey, `{"text":"hi"}`},
		{"application/json", sendKey, `{"text":"hi","reply_to":null}`},
		{"application/json", sendKey, `{"text":"hi","reply_to":"m1_x"}`},
		{"application/json", "short", `{"text":"hi"}`},
		{"application/json", sendKey, `{"text":"a\u0007"}`},
		{"application/json", sendKey, `{"text":"a","text":"b"}`},
		{"text/plain", sendKey, `{"text":"hi"}`},
	} {
		f.Add(seed.contentType, seed.key, []byte(seed.body))
	}
	fixed := map[int][]string{
		http.StatusCreated:               nil,
		http.StatusBadRequest:            {codeInvalidBody, codeInvalidKey, codeInvalidText},
		http.StatusNotFound:              {codeNotFound},
		http.StatusRequestEntityTooLarge: {codeBodyTooLarge},
		http.StatusUnsupportedMediaType:  {codeUnsupportedMediaType},
	}
	f.Fuzz(func(t *testing.T, contentType, key string, body []byte) {
		fx := newMCPFixture(t)
		fx.acceptSends()
		r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, sendTarget, bytes.NewReader(body))
		r.Header.Set("Content-Type", contentType)
		r.Header.Set("Authorization", "Bearer "+keyAlice)
		r.Header.Set(headerIdempotencyKey, key)
		rec := serve(fx.handler, r)
		codes, ok := fixed[rec.Code]
		if !ok {
			t.Fatalf("status %d %s is outside the send's refusals", rec.Code, rec.Body.String())
		}
		if rec.Code == http.StatusCreated {
			return
		}
		var answer struct {
			Error string `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &answer); err != nil || !slices.Contains(codes, answer.Error) {
			t.Fatalf("%d %s is not a fixed refusal", rec.Code, rec.Body.String())
		}
		if len(fx.sender.requests()) != 0 {
			t.Fatal("a refused request reached the sender")
		}
	})
}

func TestOtherMethodsOnTheSendRouteAnswer405ForEveryClient(t *testing.T) {
	f := newMCPFixture(t)
	for _, key := range []string{keyGroup, keyAlice} {
		rec := serve(f.handler, newRequest(t, http.MethodPut, "/v1/chats/"+aliceRef+"/messages", bearer(key)))
		requireError(t, rec, http.StatusMethodNotAllowed, codeMethodNotAllowed)
		if got := rec.Header().Get("Allow"); got != "GET, HEAD, POST" {
			t.Fatalf("%s: Allow = %q", key, got)
		}
	}
	if calls := f.sender.requests(); len(calls) != 0 {
		t.Fatalf("a refused method reached the sender: %+v", calls)
	}
}
