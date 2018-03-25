// Copyright 2016 Marc-Antoine Ruel. All rights reserved.
// Use of this source code is governed under the Apache License, Version 2.0
// that can be found in the LICENSE file.

// gohci is the Go on Hardware CI.
//
// It is designed to test hardware based Go projects, e.g. testing the commits
// on Go project on a Rasberry Pi and updating the PR status on GitHub.
//
// It implements:
// - github webhook webserver that triggers on pushes and PRs
// - runs a Go build and a list of user supplied commands
// - posts the stdout to a Github gist and updates the commit's status
package main // import "periph.io/x/gohci"

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

func mainImpl() error {
	test := flag.String("test", "", "runs a simulation locally, specify the git repository name (not URL) to test, e.g. 'periph/gohci'")
	commit := flag.String("commit", "", "commit SHA1 to test and update; will only update status on github if not 'HEAD'")
	useSSH := flag.Bool("usessh", false, "use SSH to fetch the repository instead of HTTPS; only necessary when testing")
	update := flag.Bool("update", false, "when coupled with -test and -commit, update the remote repository")
	flag.Parse()
	if runtime.GOOS != "windows" {
		log.SetFlags(0)
	}
	if len(*test) == 0 {
		if len(*commit) != 0 {
			return errors.New("-commit doesn't make sense without -test")
		}
		if *useSSH {
			return errors.New("-usessh doesn't make sense without -test")
		}
		if *update {
			return errors.New("-update can only be used with -test")
		}
	} else {
		if strings.HasPrefix(*test, "github.com/") {
			return errors.New("don't prefix -test value with 'github.com/', it is already assumed")
		}
		if len(*commit) == 0 {
			*commit = "HEAD"
		}
	}
	fileName := "gohci.yml"
	c, err := loadConfig(fileName)
	if err != nil {
		return err
	}
	log.Printf("Config: %#v", c)
	wd, err := os.Getwd()
	if err != nil {
		return err
	}
	gopath := filepath.Join(wd, "go")
	// GOPATH may not be set especially when running from systemd, so use the
	// local GOPATH. This is safer as this doesn't modify the host environment.
	os.Setenv("GOPATH", gopath)
	os.Setenv("PATH", filepath.Join(gopath, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"))
	w := newWorker(c, gopath)
	if len(*test) != 0 {
		return runLocal(w, gopath, *commit, *test, *update, *useSSH)
	}
	return runServer(c, w, wd, fileName)
}

func main() {
	if err := mainImpl(); err != nil {
		fmt.Fprintf(os.Stderr, "gohci: %s.\n", err)
		os.Exit(1)
	}
}
