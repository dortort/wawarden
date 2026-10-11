package cursor_test

import (
	"testing"

	"github.com/dortort/wawarden/internal/cursor"
)

func FuzzOpenCursor(f *testing.F) {
	s := sealer(f, key, keyID, cursor.Retired{Key: otherKey, ID: otherKeyID})
	for _, sealing := range []*cursor.Sealer{s, sealer(f, otherKey, otherKeyID)} {
		for _, p := range []cursor.Position{{}, position} {
			text, err := sealing.Cursor(binding, p)
			if err != nil {
				f.Fatalf("Cursor: %v", err)
			}
			f.Add(text)
			f.Add(text + "\n")
			f.Add(text[:len(text)-1])
		}
	}
	f.Add("")
	f.Add("2026-01-02T03:04:05Z")
	f.Fuzz(func(t *testing.T, text string) {
		p, err := s.OpenCursor(binding, text)
		if err != nil {
			requireInvalid(t, err)
			return
		}
		_, err = s.OpenCursor(cursor.Binding{Client: "zzzzzzzz", Endpoint: binding.Endpoint, Filter: binding.Filter}, text)
		requireInvalid(t, err)
		again, err := s.Cursor(binding, p)
		if err != nil || len(again) != len(text) {
			t.Fatalf("an accepted cursor re-sealed to %q, %v", again, err)
		}
		if back, err := s.OpenCursor(binding, again); err != nil || back != p {
			t.Fatalf("the re-sealed cursor opened to %v, %v, want %v", back, err, p)
		}
	})
}

func FuzzOpenRef(f *testing.F) {
	s := refSealer(f, key, keyID, cursor.Retired{Key: otherKey, ID: otherKeyID})
	current := refSealer(f, key, keyID)
	for _, sealing := range []*cursor.Sealer{s, refSealer(f, otherKey, otherKeyID)} {
		text, err := sealing.SealRef(binding.Client, ref)
		if err != nil {
			f.Fatalf("SealRef: %v", err)
		}
		f.Add(text)
		f.Add(text + "\r")
	}
	f.Add(cursor.RefPrefix)
	f.Add("m1_AAAA")
	f.Fuzz(func(t *testing.T, text string) {
		r, err := s.OpenRef(binding.Client, text)
		if err != nil {
			requireInvalid(t, err)
			return
		}
		_, err = s.OpenRef("zzzzzzzz", text)
		requireInvalid(t, err)
		again, err := s.SealRef(binding.Client, r)
		if err != nil {
			t.Fatalf("an accepted reference does not re-seal: %v", err)
		}
		if _, err := current.OpenRef(binding.Client, text); err == nil && again != text {
			t.Fatalf("a reference under the current key re-sealed to %q, want the same text", again)
		}
		if back, err := current.OpenRef(binding.Client, again); err != nil || back != r {
			t.Fatalf("the re-sealed reference opened to %+v, %v under the current key alone, want %+v", back, err, r)
		}
	})
}
