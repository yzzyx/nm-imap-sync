// Copyright © 2020 Elias Norberg
// Licensed under the GPLv3 or later.
// See COPYING at the root of the repository for details.
package config

// Config describes the available configuration layout
type Config struct {
	Maildir   string
	Mailboxes map[string]Mailbox
}
