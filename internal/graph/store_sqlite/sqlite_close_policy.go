package store_sqlite

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"unsafe"

	"modernc.org/libc"
	sqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// Every graph-file connection, including pool replacements, must opt out of
// SQLite's otherwise implicit last-connection WAL checkpoint. Store.Close owns
// the explicit final checkpoint; an error there must not secretly start a
// second checkpoint while sql.DB closes its connections. PERSIST_WAL controls
// file persistence and is not a substitute for NO_CKPT_ON_CLOSE.
func openSQLiteStoreDB(dsn string) (*sql.DB, error) {
	connector, err := newSQLiteStoreConnector(dsn)
	if err != nil {
		return nil, err
	}
	return sql.OpenDB(connector), nil
}

func newSQLiteStoreConnector(dsn string) (driver.Connector, error) {
	// The pinned driver's connector retains the default registered functions,
	// collations, connection hooks and virtual tables, unlike a fresh Driver.
	inner, err := sqlite.NewConnector(dsn)
	if err != nil {
		return nil, err
	}
	return noCloseCheckpointConnector{inner: inner}, nil
}

type noCloseCheckpointConnector struct{ inner driver.Connector }

func (c noCloseCheckpointConnector) Driver() driver.Driver { return c.inner.Driver() }

func (c noCloseCheckpointConnector) Connect(ctx context.Context) (driver.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	conn, err := c.inner.Connect(ctx)
	if err != nil {
		return nil, err
	}
	if conn == nil {
		return nil, errors.New("sqlite NO_CKPT_ON_CLOSE: connector returned no physical connection")
	}
	// Configure the raw modernc connection before read-gate wrapping. Returning
	// that same connection preserves every optional driver interface.
	if err := configureNoCloseCheckpoint(conn); err != nil {
		return nil, errors.Join(err, conn.Close())
	}
	if err := ctx.Err(); err != nil {
		return nil, errors.Join(err, conn.Close())
	}
	return conn, nil
}

func configureNoCloseCheckpoint(conn driver.Conn) error {
	value, err := sqliteNoCloseCheckpointMode(conn, 1)
	if err != nil {
		return err
	}
	if value != 1 {
		return fmt.Errorf("sqlite NO_CKPT_ON_CLOSE set returned %d, want 1", value)
	}
	value, err = sqliteNoCloseCheckpointMode(conn, -1)
	if err != nil {
		return err
	}
	if value != 1 {
		return fmt.Errorf("sqlite NO_CKPT_ON_CLOSE verification returned %d, want 1", value)
	}
	return nil
}

// sqliteNoCloseCheckpointMode uses only the existing checked db/TLS seam.
// modernc v1.61 conn.go dbConfigBool documents libc.VaList's fixed eight-byte
// slots on every target: one int32 onoff and one output pointer. This allocation
// owns both slots and its int32 output; no new driver struct offsets are used.
func sqliteNoCloseCheckpointMode(conn driver.Conn, onoff int32) (int32, error) {
	db, tls, ok := driverConnHandles(conn)
	if !ok || db == 0 || tls == nil {
		return 0, errors.New("sqlite NO_CKPT_ON_CLOSE: unsupported physical connection handles")
	}
	const allocationBytes = 24 // two 8-byte varargs plus aligned int32 output
	bp := libc.Xmalloc(tls, allocationBytes)
	if bp == 0 {
		return 0, errors.New("sqlite NO_CKPT_ON_CLOSE: varargs allocation failed")
	}
	defer libc.Xfree(tls, bp)
	out := bp + 16
	// The pointer describes our own allocation, never a driver struct field.
	value := *(**int32)(unsafe.Pointer(&out))
	*value = -1
	rc := sqlite3.Xsqlite3_db_config(tls, db, sqlite3.SQLITE_DBCONFIG_NO_CKPT_ON_CLOSE, libc.VaList(bp, onoff, out))
	if rc != sqlite3.SQLITE_OK {
		return 0, fmt.Errorf("sqlite NO_CKPT_ON_CLOSE(%d): rc=%d", onoff, rc)
	}
	return *value, nil
}
