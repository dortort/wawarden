// Package session owns session.db: the protocol library's device store, kept in a SQLite file opened with the same checks, settings and lock as the archive.
package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"

	"github.com/dortort/wawarden/internal/store/internal/db"
)

type Profile = db.Profile

type Refusal = db.Refusal

const upgradeTimeout = time.Minute

const dialect = "sqlite"

type Options struct {
	DataDir string
	UID     int
	Profile Profile
	Logger  *slog.Logger
}

type Store struct {
	db        *db.DB
	container *sqlstore.Container
}

func Open(ctx context.Context, opts Options) (*Store, error) {
	d, err := db.Open(ctx, db.Session, db.Options{DataDir: opts.DataDir, UID: opts.UID, Profile: opts.Profile, Logger: opts.Logger})
	if err != nil {
		return nil, err
	}
	container := sqlstore.NewWithDB(d.RawHandle(), dialect, nil)
	upgrade, cancel := context.WithTimeout(ctx, upgradeTimeout)
	defer cancel()
	if err := container.Upgrade(upgrade); err != nil {
		return nil, errors.Join(fmt.Errorf("session: upgrade the device store: %w", err), d.Close())
	}
	return &Store{db: d, container: container}, nil
}

func (s *Store) Device(ctx context.Context) (*store.Device, error) {
	devices, err := s.container.GetAllDevices(ctx)
	switch {
	case err != nil:
		return nil, fmt.Errorf("session: read the stored device: %w", err)
	case len(devices) > 1:
		return nil, errManyDevices
	case len(devices) == 1:
		return devices[0], nil
	}
	return s.container.NewDevice(), nil
}

var errManyDevices = errors.New("session: session.db holds more than one device")

func (s *Store) NewDevice() *store.Device { return s.container.NewDevice() }

func (s *Store) Healthy() bool { return s.db.Healthy() }

func (s *Store) Close() error { return s.db.Close() }
