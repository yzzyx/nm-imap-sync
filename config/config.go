// Copyright © 2020 Elias Norberg
// Licensed under the GPLv3 or later.
// See COPYING at the root of the repository for details.
package config

import "github.com/yzzyx/nm-imap-sync/imap"

// Config describes the available configuration layout
type Config struct {
	Maildir   string
	Mailboxes map[string]imap.Mailbox
}
