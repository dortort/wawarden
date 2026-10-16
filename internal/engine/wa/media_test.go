package wa

import (
	"bytes"
	"fmt"
	"log/slog"
	"reflect"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"google.golang.org/protobuf/proto"

	"github.com/dortort/wawarden/internal/engine"
)

const (
	testMediaCap  = 100 << 20
	testRetention = 30 * 24 * time.Hour
)

func mediaClient(t *testing.T, now time.Time, configure ...func(*Options)) *Client {
	t.Helper()
	w, _ := newWriter()
	alerts, _ := newLogger(slog.LevelWarn)
	opts := Options{
		Device: pairedDevice(), Devices: &freshDevices{}, OwnerPhone: ownerPhone, HistoryMaxBytes: 1 << 20, Writer: w, Alerts: alerts,
		MediaMaxBytes: testMediaCap, MediaRefRetention: testRetention, MediaSealer: fakeSealer{},
	}
	for _, fn := range configure {
		fn(&opts)
	}
	c, err := newClient(opts, refusingDial(t), noDownload, func() time.Time { return now })
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
			if plain := path.read(t, off, "3EB0M1", raw); !reflect.DeepEqual(withoutReference(got), plain) {
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
		"downloads off":     func(o *Options) { o.MediaMaxBytes = 0 },
		"a refusing sealer": func(o *Options) { o.MediaSealer = fakeSealer{refuse: true} },
	} {
		off := mediaClient(t, epoch.Add(time.Hour), configure)
		for _, path := range receiptPaths {
			uncaptured(t, path.name+" with "+name, path.read(t, off, "3EB0L5", byKind["document"].build(syntheticMedia(1))))
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
