package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dortort/wawarden/internal/policy"
	"github.com/dortort/wawarden/internal/store/ingest"
)

const syntheticRef = "SYNTHETIC-SEALED-MEDIA-REFERENCE"

type fakeStaged struct {
	mu        sync.Mutex
	data      []byte
	erased    int
	eraseFail bool
}

func (s *fakeStaged) Size() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return int64(len(s.data))
}

func (s *fakeStaged) ReadAt(p []byte, off int64) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if off < 0 || off >= int64(len(s.data)) {
		return 0, io.EOF
	}
	n := copy(p, s.data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (s *fakeStaged) WriteAt(p []byte, off int64) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if off < 0 || off > int64(len(s.data)) {
		return 0, errors.New("synthetic: a write would leave a hole")
	}
	s.data = append(s.data[:off], p...)
	return len(p), nil
}

func (s *fakeStaged) Truncate(size int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if size < 0 || size > int64(len(s.data)) {
		return errors.New("synthetic: truncation only shrinks")
	}
	s.data = s.data[:size]
	return nil
}

func (s *fakeStaged) Erase() {
	s.mu.Lock()
	s.erased++
	s.data = nil
	fail := s.eraseFail
	s.mu.Unlock()
	if fail {
		panic("synthetic")
	}
}

func (s *fakeStaged) erasures() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.erased
}

func mediaRequest(id string) MediaRequest {
	return MediaRequest{ID: id, Ref: syntheticRef, Kind: MediaVideo, Declared: 5 << 20, Deadline: epoch.Add(time.Minute)}
}

func (f *fakeClient) stage(staged Staged, errs ...error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fetchStaged, f.fetchErrs = staged, errs
}

func (f *fakeClient) onFetching(hook func(MediaRequest)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onFetch = hook
}

func TestAttachmentFetchesNeedAConnectedPairedEngine(t *testing.T) {
	r := newHistRig(t)
	e := newEngine(t, r)
	held, heldEngine := heldEngine(t)
	for name, e := range map[string]*Engine{"never started": e, "held by its restart budget": heldEngine} {
		if staged, err := e.FetchMedia(t.Context(), mediaRequest("A")); !errors.Is(err, ErrNotConnected) || staged != nil {
			t.Errorf("%s: FetchMedia = %v, %v", name, staged, err)
		}
	}
	if r.client.count("fetch:A")+held.client.count("fetch:A") != 0 {
		t.Fatal("a refused fetch reached the client")
	}

	r, e = readyEngine(t)
	staged := &fakeStaged{data: []byte("synthetic attachment")}
	r.client.stage(staged)
	var got MediaRequest
	r.client.onFetching(func(req MediaRequest) { got = req })
	out, err := e.FetchMedia(t.Context(), mediaRequest("A"))
	if err != nil || out != Staged(staged) {
		t.Fatalf("a ready fetch = %v, %v, want the client's staged bytes", out, err)
	}
	if canonical, ok := got.Canonical.(archiveCanonical); !ok || canonical.archive != e.pipe.archive {
		t.Fatalf("the client was asked without the archive's canonicaliser: %+v", got)
	}
	if got.Canonical = nil; got != mediaRequest("A") {
		t.Fatalf("the client was asked for %+v", got)
	}
	if staged.erasures() != 0 || out.Size() != int64(len("synthetic attachment")) {
		t.Fatalf("the staged bytes were touched: %d erasures, size %d", staged.erasures(), out.Size())
	}

	r.client.setPaired(false)
	if _, err := e.FetchMedia(t.Context(), mediaRequest("B")); !errors.Is(err, ErrNotConnected) || r.client.count("fetch:B") != 0 {
		t.Fatalf("FetchMedia while unpaired = %v", err)
	}
	r.client.setPaired(true)
	r.client.emit(Disconnected{})
	eventually(t, "the drop is seen", func() bool { return !e.Ready() })
	if _, err := e.FetchMedia(t.Context(), mediaRequest("C")); !errors.Is(err, ErrNotConnected) || r.client.count("fetch:C") != 0 {
		t.Fatalf("FetchMedia after a drop = %v", err)
	}
}

