package sync

// Update contains the base definition for an update
// that should be performed on the IMAP server
type Update struct {
	MessageID   string
	Filename    string
	AddedTags   []string
	RemovedTags []string
	Created     bool
	Folder      string
}
