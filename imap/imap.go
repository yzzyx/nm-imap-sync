// Copyright © 2020 Elias Norberg
// Licensed under the GPLv3 or later.
// See COPYING at the root of the repository for details.
package imap

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"

	"github.com/emersion/go-imap"
	uidplus "github.com/emersion/go-imap-uidplus"
	"github.com/emersion/go-imap/client"
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

type Client struct {
	*client.Client
	*uidplus.UidPlusClient
}

// Handler is responsible for reading from mailboxes and updating the notmuch index
// Note that a single handler can only read from one mailbox
type Handler struct {
	maildirPath string
	mailbox     config.Mailbox

	cfg    mailConfig
	client *Client

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
	var c *client.Client
	if h.mailbox.UseTLS {
		c, err = client.DialTLS(connectionString, tlsConfig)
	} else {
		c, err = client.Dial(connectionString)
	}

	if err != nil {
		return nil, err
	}

	h.client = &Client{
		c,
		uidplus.NewClient(c),
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
// If 'fullScan' is set to true, we will iterate through all messages, and check for
// any updated flags that doesn't match our current set
func (h *Handler) CheckMessages(ctx context.Context, syncdb *sync.DB, fullScan bool) error {
	var err error

	mailboxes, err := h.listFolders()
	if err != nil {
		return err
	}

	for _, mb := range mailboxes {
		err = createMailDir(filepath.Join(h.maildirPath, mb))
		if err != nil {
			return err
		}

		err = h.mailboxFetchMessages(ctx, syncdb, mb, fullScan)
		if err != nil {
			return err
		}
	}
	return nil
}

// createMailDir creates new directories to store maildir entries in
// with the correct subfolders and permissions
func createMailDir(mailboxPath string) error {
	if st, err := os.Stat(mailboxPath); err == nil {
		if !st.IsDir() {
			return fmt.Errorf("path %s is not a directory", mailboxPath)
		}
		// Path exists and is a directory, so we're done
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	err := os.MkdirAll(filepath.Join(mailboxPath, "tmp"), 0700)
	if err != nil {
		return err
	}

	err = os.MkdirAll(filepath.Join(mailboxPath, "cur"), 0700)
	if err != nil {
		return err
	}

	err = os.MkdirAll(filepath.Join(mailboxPath, "new"), 0700)
	if err != nil {
		return err
	}

	return nil
}
