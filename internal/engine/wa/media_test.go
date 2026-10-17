package wa

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"reflect"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/util/cbcutil"
	"google.golang.org/protobuf/proto"

	"github.com/dortort/wawarden/internal/engine"
	"github.com/dortort/wawarden/internal/engine/wa/internal/mediasrc"
	"github.com/dortort/wawarden/internal/policy"
	"github.com/dortort/wawarden/internal/stage"
)

const (
	testMediaCap  = 100 << 20
	testRetention = 30 * 24 * time.Hour
)

func newSpool(t *testing.T) *stage.Spool {
	t.Helper()
	spool, err := stage.New(stage.Options{Data: t.TempDir(), Local: true, UID: os.Getuid()})
	if err != nil {
		t.Fatalf("stage.New: %v", err)
	}
	return spool
}

func mediaClient(t *testing.T, now time.Time, configure ...func(*Options)) *Client {
	t.Helper()
	w, _ := newWriter()
	alerts, _ := newLogger(slog.LevelWarn)
	opts := Options{
		Device: pairedDevice(), Devices: &freshDevices{}, OwnerPhone: ownerPhone, HistoryMaxBytes: 1 << 20, Writer: w, Alerts: alerts,
		MediaMaxBytes: testMediaCap, MediaRefRetention: testRetention, MediaSealer: fakeSealer{}, MediaSpool: newSpool(t),
	}
	for _, fn := range configure {
		fn(&opts)
	}
	c, err := newClient(opts, refusingDial(t), noDownload, noFetch, func() time.Time { return now })
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}
	return c
}

func liveMedia(t *testing.T, c *Client, id string, raw *waE2E.Message) engine.Message {
	t.Helper()
	ev, ok := translate(live(t, group, victim, id, raw), c.capture)
	if !ok {
		t.Fatalf("message %s was not translated", id)
	}
	m, ok := ev.(engine.Message)
	if !ok || m.Kind != engine.KindMedia {
		t.Fatalf("message %s translated to %+v, want a media message", id, ev)
	}
	return m
}

func historyMedia(t *testing.T, c *Client, id string, raw *waE2E.Message, edit func(*waHistorySync.HistorySyncMsg)) engine.Message {
	t.Helper()
	row := webMessage(group, false, id, raw, &victim)
	if edit != nil {
		edit(row)
	}
	h, err := c.Decode(historyBlob(t, &waHistorySync.HistorySync{SyncType: waHistorySync.HistorySync_RECENT.Enum(), Conversations: []*waHistorySync.Conversation{conv(group.String(), row)}}))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(h.Conversations) != 1 || len(h.Conversations[0].Messages) != 1 || h.Conversations[0].Messages[0].Kind != engine.KindMedia {
		t.Fatalf("history row %s decoded to %+v, want one media message", id, h.Conversations)
	}
	return h.Conversations[0].Messages[0]
}

type receiptPath struct {
	name string
	read func(t *testing.T, c *Client, id string, raw *waE2E.Message) engine.Message
}

var receiptPaths = []receiptPath{
	{name: "live", read: liveMedia},
	{name: "history", read: func(t *testing.T, c *Client, id string, raw *waE2E.Message) engine.Message {
		return historyMedia(t, c, id, raw, nil)
	}},
}

func openedDescriptor(t *testing.T, m engine.Message) descriptor {
	t.Helper()
	pt, err := fakeSealer{}.OpenMedia(m.ID, m.MediaRef)
	if err != nil {
		t.Fatalf("OpenMedia(%s) = %v", m.ID, err)
	}
	d, ok := parseDescriptor(pt)
	if !ok {
		t.Fatalf("parseDescriptor(%x) refused the sealed descriptor", pt)
	}
	return d
}

func withoutReference(m engine.Message) engine.Message {
	m.MediaRef, m.MediaKind, m.MediaVariant, m.MediaBytes = "", "", "", 0
	return m
}

func uncaptured(t *testing.T, path string, m engine.Message) {
	t.Helper()
	if m.MediaRef != "" || m.MediaKind != "" || m.MediaVariant != "" || m.MediaBytes != 0 {
		t.Fatalf("the %s path captured %q, %q, %q, %d, want no reference", path, m.MediaRef, m.MediaKind, m.MediaVariant, m.MediaBytes)
	}
}

