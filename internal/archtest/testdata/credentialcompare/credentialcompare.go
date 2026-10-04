package credentialcompare

import "github.com/dortort/wawarden/internal/policy"

func Same(a, b policy.AdminCredential) bool { return a == b }
