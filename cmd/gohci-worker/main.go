// Copyright 2016 Marc-Antoine Ruel. All rights reserved.
// Use of this source code is governed under the Apache License, Version 2.0
// that can be found in the LICENSE file.

// gohci is the Go on Hardware CI.
//
// It is designed to test hardware based Go projects, e.g. testing the commits
// on Go project on a Rasberry Pi and updating the PR status on GitHub.
//
// It implements:
//
// - github webhook webserver that triggers on pushes and PRs
//
// - runs a Go build and a list of user supplied commands
//
// - posts the stdout to a Github gist and updates the commit's status
package main // import "periph.io/x/gohci/v0/cmd/gohci-worker"

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"runtime"
	"strings"
)

// runLocal runs the checks run.
func runLocal(w worker, org, repo, altpath, commitHash string, useSSH bool) error {
	log.Printf("Running locally")
	// The reason for using the async version is that it creates the status.
	w.enqueueCheck(org, repo, altpath, commitHash, useSSH, 0, nil)
	w.wait()
	// TODO(maruel): Return any error that occurred.
	return nil
}

func mainImpl() error {
	test := flag.String("test", "", "runs a simulation locally, specify the git repository name (not URL) to test, e.g. 'periph/gohci'")
	alt := flag.String("alt", "", "alt path to use, e.g. 'periph.io/x/gohci'")
	commit := flag.String("commit", "", "commit SHA1 to test and update; will only update status on github if not 'HEAD'")
	useSSH := flag.Bool("usessh", false, "use SSH to fetch the repository instead of HTTPS; only necessary when testing")
	flag.Parse()
	if runtime.GOOS != "windows" {
		log.SetFlags(0)
	}
	if len(*test) == 0 {
		if len(*commit) != 0 {
			return errors.New("-commit doesn't make sense without -test")
		}
		if len(*alt) != 0 {
			return errors.New("-alt doesn't make sense without -test")
		}
		if *useSSH {
			return errors.New("-usessh doesn't make sense without -test")
		}
	} else {
		if strings.HasPrefix(*test, "github.com/") {
			return errors.New("don't prefix -test value with 'github.com/', it is already assumed")
		}
	}
	defer func() {
		log.Printf("Shutting down")
	}()
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
	w := newWorkerQueue(c.Name, wd, c.Oauth2AccessToken)
	if len(*test) != 0 {
		parts := strings.SplitN(*test, "/", 2)
		return runLocal(w, parts[0], parts[1], *alt, *commit, *useSSH)
	}
	return runServer(c, w, fileName)
}

func main() {
	if err := mainImpl(); err != nil {
		fmt.Fprintf(os.Stderr, "gohci: %s.\n", err)
		os.Exit(1)
	}
}
