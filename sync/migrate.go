package sync

import (
	"context"
)

func (db *DB) migrate(ctx context.Context) error {
	migrations := []string{
		`CREATE TABLE IF NOT EXISTS 'messages' (
id INTEGER PRIMARY KEY AUTOINCREMENT,
messageid varchar(256) NOT NULL UNIQUE,
tags text NOT NULL
);`,
		`CREATE TABLE IF NOT EXISTS 'uids' (
	message_id	INTEGER NOT NULL,
	foldername	VARCHAR(256) NOT NULL,
	uidvalidity INTEGER NOT NULL,
	uid			INTEGER NOT NULL,
	FOREIGN KEY (message_id) REFERENCES messages(id)
);`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uid_unique ON uids (uidvalidity, uid);`,
	}

	for _, m := range migrations {
		_, err := db.db.ExecContext(ctx, m)
		if err != nil {
			return err
		}
	}
	return nil
}