func TestCaptureSealsEveryKindAndVariant(t *testing.T) {
	c := mediaClient(t, epoch.Add(time.Hour))
	for _, fx := range mediaFixtures {
		for _, path := range receiptPaths {
			f := syntheticMedia(4096)
			raw := fx.build(f)
			got := path.read(t, c, "3EB0M1", raw)
			noMediaSecrets(t, fx.name+" on the "+path.name+" path", got, []byte(f.path), f.key, f.sha, f.enc)
			if got.MediaRef == "" || got.MediaKind != engine.MediaKind(fx.kind) || got.MediaVariant != fx.variant || got.MediaBytes != 4096 {
				t.Errorf("%s on the %s path captured %q, %q, %d, want a reference, %q, %q, 4096", fx.name, path.name, got.MediaKind, got.MediaVariant, got.MediaBytes, fx.kind, fx.variant)
				continue
			}
			want := descriptor{directPath: f.path, mediaKey: f.key, fileSHA256: f.sha, fileEncSHA256: f.enc, length: 4096,
				kind: engine.MediaKind(fx.kind), variant: fx.variant, chat: group.String(), sender: victim.String()}
			if d := openedDescriptor(t, got); !reflect.DeepEqual(d, want) {
				t.Errorf("%s on the %s path sealed %+v, want %+v", fx.name, path.name, d, want)
			}
			off := mediaClient(t, epoch, func(o *Options) { o.MediaMaxBytes = 0 })
			plain := path.read(t, off, "3EB0M1", raw)
			noMediaSecrets(t, fx.name+" on the "+path.name+" path without capture", plain, []byte(f.path), f.key, f.sha, f.enc)
			if !reflect.DeepEqual(withoutReference(got), plain) {
				t.Errorf("%s on the %s path = %+v with capture, %+v without: the capture changed more than its four fields", fx.name, path.name, got, plain)
			}
		}
	}
}

func TestCaptureSealsTheReceivedSenderAndDirection(t *testing.T) {
	c := mediaClient(t, epoch.Add(time.Hour))
	evt := live(t, peer, owner, "3EB0M2", mediaFixtures[0].build(syntheticMedia(64)))
	evt.Info.IsFromMe = true
	ev, _ := translate(evt, c.capture)
	d := openedDescriptor(t, ev.(engine.Message))
	if d.chat != peer.String() || d.sender != owner.String() || !d.fromMe {
		t.Fatalf("sealed chat %q, sender %q, from_me %v, want %s, %s, true", d.chat, d.sender, d.fromMe, peer, owner)
	}
}

func TestCaptureLeavesViewOnceMediaWithoutAReference(t *testing.T) {
	c := mediaClient(t, epoch.Add(time.Hour))
	for _, fx := range mediaFixtures {
		for _, path := range receiptPaths {
			uncaptured(t, path.name+" "+fx.name+" in a view-once wrapper", path.read(t, c, "3EB0V1", viewOnceWrapped(fx.build(syntheticMedia(64)))))
			if fx.viewOnce == nil {
				continue
			}
			raw := fx.build(syntheticMedia(64))
			fx.viewOnce(raw)
			uncaptured(t, path.name+" "+fx.name+" with its view-once flag", path.read(t, c, "3EB0V2", raw))
		}
	}
}

func TestCaptureSkipsMessagesPastTheirReferenceExpiry(t *testing.T) {
	image := mediaFixtures[0]
	for name, tt := range map[string]struct {
		now    time.Time
		expiry func(*waE2E.Message, *waHistorySync.HistorySyncMsg)
		want   bool
	}{
		"inside the retention":         {now: epoch.Add(testRetention - time.Second), want: true},
		"at the end of the retention":  {now: epoch.Add(testRetention)},
		"past the retention":           {now: epoch.Add(testRetention + time.Hour)},
		"inside the message's expiry":  {now: epoch.Add(30 * time.Minute), expiry: ownExpiry, want: true},
		"past the message's expiry":    {now: epoch.Add(2 * time.Hour), expiry: ownExpiry},
		"inside the history row's end": {now: epoch.Add(30 * time.Minute), expiry: rowExpiry, want: true},
		"past the history row's end":   {now: epoch.Add(2 * time.Hour), expiry: rowExpiry},
	} {
		c := mediaClient(t, tt.now)
		raw := image.build(syntheticMedia(64))
		var row *waHistorySync.HistorySyncMsg
		got := historyMedia(t, c, "3EB0S1", raw, func(r *waHistorySync.HistorySyncMsg) {
			row = r
			if tt.expiry != nil {
				tt.expiry(r.Message.Message, r)
			}
		})
		paths := map[string]engine.Message{"history": got}
		if tt.expiry == nil || row.Message.EphemeralExpirationTimestamp == nil {
			paths["live"] = liveMedia(t, c, "3EB0S1", raw)
		}
		for path, m := range paths {
			if (m.MediaRef != "") != tt.want {
				t.Errorf("%s on the %s path captured %q, want a reference %v", name, path, m.MediaRef, tt.want)
			}
		}
	}
}

func ownExpiry(m *waE2E.Message, _ *waHistorySync.HistorySyncMsg) {
	m.ImageMessage.ContextInfo = &waE2E.ContextInfo{Expiration: proto.Uint32(3600)}
}

func rowExpiry(_ *waE2E.Message, r *waHistorySync.HistorySyncMsg) {
	r.Message.EphemeralExpirationTimestamp = proto.Uint64(epochSeconds + 3600)
}

