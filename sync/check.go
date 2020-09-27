package sync

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	notmuch "github.com/zenhack/go.notmuch"
)

// CheckFolders iterates through all folders in maildirPath, and
// compares the result with the existing database
func (db *DB) CheckFolders(ctx context.Context, maildirPath string, imapQueue chan<- Update) error {
	md, err := os.Open(maildirPath)
	if err != nil {
		return err
	}
	defer md.Close()

	for {
		entries, err := md.Readdir(10)
		if err != nil {
			if err == io.EOF {
				break
			}
			return err
		}

		for _, e := range entries {
			// Skip files at toplevel
			if !e.IsDir() {
				continue
			}
			name := e.Name()
			err = db.checkMailbox(ctx, filepath.Join(maildirPath, name), name, imapQueue)
			if err != nil {
				return nil
			}
		}
	}
	return nil
}

func (db *DB) checkMailbox(ctx context.Context, mailboxPath string, folderName string, imapQueue chan<- Update) error {
	curPath := filepath.Join(mailboxPath, "cur")
	md, err := os.Open(curPath)
	if err != nil {
		return err
	}
	defer md.Close()

	entries, err := md.Readdirnames(0)
	if err != nil {
		return err
	}

	err = db.Wrap(func(nmDB *notmuch.DB) error {

		for _, name := range entries {
			messagePath := filepath.Join(curPath, name)
			msg, err := nmDB.FindMessageByFilename(messagePath)
			if err != nil {
				if err == notmuch.ErrNotFound {
					// FIXME - if message is not found in notmuch, we need to index it
					//return fmt.Errorf("missing message with filename %s: %w", messagePath, err)
					continue
				}
				return fmt.Errorf("could not find message with filename %s: %w", messagePath, err)
			}

			messageID := msg.ID()

			tags := msg.Tags()
			taglist := []string{}
			tag := &notmuch.Tag{}
			for tags.Next(&tag) {
				taglist = append(taglist, tag.Value)
			}
			err = tags.Close()
			if err != nil {
				return err
			}

			err = msg.Close()
			if err != nil {
				return err
			}

			added, removed, create, err := db.CheckTags(ctx, messageID, taglist)
			if err != nil {
				return err
			}

			// queue update to imap server
			if len(added) > 0 || len(removed) > 0 || create {
				imapQueue <- Update{
					MessageID:   messageID,
					Filename:    messagePath,
					AddedTags:   added,
					RemovedTags: removed,
					Created:     create,
					Folder:      folderName,
				}
			}
		}
		return nil
	})
	return err
}