func TestAttachmentFetchesDoNotTakeTheSendSlot(t *testing.T) {
	r, e := readyEngine(t)
	r.client.stage(&fakeStaged{data: []byte("synthetic")})
	entered, release := make(chan struct{}), make(chan struct{})
	r.client.mu.Lock()
	r.client.onSend = func(SendRequest) {
		entered <- struct{}{}
		<-release
	}
	r.client.mu.Unlock()
	done := make(chan error, 1)
	go func() {
		_, err := e.Send(context.Background(), SendRequest{ID: "S", Chat: alice, Text: "synthetic"})
		done <- err
	}()
	<-entered
	if _, err := e.FetchMedia(t.Context(), mediaRequest("A")); err != nil {
		t.Errorf("a fetch during a send = %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("the send = %v", err)
	}
}

func TestAttachmentFetchOutcomesCrossTheEngineAsFixedSentinels(t *testing.T) {
	r, e := readyEngine(t)
	for _, tt := range []struct {
		err  error
		want error
	}{
		{ErrNotConnected, ErrNotConnected},
		{ErrMediaUnavailable, ErrMediaUnavailable},
		{ErrMediaExpired, ErrMediaExpired},
		{ErrMediaTooLarge, ErrMediaTooLarge},
		{ErrMediaFailed, ErrMediaFailed},
		{ErrMediaThrottled, ErrMediaThrottled},
		{fmt.Errorf("synthetic library text: %w", ErrMediaExpired), ErrMediaExpired},
		{fmt.Errorf("synthetic library text: %w", ErrMediaTooLarge), ErrMediaTooLarge},
		{fmt.Errorf("synthetic library text: %w", ErrNotConnected), ErrNotConnected},
		{errors.New("synthetic library text"), ErrMediaFailed},
		{context.Canceled, ErrMediaFailed},
		{context.DeadlineExceeded, ErrMediaFailed},
		{ErrOutcomeUnknown, ErrMediaFailed},
		{&RejectedError{Code: 479}, ErrMediaFailed},
		{ErrSendBusy, ErrMediaFailed},
		{nil, ErrMediaFailed},
	} {
		r.client.stage(nil, tt.err)
		staged, err := e.FetchMedia(t.Context(), mediaRequest("A"))
		if !errors.Is(err, tt.want) || staged != nil {
			t.Errorf("a fetch failing with %v = %v, %v, want %v", tt.err, staged, err, tt.want)
			continue
		}
		if err.Error() != tt.want.Error() {
			t.Errorf("a fetch failing with %v reads %q, want the fixed %q", tt.err, err, tt.want)
		}
	}
	for _, tt := range []struct {
		err  error
		want time.Duration
	}{
		{&ThrottledError{RetryAfter: 30 * time.Second}, 30 * time.Second},
		{fmt.Errorf("synthetic library text: %w", &ThrottledError{RetryAfter: time.Minute}), time.Minute},
		{&ThrottledError{}, 0},
	} {
		r.client.stage(nil, tt.err)
		_, err := e.FetchMedia(t.Context(), mediaRequest("A"))
		if !errors.Is(err, ErrMediaThrottled) || RetryAfter(err) != tt.want || strings.Contains(err.Error(), "synthetic") {
			t.Errorf("a fetch throttled with %v = %v, retry after %v, want %v", tt.err, err, RetryAfter(err), tt.want)
		}
	}
}

func TestThrottlingCarriesItsRetryAfter(t *testing.T) {
	for _, tt := range []struct {
		err   error
		after time.Duration
		text  string
	}{
		{&ThrottledError{RetryAfter: 30 * time.Second}, 30 * time.Second, "engine: attachment fetches are throttled, retry after 30s"},
		{&ThrottledError{RetryAfter: 1500 * time.Millisecond}, 1500 * time.Millisecond, "engine: attachment fetches are throttled, retry after 2s"},
		{fmt.Errorf("wrapped: %w", &ThrottledError{RetryAfter: time.Minute}), time.Minute, "wrapped: engine: attachment fetches are throttled, retry after 60s"},
		{&ThrottledError{}, 0, "engine: attachment fetches are throttled"},
		{&ThrottledError{RetryAfter: -time.Second}, 0, "engine: attachment fetches are throttled"},
	} {
		if !errors.Is(tt.err, ErrMediaThrottled) || RetryAfter(tt.err) != tt.after || tt.err.Error() != tt.text {
			t.Errorf("%v: Is(ErrMediaThrottled) %v, retry after %v", tt.err, errors.Is(tt.err, ErrMediaThrottled), RetryAfter(tt.err))
		}
	}
	sentinels := []error{ErrNotConnected, ErrMediaUnavailable, ErrMediaExpired, ErrMediaTooLarge, ErrMediaFailed, ErrMediaThrottled}
	for i, err := range sentinels {
		if RetryAfter(err) != 0 {
			t.Errorf("RetryAfter(%v) = %v", err, RetryAfter(err))
		}
		for j, other := range sentinels {
			if i != j && errors.Is(err, other) {
				t.Errorf("%v reads as %v", err, other)
			}
		}
	}
	if RetryAfter(nil) != 0 {
		t.Error("RetryAfter(nil) is not zero")
	}
}

func TestStagedBytesReturnedWithAnErrorAreErased(t *testing.T) {
	r, e := readyEngine(t)
	for _, err := range []error{ErrMediaExpired, &ThrottledError{RetryAfter: time.Second}, errors.New("synthetic"), context.Canceled} {
		staged := &fakeStaged{data: []byte("synthetic partial attachment")}
		r.client.stage(staged, err)
		out, got := e.FetchMedia(t.Context(), mediaRequest("A"))
		if out != nil || got == nil {
			t.Errorf("a fetch failing with %v returned %v, %v", err, out, got)
		}
		if staged.erasures() != 1 {
			t.Errorf("a fetch failing with %v left the staged bytes with %d erasures", err, staged.erasures())
		}
	}
}

func TestAPanickingFetchFailsAndIsCounted(t *testing.T) {
	r, e := readyEngine(t)
	r.reportPanics()
	r.client.stage(&fakeStaged{data: []byte("synthetic")})
	r.client.onFetching(func(MediaRequest) { panic("synthetic") })
	if staged, err := e.FetchMedia(t.Context(), mediaRequest("A")); !errors.Is(err, ErrMediaFailed) || staged != nil {
		t.Fatalf("a panicking fetch = %v, %v", staged, err)
	}
	if r.counter("wawarden_panics_total", "name", "engine.media") != 1 {
		t.Fatal("the panic was not counted")
	}
	r.client.onFetching(nil)
	erasing := &fakeStaged{data: []byte("synthetic"), eraseFail: true}
	r.client.stage(erasing, ErrMediaExpired)
	if staged, err := e.FetchMedia(t.Context(), mediaRequest("B")); !errors.Is(err, ErrMediaFailed) || staged != nil || erasing.erasures() != 1 {
		t.Fatalf("a fetch whose erase panics = %v, %v", staged, err)
	}
	if r.counter("wawarden_panics_total", "name", "engine.media") != 2 {
		t.Fatal("the panicking erase was not counted")
	}
	staged := &fakeStaged{data: []byte("synthetic")}
	r.client.stage(staged)
	if out, err := e.FetchMedia(t.Context(), mediaRequest("C")); err != nil || out != Staged(staged) {
		t.Fatalf("a fetch after the panics = %v, %v", out, err)
	}
}

func TestNoBoundaryMethodIsNamedForADownloadOrAnUpload(t *testing.T) {
	for _, typ := range []reflect.Type{reflect.TypeFor[Client](), reflect.TypeFor[*Engine]()} {
		if _, ok := typ.MethodByName("FetchMedia"); !ok {
			t.Errorf("%v has no FetchMedia", typ)
		}
		for i := range typ.NumMethod() {
			name := typ.Method(i).Name
			if name == "DownloadHistory" {
				continue
			}
			if strings.HasPrefix(name, "Download") || strings.HasPrefix(name, "Upload") {
				t.Errorf("%v has a method named %s", typ, name)
			}
		}
	}
}

func TestAnExpiredFetchLeadsToTheReferenceBeingSwept(t *testing.T) {
	r := newHistRig(t, withDownloads(true, DefaultMediaRefRetention))
	e := newEngine(t, r)
	e.Start(t.Context(), 1)
	t.Cleanup(func() { _ = e.Stop(context.Background()) })
	r.client.waitFor(t, "connect")
	r.client.emit(Connected{})
	eventually(t, "the engine is ready", e.Ready)
	now := time.Now()
	for _, id := range []string{"M1", "M2"} {
		m := withMedia(dm(id, alice, ""), MediaImage, "", 1234)
		m.Timestamp = now
		if !r.client.emit(m) {
			t.Fatalf("%s was not acknowledged", id)
		}
	}
	eventually(t, "both media messages are stored", func() bool {
		_, one := r.find(alice, "M1", alice)
		_, two := r.find(alice, "M2", alice)
		return one && two
	})
	request := func(id string) MediaRequest {
		req := mediaRequest(id)
		req.Kind, req.Declared, req.Chat, req.Sender = MediaImage, 1234, chat(t, alice), chat(t, alice)
		return req
	}
	var got MediaRequest
	r.client.onFetching(func(req MediaRequest) { got = req })
	r.client.stage(nil, ErrMediaFailed)
	if _, err := e.FetchMedia(t.Context(), request("M2")); !errors.Is(err, ErrMediaFailed) || err.Error() != ErrMediaFailed.Error() {
		t.Fatalf("a failed fetch = %v", err)
	}
	if got.Canonical = nil; got != request("M2") {
		t.Fatalf("the client was asked for %+v", got)
	}
	r.client.stage(nil, fmt.Errorf("synthetic: %w", ErrMediaExpired))
	if _, err := e.FetchMedia(t.Context(), request("M1")); !errors.Is(err, ErrMediaExpired) || err.Error() != ErrMediaExpired.Error() {
		t.Fatalf("an expired fetch = %v", err)
	}
	e.pipe.sweep(t.Context())
	if err := e.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if len(r.logs.events("media_expire_failed")) != 0 {
		t.Fatalf("marking the reference failed: %s", r.logs)
	}
	db := r.inspect()
	if got := query[string](t, db, "SELECT group_concat(m.id) FROM media_refs JOIN messages m USING (seq)"); got != "M2" {
		t.Fatalf("media references left: %q, want only the one whose fetch failed", got)
	}
}

func TestAFailedExpiryMarkIsLoggedWithoutContent(t *testing.T) {
	r, e := readyEngine(t)
	req := mediaRequest("not a message id")
	req.Chat, req.Sender = chat(t, alice), chat(t, alice)
	r.client.stage(nil, ErrMediaExpired)
	if _, err := e.FetchMedia(t.Context(), req); !errors.Is(err, ErrMediaExpired) || err.Error() != ErrMediaExpired.Error() {
		t.Fatalf("an expired fetch whose mark failed = %v", err)
	}
	if n := len(r.logs.events("media_expire_failed")); n != 1 {
		t.Fatalf("%d media_expire_failed events, want 1: %s", n, r.logs)
	}
	for _, secret := range []string{"15550100001", "not a message id", syntheticRef} {
		if strings.Contains(r.logs.String(), secret) {
			t.Fatalf("%q reached the logs:\n%s", secret, r.logs)
		}
	}
}

func TestTheMediaCanonicaliserFollowsTheArchivesMappings(t *testing.T) {
	r, e := readyEngine(t)
	pn, lid := chat(t, "15550100077@s.whatsapp.net"), chat(t, "100000000000077@lid")
	r.write(func(tx *ingest.Tx) error {
		_, err := tx.LearnLID(lid, pn, ingest.MappingHistory, epoch)
		return err
	})
	c := archiveCanonical{archive: e.pipe.archive}
	for _, tt := range []struct{ in, want policy.CanonicalChat }{
		{pn, lid},
		{lid, lid},
		{chat(t, "15550100078@s.whatsapp.net"), chat(t, "15550100078@s.whatsapp.net")},
		{chat(t, "120363000000000001@g.us"), chat(t, "120363000000000001@g.us")},
	} {
		if got, err := c.Canonical(t.Context(), tt.in); err != nil || got != tt.want {
			t.Fatalf("Canonical(%s) = %v, %v, want %s", tt.in.JID(), got.JID(), err, tt.want.JID())
		}
	}
	if _, err := c.Canonical(t.Context(), policy.CanonicalChat{}); err == nil {
		t.Fatal("Canonical accepted an invalid chat")
	}
}
