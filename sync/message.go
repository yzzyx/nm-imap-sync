package sync

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// UID is used to identify the message on the IMAP server
// Unfortunately, there's no good way to uniquely identify a message,
// and even though all our messages in notmuch will have a message-id,
// that id can have been generated locally.
type UID struct {
	FolderName  string
	UIDValidity int
	UID         int
}

// MessageInfo is used to identify a message
type MessageInfo struct {
	MessageID string

	// We need to keep a list of UID's that this message corresponds to, since a single
	// message can have been copied to multiple folders
	UIDs []UID

	AddedTags   []string // AddedTags lists the flags to be added on the other side
	RemovedTags []string // RemovedTags lists the flags to be removed from the other side
	WantedTags  []string // WantedTags is the list of tags that we'll have after we've applied the changes
	Created     bool     // If set to true, we haven't got this message in the database yet
}

// CheckTagsUID fetches tags for a messages based on UID and compares them to the list of wanted tags
func (db *DB) CheckTagsUID(ctx context.Context, folderName string, uidValidity int, uid int, wantedTags []string) (info MessageInfo, err error) {
	var tags string
	query := `SELECT tags, messageid FROM uids
INNER JOIN messages ON messages.id = uids.message_id
WHERE folderName = ? AND uidvalidity = ? AND uid = ?`

	info.WantedTags = wantedTags
	info.UIDs = []UID{{
		FolderName:  folderName,
		UIDValidity: uidValidity,
		UID:         uid,
	}}

	err = db.db.QueryRowContext(ctx, query, folderName, uidValidity, uid).
		Scan(&tags, &info.MessageID)
	if err != nil {
		if err == sql.ErrNoRows {
			info.Created = true
			info.AddedTags = wantedTags
			return info, nil
		}
		return info, err
	}

	db.compareTags(&info, tags, wantedTags)
	return info, nil
}

// CheckTags fetches tags for a message based on MessageID, and compares those tags to list the of wanted tags
func (db *DB) CheckTags(ctx context.Context, folderName string, messageid string, wantedTags []string) (info MessageInfo, err error) {
	var tags string
	info.MessageID = messageid
	info.WantedTags = wantedTags

	query := `SELECT tags, foldername, uidvalidity, uid FROM messages
INNER JOIN uids ON uids.message_id = messages.id
WHERE messageid = ?`

	rows, err := db.db.QueryContext(ctx, query, messageid)
	if err != nil {
		return info, err
	}
	defer rows.Close()

	for rows.Next() {
		uid := UID{}

		err = rows.Scan(&tags, &uid.FolderName, &uid.UIDValidity, &uid.UID)
		if err != nil {
			return info, err
		}

		info.UIDs = append(info.UIDs, uid)
	}

	// We found no matches
	if len(info.UIDs) == 0 {
		info.Created = true
		info.AddedTags = wantedTags
		// We need to add an UID entry that only contains the foldername,
		// so that we can sync it to the server correctly later on
		info.UIDs = []UID{{
			FolderName: folderName,
		}}
		return info, nil
	}

	db.compareTags(&info, tags, wantedTags)
	return info, nil
}

func (db *DB) compareTags(info *MessageInfo, tags string, wantedTags []string) {
	dbMap := map[string]struct{}{}
	dbTags := strings.Split(tags, ",")
	for _, t := range dbTags {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		dbMap[t] = struct{}{}
	}

	for _, t := range wantedTags {
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
}

// AddMessageInfo updates the list of synchronized tags for a message
func (db *DB) AddMessageSyncInfo(info MessageInfo, tags []string) error {
	// We need to insert the messageid into 'messages', and also update the 'uids'-table
	query := `INSERT INTO messages(messageid, tags) VALUES(?, ?)
  ON CONFLICT(messageid) DO UPDATE SET tags=?;`

	tagStr := strings.Join(tags, ",")
	_, err := db.db.Exec(query, info.MessageID, tagStr, tagStr)
	if err != nil {
		return fmt.Errorf("cannot exec query %s: %w", query, err)
	}

	for _, uid := range info.UIDs {
		query = `INSERT INTO uids(message_id, foldername, uidvalidity, uid)
			 SELECT id, ?, ?, ? FROM messages WHERE messageid = ?
  ON CONFLICT(uidvalidity, uid) DO NOTHING;`

		_, err = db.db.Exec(query, uid.FolderName, uid.UIDValidity, uid.UID, info.MessageID)
		if err != nil {
			return fmt.Errorf("cannot exec query %s: %w", query, err)
		}
	}
	return nil
}
