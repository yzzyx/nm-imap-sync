package sync

import (
	"context"
)

func (db *DB) migrate(ctx context.Context) error {
	migrations := []string{
		`CREATE TABLE IF NOT EXISTS 'messages' (
messageid varchar(256) PRIMARY KEY,
tags text
);`,
	}

	for _, m := range migrations {
		_, err := db.db.ExecContext(ctx, m)
		if err != nil {
			return err
		}
	}
	return nil
}
