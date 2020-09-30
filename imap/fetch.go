package imap

import (
	"context"
	"crypto/md5"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/emersion/go-imap"
	"github.com/schollz/progressbar/v3"
	"github.com/yzzyx/nm-imap-sync/sync"
	notmuch "github.com/zenhack/go.notmuch"
)

// getMessage downloads a message from the server from a mailbox, and stores it in a maildir
func (h *Handler) getMessage(syncdb *sync.DB, mailbox string, uid uint32) error {
	// Select INBOX
	mailboxInfo, err := h.client.Select(mailbox, false)
	if err != nil {
		return err
	}

	// Download whole body
	section := &imap.BodySectionName{
		Peek: true, // Do not update seen-flags
	}
	items := []imap.FetchItem{section.FetchItem(), imap.FetchFlags}
	seqSet := new(imap.SeqSet)
	seqSet.AddNum(uid)

	messages := make(chan *imap.Message)
	done := make(chan error)
	go func() {
		done <- h.client.UidFetch(seqSet, items, messages)
	}()

	msg := <-messages
	if msg == nil {
		return errors.New("Server didn't return message")
	}

	r := msg.GetBody(section)
	if r == nil {
		return errors.New("Server didn't return message body")
	}

	err = <-done
	if err != nil {
		return err
	}

	md5hash := md5.New()
	tmpFilename := fmt.Sprintf("%d_%d.%d.%s,U=%d", time.Now().Unix(), <-h.seqNumChan, h.processID, h.hostname, uid)
	mailboxPath := filepath.Join(h.maildirPath, mailbox)
	tmpPath := filepath.Join(mailboxPath, "tmp", tmpFilename)

	err = os.MkdirAll(filepath.Join(mailboxPath, "tmp"), 0700)
	if err != nil {
		return err
	}

	err = os.MkdirAll(filepath.Join(mailboxPath, "cur"), 0700)
	if err != nil {
		return err
	}

	fd, err := os.Create(tmpPath)
	if err != nil {
		return err
	}

	multiwriter := io.MultiWriter(fd, md5hash)
	_, err = io.Copy(multiwriter, r)
	if err != nil {
		// Perform cleanup
		_ = fd.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	_ = fd.Close()

	sum := fmt.Sprintf("%x", md5hash.Sum(nil))
	newFilename := fmt.Sprintf("%s,FMD5=%s", tmpFilename, sum)
	newPath := filepath.Join(mailboxPath, "cur", newFilename)
	err = os.Rename(tmpPath, newPath)
	if err != nil {
		// Could not rename file - discard old entry to avoid duplicates
		_ = os.Remove(tmpPath)
		return err
	}

	/*
		notmuch flag translations
		'D'     Adds the "draft" tag to the message
		'F'     Adds the "flagged" tag to the message
		'P'     Adds the "passed" tag to the message
		'R'     Adds the "replied" tag to the message
		'S'     Removes the "unread" tag from the message
	*/

	imapFlags, seen := h.translateFlags(msg.Flags)

	if !seen {
		imapFlags["unread"] = true
	}

	var messageID string
	err = syncdb.WrapRW(func(db *notmuch.DB) error {
		// Add file to index
		m, err := db.AddMessage(newPath)
		if err != nil {
			if errors.Is(err, notmuch.ErrDuplicateMessageID) {
				// We've already seen this one
				return nil
			}
			return err
		}
		defer m.Close()

		// Read the message id from notmuch, since it's possible
		// we had to generate one
		messageID = m.ID()

		for f := range imapFlags {
			err = m.AddTag(f)
			if err != nil {
				return err
			}
		}

		// Make a copy of our current flag-set,
		// since we need to store the current state in our sync database,
		// but still want to keep track of duplicates
		currentFlags := make(map[string]bool, len(imapFlags))
		for f, v := range imapFlags {
			currentFlags[f] = v
		}

		// Add additional tags specified in config file
		if extraTags, ok := h.mailbox.FolderTags[mailbox]; ok {
			for _, tag := range strings.Split(extraTags, ",") {
				tag = strings.TrimSpace(tag)
				if strings.HasPrefix(tag, "-") {
					// Only remove tag if we have it
					tag := tag[1:]
					if hasFlag := currentFlags[tag]; hasFlag {
						err = m.RemoveTag(tag)
						delete(currentFlags, tag)
					}
				} else {
					if hasFlag := currentFlags[tag]; !hasFlag {
						err = m.AddTag(tag)
						currentFlags[tag] = true
					}
				}

				if err != nil {
					return err
				}
			}
		}
		return nil
	})

	if err != nil {
		return err
	}

	flagSlice := make([]string, 0, len(imapFlags))
	for f := range imapFlags {
		flagSlice = append(flagSlice, f)
	}
	// The flags in `imapFlags` already exist on the server,
	// so we add these to our sync-db. Any additional flags will then
	// be synchronized to the IMAP server on the next run
	err = syncdb.AddMessageSyncInfo(sync.MessageInfo{
		MessageID:   messageID,
		FolderName:  mailboxInfo.Name,
		UIDValidity: int(mailboxInfo.UidValidity),
		UID:         int(uid),
	}, flagSlice)
	return err
}

// mailboxFetchMessages checks for any new messages in mailbox
func (h *Handler) mailboxFetchMessages(ctx context.Context, syncdb *sync.DB, mailbox string, fullSync bool) error {
	mbox, err := h.client.Select(mailbox, false)
	if err != nil {
		return err
	}

	if mbox.Messages == 0 {
		return nil
	}

	// Search for new UID's
	seqSet := new(imap.SeqSet)

	lastSeenUID := uint32(0)
	if !fullSync {
		lastSeenUID = h.getLastSeenUID(mailbox)
	}
	// Note that we search from lastSeenUID to MAX, instead of
	//   lastSeenUID to '*', because the latter always returns at least one entry
	seqSet.AddRange(lastSeenUID+1, math.MaxUint32)

	// Fetch envelope information (contains messageid, and UID, which we'll use to fetch the body
	items := []imap.FetchItem{imap.FetchFlags, imap.FetchUid}

	messages := make(chan *imap.Message, 100)
	errchan := make(chan error, 1)

	go func() {
		if err := h.client.UidFetch(seqSet, items, messages); err != nil {
			errchan <- err
		}
	}()

	type Update struct {
		UID  uint32
		Seen bool
		Info sync.MessageInfo
	}

	var updateList []Update
	for msg := range messages {
		if msg == nil {
			// We're done
			break
		}

		if msg.Uid == 0 {
			return errors.New("server did not return UID")
		}

		if msg.Uid > lastSeenUID {
			lastSeenUID = msg.Uid
		}

		serverFlagMap, seen := h.translateFlags(msg.Flags)

		update := Update{
			UID: msg.Uid,
		}

		// The seen-flag means that it's marked as seen by the IMAP server -
		// This flag is automatically added by the server once we download them,
		// so if it's set it probably means that we have a brand new email on our hands,
		// that has not been handled by any sync-client yet.
		if seen {
			// If we've seen this message before, we just compare our flags with the
			// flags on the server - if they differ, we'll update it later
			serverFlags := make([]string, 0, len(serverFlagMap))
			for flag := range serverFlagMap {
				serverFlags = append(serverFlags, flag)
			}

			info, err := syncdb.CheckTagsUID(ctx, mailbox, int(mbox.UidValidity), int(msg.Uid), serverFlags)
			if err != nil {
				return err
			}
			info.UID = int(msg.Uid)
			info.UIDValidity = int(mbox.UidValidity)
			update.Info = info

			if !info.Created && len(info.AddedTags) == 0 && len(info.RemovedTags) == 0 {
				fmt.Println("No update for", msg.Uid, info.MessageID)
				continue
			}

			if info.Created {
				seen = false
			}
		}
		update.Seen = seen
		updateList = append(updateList, update)
	}

	// Check if an error occurred while fetching data
	select {
	case err := <-errchan:
		return err
	default:
	}

	progress := progressbar.NewOptions(len(updateList), progressbar.OptionSetDescription(mailbox))
	for _, update := range updateList {
		progress.Add(1)

		if !update.Seen || update.Info.MessageID == "" {
			// This is the first time we've dealt with this,
			// so we'll have to download the message and import it into notmuch
			err = h.getMessage(syncdb, mailbox, update.UID)
		} else {
			// Messages that we've already seen before only needs their flags adjusted
			err = syncdb.WrapRW(func(db *notmuch.DB) error {
				msg, err := db.FindMessage(update.Info.MessageID)
				if err != nil {
					return err
				}

				for _, tag := range update.Info.AddedTags {
					err = msg.AddTag(tag)
					if err != nil {
						return err
					}
				}

				for _, tag := range update.Info.RemovedTags {
					err = msg.RemoveTag(tag)
					if err != nil {
						return err
					}
				}

				err = syncdb.AddMessageSyncInfo(update.Info, update.Info.WantedTags)
				return err
			})
		}

		if err != nil {
			return err
		}
	}
	h.setLastSeenUID(mailbox, lastSeenUID)
	return nil
}
