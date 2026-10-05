//go:build !dev

package app

import "github.com/dortort/wawarden/internal/config"

func engineFor(config.Config) engineSource { return openWhatsApp }
