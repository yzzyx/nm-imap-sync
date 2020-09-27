package sync

import (
	"context"
	"database/sql"
	"path/filepath"

	notmuch "github.com/zenhack/go.notmuch"
)

// DB is a structure for checking the
// sync status of messages in a maildir,
type DB struct {
	dbpath   string
	db       *sql.DB
	nmDBPath string
	nmdb     *notmuch.DB
}

// New creates a new sync-db instance, and applies all migrations
func New(ctx context.Context, dbPath string) (*DB, error) {
	syncdbPath := filepath.Join(dbPath, ".nmsyncdb")
	sqliteDatabase, err := sql.Open("sqlite3", syncdbPath) // Open the created SQLite File
	if err != nil {
		return nil, err
	}

	db := &DB{
		dbpath: dbPath,
		db:     sqliteDatabase,
	}

	err = db.createOrUpgrade()
	if err != nil {
		return nil, err
	}

	err = db.migrate(ctx)
	if err != nil {
		db.db.Close()
		return nil, err
	}

	return db, nil
}

// Close closes the underlying database
func (db *DB) Close() {
	if db.db != nil {
		db.db.Close()
	}

	if db.nmdb != nil {
		db.nmdb.Close()
	}
}