func TestCaptureRefusesInvalidOrOversizedAttachments(t *testing.T) {
	sizes := map[string]int64{"image": imageCeiling, "sticker": stickerCeiling, "document": testMediaCap}
	byKind := map[string]mediaFixture{}
	for _, fx := range mediaFixtures {
		if _, ok := byKind[fx.kind]; !ok {
			byKind[fx.kind] = fx
		}
	}
	c := mediaClient(t, epoch.Add(time.Hour))
	for kind, ceiling := range sizes {
		for _, path := range receiptPaths {
			if got := path.read(t, c, "3EB0L1", byKind[kind].build(syntheticMedia(ceiling))); got.MediaBytes != ceiling || got.MediaRef == "" {
				t.Errorf("a %s of exactly its cap on the %s path captured %d bytes, %q, want a reference", kind, path.name, got.MediaBytes, got.MediaRef)
			}
			uncaptured(t, path.name+" "+kind+" over its cap", path.read(t, c, "3EB0L2", byKind[kind].build(syntheticMedia(ceiling+1))))
		}
	}
	small := mediaClient(t, epoch.Add(time.Hour), func(o *Options) { o.MediaMaxBytes = 1000 })
	uncaptured(t, "live image over a cap below its ceiling", liveMedia(t, small, "3EB0L3", byKind["image"].build(syntheticMedia(1001))))
	for name, change := range map[string]func(*mediaFields){
		"a relative path":       func(f *mediaFields) { f.path = "example/m1" },
		"no path":               func(f *mediaFields) { f.path = "" },
		"no media key":          func(f *mediaFields) { f.key = nil },
		"a short plaintext SHA": func(f *mediaFields) { f.sha = f.sha[:31] },
		"no plaintext SHA":      func(f *mediaFields) { f.sha = nil },
		"a long encrypted SHA":  func(f *mediaFields) { f.enc = append(f.enc, 0) },
		"no encrypted SHA":      func(f *mediaFields) { f.enc = nil },
	} {
		for _, fx := range mediaFixtures {
			f := syntheticMedia(64)
			change(&f)
			for _, path := range receiptPaths {
				uncaptured(t, path.name+" "+fx.name+" with "+name, path.read(t, c, "3EB0L4", fx.build(f)))
			}
		}
	}
	for name, configure := range map[string]func(*Options){
		"downloads off":               func(o *Options) { o.MediaMaxBytes, o.MediaSealer, o.MediaSpool = 0, nil, nil },
		"downloads off with a sealer": func(o *Options) { o.MediaMaxBytes = 0 },
		"a refusing sealer":           func(o *Options) { o.MediaSealer = fakeSealer{refuse: true} },
	} {
		off := mediaClient(t, epoch.Add(time.Hour), configure)
		for _, fx := range mediaFixtures {
			for declared, length := range map[string]int64{"one byte": 1, "zero bytes": 0, "no length": -1} {
				for _, path := range receiptPaths {
					where := fmt.Sprintf("%s %s declaring %s with %s", path.name, fx.name, declared, name)
					uncaptured(t, where, path.read(t, off, "3EB0L5", fx.build(syntheticMedia(length))))
				}
			}
		}
	}
}

func TestTheDispatcherCapturesWithTheClientsOptions(t *testing.T) {
	r := newRig(t, pairedDevice(), func(o *Options) {
		o.MediaMaxBytes, o.MediaRefRetention, o.MediaSealer = 1<<20, time.Hour, fakeSealer{}
	})
	evt := live(t, peer, peer, "3EB0D1", mediaFixtures[0].build(syntheticMedia(64)))
	evt.Info.Timestamp = time.Now()
	r.dispatch(evt)
	got := r.events()
	if len(got) != 1 || got[0].(engine.Message).MediaRef == "" {
		t.Fatalf("the dispatcher delivered %+v, want one message with a reference", got)
	}
}

func TestDescriptorEncodingIsStrict(t *testing.T) {
	d := descriptor{directPath: "/example/m1", mediaKey: randomBytes(32), fileSHA256: randomBytes(32), fileEncSHA256: randomBytes(32), length: 1 << 33,
		kind: engine.MediaSticker, variant: engine.VariantLottie, chat: group.String(), sender: victim.String(), fromMe: true}
	b := d.marshal()
	if got, ok := parseDescriptor(b); !ok || !reflect.DeepEqual(got, d) {
		t.Fatalf("parseDescriptor(marshal(d)) = %+v, %v, want %+v", got, ok, d)
	}
	refused := map[string][]byte{
		"nothing":            nil,
		"an unknown version": append([]byte{2}, b[1:]...),
		"a trailing byte":    append(bytes.Clone(b), 0),
		"a long varint":      append([]byte{descriptorVersion, 0x8b, 0x00}, b[2:]...),
	}
	for n := 1; n < len(b); n++ {
		refused[fmt.Sprintf("the first %d bytes", n)] = b[:n]
	}
	short := descriptor{directPath: "/p", mediaKey: []byte{1}, length: 1}
	lengthField := short.marshal()
	at := bytes.Index(lengthField, []byte{8, 0, 0, 0, 0, 0, 0, 0, 1})
	refused["a seven-byte length"] = append(append(bytes.Clone(lengthField[:at]), 7), lengthField[at+2:]...)
	flag := short.marshal()
	flag[len(flag)-1] = 2
	refused["a direction byte of 2"] = flag
	for name, bad := range refused {
		if _, ok := parseDescriptor(bad); ok {
			t.Errorf("parseDescriptor accepted %s", name)
		}
	}
}

