package imap

import (
	"errors"
	"fmt"

	"github.com/emersion/go-imap"
	"github.com/yzzyx/nm-imap-sync/sync"
)

// Update will add or remove flags to messages according to msgUpdate
func (h *Handler) Update(syncdb *sync.DB, msgUpdate sync.Update) error {
	if msgUpdate.Created {
		return h.createMessage(msgUpdate)
	}

	// Check if we actually have to do anything
	if len(msgUpdate.AddedTags) == 0 && len(msgUpdate.RemovedTags) == 0 {
		return nil
	}

	status, err := h.client.Select(msgUpdate.FolderName, false)
	if err != nil {
		return err
	}

	if int(status.UidValidity) != msgUpdate.UIDValidity {
		return fmt.Errorf("mailbox %s has new UIDValidity - currently unsupported", msgUpdate.FolderName)
	}

	updateList := []struct {
		item imap.StoreItem
		tags []string
	}{
		{item: imap.FormatFlagsOp(imap.AddFlags, true), tags: msgUpdate.AddedTags},
		{item: imap.FormatFlagsOp(imap.RemoveFlags, true), tags: msgUpdate.RemovedTags},
	}

	for _, update := range updateList {
		if len(update.tags) == 0 {
			continue
		}
		seqSet := new(imap.SeqSet)
		seqSet.AddNum(uint32(msgUpdate.UID))

		// UidStore / Store expects a list of interface{}, it can't handle []string
		tags := make([]interface{}, 0, len(update.tags))
		for _, v := range update.tags {
			tags = append(tags, v)
		}

		err := h.client.UidStore(seqSet, update.item, tags, nil)
		if err != nil {
			return err
		}
	}

	// Write updated info back to database
	err = syncdb.AddMessageSyncInfo(msgUpdate.MessageInfo, msgUpdate.WantedTags)
	return err
}

func (h *Handler) createMessage(msgUpdate sync.Update) error {
	return errors.New("create not supported")
}
