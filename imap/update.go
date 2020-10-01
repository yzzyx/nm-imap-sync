package imap

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/emersion/go-imap"
	"github.com/yzzyx/nm-imap-sync/sync"
)

// Update will add or remove flags to messages according to msgUpdate
func (h *Handler) Update(syncdb *sync.DB, msgUpdate sync.Update) error {
	if msgUpdate.Created {
		return h.createMessage(syncdb, msgUpdate, msgUpdate.UIDs[0])
	}

	// Check if we actually have to do anything
	if len(msgUpdate.AddedTags) == 0 && len(msgUpdate.RemovedTags) == 0 {
		return nil
	}

	// Update all UID's in list
	for _, uid := range msgUpdate.UIDs {
		err := h.updateUID(syncdb, msgUpdate, uid)
		if err != nil {
			return err
		}
	}
	return nil
}

func (h *Handler) updateUID(syncdb *sync.DB, msgUpdate sync.Update, uid sync.UID) error {
	status, err := h.client.Select(uid.FolderName, false)
	if err != nil {
		return err
	}

	if int(status.UidValidity) != uid.UIDValidity {
		return fmt.Errorf("mailbox %s has new UIDValidity - currently unsupported", uid.FolderName)
	}

	updateList := []struct {
		item imap.StoreItem
		tags []string
	}{
		{item: imap.FormatFlagsOp(imap.AddFlags, true), tags: msgUpdate.AddedTags},
		{item: imap.FormatFlagsOp(imap.RemoveFlags, true), tags: msgUpdate.RemovedTags},
	}

	for _, update := range updateList {
		// UidStore / Store expects a list of interface{}, it can't handle []string
		tags := make([]interface{}, 0, len(update.tags))
		for _, v := range update.tags {

			// Ignored tags will not be added or removed from the server
			ignoreTag := false
			for _, ignore := range h.mailbox.IgnoredTags {
				if v == ignore {
					ignoreTag = true
				}
			}
			if ignoreTag {
				continue
			}

			tags = append(tags, v)
		}

		if len(tags) == 0 {
			continue
		}
		seqSet := new(imap.SeqSet)
		seqSet.AddNum(uint32(uid.UID))

		err := h.client.UidStore(seqSet, update.item, tags, nil)
		if err != nil {
			return err
		}
	}

	// Write updated info back to database
	err = syncdb.AddMessageSyncInfo(msgUpdate.MessageInfo, msgUpdate.WantedTags)
	return err
}

func (h *Handler) createMessage(syncdb *sync.DB, msgUpdate sync.Update, uidInfo sync.UID) error {

	fd, err := os.Open(msgUpdate.Filename)
	if err != nil {
		return err
	}
	defer fd.Close()

	hasUIDPlus, err := h.client.SupportUidPlus()
	if err != nil {
		return err
	}

	if !hasUIDPlus {
		return errors.New("server does not support UIDPLUS, which is currently required for pushing new messages to server")
	}

	uidValidity, uid, err := h.client.UidPlusClient.Append(uidInfo.FolderName, msgUpdate.AddedTags, time.Now(), &FileLiteral{fd})
	if err != nil {
		return err
	}

	// Servers are not forced to return UID.
	// If we didn't get it, we won't add the message back to our db,
	// and pick it up when we sync back.
	// Note that this requires that we have a message id to match on.
	if uidValidity == 0 || uid == 0 {
		return nil
	}

	// Write updated info back to database
	uidInfo.UIDValidity = int(uidValidity)
	uidInfo.UID = int(uid)
	msgUpdate.MessageInfo.UIDs = []sync.UID{uidInfo}
	err = syncdb.AddMessageSyncInfo(msgUpdate.MessageInfo, msgUpdate.AddedTags)
	return err
}
