// Copyright © 2020 Elias Norberg
// Licensed under the GPLv3 or later.
// See COPYING at the root of the repository for details.
package imap

import (
	"crypto/md5"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
	"github.com/schollz/progressbar/v3"
	"github.com/yzzyx/nm-imap-sync/config"
	"github.com/yzzyx/nm-imap-sync/sync"
	notmuch "github.com/zenhack/go.notmuch"
)

type mailConfig struct {
	// Keep track of last seen UID for each mailbox
	LastSeenUID map[string]uint32
}

// IndexUpdate is used to signal that a message should be tagged with specific information
type IndexUpdate struct {
	Path      string   // Path to file to be updated
	MessageID string   // MessageID to be updated
	Tags      []string // Tags to add/remove from message (entries prefixed with "-" will be removed)
}

// Handler is responsible for reading from mailboxes and updating the notmuch index
// Note that a single handler can only read from one mailbox
type Handler struct {
	maildirPath string
	mailbox     config.Mailbox

	cfg    mailConfig
	client *client.Client

	// Used internally to generate maildir files
	seqNumChan <-chan int
	processID  int
	hostname   string
}

// New creates a new Handler for processing IMAP mailboxes
func New(maildirPath string, mailbox config.Mailbox) (*Handler, error) {
	var err error
	h := Handler{}
	h.hostname, err = os.Hostname()
	if err != nil {
		return nil, err
	}

	h.mailbox = mailbox

	if h.mailbox.Server == "" {
		return nil, errors.New("imap server address not configured")
	}
	if h.mailbox.Username == "" {
		return nil, errors.New("imap username not configured")
	}
	if h.mailbox.Password == "" {
		return nil, errors.New("imap password not configured")
	}

	// Set default port
	if h.mailbox.Port == 0 {
		h.mailbox.Port = 143
		if h.mailbox.UseTLS {
			h.mailbox.Port = 993
		}
	}

	connectionString := fmt.Sprintf("%s:%d", h.mailbox.Server, h.mailbox.Port)
	tlsConfig := &tls.Config{ServerName: h.mailbox.Server}
	if h.mailbox.UseTLS {
		h.client, err = client.DialTLS(connectionString, tlsConfig)
	} else {
		h.client, err = client.Dial(connectionString)
	}

	if err != nil {
		return nil, err
	}

	// Start a TLS session
	if h.mailbox.UseStartTLS {
		if err = h.client.StartTLS(tlsConfig); err != nil {
			return nil, err
		}
	}

	err = h.client.Login(h.mailbox.Username, h.mailbox.Password)
	if err != nil {
		return nil, err
	}

	// Generate unique sequence numbers
	seqNumChan := make(chan int)
	go func() {
		seqNum := 1
		for {
			seqNumChan <- seqNum
			seqNum++
		}
	}()
	h.seqNumChan = seqNumChan
	h.processID = os.Getpid()
	h.maildirPath = maildirPath

	h.cfg.LastSeenUID = make(map[string]uint32)
	// Get list of timestamps etc.
	data, err := ioutil.ReadFile(filepath.Join(maildirPath, ".imap-uids"))
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}
	} else {
		err = json.Unmarshal(data, &h.cfg)
		if err != nil {
			return nil, err
		}
	}
	return &h, nil
}

// Close closes all open handles, flushes channels and saves configuration data
func (h *Handler) Close() error {
	data, err := json.Marshal(h.cfg)
	if err != nil {
		return err
	}

	err = ioutil.WriteFile(filepath.Join(h.maildirPath, ".imap-uids"), data, 0700)
	if err != nil {
		return err
	}

	err = h.client.Close()
	if err != nil {
		return err
	}

	err = h.client.Logout()
	return err
}

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
			ignoreTag := false
			for _, ignore := range h.mailbox.IgnoredTags {
				if f == ignore {
					ignoreTag = true
				}
			}
			if ignoreTag {
				continue
			}

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

		// Add all messages to inbox, if they're not already flagged
		if hasFlag := currentFlags["inbox"]; !hasFlag {
			err = m.AddTag("inbox")
			if err != nil {
				return err
			}
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

// GetLastFetched returns the timestamp when we last checked this mailbox
func (h *Handler) getLastSeenUID(mailbox string) uint32 {
	if uid, ok := h.cfg.LastSeenUID[mailbox]; ok {
		return uid
	}
	return 0
}

func (h *Handler) setLastSeenUID(mailbox string, uid uint32) {
	h.cfg.LastSeenUID[mailbox] = uid
}

// seenMessage returns true if we've already seen this message
func (h *Handler) seenMessage(nmdb *sync.DB, messageID string) (bool, error) {
	// Remove surrounding tags
	if (strings.HasPrefix(messageID, "<") && strings.HasSuffix(messageID, ">")) ||
		(strings.HasPrefix(messageID, "\"") && strings.HasSuffix(messageID, "\"")) {
		messageID = messageID[1 : len(messageID)-1]
	}

	// We cannot match without a message id
	if messageID == "" {
		return false, nil
	}

	retval := false
	err := nmdb.Wrap(func(db *notmuch.DB) error {
		msg, err := db.FindMessage(messageID)
		if err == nil {
			msg.Close()
			retval = true
			return nil
		}
		if err != notmuch.ErrNotFound {
			return err
		}
		return nil
	})

	return retval, err
}

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
	items := []imap.FetchItem{imap.FetchEnvelope, imap.FetchUid}

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

func (h *Handler) listFolders() ([]string, error) {

	includeAll := false
	// If no specific folders are listed to be included, assume all folders should be included
	if len(h.mailbox.Folders.Include) == 0 {
		includeAll = true
	}

	// Make a map of included and excluded mailboxes
	includedFolders := make(map[string]bool)
	for _, folder := range h.mailbox.Folders.Include {
		// Note - we set this to false to keep track of if it exists on the server or not
		includedFolders[folder] = false
	}

	excludedFolders := make(map[string]bool)
	for _, folder := range h.mailbox.Folders.Exclude {
		excludedFolders[folder] = true
	}

	mboxChan := make(chan *imap.MailboxInfo, 10)
	errChan := make(chan error, 1)
	go func() {
		if err := h.client.List("", "*", mboxChan); err != nil {
			errChan <- err
		}
	}()

	var folderNames []string
	for mb := range mboxChan {
		if mb == nil {
			// We're done
			break
		}

		// Check if this mailbox should be excluded
		if _, ok := excludedFolders[mb.Name]; ok {
			continue
		}

		if !includeAll {
			if _, ok := includedFolders[mb.Name]; !ok {
				continue
			}
			includedFolders[mb.Name] = true
		}

		folderNames = append(folderNames, mb.Name)
	}

	// Check if an error occurred while fetching data
	select {
	case err := <-errChan:
		return nil, err
	default:
	}

	// Check if any of the specified folders were missing on the server
	for folder, seen := range includedFolders {
		if !seen {
			return nil, fmt.Errorf("folder %s not found on server", folder)
		}
	}

	return folderNames, nil
}

// CheckMessages checks for new/unindexed messages on the server
func (h *Handler) CheckMessages(syncdb *sync.DB) error {
	var err error

	mailboxes, err := h.listFolders()
	if err != nil {
		return err
	}

	for _, mb := range mailboxes {
		err = h.mailboxFetchMessages(syncdb, mb)
		if err != nil {
			return err
		}
	}
	return nil
}
