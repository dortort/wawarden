//go:build !dev

package config

type Dev struct{}

func devSettings(map[string]string) (Dev, *Refusal) { return Dev{}, nil }
