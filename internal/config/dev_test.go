//go:build dev

package config

import "testing"

func TestFakeEngineSetting(t *testing.T) {
	if cfg, r := Load(environ(withDataDir(t, nil)), testOptions()); r != nil || cfg.Dev != (Dev{}) {
		t.Fatalf("Load without the variable = %+v, %v", cfg.Dev, r)
	}
	for value, want := range map[string]Dev{"0": {}, "1": {FakeEngine: true}, "wrong_account": {FakeEngine: true, FakeWrongAccount: true}} {
		cfg, r := Load(environ(withDataDir(t, map[string]string{envDevFakeEngine: value})), testOptions())
		if r != nil || cfg.Dev != want {
			t.Fatalf("WAWARDEN_DEV_FAKE_ENGINE=%s: %+v, %v; want %+v", value, cfg.Dev, r, want)
		}
	}
	for _, value := range []string{"", "2", "true", "yes", " 1", "1 ", "WRONG_ACCOUNT", "wrong-account"} {
		_, r := Load(environ(withDataDir(t, map[string]string{envDevFakeEngine: value})), testOptions())
		if r == nil || r.Reason != reasonDevFakeEngineInvalid || r.Variable != envDevFakeEngine {
			t.Fatalf("WAWARDEN_DEV_FAKE_ENGINE=%q gave %v, want the refusal %s", value, r, reasonDevFakeEngineInvalid)
		}
	}
}
