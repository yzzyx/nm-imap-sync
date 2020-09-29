package imap

import (
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
	seen := false
	imapFlags := map[string]bool{}

	// Add flags from imap
	for _, flag := range msg.Flags {
		switch flag {
		case imap.SeenFlag:
			seen = true
		case imap.AnsweredFlag:
			imapFlags["replied"] = true
		case imap.DeletedFlag:
			// NOTE - the deleted flag is special in IMAP
			// usually, all deleted messages will be permanently removed from the server when we close the folder
			imapFlags["deleted"] = true
		case imap.DraftFlag:
			imapFlags["draft"] = true
		case imap.FlaggedFlag:
			imapFlags["flagged"] = true
		default:
			// We ignore other builtin flags
			if flag[0] == '\\' {
				continue
			}
			ignoreTag := false
			for _, ignore := range h.mailbox.IgnoredTags {
				if flag == ignore {
					ignoreTag = true
				}
			}
			if ignoreTag {
				continue
			}
			imapFlags[flag] = true
		}
	}

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
func (h *Handler) mailboxFetchMessages(syncdb *sync.DB, mailbox string) error {
	mbox, err := h.client.Select(mailbox, false)
	if err != nil {
		return err
	}

	if mbox.Messages == 0 {
		return nil
	}

	// Search for new UID's
	seqSet := new(imap.SeqSet)

	lastSeenUID := h.getLastSeenUID(mailbox)
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

	var uidList []uint32
	for msg := range messages {
		if msg == nil {
			// We're done
			break
		}

		if msg.Envelope == nil {
			return errors.New("server returned empty envelope")
		}

		if msg.Uid > lastSeenUID {
			lastSeenUID = msg.Uid
		}

		seen, err := h.seenMessage(syncdb, msg.Envelope.MessageId)
		if err != nil {
			return err
		}
		if seen {
			// We've already seen this message
			fmt.Println("Already seen", msg.Uid, msg.Envelope.MessageId)
			continue
		}
		//fmt.Println("Adding to list", msg.Uid, msg.Envelope.MessageId)
		uidList = append(uidList, msg.Uid)
	}

	// Check if an error occurred while fetching data
	select {
	case err := <-errchan:
		return err
	default:
	}

	progress := progressbar.NewOptions(len(uidList), progressbar.OptionSetDescription(mailbox))
	for _, uid := range uidList {
		progress.Add(1)
		err = h.getMessage(syncdb, mailbox, uid)
		if err != nil {
			return err
		}
	}
	h.setLastSeenUID(mailbox, lastSeenUID)
	return nil
}
