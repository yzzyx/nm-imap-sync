package sync

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

func (db *DB) CheckTags(ctx context.Context, messageid string, currentTags []string) (added []string, removed []string, created bool, err error) {
	var tags string
	err = db.db.QueryRowContext(ctx, "SELECT tags FROM messages WHERE messageid = ?", messageid).Scan(&tags)
	if err != nil {
		if err == sql.ErrNoRows {
			return currentTags, nil, true, nil
		}
		return nil, nil, false, err
	}

	dbMap := map[string]struct{}{}
	dbTags := strings.Split(tags, ",")
	for _, t := range dbTags {
		dbMap[t] = struct{}{}
	}

	for _, t := range currentTags {
		if _, ok := dbMap[t]; ok {
			delete(dbMap, t)
			continue
		}
		added = append(added, t)
	}

	for t := range dbMap {
		removed = append(removed, t)
	}

	return added, removed, false, nil
}

// AddMessageInfo updates the list of synchronized tags for a message
func (db *DB) AddMessageSyncInfo(messageid string, tags []string) error {
	query := `INSERT INTO messages(messageid, tags) VALUES(?, ?)
  ON CONFLICT(messageid) DO UPDATE SET tags=?;`

	tagStr := strings.Join(tags, ",")
	_, err := db.db.Exec(query, messageid, tagStr, tagStr)
	if err != nil {
		return fmt.Errorf("cannot exec query %s: %w", query, err)

	}
	return nil
}
