package grantfield

import "github.com/dortort/wawarden/internal/policy"

func Forge() policy.ReadGrant {
	var g policy.ReadGrant
	g.ok = true
	return g
}
