package app

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"log/slog"
	"maps"
	"math"
	"net/http"
	"net/netip"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dortort/wawarden/internal/api"
	"github.com/dortort/wawarden/internal/attach"
	"github.com/dortort/wawarden/internal/engine"
	"github.com/dortort/wawarden/internal/logx"
	"github.com/dortort/wawarden/internal/policy"
	"github.com/dortort/wawarden/internal/stage"
	"github.com/dortort/wawarden/internal/store/admin"
	"github.com/dortort/wawarden/internal/store/ingest"
	"github.com/dortort/wawarden/internal/store/scoped"
	"github.com/dortort/wawarden/internal/token"
)

type memStaged struct {
	mu     sync.Mutex
	b      []byte
	erased bool
}

func newMemStaged(b []byte) *memStaged { return &memStaged{b: bytes.Clone(b)} }

func (m *memStaged) Size() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return int64(len(m.b))
}

func (m *memStaged) ReadAt(p []byte, off int64) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if off >= int64(len(m.b)) {
		return 0, io.EOF
	}
	n := copy(p, m.b[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (m *memStaged) WriteAt(p []byte, off int64) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if end := off + int64(len(p)); end > int64(len(m.b)) {
		return 0, errors.New("synthetic: write past the staged length")
	}
	return copy(m.b[off:], p), nil
}

func (m *memStaged) Truncate(size int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if size > int64(len(m.b)) {
		return errors.New("synthetic: truncate growth")
	}
	m.b = m.b[:size]
	return nil
}

func (m *memStaged) Erase() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.erased = true
	clear(m.b)
}

func (m *memStaged) wasErased() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.erased
}

type fakeFetcher struct {
	mu       sync.Mutex
	ready    bool
	requests []engine.MediaRequest
	staged   []*memStaged
	data     []byte
	err      error
}

func (f *fakeFetcher) Ready() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ready
}

func (f *fakeFetcher) FetchMedia(_ context.Context, req engine.MediaRequest) (engine.Staged, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, req)
	if f.err != nil {
		return nil, f.err
	}
	s := newMemStaged(f.data)
	f.staged = append(f.staged, s)
	return s, nil
}

func (f *fakeFetcher) last() *memStaged {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.staged[len(f.staged)-1]
}

var (
	mediaAlice = mustNormalize("15550100001@s.whatsapp.net")
	mediaGroup = mustNormalize("120363000000000001@g.us")
)

func mustNormalize(jid string) policy.CanonicalChat {
	c, ok := policy.Normalize(jid)
	if !ok {
		panic("synthetic chat refused: " + jid)
	}
	return c
}

func mediaRequest(kind string, declared int) api.MediaFetch {
	return api.MediaFetch{Chat: mediaGroup, ID: "SYNTHETIC-M1", Sender: mediaAlice, Kind: kind, Declared: int64(declared)}
}

func readSpool(t *testing.T, s api.MediaSpool) []byte {
	t.Helper()
	out := make([]byte, s.Size())
	if n, err := s.ReadAt(out, 0); n != len(out) || err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("ReadAt = %d, %v", n, err)
	}
	return out
}

func filledImage(w, h int, c color.Color) *image.RGBA {
	m := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			m.Set(x, y, c)
		}
	}
	return m
}

func jpegBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := jpeg.Encode(&b, filledImage(w, h, color.RGBA{R: 0xd0, G: 0x20, B: 0x20, A: 0xff}), &jpeg.Options{Quality: 95}); err != nil {
		t.Fatalf("jpeg.Encode: %v", err)
	}
	return b.Bytes()
}

func pngBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, filledImage(w, h, color.RGBA{G: 0xd0, A: 0xff})); err != nil {
		t.Fatalf("png.Encode: %v", err)
	}
	return b.Bytes()
}

func withFrame(t *testing.T, j []byte, w, h uint16, progressive bool) []byte {
	t.Helper()
	out := bytes.Clone(j)
	for i := 2; i+9 <= len(out); i += 2 + int(binary.BigEndian.Uint16(out[i+2:])) {
		if out[i] != 0xff {
			t.Fatalf("no marker at %d", i)
		}
		if out[i+1] != 0xc0 {
			continue
		}
		if progressive {
			out[i+1] = 0xc2
		}
		binary.BigEndian.PutUint16(out[i+5:], h)
		binary.BigEndian.PutUint16(out[i+7:], w)
		return out
	}
	t.Fatal("no baseline frame header")
	return nil
}