func TestTheDescriptorNamesOnlyTheKindsAndVariantsItKnows(t *testing.T) {
	base := descriptor{directPath: "/example/m1", mediaKey: randomBytes(32), fileSHA256: randomBytes(32), fileEncSHA256: randomBytes(32), length: 1}
	for kind, variants := range map[engine.MediaKind][]string{
		engine.MediaImage:    {"gif", "voice_note"},
		engine.MediaVideo:    {"voice_note", "animated"},
		engine.MediaAudio:    {"gif", "lottie"},
		engine.MediaDocument: {"video_note"},
		engine.MediaSticker:  {"gif"},
		"thumbnail":          {""},
	} {
		for _, v := range variants {
			d := base
			d.kind, d.variant = kind, v
			if _, ok := d.declared(); ok {
				t.Errorf("a %s descriptor with variant %q was accepted", kind, v)
			}
		}
	}
	over := base
	over.kind, over.length = engine.MediaDocument, mediaMaxDeclared+1
	if _, ok := over.declared(); ok {
		t.Error("a descriptor over 2 GiB was accepted")
	}
}

type fakeFetch struct {
	calls int
	src   whatsmeow.DownloadableMessage
	job   bool
	dst   whatsmeow.File
	write []byte
	then  func(ctx context.Context) error
}

func (f *fakeFetch) fetch(ctx context.Context, _ *whatsmeow.Client, src mediasrc.Source, dst whatsmeow.File) error {
	f.calls++
	f.src, _ = src.Message()
	_, f.job = ctx.Value(mediaJobKey{}).(*mediaJob)
	f.dst = dst
	if f.write != nil {
		if _, err := io.Copy(dst, bytes.NewReader(f.write)); err != nil {
			return err
		}
	}
	if f.then != nil {
		return f.then(ctx)
	}
	return nil
}

func captured(t *testing.T, c *Client, fx mediaFixture, length int64) (engine.Message, mediaFields) {
	t.Helper()
	f := syntheticMedia(length)
	m := liveMedia(t, c, "3EB0F1", fx.build(f))
	if m.MediaRef == "" {
		t.Fatalf("%s of %d bytes was not captured", fx.name, length)
	}
	return m, f
}

type sameRow struct{}

func (sameRow) Canonical(_ context.Context, c policy.CanonicalChat) (policy.CanonicalChat, error) {
	return c, nil
}

type mappedRow map[string]policy.CanonicalChat

func (m mappedRow) Canonical(_ context.Context, c policy.CanonicalChat) (policy.CanonicalChat, error) {
	if to, ok := m[c.JID()]; ok {
		return to, nil
	}
	return c, nil
}

type failingRow struct{}

func (failingRow) Canonical(context.Context, policy.CanonicalChat) (policy.CanonicalChat, error) {
	return policy.CanonicalChat{}, errors.New("synthetic: the archive is busy")
}

func rowOf(t *testing.T, jid string) policy.CanonicalChat {
	t.Helper()
	c, ok := policy.Normalize(jid)
	if !ok {
		t.Fatalf("Normalize(%q) rejected a test identifier", jid)
	}
	return c
}

func bind(req engine.MediaRequest, m engine.Message) engine.MediaRequest {
	req.Chat, _ = policy.Normalize(m.Chat)
	req.Sender, _ = policy.Normalize(m.Sender)
	req.Canonical = sameRow{}
	return req
}

func request(m engine.Message) engine.MediaRequest {
	return bind(engine.MediaRequest{ID: m.ID, Ref: m.MediaRef, Kind: m.MediaKind, Declared: m.MediaBytes}, m)
}

func stagedBytes(t *testing.T, s engine.Staged) []byte {
	t.Helper()
	out := make([]byte, s.Size())
	if n, err := s.ReadAt(out, 0); n != len(out) || err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("ReadAt = %d, %v", n, err)
	}
	return out
}

