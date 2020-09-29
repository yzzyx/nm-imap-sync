package sync

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// MessageInfo is used to identify a message
type MessageInfo struct {
	MessageID string

	// The following fields are used to identify the message on the IMAP server
	// Unfortunately, there's no good way to uniquely identify a message,
	// and even though all our messages in notmuch will have a message-id,
	// that id can have been generated locally.
	FolderName  string
	UIDValidity int
	UID         int

	AddedTags   []string // AddedTags lists the flags to be added on the other side
	RemovedTags []string // RemovedTags lists the flags to be removed from the other side
	WantedTags  []string // WantedTags is the list of tags that we'll have after we've applied the changes
	Created     bool     // If set to true, we haven't got this message in the database yet
}

func (db *DB) CheckTags(ctx context.Context, folderName string, messageid string, currentTags []string) (info MessageInfo, err error) {
	var tags string
	info.FolderName = folderName
	info.MessageID = messageid
	info.WantedTags = currentTags

	err = db.db.QueryRowContext(ctx, "SELECT tags, foldername, uidvalidity, uid FROM messages WHERE messageid = ?", messageid).
		Scan(&tags, &info.FolderName, &info.UIDValidity, &info.UID)
	if err != nil {
		if err == sql.ErrNoRows {
			info.Created = true
			info.AddedTags = currentTags
			return info, nil
		}
		return info, err
	}

	dbMap := map[string]struct{}{}
	dbTags := strings.Split(tags, ",")
	for _, t := range dbTags {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		dbMap[t] = struct{}{}
	}

	for _, t := range currentTags {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}

		if _, ok := dbMap[t]; ok {
			delete(dbMap, t)
			continue
		}
		info.AddedTags = append(info.AddedTags, t)
	}

	for t := range dbMap {
		if t == "" {
			continue
		}
		info.RemovedTags = append(info.RemovedTags, t)
	}

	return info, nil
}

// AddMessageInfo updates the list of synchronized tags for a message
func (db *DB) AddMessageSyncInfo(info MessageInfo, tags []string) error {
	query := `INSERT INTO messages(messageid, tags, foldername, uidvalidity, uid) VALUES(?, ?, ?, ?, ?)
  ON CONFLICT(messageid) DO UPDATE SET tags=?;`

	tagStr := strings.Join(tags, ",")
	_, err := db.db.Exec(query, info.MessageID, tagStr, info.FolderName, info.UIDValidity, info.UID, tagStr)
	if err != nil {
		return fmt.Errorf("cannot exec query %s: %w", query, err)
	}
	return nil
}