func corruptPNG(t *testing.T) []byte {
	t.Helper()
	b := pngBytes(t, 64, 48)
	b[bytes.Index(b, []byte("IDAT"))+8] ^= 0xff
	return b
}

func TestMediaServiceRunsThePolicyOncePerJob(t *testing.T) {
	pdf := append([]byte("%PDF-1.7\n"), bytes.Repeat([]byte("synthetic "), 300)...)
	f := &fakeFetcher{ready: true, data: pdf}
	s := newMediaService(f, nil, time.Now)
	calls := 0
	s.policy = func(ctx context.Context, kind, variant string, sp attach.Spool) (attach.Outcome, error) {
		calls++
		return attach.Apply(ctx, kind, variant, sp)
	}
	sp, err := s.Fetch(t.Context(), scoped.SealedRef{}, mediaRequest("document", len(pdf)))
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	first, second := readSpool(t, sp), readSpool(t, sp)
	if !bytes.Equal(first, pdf) || !bytes.Equal(second, pdf) || calls != 1 {
		t.Fatalf("the policy ran %d times for one job", calls)
	}
	if sp.ServedType() != "application/pdf" || sp.Format() != "pdf" || sp.Normalised() || !sp.InMemory() || sp.Size() != int64(len(pdf)) || sp.Fetched() != int64(len(pdf)) {
		t.Fatalf("spool = %s %s normalised %v in memory %v size %d fetched %d", sp.ServedType(), sp.Format(), sp.Normalised(), sp.InMemory(), sp.Size(), sp.Fetched())
	}
	want := engine.MediaRequest{ID: "SYNTHETIC-M1", Kind: engine.MediaDocument, Declared: int64(len(pdf)), Chat: mediaGroup, Sender: mediaAlice}
	if len(f.requests) != 1 || f.requests[0] != want {
		t.Fatalf("requests = %+v, want %+v", f.requests, want)
	}
	if _, err := s.Fetch(t.Context(), scoped.SealedRef{}, mediaRequest("document", len(pdf))); err != nil || calls != 2 {
		t.Fatalf("a second job ran the policy %d times in all: %v", calls, err)
	}
	sp.Erase()
	sp.Erase()
	if _, err := sp.ReadAt(make([]byte, 1), 0); !errors.Is(err, errSpoolErased) || !f.staged[0].wasErased() {
		t.Fatalf("ReadAt after Erase = %v, staged erased %v", err, f.staged[0].wasErased())
	}
}

func TestAJPEGDocumentIsNormalisedBeforeTheJobTurnsReady(t *testing.T) {
	raw := jpegBytes(t, 64, 48)
	f := &fakeFetcher{ready: true, data: raw}
	sp, err := newMediaService(f, nil, time.Now).Fetch(t.Context(), scoped.SealedRef{}, mediaRequest("document", len(raw)))
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !sp.Normalised() || sp.ServedType() != "image/jpeg" || sp.Format() != "image" || !sp.InMemory() || sp.Fetched() != int64(len(raw)) {
		t.Fatalf("spool = %s %s normalised %v in memory %v fetched %d", sp.ServedType(), sp.Format(), sp.Normalised(), sp.InMemory(), sp.Fetched())
	}
	if !f.last().wasErased() {
		t.Fatal("the raw spool was kept beside the normalised bytes")
	}
	served := readSpool(t, sp)
	m, err := jpeg.Decode(bytes.NewReader(served))
	if err != nil || m.Bounds() != image.Rect(0, 0, 64, 48) {
		t.Fatalf("the served bytes decode to %v, %v", m, err)
	}
	if n, err := sp.ReadAt(make([]byte, 4), sp.Size()); n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("ReadAt at the end = %d, %v", n, err)
	}
	if _, err := sp.ReadAt(make([]byte, 4), -1); !errors.Is(err, errSpoolOffset) {
		t.Fatalf("ReadAt at a negative offset = %v", err)
	}
	sp.Erase()
	if _, err := sp.ReadAt(make([]byte, 1), 0); !errors.Is(err, errSpoolErased) {
		t.Fatalf("ReadAt after Erase = %v", err)
	}
}