func TestFetchMediaStagesInRAMUpToTheThresholdAndOnDiskAbove(t *testing.T) {
	document := mediaFixtures[6]
	for name, tt := range map[string]struct {
		length int64
		disk   bool
	}{
		"a small attachment":       {length: 4096},
		"an empty attachment":      {length: 0},
		"exactly the threshold":    {length: mediaRAMThreshold},
		"just above the threshold": {length: mediaRAMThreshold + 1, disk: true},
	} {
		c := mediaClient(t, epoch.Add(time.Hour))
		m, f := captured(t, c, document, tt.length)
		plain := randomBytes(tt.length)
		fake := &fakeFetch{write: plain}
		c.fetch = fake.fetch
		staged, err := c.FetchMedia(t.Context(), request(m))
		if err != nil {
			t.Fatalf("FetchMedia of %s = %v", name, err)
		}
		switch s := staged.(type) {
		case *capFile:
			if tt.disk || int64(cap(s.buf)) != tt.length+mediaOverhead {
				t.Errorf("%s staged in RAM with capacity %d, want disk %v or capacity %d", name, cap(s.buf), tt.disk, tt.length+mediaOverhead)
			}
		case *stage.Sealed:
			if !tt.disk {
				t.Errorf("%s staged on disk", name)
			}
		default:
			t.Fatalf("%s staged in %T", name, staged)
		}
		if fake.dst != any(staged) || !fake.job || !bytes.Equal(stagedBytes(t, staged), plain) {
			t.Errorf("%s: the seam wrote into %T with a job %v, and the staged bytes differ from the plaintext", name, fake.dst, fake.job)
		}
		doc, ok := fake.src.(*waE2E.DocumentMessage)
		if !ok || doc.GetDirectPath() != f.path || !bytes.Equal(doc.GetMediaKey(), f.key) || !bytes.Equal(doc.GetFileSHA256(), f.sha) ||
			!bytes.Equal(doc.GetFileEncSHA256(), f.enc) || doc.GetFileLength() != *f.fileLength() {
			t.Errorf("%s: the seam was handed %#v, want the captured document", name, fake.src)
		}
		staged.Erase()
	}
}

func TestDownloadableBuildsTheRecordedClass(t *testing.T) {
	want := map[engine.MediaKind]struct {
		class string
		mms   whatsmeow.MediaType
	}{
		engine.MediaImage:    {"*waE2E.ImageMessage", whatsmeow.MediaImage},
		engine.MediaVideo:    {"*waE2E.VideoMessage", whatsmeow.MediaVideo},
		engine.MediaAudio:    {"*waE2E.AudioMessage", whatsmeow.MediaAudio},
		engine.MediaDocument: {"*waE2E.DocumentMessage", whatsmeow.MediaDocument},
		engine.MediaSticker:  {"*waE2E.StickerMessage", whatsmeow.MediaImage},
	}
	for kind, w := range want {
		d := descriptor{directPath: "/example/m1", mediaKey: randomBytes(32), fileSHA256: randomBytes(32), fileEncSHA256: randomBytes(32), length: 7, kind: kind}
		msg, ok := downloadable(d).Message()
		if !ok || fmt.Sprintf("%T", msg) != w.class || whatsmeow.GetMediaType(msg) != w.mms || msg.GetDirectPath() != d.directPath ||
			!bytes.Equal(msg.GetMediaKey(), d.mediaKey) || !bytes.Equal(msg.GetFileSHA256(), d.fileSHA256) || !bytes.Equal(msg.GetFileEncSHA256(), d.fileEncSHA256) {
			t.Errorf("downloadable(%s) = %T %v, want a %s of media type %s carrying the descriptor", kind, msg, msg, w.class, w.mms)
		}
	}
	if msg, ok := downloadable(descriptor{kind: "thumbnail"}).Message(); ok {
		t.Errorf("downloadable of an unknown kind = %v", msg)
	}
}

