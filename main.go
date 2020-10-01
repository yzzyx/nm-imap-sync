// Copyright © 2020 Elias Norberg
// Licensed under the GPLv3 or later.
// See COPYING at the root of the repository for details.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/schollz/progressbar/v3"
	"github.com/yzzyx/nm-imap-sync/config"
	"github.com/yzzyx/nm-imap-sync/imap"
	"github.com/yzzyx/nm-imap-sync/sync"
	notmuch "github.com/zenhack/go.notmuch"
	"gopkg.in/yaml.v2"
)

func indexAllFiles(db *notmuch.DB, lastRuntime time.Time, dirpath string) error {
	fd, err := os.Open(dirpath)
	if err != nil {
		panic(err)
	}
	defer fd.Close()

	var entries []os.FileInfo
	for {
		entries, err = fd.Readdir(5)
		if err != nil && err != io.EOF {
			return err
		}

		if len(entries) == 0 {
			break
		}

		for k := range entries {
			name := entries[k].Name()
			if strings.HasPrefix(name, ".") {
				continue
			}

			newPath := filepath.Join(dirpath, name)
			if entries[k].IsDir() {
				err = indexAllFiles(db, lastRuntime, newPath)
				if err != nil {
					return err
				}
			} else if entries[k].ModTime().After(lastRuntime) {
				m, err := db.AddMessage(newPath)
				if err != nil {
					if errors.Is(err, notmuch.ErrDuplicateMessageID) {
						// We've already seen this one
						continue
					}
					return err
				}
				fmt.Println(newPath)
				m.Close()
			}
		}
	}
	return nil
}

func userHomeDir() string {
	if runtime.GOOS == "windows" {
		home := os.Getenv("HOMEDRIVE") + os.Getenv("HOMEPATH")
		if home == "" {
			home = os.Getenv("USERPROFILE")
		}
		return home
	}
	return os.Getenv("HOME")
}

func parsePathSetting(inPath string) string {
	if strings.HasPrefix(inPath, "$HOME") {
		inPath = userHomeDir() + inPath[5:]
	} else if strings.HasPrefix(inPath, "~/") {
		inPath = userHomeDir() + inPath[1:]
	}

	if strings.HasPrefix(inPath, "$") {
		end := strings.Index(inPath, string(os.PathSeparator))
		inPath = os.Getenv(inPath[1:end]) + inPath[end:]
	}
	if filepath.IsAbs(inPath) {
		return filepath.Clean(inPath)
	}

	p, err := filepath.Abs(inPath)
	if err == nil {
		return filepath.Clean(p)
	}
	return ""
}

func main() {
	ctx := context.Background()
	configPath := filepath.Join(userHomeDir(), ".config", "mr")

	fullScan := flag.Bool("full-scan", false, "Scan all messages on server for changes")
	//dryRun := flag.Bool("dry-run", false, "Do not download any mail, only show which actions would be performed")
	flag.Parse()

	cfgData, err := ioutil.ReadFile("./config.yml")
	if err != nil {
		fmt.Printf("Cannot read config file '%s': %s\n", configPath, err)
		os.Exit(1)
	}

	cfg := config.Config{}
	err = yaml.Unmarshal(cfgData, &cfg)
	if err != nil {
		fmt.Printf("Cannot parse config file '%s': %s\n", configPath, err)
		os.Exit(1)
	}

	if cfg.Maildir == "" {
		cfg.Maildir = "~/.mail"
	}

	maildirPath := parsePathSetting(cfg.Maildir)

	syncdb, err := sync.New(ctx, maildirPath)
	if err != nil {
		fmt.Printf("Cannot initialize sync database: %s\n", err)
		os.Exit(1)
	}
	defer syncdb.Close()

	// Create maildir if it doesnt exist
	err = os.MkdirAll(maildirPath, 0700)
	if err != nil {
		panic(err)
	}

	// Create a IMAP setup for each mailbox
	for name, mailbox := range cfg.Mailboxes {
		mailbox.DBPath = maildirPath
		folderPath := filepath.Join(maildirPath, name)
		err = os.MkdirAll(folderPath, 0700)
		if err != nil {
			panic(err)
		}

		imapQueue := make(chan sync.Update, 10000)

		go func() {
			err = syncdb.CheckFolders(ctx, mailbox, folderPath, imapQueue)
			if err != nil {
				log.Printf("cannot check folders for new tags: %v\n", err)
				return
			}
			close(imapQueue)
		}()

		h, err := imap.New(folderPath, mailbox)
		if err != nil {
			log.Printf("cannot initalize new imap connection: %v\n", err)
			return
		}

		progress := progressbar.NewOptions(-1, progressbar.OptionSetDescription("updating server flags"))
		for msgUpdate := range imapQueue {
			progress.Add(1)
			err = h.Update(syncdb, msgUpdate)
			if err != nil {
				log.Printf("cannot update message on server: %v\n", err)
				return
			}
		}
		progress.Finish()

		err = h.CheckMessages(ctx, syncdb, *fullScan)
		if err != nil {
			log.Printf("cannot check for new messages on server: %v\n", err)
			return
		}

		err = h.Close()
		if err != nil {
			log.Printf("Cannot close imap handler: %v", err)
			return
		}
	}

	return
}
