package config

// Mailbox defines the available options for a IMAP mailbox to pull from
type Mailbox struct {
	Server      string
	Port        int
	Username    string
	Password    string
	UseTLS      bool `yaml:"use_tls"`
	UseStartTLS bool `yaml:"use_starttls"`
	Folders     struct {
		Include []string
		Exclude []string
	}

	// This is a list of flags that should not be synchronized  between client and server.
	// I.e. when fetching messages from an Exchange 2010 server we usually want to ignore $MDNSent
	IgnoredTags []string          `yaml:"ignored_tags"`
	FolderTags  map[string]string `yaml:"folder_tags"`

	DBPath string // This is usually inherited from the base configuration
}