func TestFetchMediaMapsFailuresToSentinels(t *testing.T) {
	image := mediaFixtures[0]
	throttled := func(ctx context.Context) error {
		return ctx.Value(mediaJobKey{}).(*mediaJob).end(&mediaThrottledError{RetryAfter: 90 * time.Second})
	}
	ended := func(cause error) func(context.Context) error {
		return func(ctx context.Context) error {
			ctx.Value(mediaJobKey{}).(*mediaJob).cancel(cause)
			return fmt.Errorf("synthetic: %w", context.Cause(ctx))
		}
	}
	returns := func(err error) func(context.Context) error {
		return func(context.Context) error { return fmt.Errorf("synthetic library text: %w", err) }
	}
	status := func(code int) error { return whatsmeow.DownloadHTTPError{Response: &http.Response{StatusCode: code}} }
	for name, tt := range map[string]struct {
		then  func(context.Context) error
		write int64
		want  error
	}{
		"a 403":                      {then: returns(whatsmeow.ErrMediaDownloadFailedWith403), want: engine.ErrMediaExpired},
		"a 404":                      {then: returns(whatsmeow.ErrMediaDownloadFailedWith404), want: engine.ErrMediaExpired},
		"a 410":                      {then: returns(whatsmeow.ErrMediaDownloadFailedWith410), want: engine.ErrMediaExpired},
		"a server's 410 response":    {then: returns(status(http.StatusGone)), want: engine.ErrMediaExpired},
		"a server's 400 response":    {then: returns(status(http.StatusBadRequest)), want: engine.ErrMediaFailed},
		"a plaintext hash mismatch":  {then: returns(whatsmeow.ErrInvalidMediaSHA256), want: engine.ErrMediaFailed},
		"an encrypted hash mismatch": {then: returns(whatsmeow.ErrInvalidMediaEncSHA256), want: engine.ErrMediaFailed},
		"a MAC mismatch":             {then: returns(whatsmeow.ErrInvalidMediaHMAC), want: engine.ErrMediaFailed},
		"a short file":               {then: returns(whatsmeow.ErrTooShortFile), want: engine.ErrMediaFailed},
		"no connection":              {then: returns(whatsmeow.ErrNotConnected), want: engine.ErrNotConnected},
		"no client":                  {then: returns(whatsmeow.ErrClientIsNil), want: engine.ErrNotConnected},
		"a stalled request":          {then: returns(errMediaStall), want: engine.ErrMediaFailed},
		"the job's deadline":         {then: ended(errMediaDeadline), want: engine.ErrMediaFailed},
		"the job's byte budget":      {then: ended(errMediaEgress), want: engine.ErrMediaFailed},
		"a failed watchdog":          {then: ended(errMediaWatchdog), want: engine.ErrMediaFailed},
		"a 404 after the job ended": {then: func(ctx context.Context) error {
			_ = ended(errMediaDeadline)(ctx)
			return whatsmeow.ErrMediaDownloadFailedWith404
		}, want: engine.ErrMediaFailed},
		"plaintext over the declared": {write: 1, want: engine.ErrMediaFailed},
	} {
		c := mediaClient(t, epoch.Add(time.Hour))
		m, _ := captured(t, c, image, 64)
		fake := &fakeFetch{write: randomBytes(64 + tt.write), then: tt.then}
		c.fetch = fake.fetch
		staged, err := c.FetchMedia(t.Context(), request(m))
		if err != tt.want || staged != nil {
			t.Errorf("FetchMedia with %s = %v, %v, want exactly %v and nothing staged", name, staged, err, tt.want)
		}
		if f, ok := fake.dst.(*capFile); !ok || f.buf != nil || f.Size() != 0 {
			t.Errorf("FetchMedia with %s left %T staged", name, fake.dst)
		}
	}
	c := mediaClient(t, epoch.Add(time.Hour))
	m, _ := captured(t, c, image, 64)
	c.fetch = (&fakeFetch{then: throttled}).fetch
	_, err := c.FetchMedia(t.Context(), request(m))
	var th *engine.ThrottledError
	if !errors.As(err, &th) || th.RetryAfter != 90*time.Second || !errors.Is(err, engine.ErrMediaThrottled) {
		t.Fatalf("FetchMedia when the server throttles = %v, want a throttle of 90s", err)
	}
}

func TestFetchMediaRefusesBeforeTheSeam(t *testing.T) {
	image, document := mediaFixtures[0], mediaFixtures[6]
	now := epoch.Add(time.Hour)
	c := mediaClient(t, now)
	m, f := captured(t, c, image, 64)
	big, _ := captured(t, c, document, 2<<20)
	forged := func(d descriptor, declared int64) engine.MediaRequest {
		ref, _ := fakeSealer{}.SealMedia(m.ID, d.marshal())
		return bind(engine.MediaRequest{ID: m.ID, Ref: ref, Kind: d.kind, Declared: declared}, m)
	}
	valid := descriptor{directPath: f.path, mediaKey: f.key, fileSHA256: f.sha, fileEncSHA256: f.enc, length: 64, kind: engine.MediaImage, chat: m.Chat, sender: m.Sender}
	relative, oversized, unknown := valid, valid, valid
	relative.directPath = "example/m1"
	oversized.length = imageCeiling + 1
	unknown.kind = "thumbnail"
	for name, tt := range map[string]struct {
		client *Client
		req    engine.MediaRequest
		want   error
	}{
		"downloads off":              {client: mediaClient(t, now, func(o *Options) { o.MediaMaxBytes = 0 }), req: request(m), want: engine.ErrMediaUnavailable},
		"a reference for another id": {req: bind(engine.MediaRequest{ID: "3EB0F2", Ref: m.MediaRef, Kind: m.MediaKind, Declared: m.MediaBytes}, m), want: engine.ErrMediaUnavailable},
		"no reference":               {req: bind(engine.MediaRequest{ID: m.ID, Kind: m.MediaKind, Declared: m.MediaBytes}, m), want: engine.ErrMediaUnavailable},
		"another kind":               {req: bind(engine.MediaRequest{ID: m.ID, Ref: m.MediaRef, Kind: engine.MediaVideo, Declared: m.MediaBytes}, m), want: engine.ErrMediaUnavailable},
		"another length":             {req: bind(engine.MediaRequest{ID: m.ID, Ref: m.MediaRef, Kind: m.MediaKind, Declared: 65}, m), want: engine.ErrMediaUnavailable},
		"a relative path":            {req: forged(relative, 64), want: engine.ErrMediaUnavailable},
		"an unknown kind":            {req: forged(unknown, 64), want: engine.ErrMediaUnavailable},
		"an image over its ceiling":  {req: forged(oversized, imageCeiling+1), want: engine.ErrMediaTooLarge},
		"a lowered cap":              {client: mediaClient(t, now, func(o *Options) { o.MediaMaxBytes = 1 << 20 }), req: request(big), want: engine.ErrMediaTooLarge},
		"a deadline already passed":  {req: bind(engine.MediaRequest{ID: m.ID, Ref: m.MediaRef, Kind: m.MediaKind, Declared: m.MediaBytes, Deadline: now}, m), want: engine.ErrMediaFailed},
	} {
		client := tt.client
		if client == nil {
			client = c
		}
		fake := &fakeFetch{}
		client.fetch = fake.fetch
		if staged, err := client.FetchMedia(t.Context(), tt.req); err != tt.want || staged != nil || fake.calls != 0 {
			t.Errorf("FetchMedia with %s = %v, %v after %d seam calls, want exactly %v and no call", name, staged, err, fake.calls, tt.want)
		}
	}
}