func refusedImages(t *testing.T) map[string][]byte {
	base := jpegBytes(t, 64, 48)
	return map[string][]byte{
		"a jpeg over 50 MP":                  withFrame(t, base, 10000, 5001, false),
		"a progressive jpeg over the budget": withFrame(t, base, 4800, 3600, true),
		"a corrupt png":                      corruptPNG(t),
	}
}

func TestRefusedImageDocumentsKeepTheirRawSpool(t *testing.T) {
	for name, data := range refusedImages(t) {
		t.Run(name, func(t *testing.T) {
			f := &fakeFetcher{ready: true, data: data}
			s := newMediaService(f, nil, time.Now)
			sp, err := s.Fetch(t.Context(), scoped.SealedRef{}, mediaRequest("document", len(data)))
			if err != nil {
				t.Fatalf("Fetch: %v", err)
			}
			if sp.ServedType() != "application/octet-stream" || sp.Normalised() || sp.Format() != "image" || !bytes.Equal(readSpool(t, sp), data) {
				t.Fatalf("spool = %s %s normalised %v", sp.ServedType(), sp.Format(), sp.Normalised())
			}
			for _, kind := range []string{"image", "sticker"} {
				if _, err := s.Fetch(t.Context(), scoped.SealedRef{}, mediaRequest(kind, len(data))); !errors.Is(err, api.ErrMediaUnsupported) {
					t.Fatalf("Fetch as %s = %v, want media_unsupported", kind, err)
				}
				if !f.last().wasErased() {
					t.Fatalf("the refused %s's spool was kept", kind)
				}
			}
		})
	}
}

func TestEngineFailuresBecomeFixedAnswers(t *testing.T) {
	for _, tt := range []struct {
		err  error
		want error
	}{
		{engine.ErrNotConnected, api.ErrNotConnected},
		{engine.ErrMediaExpired, api.ErrMediaExpired},
		{engine.ErrMediaTooLarge, api.ErrMediaTooLarge},
		{engine.ErrMediaUnavailable, api.ErrMediaUnavailable},
		{engine.ErrMediaFailed, api.ErrMediaFailed},
		{errors.New("synthetic library text"), api.ErrMediaFailed},
		{&engine.ThrottledError{RetryAfter: 90 * time.Second}, api.Busy(90 * time.Second)},
	} {
		s := newMediaService(&fakeFetcher{ready: true, err: tt.err}, nil, time.Now)
		if _, err := s.Fetch(t.Context(), scoped.SealedRef{}, mediaRequest("document", 10)); !reflect.DeepEqual(err, tt.want) {
			t.Fatalf("Fetch failing with %v = %v, want %v", tt.err, err, tt.want)
		}
	}
	s := newMediaService(&fakeFetcher{ready: true, data: []byte("synthetic")}, nil, time.Now)
	s.policy = func(context.Context, string, string, attach.Spool) (attach.Outcome, error) {
		return attach.Outcome{}, attach.ErrSpool
	}
	if _, err := s.Fetch(t.Context(), scoped.SealedRef{}, mediaRequest("document", 9)); !errors.Is(err, api.ErrMediaFailed) {
		t.Fatalf("a spool failure in the policy = %v, want media_failed", err)
	}
}

func TestTheCoolDownHoldsEveryFetchForItsClampedWait(t *testing.T) {
	for _, tt := range []struct {
		retryAfter time.Duration
		want       time.Duration
	}{
		{0, time.Minute},
		{5 * time.Second, 30 * time.Second},
		{90 * time.Second, 90 * time.Second},
		{20 * time.Minute, 10 * time.Minute},
	} {
		now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
		clock := func() time.Time { return now }
		f := &fakeFetcher{ready: true, err: &engine.ThrottledError{RetryAfter: tt.retryAfter}}
		s := newMediaService(f, nil, clock)
		_, err := s.Fetch(t.Context(), scoped.SealedRef{}, mediaRequest("document", 10))
		if !reflect.DeepEqual(err, api.Busy(tt.want)) {
			t.Fatalf("a throttled fetch = %v", err)
		}
		now = now.Add(tt.want - time.Second)
		if err := s.Precheck(10); !reflect.DeepEqual(err, api.Busy(time.Second)) {
			t.Fatalf("Precheck during the cool-down = %v", err)
		}
		now = now.Add(time.Second)
		if err := s.Precheck(10); err != nil {
			t.Fatalf("Precheck after a cool-down of %v = %v", tt.want, err)
		}
	}
}

