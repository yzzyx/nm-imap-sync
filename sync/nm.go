package sync

import (
	"errors"

	notmuch "github.com/zenhack/go.notmuch"
)

// Wrap creates a readonly database connection, and executes the 'fn' function with this connection
func (db *DB) Wrap(fn func(db *notmuch.DB) error) error {
	return db.wrap(notmuch.DBReadOnly, fn)
}

// WrapRW creates a readwrite-connection and exectues the 'fn' function with this connection
func (db *DB) WrapRW(fn func(db *notmuch.DB) error) error {
	return db.wrap(notmuch.DBReadWrite, fn)
}

func (db *DB) wrap(mode notmuch.DBMode, fn func(*notmuch.DB) error) error {
	if mode == notmuch.DBReadWrite && db.nmdb != nil {
		err := db.nmdb.Close()
		if err != nil {
			return err
		}
	}

	nmdb, err := notmuch.Open(db.dbpath, mode)
	if err != nil && errors.Is(err, notmuch.ErrFileError) {
		nmdb, err = notmuch.Create(db.dbpath)
	}

	if err != nil {
		return err
	}

	if mode == notmuch.DBReadWrite {
		defer nmdb.Close()
	}
	err = fn(nmdb)
	return err
}

// createOrUpgrade opens the database located at 'p' and upgrades it if necessary,
// or creates it if it doesn't exist yet.
func (db *DB) createOrUpgrade() error {
	nmdb, err := notmuch.Open(db.dbpath, notmuch.DBReadWrite)
	if err != nil && errors.Is(err, notmuch.ErrFileError) {
		nmdb, err = notmuch.Create(db.dbpath)
	}
	if err != nil {
		return err
	}
	defer nmdb.Close()

	if nmdb.NeedsUpgrade() {
		err = nmdb.Upgrade()
		if err != nil {
			return err
		}
	}
	return nil
}