func TestAReferenceOpensOnlyForItsOwnRow(t *testing.T) {
	image := mediaFixtures[0]
	c := mediaClient(t, epoch.Add(time.Hour))
	m, _ := captured(t, c, image, 64)
	victimLID := rowOf(t, "100000000000077@lid")
	moved := func(edit func(*engine.MediaRequest)) engine.MediaRequest {
		req := request(m)
		edit(&req)
		return req
	}
	for name, req := range map[string]engine.MediaRequest{
		"a row in another chat":           moved(func(r *engine.MediaRequest) { r.Chat = rowOf(t, "120363000000000002@g.us") }),
		"a row from another sender":       moved(func(r *engine.MediaRequest) { r.Sender = rowOf(t, "15550100004@s.whatsapp.net") }),
		"a row the chat and sender swap":  moved(func(r *engine.MediaRequest) { r.Chat, r.Sender = r.Sender, r.Chat }),
		"no canonicaliser":                moved(func(r *engine.MediaRequest) { r.Canonical = nil }),
		"a canonicaliser that fails":      moved(func(r *engine.MediaRequest) { r.Canonical = failingRow{} }),
		"a sender merged into another id": moved(func(r *engine.MediaRequest) { r.Canonical = mappedRow{victim.String(): victimLID} }),
	} {
		fake := &fakeFetch{}
		c.fetch = fake.fetch
		if staged, err := c.FetchMedia(t.Context(), req); err != engine.ErrMediaUnavailable || staged != nil || fake.calls != 0 {
			t.Errorf("FetchMedia for %s = %v, %v after %d seam calls, want the refusal before the seam", name, staged, err, fake.calls)
		}
	}
	unparsable := descriptor{directPath: "/v/t62.7118-24/synthetic", mediaKey: []byte{1}, fileSHA256: make([]byte, 32), fileEncSHA256: make([]byte, 32), length: 64,
		kind: engine.MediaImage, chat: "not-a-chat", sender: m.Sender}
	ref, _ := fakeSealer{}.SealMedia(m.ID, unparsable.marshal())
	fake := &fakeFetch{}
	c.fetch = fake.fetch
	if _, err := c.FetchMedia(t.Context(), bind(engine.MediaRequest{ID: m.ID, Ref: ref, Kind: engine.MediaImage, Declared: 64}, m)); err != engine.ErrMediaUnavailable || fake.calls != 0 {
		t.Fatalf("FetchMedia of a reference whose chat does not parse = %v after %d seam calls", err, fake.calls)
	}

	merged := moved(func(r *engine.MediaRequest) {
		r.Sender, r.Canonical = victimLID, mappedRow{victim.String(): victimLID}
	})
	fake = &fakeFetch{write: bytes.Repeat([]byte{7}, 64)}
	c.fetch = fake.fetch
	staged, err := c.FetchMedia(t.Context(), merged)
	if err != nil || fake.calls != 1 {
		t.Fatalf("FetchMedia of a reference sealed under a phone number after its sender's merge = %v after %d seam calls", err, fake.calls)
	}
	staged.Erase()
}

