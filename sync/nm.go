package sync

import (
	"errors"

	notmuch "github.com/zenhack/go.notmuch"
)

// Wrap creates a readonly database connection, and executes the 'fn' function with this connection
func (s *DB) Wrap(fn func(db *notmuch.DB) error) error {
	return s.wrap(notmuch.DBReadOnly, fn)
}

// WrapRW creates a readwrite-connection and exectues the 'fn' function with this connection
func (s *DB) WrapRW(fn func(db *notmuch.DB) error) error {
	return s.wrap(notmuch.DBReadWrite, fn)
}

func (s *DB) wrap(mode notmuch.DBMode, fn func(*notmuch.DB) error) error {
	if mode == notmuch.DBReadWrite && s.db != nil {
		err := s.db.Close()
		if err != nil {
			return err
		}
	}

	db, err := notmuch.Open(s.dbpath, mode)
	if err != nil && errors.Is(err, notmuch.ErrFileError) {
		db, err = notmuch.Create(s.dbpath)
	}

	if mode == notmuch.DBReadWrite {
		defer db.Close()
	}
	err = fn(db)
	return err
}

// createOrUpgrade opens the database located at 'p' and upgrades it if necessary,
// or creates it if it doesn't exist yet.
func (s *DB) createOrUpgrade() error {
	db, err := notmuch.Open(s.dbpath, notmuch.DBReadWrite)
	if err != nil && errors.Is(err, notmuch.ErrFileError) {
		db, err = notmuch.Create(s.dbpath)
	}
	if err != nil {
		return err
	}
	defer db.Close()

	if db.NeedsUpgrade() {
		err = db.Upgrade()
		if err != nil {
			return err
		}
	}
	return nil
}
