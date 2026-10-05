package wa

import (
	"context"
	"errors"
	"net/http"

	"go.mau.fi/whatsmeow"

	"github.com/dortort/wawarden/internal/engine"
)

var errVersionFetch = errors.New("wa: the WhatsApp Web version could not be fetched")

type Versions struct {
	client *http.Client
}

var _ engine.VersionSource = (*Versions)(nil)

func NewVersions() *Versions {
	return newVersions(newTransport(systemDial()))
}

func newVersions(rt http.RoundTripper) *Versions {
	return &Versions{client: &http.Client{
		Transport:     capped{next: rt, max: maxVersionPage},
		Timeout:       versionTimeout,
		CheckRedirect: sameHostRedirects,
	}}
}

func (v *Versions) Latest(ctx context.Context) (engine.Version, error) {
	if v.client == nil {
		return engine.Version{}, errVersionFetch
	}
	got, err := whatsmeow.GetLatestVersion(ctx, v.client)
	if err != nil || got == nil {
		return engine.Version{}, errVersionFetch
	}
	return engine.Version(*got), nil
}
