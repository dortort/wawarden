package policy

import "testing"

func TestOwnerDigits(t *testing.T) {
	for in, want := range map[string]string{
		"+15550100009":     "15550100009",
		"+1555010":         "1555010",
		"+155501000912345": "155501000912345",
	} {
		if got, ok := OwnerDigits(in); !ok || got != want {
			t.Errorf("OwnerDigits(%q) = %q, %v, want %q", in, got, ok, want)
		}
	}
	for _, in := range []string{
		"", "+", "15550100009", "+155501", "+1555010009123456", "+05550100009", "++15550100009", "+1555 0100009",
		"+1555-0100009", "+1555010000٩", "+15550100009\n", " +15550100009", "+15550100009@s.whatsapp.net",
	} {
		if got, ok := OwnerDigits(in); ok || got != "" {
			t.Errorf("OwnerDigits(%q) = %q, %v, want a refusal", in, got, ok)
		}
	}
}
