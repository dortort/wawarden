package app

import (
	"fmt"
	"path/filepath"

	"github.com/dortort/wawarden/internal/stage"
)

func purgeSpool(dataDir string) (string, error) {
	data, err := filepath.Abs(dataDir)
	if err != nil {
		return "", fmt.Errorf("app: resolve the data directory: %w", err)
	}
	if _, err := stage.Purge(data); err != nil {
		return "", fmt.Errorf("app: remove the attachments staged before this start: %w", err)
	}
	return data, nil
}
