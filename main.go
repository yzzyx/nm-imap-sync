// Copyright © 2020 Elias Norberg
// Licensed under the GPLv3 or later.
// See COPYING at the root of the repository for details.
package main

import (
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/yzzyx/nm-imap-sync/config"
	"github.com/yzzyx/nm-imap-sync/imap"
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

	var db *notmuch.DB
	configPath := filepath.Join(userHomeDir(), ".config", "mr")

	//dryRun := flag.Bool("dry-run", false, "Do not download any mail, only show which actions would be performed")

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

	// Create maildir if it doesnt exist
	err = os.MkdirAll(maildirPath, 0700)
	if err != nil {
		panic(err)
	}

	//if *dryRun {
	//	mode = notmuch.DBReadOnly
	//}
	db, err = notmuch.Open(maildirPath, notmuch.DBReadWrite)
	if err != nil && errors.Is(err, notmuch.ErrFileError) {
		fmt.Println("Creating database...")
		db, err = notmuch.Create(maildirPath)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "Could not open database: %v\n", err)
		return
	}

	defer db.Close()

	if db.NeedsUpgrade() {
		fmt.Println("Database needs upgrade")
		err = db.Upgrade()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Could not upgrade database: %v\n", err)
			return
		}
	}

	//ts := time.Time{}
	//lastIndexedPath := filepath.Join(configPath, "lastindexed")
	//data, err := ioutil.ReadFile(lastIndexedPath)
	//if err == nil {
	//	err = json.Unmarshal(data, &ts)
	//	if err != nil {
	//		fmt.Println("Cannot unmarshal last index date:", err)
	//		return
	//	}
	//}

	//now := time.Now()

	// FIXME - Wrap this in a command
	// Reindex all files
	//fmt.Println("Indexing mailfiles...")
	//err = indexAllFiles(db, ts, maildirPath)
	//if err != nil {
	//	fmt.Println("Could not index maildir:", err)
	//	return
	//}

	//data, err = json.Marshal(now)
	//if err == nil {
	//	err = ioutil.WriteFile(lastIndexedPath, data, 0600)
	//	if err != nil {
	//		fmt.Println("Could not update last indexed timestamp:", err)
	//		return
	//	}
	//}

	//if h.cfg.IndexedMailDir == false {
	//	err = indexAllFiles(db, time.Time{}, h.maildirPath)
	//	if err != nil {
	//		return nil, err
	//	}
	//}

	// Create a IMAP setup for each mailbox
	for name, mailbox := range cfg.Mailboxes {
		folderPath := filepath.Join(maildirPath, name)
		err = os.MkdirAll(folderPath, 0700)
		if err != nil {
			panic(err)
		}

		h, err := imap.New(db, folderPath, mailbox)
		if err != nil {
			log.Fatal(err)
		}
		defer h.Close()

		err = h.CheckMessages()
		if err != nil {
			log.Fatal(err)
		}
	}

	return
}