func TestPrecheckRefusesWithoutAConnectionOrDiskHeadroom(t *testing.T) {
	data := t.TempDir()
	spool, err := stage.New(stage.Options{Data: data, Local: true, UID: os.Geteuid()})
	if err != nil {
		t.Fatalf("stage.New: %v", err)
	}
	f := &fakeFetcher{}
	s := newMediaService(f, spool, time.Now)
	if err := s.Precheck(10); !errors.Is(err, api.ErrNotConnected) {
		t.Fatalf("Precheck while disconnected = %v", err)
	}
	f.ready = true
	for _, declared := range []int64{10, 16 << 20, 16<<20 + 1} {
		if err := s.Precheck(declared); err != nil {
			t.Fatalf("Precheck(%d) = %v", declared, err)
		}
	}
	held, err := spool.Admit(10, func(error) {})
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if err := s.Precheck(16<<20 + 1); !reflect.DeepEqual(err, api.Busy(headroomWait)) {
		t.Fatalf("Precheck beside a live disk spool = %v", err)
	}
	if err := s.Precheck(16 << 20); err != nil {
		t.Fatalf("Precheck of a RAM fetch beside a live disk spool = %v", err)
	}
	held.Erase()
	full, err := stage.New(stage.Options{Data: t.TempDir(), Local: true, MinFree: math.MaxUint64 / 2, UID: os.Geteuid()})
	if err != nil {
		t.Fatalf("stage.New: %v", err)
	}
	if err := newMediaService(f, full, time.Now).Precheck(16<<20 + 1); !reflect.DeepEqual(err, api.Busy(headroomWait)) {
		t.Fatalf("Precheck without headroom = %v", err)
	}
}

func TestADiskSpoolIsReportedOnDisk(t *testing.T) {
	spool, err := stage.New(stage.Options{Data: t.TempDir(), Local: true, UID: os.Geteuid()})
	if err != nil {
		t.Fatalf("stage.New: %v", err)
	}
	pdf := append([]byte("%PDF-1.7\n"), bytes.Repeat([]byte("synthetic "), 300)...)
	sealed, err := spool.Admit(int64(len(pdf)), func(error) {})
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if _, err := sealed.WriteAt(pdf, 0); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	s := newMediaService(stagedFetcher{sealed}, spool, time.Now)
	sp, err := s.Fetch(t.Context(), scoped.SealedRef{}, mediaRequest("document", len(pdf)))
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if sp.InMemory() || !bytes.Equal(readSpool(t, sp), pdf) {
		t.Fatalf("a disk spool reported in memory %v", sp.InMemory())
	}
	sp.Erase()
	if _, err := spool.Admit(10, func(error) {}); err != nil {
		t.Fatalf("Admit after the disk spool's erasure = %v", err)
	}
}

type stagedFetcher struct{ s engine.Staged }

func (stagedFetcher) Ready() bool { return true }

func (f stagedFetcher) FetchMedia(context.Context, engine.MediaRequest) (engine.Staged, error) {
	return f.s, nil
}

