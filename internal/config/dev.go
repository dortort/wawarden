//go:build dev

package config

const (
	envDevFakeEngine = "WAWARDEN_DEV_FAKE_ENGINE"

	reasonDevFakeEngineInvalid = "dev_fake_engine_invalid"
)

type Dev struct {
	FakeEngine       bool
	FakeWrongAccount bool
}

func init() { known = append(known, envDevFakeEngine) }

func devSettings(env map[string]string) (Dev, *Refusal) {
	v, ok := env[envDevFakeEngine]
	switch {
	case !ok || v == "0":
		return Dev{}, nil
	case v == "1":
		return Dev{FakeEngine: true}, nil
	case v == "wrong_account":
		return Dev{FakeEngine: true, FakeWrongAccount: true}, nil
	}
	return Dev{}, &Refusal{Reason: reasonDevFakeEngineInvalid, Variable: envDevFakeEngine, detail: "must be 0, 1 or wrong_account"}
}
