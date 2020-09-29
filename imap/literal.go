package imap

import "os"

// FileLiteral wraps a file in order to support the imap.Literal interface
type FileLiteral struct {
	*os.File
}

// Len returns the size
func (l *FileLiteral) Len() int {
	stat, err := l.Stat()
	if err != nil {
		return -1
	}

	return int(stat.Size())
}
