package db

import (
	"context"
	"database/sql/driver"
	"errors"
	"sync"

	"modernc.org/sqlite"
)

type driverConn interface {
	driver.Conn
	driver.ConnBeginTx
	driver.ConnPrepareContext
	driver.ExecerContext
	driver.QueryerContext
	driver.Pinger
	NewBackup(dstURI string) (*sqlite.Backup, error)
}

var errReconnect = errors.New("db: the connection that held the lock is gone, and a new one is refused")

type connector struct {
	drv       *sqlite.Driver
	dsn       string
	keepAlive bool
	onLost    func()

	mu        sync.Mutex
	connected bool
	lost      bool
}

func newConnector(dsn string, hook func(sqlite.ExecQuerierContext) error, keepAlive bool, onLost func()) *connector {
	drv := &sqlite.Driver{}
	drv.RegisterConnectionHook(func(c sqlite.ExecQuerierContext, _ string) error { return hook(c) })
	return &connector{drv: drv, dsn: dsn, keepAlive: keepAlive, onLost: onLost}
}

func (c *connector) Connect(ctx context.Context) (driver.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.connected && c.keepAlive {
		if !c.lost {
			c.lost = true
			c.onLost()
		}
		return nil, errReconnect
	}
	dc, err := c.drv.Open(c.dsn)
	if err != nil {
		return nil, err
	}
	full, ok := dc.(driverConn)
	if !ok {
		return nil, errors.Join(errors.New("db: the driver's connection lacks a required method"), dc.Close())
	}
	c.connected = true
	if !c.keepAlive {
		return full, nil
	}
	return &keptConn{driverConn: full}, nil
}

func (c *connector) Driver() driver.Driver { return c.drv }

func (c *connector) lostConnection() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lost
}

// keptConn stays valid after an interrupted statement: the driver would otherwise discard it, and closing the connection releases the exclusive lock.
type keptConn struct{ driverConn }

func (k *keptConn) IsValid() bool { return true }

func (k *keptConn) ResetSession(context.Context) error { return nil }
