package logx

import (
	"encoding/json"
	"errors"
	"log/slog"
	"regexp"
	"strings"
	"testing"

	"github.com/dortort/wawarden/internal/token"
)

var generatedShape = regexp.MustCompile(`^ww(adm_|_[a-z2-7]{8}_)[A-Za-z0-9_-]{43}_[0-9a-f]{8}$`)

type generated struct{ full, secret, kept string }

func generateTokens(t *testing.T, n int) []generated {
	t.Helper()
	var out []generated
	for i := range n {
		if i%2 == 0 {
			full := token.NewAdmin()
			out = append(out, generated{full: full, secret: full[len(adminPrefix):], kept: adminPrefix + redacted})
			continue
		}
		id, full := token.NewClient()
		out = append(out, generated{full: full, secret: full[len(clientPrefix)+9:], kept: clientPrefix + id + "_" + redacted})
	}
	for _, g := range out {
		if !generatedShape.MatchString(g.full) || !tokenShape.MatchString(g.full) || tokenShape.FindString(g.full) != g.full {
			t.Fatalf("a generated token does not have the shape the writer redacts: %d characters", len(g.full))
		}
	}
	return out
}

func TestEveryGeneratedTokenIsRedactedInAnyContext(t *testing.T) {
	contexts := []func(a, b string) string{
		func(a, _ string) string { return a },
		func(a, _ string) string { return "Authorization: Bearer " + a },
		func(a, _ string) string { return `{"token":"` + a + `"}` },
		func(a, _ string) string { return "/v1/chats?token=" + a + "&x=1" },
		func(a, b string) string { return a + " and " + b },
		func(a, _ string) string { return a + "@s.whatsapp.net" },
		func(a, b string) string { return "x" + a + b + "y" },
		func(a, _ string) string { return "token:" + a + "." },
	}
	tokens := generateTokens(t, 3000)
	for i, g := range tokens {
		other := tokens[(i+1)%len(tokens)]
		w, out := keyedWriter(t)
		for c, context := range contexts {
			got := through(t, w, out, context(g.full, other.full)+"\n")
			if strings.Contains(got, g.secret) || strings.Contains(got, other.secret[:20]) || !strings.Contains(got, g.kept) {
				t.Fatalf("context %d: %q kept a secret or lost the marker %q", c, got, g.kept)
			}
		}
	}
}

func TestATokenSplitAcrossWritesIsRedacted(t *testing.T) {
	_, full := token.NewClient()
	for _, cut := range []int{1, 3, 4, 11, 12, 30, 55, 63} {
		w, out := keyedWriter(t)
		line := `{"msg":"x","token":"` + full + `"}` + "\n"
		at := len(`{"msg":"x","token":"`) + cut
		if _, err := w.Write([]byte(line[:at])); err != nil {
			t.Fatalf("Write: %v", err)
		}
		if _, err := w.Write([]byte(line[at:])); err != nil {
			t.Fatalf("Write: %v", err)
		}
		if strings.Contains(out.String(), full[12:]) || !json.Valid([]byte(out.String())) {
			t.Fatalf("cut at %d: %q", cut, out.String())
		}
	}
}

func TestNearMissesAreKept(t *testing.T) {
	admin := token.NewAdmin()
	id, client := token.NewClient()
	for _, near := range []string{
		"ww_" + strings.ToUpper(id) + client[11:],
		client[:12] + client[13:],
		"ww_" + id[:7] + client[11:],
		client[:len(client)-1] + "g",
		admin[:len(admin)-9],
		"www.example.test",
		"ww_ww_ww_",
		"ww_abcdefgh_[redacted] and wwadm_[redacted]",
	} {
		w, out := keyedWriter(t)
		if got := through(t, w, out, near+"\n"); got != near+"\n" {
			t.Fatalf("%q became %q", near, got)
		}
		if RedactTokens(near) != near {
			t.Fatalf("RedactTokens changed %q", near)
		}
	}
}

func TestRedactTokensActsOnAString(t *testing.T) {
	tokens := generateTokens(t, 200)
	for _, g := range tokens {
		in := "a " + g.full + " b"
		if got := RedactTokens(in); got != "a "+g.kept+" b" {
			t.Fatalf("RedactTokens = %q, want %q", got, "a "+g.kept+" b")
		}
	}
	if got := RedactTokens("no token here"); got != "no token here" {
		t.Fatalf("RedactTokens of plain text = %q", got)
	}
}

func TestTokensNeverReachTheOutputThroughTheLoggers(t *testing.T) {
	w, out := keyedWriter(t)
	logger := New(w, slog.LevelDebug)
	tokens := generateTokens(t, 40)
	for i, g := range tokens {
		values := []any{g.full, []byte(g.full), errors.New(g.full), stringer{g.full}, map[string]any{"t": g.full}, []any{g.full}}
		for j, v := range values {
			logger.Info("message "+g.full, "k", v)
			logger.With("w", v).WithGroup(g.full).Info("with", slog.Any("k", v))
			logger.Info("group", slog.Group("g", slog.Any(g.full, v)), slog.Int("i", i*10+j))
		}
	}
	text := out.String()
	for _, g := range tokens {
		if strings.Contains(text, g.secret) {
			t.Fatalf("a token's secret reached the output:\n%s", text)
		}
	}
	if !strings.Contains(text, adminPrefix+redacted) || !strings.Contains(text, "_"+redacted) {
		t.Fatal("no redaction marker in the output, so the test proves nothing")
	}
	for line := range strings.Lines(text) {
		if !json.Valid([]byte(line)) {
			t.Fatalf("not a JSON line: %q", line)
		}
	}
}
