package digestcompare

import "github.com/dortort/wawarden/internal/token"

func Same(a, b token.Digest) bool { return a == b }
