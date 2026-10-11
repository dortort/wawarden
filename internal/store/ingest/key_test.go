package ingest

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var keyModel = regexp.MustCompile(`^[\x21-\x7e]{8,128}$`)

func TestParseKey(t *testing.T) {
	for _, tt := range []struct {
		name, in string
		want     bool
	}{
		{"eight printable bytes", "abcdefgh", true},
		{"seven bytes", "abcdefg", false},
		{"128 bytes", strings.Repeat("k", 128), true},
		{"129 bytes", strings.Repeat("k", 129), false},
		{"the edges of the printable range", "!~!~!~!~", true},
		{"punctuation", "\"#$%&'()*+,-./:;<=>?@[\\]^_`{|}", true},
		{"a quoted structured-field string", `"abcdefgh"`, true},
		{"a space", "abcd efgh", false},
		{"a tab", "abcd\tefgh", false},
		{"NUL", "abcd\x00efgh", false},
		{"DEL", "abcd\x7fefgh", false},
		{"a non-ASCII byte", "abcd\xc3\xa9fgh", false},
		{"empty", "", false},
	} {
		k, ok := ParseKey(tt.in)
		switch {
		case ok != tt.want || k.Valid() != tt.want || ok != keyModel.MatchString(tt.in):
			t.Errorf("%s: ParseKey ok %v, Valid %v, want %v", tt.name, ok, k.Valid(), tt.want)
		case ok && k.Value() != tt.in:
			t.Errorf("%s: Value() differs from the input", tt.name)
		case !ok && k != (Key{}):
			t.Errorf("%s: a refused key is not the zero key", tt.name)
		}
	}
	if (Key{}).Valid() || (Key{}).Value() != "" {
		t.Fatal("the zero key is valid")
	}
}

func TestKeysNeverPrint(t *testing.T) {
	const secret = "key-material-0001"
	in := request(t, secret, alice, "synthetic", epoch)
	in.MsgID = "3EB0PRINTED"
	var js, text bytes.Buffer
	slog.New(slog.NewJSONHandler(&js, nil)).Info("send", slog.Any("key", in.Key), slog.Any("request", in))
	slog.New(slog.NewTextHandler(&text, nil)).Info("send", slog.Any("key", in.Key), slog.Any("request", in))
	for _, out := range []string{
		fmt.Sprint(in.Key), fmt.Sprintf("%v %+v %#v %s %q %x", in.Key, in.Key, in.Key, in.Key, in.Key, in.Key),
		fmt.Sprintf("%v %+v %#v", in, in, in), js.String(), text.String(),
	} {
		if strings.Contains(out, secret) || strings.Contains(out, hex.EncodeToString([]byte(secret))) {
			t.Fatalf("an idempotency key was printed: %s", out)
		}
	}
}

func TestFingerprintIsPinned(t *testing.T) {
	const ref = "0123456789abcdef0123456789abcdef"
	reply := &ReplyTarget{ID: "3EB0AABBCCDD", Sender: chat(t, alice), Seq: 42}
	plain, replied := Fingerprint(ref, "synthetic text", nil), Fingerprint(ref, "synthetic text", reply)
	if got := hex.EncodeToString(plain[:]); got != "168c0b88b885f1995b1957d4a90203be3a5657be41ec8ce220a5b400b50a9b0e" {
		t.Fatalf("Fingerprint without a reply = %s", got)
	}
	if got := hex.EncodeToString(replied[:]); got != "2f92d361994b28253c5fa174c2aca98503bc60764a65aa9eae701084a0770879" {
		t.Fatalf("Fingerprint with a reply = %s", got)
	}
	distinct := map[[fingerprintSize]byte]string{}
	for name, fp := range map[string][fingerprintSize]byte{
		"plain":                    plain,
		"replied":                  replied,
		"another chat":             Fingerprint(ref[:31]+"0", "synthetic text", nil),
		"another text":             Fingerprint(ref, "synthetic text.", nil),
		"the boundary moved":       Fingerprint(ref+"s", "ynthetic text", nil),
		"another reply id":         Fingerprint(ref, "synthetic text", &ReplyTarget{ID: "3EB0AABBCCDE", Sender: chat(t, alice), Seq: 42}),
		"another reply row":        Fingerprint(ref, "synthetic text", &ReplyTarget{ID: "3EB0AABBCCDD", Sender: chat(t, alice), Seq: 43}),
		"an empty reply":           Fingerprint(ref, "synthetic text", &ReplyTarget{}),
		"reply id moved into text": Fingerprint(ref, "synthetic text\x013EB0AABBCCDD", nil),
	} {
		if other, ok := distinct[fp]; ok {
			t.Fatalf("%s and %s share a fingerprint", name, other)
		}
		distinct[fp] = name
	}
}

func FuzzParseKey(f *testing.F) {
	for _, seed := range []string{
		"", "abcdefgh", "abcdefg", "abcd efgh", "abcd\x00efgh", "abcd\x7fefgh", "abcd\xffefgh", `"quoted-key"`,
		strings.Repeat("k", 128), strings.Repeat("k", 129),
	} {
		f.Add(seed)
	}
	s, err := Open(f.Context(), Options{DataDir: f.TempDir(), UID: os.Geteuid(), Profile: ProfileLocal, MinFreeBytes: 1, Logger: testLogger(),
		ReadTimeout: testDeadline, WriteTimeout: testDeadline, RewriteTimeout: testDeadline})
	if err != nil {
		f.Fatalf("Open: %v", err)
	}
	f.Cleanup(func() { _ = s.Close() })
	var n atomic.Int64
	f.Fuzz(func(t *testing.T, raw string) {
		k, ok := ParseKey(raw)
		if ok != keyModel.MatchString(raw) {
			t.Fatalf("ParseKey(%+q) = %v, the model says %v", raw, ok, !ok)
		}
		if !ok {
			if k.Valid() {
				t.Fatalf("ParseKey(%+q) refused a key it calls valid", raw)
			}
			return
		}
		if k.Value() != raw {
			t.Fatalf("ParseKey(%+q).Value() = %+q", raw, k.Value())
		}
		i := n.Add(1)
		id := fmt.Sprintf("%08x", i)
		in := SendReservation{Key: k, Chat: chat(t, alice), MsgID: "3EB0FUZZ", Clock: clockAt(epoch.Add(time.Duration(i) * 2 * time.Hour)),
			Limits: defaultLimits, Ready: func() bool { return true }}
		if res, err := s.ReserveSend(grantFor(t, id, true, alice), t.Context(), in); err != nil || res.Kind != Reserved {
			t.Fatalf("ReserveSend(%+q) = %+v, %v", raw, res, err)
		}
		if got := scalar[string](t, s, "SELECT key FROM idempotency WHERE client_id = ?", id); got != raw {
			t.Fatalf("the key %+q reads back as %+q", raw, got)
		}
	})
}

func TestAReKeyedReplySenderKeepsTheFingerprint(t *testing.T) {
	const ref = "0123456789abcdef0123456789abcdef"
	before := Fingerprint(ref, "synthetic text", &ReplyTarget{ID: "3EB0AABBCCDD", Sender: chat(t, bob), Seq: 42})
	if after := Fingerprint(ref, "synthetic text", &ReplyTarget{ID: "3EB0AABBCCDD", Sender: chat(t, bobLID), Seq: 42}); after != before {
		t.Fatal("the fingerprint changed when the reply target's sender was re-keyed")
	}
}