func TestClientLivenessFollowsTheClientTable(t *testing.T) {
	var now atomic.Int64
	now.Store(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC).UnixMilli())
	archive, err := ingest.Open(t.Context(), ingest.Options{DataDir: t.TempDir(), UID: os.Geteuid(), Profile: ingest.ProfileLocal,
		Logger: logx.New(logx.NewWriter(io.Discard), slog.LevelInfo), Master: testMaster(t), Now: func() time.Time { return time.UnixMilli(now.Load()) }})
	if err != nil {
		t.Fatalf("ingest.Open: %v", err)
	}
	t.Cleanup(func() { _ = archive.Close() })
	clients := archive.Clients()
	live := clientLive(clients)
	c, _, err := clients.Create(t.Context(), policy.ClientSpec{Name: "synthetic media reader", AllChats: true, AllowMedia: true})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	brief, _, err := clients.Create(t.Context(), policy.ClientSpec{Name: "synthetic brief reader", AllChats: true, AllowMedia: true, ExpiresInDays: 1})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	for _, id := range []string{c.ID, brief.ID} {
		if ok, err := live(t.Context(), id); !ok || err != nil {
			t.Fatalf("a live client = %v, %v", ok, err)
		}
	}
	if _, err := clients.Revoke(t.Context(), c.ID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if ok, err := live(t.Context(), c.ID); ok || err != nil {
		t.Fatalf("a revoked client = %v, %v", ok, err)
	}
	now.Store(brief.ExpiresAt.UnixMilli())
	if got, err := clients.Get(t.Context(), brief.ID); err != nil || got.State != admin.StateExpired {
		t.Fatalf("Get after the expiry = %v, %v", got.State, err)
	}
	if ok, err := live(t.Context(), brief.ID); ok || err != nil {
		t.Fatalf("an expired client = %v, %v", ok, err)
	}
	if ok, err := live(t.Context(), "zzzzzzzz"); ok || err != nil {
		t.Fatalf("an unknown client = %v, %v", ok, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if ok, err := live(ctx, c.ID); ok || err == nil || errors.Is(err, admin.ErrNotFound) {
		t.Fatalf("a failed lookup = %v, %v", ok, err)
	}
}

func TestAttachmentsDownloadThroughTheApp(t *testing.T) {
	cfg := testConfig(t, "")
	cfg.MediaMaxBytes = 100 << 20
	inputs := refusedImages(t)
	names := slices.Sorted(maps.Keys(inputs))
	data := map[string][]byte{}
	for i, name := range names {
		data[fmt.Sprintf("SYNTHETIC-DOC%d", i)], data[fmt.Sprintf("SYNTHETIC-IMG%d", i)] = inputs[name], inputs[name]
	}
	client := newStubClient()
	client.media = func(req engine.MediaRequest) (engine.Staged, error) {
		return newMemStaged(data[strings.TrimPrefix(req.Ref, "SYNTHETIC-SEALED-")]), nil
	}
	a, err := newAppWith(t.Context(), cfg, logx.NewWriter(&syncBuffer{}), testMaster(t), nil, fixed(engineParts{client: client, versions: stubVersions{}, decoder: stubDecoder{}}))
	if err != nil {
		t.Fatalf("newAppWith: %v", err)
	}
	var credentials []string
	for i := range names {
		peer := mustNormalize(fmt.Sprintf("1555010010%d@s.whatsapp.net", i))
		if err := a.archive.Write(t.Context(), "test.inbound", func(tx *ingest.Tx) error {
			for j, kind := range []ingest.MediaKind{ingest.MediaDocument, ingest.MediaImage} {
				id := fmt.Sprintf("SYNTHETIC-%s%d", []string{"DOC", "IMG"}[j], i)
				if _, _, err := tx.InsertMessage(ingest.Message{Chat: peer, ID: id, Sender: peer, Origin: ingest.OriginLive,
					Timestamp: time.Now().Add(time.Duration(j-2) * time.Minute), Kind: ingest.KindMedia, MediaType: "image/png", Ingested: time.Now(),
					Media: &ingest.Media{Ref: "SYNTHETIC-SEALED-" + id, Kind: kind, Declared: int64(len(data[id])), Retention: time.Hour}}); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatalf("Write: %v", err)
		}
		_, credential, err := a.archive.Clients().Create(t.Context(), policy.ClientSpec{Name: fmt.Sprintf("synthetic media reader %d", i), Read: []string{peer.JID()}, AllowMedia: true})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		credentials = append(credentials, credential)
	}
	stop := run(t, a)
	<-client.connected
	if !client.emit(engine.Connected{}) {
		t.Fatal("the engine did not register its event handler")
	}
	waitUntil(t, "the engine is ready", a.engine.Ready)
	to := addr(t, a, listenerClient)
	settle := func(path string, auth http.Header) reply {
		t.Helper()
		if r := mustDo(t, http.MethodPost, to, path, auth); r.status != http.StatusAccepted {
			t.Fatalf("POST %s = %d %s", path, r.status, r.body)
		}
		var got reply
		waitUntil(t, "the job", func() bool {
			got = mustDo(t, http.MethodGet, to, path, auth)
			return got.status != http.StatusAccepted
		})
		return got
	}
	for i, name := range names {
		auth := bearer(credentials[i])
		var chats struct {
			Chats []struct {
				ID string `json:"id"`
			} `json:"chats"`
		}
		if r := mustDo(t, http.MethodGet, to, "/v1/chats", auth); r.status != http.StatusOK || json.Unmarshal([]byte(r.body), &chats) != nil || len(chats.Chats) != 1 {
			t.Fatalf("GET /v1/chats = %d %s", r.status, r.body)
		}
		var page struct {
			Messages []struct {
				Ref string `json:"mref"`
			} `json:"messages"`
		}
		if r := mustDo(t, http.MethodGet, to, "/v1/chats/"+chats.Chats[0].ID+"/messages", auth); r.status != http.StatusOK || json.Unmarshal([]byte(r.body), &page) != nil || len(page.Messages) != 2 {
			t.Fatalf("GET messages = %d %s", r.status, r.body)
		}
		imagePath, document := "/v1/messages/"+page.Messages[0].Ref+"/media", "/v1/messages/"+page.Messages[1].Ref+"/media"
		got := settle(document, auth)
		if got.status != http.StatusOK || got.body != string(inputs[name]) || got.header.Get("Content-Type") != "application/octet-stream" ||
			got.header.Get("Content-Disposition") != `attachment; filename="wawarden-document.bin"` {
			t.Fatalf("GET %s as a document = %d %v with %d bytes", name, got.status, got.header, len(got.body))
		}
		if r := mustDo(t, http.MethodGet, to, document, auth); r.status != http.StatusConflict {
			t.Fatalf("GET after the whole-body delivery = %d %s", r.status, r.body)
		}
		if got := settle(imagePath, auth); got.status != http.StatusUnprocessableEntity || got.body != `{"error":"media_unsupported"}` {
			t.Fatalf("GET %s as an image = %d %s", name, got.status, got.body)
		}
	}
	client.mu.Lock()
	var refs []string
	for _, req := range client.fetches {
		refs = append(refs, req.Ref)
	}
	client.mu.Unlock()
	if len(refs) != 2*len(names) || !strings.HasPrefix(refs[0], "SYNTHETIC-SEALED-SYNTHETIC-DOC") {
		t.Fatalf("the engine was asked for %q", refs)
	}
	if err := stop(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	select {
	case <-a.media.stopped:
	default:
		t.Fatal("the attachment jobs were not stopped")
	}
}

func onlyChatMessages(t *testing.T, to netip.AddrPort, auth http.Header) []string {
	t.Helper()
	var chats struct {
		Chats []struct {
			ID string `json:"id"`
		} `json:"chats"`
	}
	if r := mustDo(t, http.MethodGet, to, "/v1/chats", auth); r.status != http.StatusOK || json.Unmarshal([]byte(r.body), &chats) != nil || len(chats.Chats) != 1 {
		t.Fatalf("GET /v1/chats = %d %s", r.status, r.body)
	}
	var page struct {
		Messages []struct {
			Ref string `json:"mref"`
		} `json:"messages"`
	}
	if r := mustDo(t, http.MethodGet, to, "/v1/chats/"+chats.Chats[0].ID+"/messages", auth); r.status != http.StatusOK || json.Unmarshal([]byte(r.body), &page) != nil {
		t.Fatalf("GET messages = %d %s", r.status, r.body)
	}
	var refs []string
	for _, m := range page.Messages {
		refs = append(refs, m.Ref)
	}
	return refs
}

func TestThePauseHoldsDownloadsUntilItIsLifted(t *testing.T) {
	adminToken := token.NewAdmin()
	cfg := testConfig(t, adminToken)
	cfg.MediaMaxBytes = 16 << 20
	pdf := []byte("%PDF-1.4\n% synthetic document\n%%EOF\n")
	client := newStubClient()
	client.media = func(engine.MediaRequest) (engine.Staged, error) { return newMemStaged(pdf), nil }
	a, err := newAppWith(t.Context(), cfg, logx.NewWriter(&syncBuffer{}), testMaster(t), nil, fixed(engineParts{client: client, versions: stubVersions{}, decoder: stubDecoder{}}))
	if err != nil {
		t.Fatalf("newAppWith: %v", err)
	}
	peer := mustNormalize("15550100101@s.whatsapp.net")
	if err := a.archive.Write(t.Context(), "test.inbound", func(tx *ingest.Tx) error {
		_, _, err := tx.InsertMessage(ingest.Message{Chat: peer, ID: "SYNTHETIC-PDF", Sender: peer, Origin: ingest.OriginLive,
			Timestamp: time.Now().Add(-time.Minute), Kind: ingest.KindMedia, MediaType: "application/pdf", Ingested: time.Now(),
			Media: &ingest.Media{Ref: "SYNTHETIC-SEALED-PDF", Kind: ingest.MediaDocument, Declared: int64(len(pdf)), Retention: time.Hour}})
		if err != nil {
			return err
		}
		if err := tx.PutRevokeTombstone(peer, "SYNTHETIC-REVOKED", peer, time.Now()); err != nil {
			return err
		}
		return tx.PutRevokeTombstone(peer, "SYNTHETIC-REVOKED-2", peer, time.Now())
	}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	_, credential, err := a.archive.Clients().Create(t.Context(), policy.ClientSpec{Name: "synthetic media reader", Read: []string{peer.JID()}, AllowMedia: true})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	stop := run(t, a)
	<-client.connected
	if !client.emit(engine.Connected{}) {
		t.Fatal("the engine did not register its event handler")
	}
	waitUntil(t, "the engine is ready", a.engine.Ready)
	to, admin, auth := addr(t, a, listenerClient), addr(t, a, listenerAdmin), bearer(credential)
	status := func(want string) {
		t.Helper()
		if r := mustDo(t, http.MethodGet, admin, "/admin/v1/status", bearer(adminToken)); !strings.Contains(r.body, want) {
			t.Fatalf("status = %d %s, want %s", r.status, r.body, want)
		}
	}
	status(`"media_refs_live":1,"media_refs_expired":0,"revoke_tombstones":2}`)
	status(`"downloads":"on"`)
	path := "/v1/messages/" + onlyChatMessages(t, to, auth)[0] + "/media"
	if r := post(t, admin, "/admin/v1/media", bearer(adminToken), `{"paused":true}`); r.status != http.StatusOK || r.body != `{"paused":true}` {
		t.Fatalf("pause = %d %s", r.status, r.body)
	}
	status(`"downloads":"paused"`)
	if r := mustDo(t, http.MethodPost, to, path, auth); r.status != http.StatusServiceUnavailable || r.body != `{"error":"busy"}` || r.header.Get("Retry-After") != "60" {
		t.Fatalf("POST while paused = %d %s Retry-After %q", r.status, r.body, r.header.Get("Retry-After"))
	}
	client.mu.Lock()
	fetched := len(client.fetches)
	client.mu.Unlock()
	if fetched != 0 {
		t.Fatalf("the engine was asked for %d attachments while paused", fetched)
	}
	if r := post(t, admin, "/admin/v1/media", bearer(adminToken), `{"paused":false}`); r.status != http.StatusOK || r.body != `{"paused":false}` {
		t.Fatalf("resume = %d %s", r.status, r.body)
	}
	status(`"downloads":"on"`)
	if r := mustDo(t, http.MethodPost, to, path, auth); r.status != http.StatusAccepted {
		t.Fatalf("POST once resumed = %d %s", r.status, r.body)
	}
	var got reply
	waitUntil(t, "the job", func() bool {
		got = mustDo(t, http.MethodGet, to, path, auth)
		return got.status != http.StatusAccepted
	})
	if got.status != http.StatusOK || got.body != string(pdf) || got.header.Get("Content-Type") != "application/pdf" {
		t.Fatalf("GET once resumed = %d %v %q", got.status, got.header, got.body)
	}
	if err := stop(); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestDownloadsOffBuildNoMediaRegistry(t *testing.T) {
	a, _, stop := start(t, testConfig(t, ""), noClients{})
	if a.media != nil {
		t.Fatal("downloads that are off built the attachment registry")
	}
	if err := stop(); err != nil {
		t.Fatalf("Run: %v", err)
	}
}
