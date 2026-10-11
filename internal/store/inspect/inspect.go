// Package inspect checks read-only copies of the archive and the device store taken from a backup and reports counts, never an identifier.
package inspect

import (
	"context"
	"errors"
	"time"

	"github.com/dortort/wawarden/internal/store/admin"
	"github.com/dortort/wawarden/internal/store/ingest"
	"github.com/dortort/wawarden/internal/store/internal/db"
)

const (
	checkLimit  = 30 * time.Minute
	notReadable = "the copy cannot be read as a database"
	integrityOK = "ok"

	selectIntegrity     = "SELECT integrity_check FROM pragma_integrity_check LIMIT 5"
	selectUserVersion   = "SELECT user_version FROM pragma_user_version"
	selectAuditRows     = "SELECT count(*) FROM audit"
	selectDeviceVersion = "SELECT version FROM whatsmeow_version"
	selectDevices       = "SELECT count(*) FROM whatsmeow_device"
	selectPreKeys       = "SELECT count(*) FROM whatsmeow_pre_keys"
	selectSessions      = "SELECT count(*) FROM whatsmeow_sessions"
)

type Integrity struct {
	OK       bool
	Problems []string
	Counted  bool
}

type Archive struct {
	Integrity
	SchemaVersion    int
	Chats            int64
	Messages         int64
	BlobsPending     int64
	BlobsQuarantined int64
	InboxBacklog     int64
	InboxQuarantined int64
	ClientsActive    int64
	AuditRows        int64
}

type Device struct {
	Integrity
	SchemaVersion int
	Paired        bool
	PreKeys       int64
	Sessions      int64
}

func Files() (archive, session string) { return db.Archive.File(), db.Session.File() }

func LatestArchiveVersion() int { return ingest.LatestSchemaVersion() }

func FreeBytes(dir string) (uint64, error) { return db.FreeBytesOf(dir) }

func CheckArchive(ctx context.Context, path string) (Archive, error) {
	var a Archive
	err := inspect(ctx, path, &a.Integrity, func(ctx context.Context, d *db.DB) error {
		if err := d.Read(ctx, "inspect.archive", func(ctx context.Context, q db.Querier) error {
			if err := q.QueryRowContext(ctx, selectUserVersion).Scan(&a.SchemaVersion); err != nil {
				return err
			}
			return q.QueryRowContext(ctx, selectAuditRows).Scan(&a.AuditRows)
		}); err != nil {
			return err
		}
		c, err := admin.New(d).Counters(ctx)
		if err != nil {
			return err
		}
		clients, err := admin.NewClients(d, nil, time.Now).Counts(ctx)
		if err != nil {
			return err
		}
		a.Chats, a.Messages, a.BlobsPending, a.BlobsQuarantined = c.Chats, c.Messages, c.BlobsPending, c.BlobsQuarantined
		a.InboxBacklog, a.InboxQuarantined, a.ClientsActive = c.InboxBacklog, c.InboxQuarantined, clients.Active
		return nil
	})
	return a, err
}

func CheckDevice(ctx context.Context, path string) (Device, error) {
	var dev Device
	err := inspect(ctx, path, &dev.Integrity, func(ctx context.Context, d *db.DB) error {
		var devices int64
		err := d.Read(ctx, "inspect.device", func(ctx context.Context, q db.Querier) error {
			return errors.Join(
				q.QueryRowContext(ctx, selectDeviceVersion).Scan(&dev.SchemaVersion),
				q.QueryRowContext(ctx, selectDevices).Scan(&devices),
				q.QueryRowContext(ctx, selectPreKeys).Scan(&dev.PreKeys),
				q.QueryRowContext(ctx, selectSessions).Scan(&dev.Sessions),
			)
		})
		dev.Paired = devices > 0
		return err
	})
	return dev, err
}

func inspect(ctx context.Context, path string, integrity *Integrity, count func(context.Context, *db.DB) error) error {
	d, err := db.OpenCopy(ctx, path, checkLimit)
	if err != nil {
		integrity.Problems = []string{notReadable}
		return ctx.Err()
	}
	return errors.Join(checkAndCount(ctx, d, integrity, count), d.Close())
}

func checkAndCount(ctx context.Context, d *db.DB, integrity *Integrity, count func(context.Context, *db.DB) error) error {
	var lines []string
	err := d.Read(ctx, "inspect.integrity", func(ctx context.Context, q db.Querier) error {
		rows, err := q.QueryContext(ctx, selectIntegrity)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				return err
			}
			lines = append(lines, line)
		}
		return rows.Err()
	})
	switch {
	case ctx.Err() != nil:
		return ctx.Err()
	case err != nil:
		integrity.Problems = []string{notReadable}
	case len(lines) == 1 && lines[0] == integrityOK:
		integrity.OK = true
	default:
		integrity.Problems = lines
	}
	if !integrity.OK {
		return nil
	}
	err = count(ctx, d)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	integrity.Counted = err == nil
	return nil
}