func TestFetchMediaOnDiskReleasesTheSpoolOnFailure(t *testing.T) {
	document := mediaFixtures[6]
	spool := newSpool(t)
	c := mediaClient(t, epoch.Add(time.Hour), func(o *Options) { o.MediaSpool = spool })
	m, _ := captured(t, c, document, mediaRAMThreshold+1)
	held, err := spool.Admit(1, func(error) {})
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	busy := &fakeFetch{}
	c.fetch = busy.fetch
	if staged, err := c.FetchMedia(t.Context(), request(m)); err != engine.ErrMediaFailed || staged != nil || busy.calls != 0 {
		t.Fatalf("FetchMedia with the spool busy = %v, %v after %d calls, want ErrMediaFailed and no call", staged, err, busy.calls)
	}
	held.Erase()
	over := &fakeFetch{write: make([]byte, mediaRAMThreshold+1+mediaOverhead+1)}
	c.fetch = over.fetch
	if staged, err := c.FetchMedia(t.Context(), request(m)); err != engine.ErrMediaFailed || staged != nil {
		t.Fatalf("FetchMedia writing past the spool's limit = %v, %v, want ErrMediaFailed", staged, err)
	}
	fits := &fakeFetch{write: randomBytes(mediaRAMThreshold + 1)}
	c.fetch = fits.fetch
	staged, err := c.FetchMedia(t.Context(), request(m))
	if err != nil {
		t.Fatalf("FetchMedia after a refused write = %v, want the spool released", err)
	}
	if !bytes.Equal(stagedBytes(t, staged), fits.write) {
		t.Fatal("the disk staging returned other bytes than the seam wrote")
	}
	staged.Erase()
}

func TestFetchMediaThroughTheProductionSeamNeedsAConnection(t *testing.T) {
	c := mediaClient(t, epoch.Add(time.Hour))
	m, _ := captured(t, c, mediaFixtures[0], 64)
	c.fetch = downloadMediaFile
	if staged, err := c.FetchMedia(t.Context(), request(m)); err != engine.ErrNotConnected || staged != nil {
		t.Fatalf("FetchMedia without a connection = %v, %v, want exactly engine.ErrNotConnected and nothing staged", staged, err)
	}
	c.mu.Lock()
	c.cli = nil
	c.mu.Unlock()
	if staged, err := c.FetchMedia(t.Context(), request(m)); err != engine.ErrNotConnected || staged != nil {
		t.Fatalf("FetchMedia without a client = %v, %v, want exactly engine.ErrNotConnected and nothing staged", staged, err)
	}
}

func TestTheMediaDownloaderRefusesAnEmptySource(t *testing.T) {
	r := newRig(t, pairedDevice())
	var declared mediasrc.Source
	literal := mediasrc.Source{}
	for name, src := range map[string]mediasrc.Source{"a declared variable": declared, "new": *new(mediasrc.Source), "a composite literal": literal, "Wrap(nil)": mediasrc.Wrap(nil)} {
		for _, cli := range []*whatsmeow.Client{r.c.current(), nil} {
			staged := newCapFile(1<<20, 0)
			if err := downloadMediaFile(t.Context(), cli, src, staged); !errors.Is(err, errMediaSource) || len(staged.bytes()) != 0 {
				t.Fatalf("a download of %s = %v, %d bytes, want errMediaSource before the library is called", name, err, len(staged.bytes()))
			}
		}
	}
}

func TestTheRAMStagingErasesItsBuffer(t *testing.T) {
	f := newCapFile(64+mediaOverhead, 64)
	if _, err := f.Write(bytes.Repeat([]byte{0xa5}, 64)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	backing := f.buf[:cap(f.buf)]
	f.Erase()
	f.Erase()
	if !bytes.Equal(backing, make([]byte, len(backing))) || f.Size() != 0 || f.buf != nil {
		t.Fatalf("after Erase the buffer holds %x and the file %d bytes", backing, f.Size())
	}
	if _, err := f.WriteAt([]byte{1}, 0); err == nil {
		t.Fatal("an erased staging file accepted a write")
	}
}

func TestTheDiskStagingDecryptsInPlace(t *testing.T) {
	plain := randomBytes(3*(32<<10) + 123)
	key, iv, macKey := randomBytes(32), randomBytes(16), randomBytes(32)
	sealed, err := newSpool(t).Admit(int64(len(plain)), func(error) {})
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	defer sealed.Erase()
	if _, _, _, _, err := cbcutil.EncryptStream(key, iv, macKey, bytes.NewReader(plain), sealed); err != nil {
		t.Fatalf("EncryptStream: %v", err)
	}
	if err := sealed.Truncate(sealed.Size() - 10); err != nil {
		t.Fatalf("Truncate: %v", err)
	}
	if _, err := sealed.Seek(0, io.SeekStart); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	if err := cbcutil.DecryptFile(key, iv, sealed); err != nil {
		t.Fatalf("DecryptFile: %v", err)
	}
	if got := stagedBytes(t, sealed); !bytes.Equal(got, plain) {
		t.Fatalf("decrypted %d bytes, want the %d plaintext bytes", len(got), len(plain))
	}
}
