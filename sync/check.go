package sync

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/yzzyx/nm-imap-sync/config"
	notmuch "github.com/zenhack/go.notmuch"
)

// CheckFolders iterates through all folders in maildirPath, and
// compares the result with the existing database
func (db *DB) CheckFolders(ctx context.Context, mailbox config.Mailbox, maildirPath string, imapQueue chan<- Update) error {
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

			// Check if folder is included in sync
			var include bool
			if len(mailbox.Folders.Include) > 0 {
				for _, includeFolder := range mailbox.Folders.Include {
					if name == includeFolder {
						include = true
						break
					}
				}
			} else {
				include = true
				for _, includeFolder := range mailbox.Folders.Exclude {
					if name == includeFolder {
						include = false
						break
					}
				}
			}
			if !include {
				continue
			}

			err = db.checkMailbox(ctx, filepath.Join(maildirPath, name), name, imapQueue)
			if err != nil {
				return err
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
				// The signed and attachment tags are special, since its set based on the contents of the email.
				// It can therefore not be added or removed during sync
				if tag.Value == "attachment" || tag.Value == "signed" {
					continue
				}
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

			info, err := db.CheckTags(ctx, folderName, messageID, taglist)
			if err != nil {
				return err
			}

			// queue update to imap server
			if len(info.AddedTags) > 0 || len(info.RemovedTags) > 0 || info.Created {
				imapQueue <- Update{
					MessageInfo: info,
					Filename:    messagePath,
				}
			}
		}
		return nil
	})
	return err
}
