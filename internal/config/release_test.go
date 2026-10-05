//go:build !dev

package config

import "testing"

func TestReleaseBuildsRefuseTheFakeEngine(t *testing.T) {
	for _, value := range []string{"0", "1", "wrong_account"} {
		_, r := Load(environ(withDataDir(t, map[string]string{"WAWARDEN_DEV_FAKE_ENGINE": value})), testOptions())
		if r == nil || r.Reason != reasonDevVariableInRelease || r.Variable != "WAWARDEN_DEV_FAKE_ENGINE" {
			t.Fatalf("a release build given WAWARDEN_DEV_FAKE_ENGINE=%s: %v, want the refusal %s", value, r, reasonDevVariableInRelease)
		}
	}
}
