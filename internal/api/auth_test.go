package api

import (
	"net/http"
	"testing"
)

func TestBearerToken(t *testing.T) {
	tests := []struct {
		name   string
		values []string
		want   string
		ok     bool
	}{
		{name: "bearer", values: []string{"Bearer synthetic-1"}, want: "synthetic-1", ok: true},
		{name: "scheme in lower case", values: []string{"bearer synthetic-1"}, want: "synthetic-1", ok: true},
		{name: "scheme in upper case", values: []string{"BEARER synthetic-1"}, want: "synthetic-1", ok: true},
		{name: "missing"},
		{name: "empty", values: []string{""}},
		{name: "scheme only", values: []string{"Bearer"}},
		{name: "scheme and space", values: []string{"Bearer "}},
		{name: "two spaces", values: []string{"Bearer  synthetic-1"}},
		{name: "tab separator", values: []string{"Bearer\tsynthetic-1"}},
		{name: "two words", values: []string{"Bearer synthetic-1 synthetic-2"}},
		{name: "embedded tab", values: []string{"Bearer synthetic-1\tsynthetic-2"}},
		{name: "other scheme", values: []string{"Basic synthetic-1"}},
		{name: "no separator", values: []string{"Bearersynthetic-1"}},
		{name: "two headers", values: []string{"Bearer synthetic-1", "Bearer synthetic-1"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := http.Header{}
			for _, v := range tt.values {
				h.Add("Authorization", v)
			}
			got, ok := bearerToken(h)
			if got != tt.want || ok != tt.ok {
				t.Fatalf("bearerToken() = %q, %v; want %q, %v", got, ok, tt.want, tt.ok)
			}
		})
	}
}
