package hooks

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/dortort/wawarden/internal/api/dto"
)

func sampleEnvelope() Envelope {
	name, text := "Alice <A&B>", "Hello \"world\"\n"
	return Envelope{
		ID: "evt_1", Event: EventMessageCreated, Time: "2026-10-05T12:00:00.000Z", Webhook: "wh_0123456789abcdef",
		Message: &dto.Message{
			Ref: "m1_REF", Chat: strings.Repeat("a", 32), Sender: dto.Sender{ID: "15550100001@s.whatsapp.net", Name: &name},
			TS: "2026-10-05T12:00:00.000Z", Kind: "text", Text: &text, TextDisplay: &text, Origin: "peer", Untrusted: true,
		},
	}
}

func TestTheDefaultBodyIsTheEnvelope(t *testing.T) {
	body, err := Body(sampleEnvelope(), "", ContentJSON)
	if err != nil {
		t.Fatalf("Body: %v", err)
	}
	var got struct {
		ID, Event, Webhook string
		Message            dto.Message
	}
	if err := json.Unmarshal(body, &got); err != nil || got.Event != EventMessageCreated || got.Message.Ref != "m1_REF" || *got.Message.Text != "Hello \"world\"\n" {
		t.Fatalf("Body = %s, %v", body, err)
	}
}

func TestAJSONTemplateKeepsItsShapeAndTypes(t *testing.T) {
	tmpl := `{"z":1.50,"text":"{{message.text}}","line":"From {{ message.sender.name }}: {{message.text}}","from_me":"{{message.from_me}}",` +
		`"reply":"{{message.reply_to}}","message":"{{message}}","nested":[true,null,{"{{message.kind}}":"{{event}}"}],"plain":"no placeholder"}`
	if _, err := Normalize(Spec{URL: "https://h.example.com", Payload: tmpl}); err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	body, err := Body(sampleEnvelope(), tmpl, ContentJSON)
	if err != nil {
		t.Fatalf("Body: %v", err)
	}
	if !json.Valid(body) {
		t.Fatalf("Body = %s, not JSON", body)
	}
	for _, want := range []string{
		`{"z":1.50,"text":"Hello \"world\"\n",`,
		`"from_me":false`,
		`"reply":null`,
		`"message":{"chat":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",`,
		`"nested":[true,null,{"{{message.kind}}":"message.created"}]`,
		`"plain":"no placeholder"}`,
	} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("Body = %s, lacks %s", body, want)
		}
	}
	var got struct{ Line string }
	if err := json.Unmarshal(body, &got); err != nil || got.Line != "From Alice <A&B>: Hello \"world\"\n" {
		t.Fatalf("line = %q, %v", got.Line, err)
	}
}

func TestATextTemplateIsInterpolated(t *testing.T) {
	body, err := Body(sampleEnvelope(), "{{message.sender.name}} wrote {{message.text}}in {{message.chat}}; reply {{message.reply_to}}.", ContentText)
	if err != nil {
		t.Fatalf("Body: %v", err)
	}
	if want := "Alice <A&B> wrote Hello \"world\"\nin " + strings.Repeat("a", 32) + "; reply ."; string(body) != want {
		t.Fatalf("Body = %q, want %q", body, want)
	}
}

func TestARenderedBodyIsCapped(t *testing.T) {
	e := sampleEnvelope()
	text := strings.Repeat("x", 32<<10)
	e.Message.Text = &text
	tmpl := `[` + strings.TrimSuffix(strings.Repeat(`"{{message.text}}",`, maxPlaceholders), ",") + `]`
	if _, err := Body(e, tmpl, ContentJSON); !errors.Is(err, errBodyLarge) {
		t.Fatalf("Body of %d placeholders of 32 KiB = %v, want too large", maxPlaceholders, err)
	}
	text = "short"
	if _, err := Body(e, tmpl, ContentJSON); err != nil {
		t.Fatalf("Body of short texts: %v", err)
	}
}

func TestEveryEnvelopeFieldCanBeNamed(t *testing.T) {
	for _, p := range []string{"id", "event", "time", "webhook", "message", "message.mref", "message.chat", "message.sender", "message.sender.id",
		"message.sender.name", "message.from_me", "message.ts", "message.kind", "message.text", "message.text_display", "message.media_type",
		"message.reply_to", "message.origin", "message.untrusted", "message.revoked", "message.edited_at"} {
		if !knownPaths[p] {
			t.Errorf("the template cannot name %s", p)
		}
	}
	for _, p := range []string{"message.seq", "secret", "message.sender.phone", "headers"} {
		if knownPaths[p] {
			t.Errorf("the template can name %s", p)
		}
	}
}
