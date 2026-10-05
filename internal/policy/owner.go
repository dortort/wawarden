package policy

import "strings"

const (
	minOwnerDigits = 7
	maxOwnerDigits = 15
)

func OwnerDigits(e164 string) (string, bool) {
	d, ok := strings.CutPrefix(e164, "+")
	if !ok || len(d) < minOwnerDigits || !digits(d, maxOwnerDigits) || d[0] == '0' {
		return "", false
	}
	return d, true
}
