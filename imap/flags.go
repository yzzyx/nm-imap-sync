package imap

import "github.com/emersion/go-imap"

func (h *Handler) translateFlags(imapFlags []string) (outputFlags map[string]bool, seen bool) {
	outputFlags = make(map[string]bool, len(imapFlags))

	// Add flags from imap
	for _, flag := range imapFlags {
		switch flag {
		case imap.SeenFlag:
			seen = true
		case imap.AnsweredFlag:
			outputFlags["replied"] = true
		case imap.DeletedFlag:
			// NOTE - the deleted flag is special in IMAP
			// usually, all deleted messages will be permanently removed from the server when we close the folder
			outputFlags["deleted"] = true
		case imap.DraftFlag:
			outputFlags["draft"] = true
		case imap.FlaggedFlag:
			outputFlags["flagged"] = true
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
			outputFlags[flag] = true
		}
	}

	if !seen {
		outputFlags["unread"] = true
	}

	return outputFlags, seen
}
