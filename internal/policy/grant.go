package policy

import "github.com/dortort/wawarden/internal/policy/internal/seal"

type (
	ReadGrant  = seal.ReadGrant
	WriteGrant = seal.WriteGrant
	AdminGrant = seal.AdminGrant
)
