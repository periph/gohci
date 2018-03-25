// Copyright 2017 Marc-Antoine Ruel. All rights reserved.
// Use of this source code is governed under the Apache License, Version 2.0
// that can be found in the LICENSE file.

package main

import (
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"reflect"
	"strings"
	"time"

	"github.com/google/go-github/github"
	fsnotify "gopkg.in/fsnotify.v1"
	"periph.io/x/gohci/lib"
)

// runServer runs the web server.
func runServer(c *config, wkr *worker, wd, fileName string) error {
	thisFile, err := os.Executable()
	if err != nil {
		return err
	}
	log.Printf("Running in: %s", wd)
	log.Printf("Executable: %s", thisFile)
	log.Printf("Name: %s", c.Name)
	log.Printf("AltPath: %s", c.AltPath)

	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", c.Port))
	if err != nil {
		return err
	}
	a := ln.Addr().String()
	ln.Close()
	log.Printf("Listening on: %s", a)

	s := &server{c: c, w: wkr, start: time.Now()}
	http.Handle("/", s)
	go http.ListenAndServe(a, nil)

	w, err := fsnotify.NewWatcher()
	if err != nil {
		log.Printf("Failed to initialize watcher: %v", err)
	} else if err = w.Add(thisFile); err != nil {
		log.Printf("Failed to initialize watcher: %v", err)
	} else if err = w.Add(fileName); err != nil {
		log.Printf("Failed to initialize watcher: %v", err)
	}

	lib.SetConsoleTitle(fmt.Sprintf("gohci - %s - %s", a, wd))
	if err == nil {
		select {
		case <-w.Events:
		case err = <-w.Errors:
			log.Printf("Waiting failure: %v", err)
		}
	} else {
		// Hang so the server actually run.
		select {}
	}
	// Ensures no task is running.
	s.w.wg.Wait()
	return err
}

// server is the HTTP server and manages the task queue server.
type server struct {
	c     *config
	w     *worker
	start time.Time
}

// ServeHTTP handles all HTTP requests and triggers a task if relevant.
//
// While the task is started asynchronously, a synchronous status update is
// done so the user is immediately alerted that the task is pending on the
// host. Only one task runs at a time.
func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	log.Printf("%-4s %-21s %s", r.Method, r.RemoteAddr, r.URL.Path)
	defer r.Body.Close()
	// The path must be the root path.
	if r.URL.Path != "" && r.URL.Path != "/" {
		log.Printf("- Unexpected path %s", r.URL.Path)
		http.NotFound(w, r)
		return
	}
	if r.Method == "HEAD" {
		w.WriteHeader(200)
		return
	}
	if r.Method == "GET" {
		// Return the uptime. This is a small enough information leak.
		io.WriteString(w, time.Since(s.start).String())
		return
	}
	if r.Method != "POST" {
		http.Error(w, "Invalid method", http.StatusMethodNotAllowed)
		log.Printf("- invalid method %s", r.Method)
		return
	}
	payload, err := github.ValidatePayload(r, []byte(s.c.WebHookSecret))
	if err != nil {
		http.Error(w, "Invalid secret", http.StatusUnauthorized)
		log.Printf("- invalid secret")
		return
	}
	s.handleHook(w, github.WebHookType(r), payload)
	io.WriteString(w, "{}")
}

// handleHook handles a validated github webhook.
func (s *server) handleHook(w http.ResponseWriter, t string, payload []byte) {
	if t == "ping" {
		return
	}
	event, err := github.ParseWebHook(t, payload)
	if err != nil {
		http.Error(w, "Invalid payload", http.StatusBadRequest)
		log.Printf("- invalid payload")
		return
	}
	// Process the rest asynchronously so the hook doesn't take too long.
	switch event := event.(type) {
	case *github.CommitCommentEvent:
		// https://developer.github.com/v3/activity/events/types/#commitcommentevent
		if s.isSuperUser(*event.Sender.Login) && strings.HasPrefix(*event.Comment.Body, "gohci:") {
			// TODO(maruel): The commit could be on a branch never fetched?
			j := newJobRequest(*event.Repo.Owner.Login, *event.Repo.Name, *event.Repo.Private, *event.Comment.CommitID, 0)
			s.w.enqueueCheck(j, nil)
		}

	case *github.IssueCommentEvent:
		// We'd need the PR's commit head but it is not in the webhook payload.
		// This means we'd require read access to the issues, which the OAuth
		// token shouldn't have. This is because there is no read access to the
		// issue without write access.
		if event.Issue.PullRequestLinks == nil {
			log.Printf("- ignoring issue #%d", *event.Issue.Number)
		} else {
			switch *event.Action {
			case "created":
				// || *event.Issue.AuthorAssociation == "CONTRIBUTOR"
				if s.isSuperUser(*event.Sender.Login) && strings.TrimSpace(*event.Comment.Body) == "gohci" {
					// The commit hash is not provided. :(
					j := newJobRequest(*event.Repo.Owner.Login, *event.Repo.Name, *event.Repo.Private, "", *event.Issue.Number)
					// Immediately fetch the issue head commit inside the webhook, since
					// it's a race condition.
					if !j.commitHashForPR() {
						log.Printf("- failed to get HEAD for issue #%d", *event.Issue.Number)
					} else {
						s.w.enqueueCheck(j, nil)
					}
				} else {
					log.Printf("- ignoring issue #%d comment from user %q", *event.Issue.Number, *event.Sender.Login)
				}
			default:
				log.Printf("- ignoring PR #%d comment", *event.Issue.Number)
			}
		}

	case *github.PullRequestEvent:
		log.Printf("- PR %s #%d %s %s", *event.Repo.FullName, *event.PullRequest.Number, *event.Sender.Login, *event.Action)
		switch *event.Action {
		case "opened", "synchronize":
			// TODO(maruel): If a reviewer is set, it has to be set by a repository
			// owner (?) If so, then it would be safe to run.
			if s.isSuperUser(*event.Sender.Login) {
				j := newJobRequest(*event.Repo.Owner.Login, *event.Repo.Name, *event.Repo.Private, *event.PullRequest.Head.SHA, *event.PullRequest.Number)
				s.w.enqueueCheck(j, nil)
			} else {
				log.Printf("- ignoring PR from not super user %q", *event.PullRequest.Head.Repo.FullName)
			}
		//case "pull_request_review_comment":
		default:
			log.Printf("- ignoring action %q for PR from %q", *event.Action, *event.Sender.Login)
		}

	case *github.PullRequestReviewCommentEvent:
		switch *event.Action {
		case "created", "edited":
			// || *event.PullRequest.AuthorAssociation == "CONTRIBUTOR"
			if s.isSuperUser(*event.Sender.Login) && strings.TrimSpace(*event.Comment.Body) == "gohci" {
				j := newJobRequest(*event.Repo.Owner.Login, *event.Repo.Name, *event.Repo.Private, *event.PullRequest.Head.SHA, *event.PullRequest.Number)
				s.w.enqueueCheck(j, nil)
			} else {
				log.Printf("- ignoring issue #%d comment from user %q", *event.PullRequest.Number, *event.Sender.Login)
			}
		default:
			log.Printf("- ignoring PR #%d comment", *event.PullRequest.Number)
		}

	case *github.PushEvent:
		if event.HeadCommit == nil {
			log.Printf("- Push %s %s <deleted>", *event.Repo.FullName, *event.Ref)
		} else {
			log.Printf("- Push %s %s %s", *event.Repo.FullName, *event.Ref, *event.HeadCommit.ID)
			// TODO(maruel): Potentially leverage event.Repo.DefaultBranch or
			// event.Repo.MasterBranch?
			if !strings.HasPrefix(*event.Ref, "refs/heads/") {
				log.Printf("- ignoring branch %q for push", *event.Ref)
			} else {
				var blame []string
				if *event.Ref == "refs/heads/master" {
					author := *event.HeadCommit.Author.Login
					committer := *event.HeadCommit.Committer.Login
					if author != committer {
						blame = []string{author, committer}
					} else {
						blame = []string{author}
					}
				}
				j := newJobRequest(*event.Repo.Owner.Name, *event.Repo.Name, *event.Repo.Private, *event.HeadCommit.ID, 0)
				s.w.enqueueCheck(j, blame)
			}
		}
	default:
		log.Printf("- ignoring hook type %s", reflect.TypeOf(event).Elem().Name())
	}
}

// isSuperUser returns true if the user can trigger tasks.
func (s *server) isSuperUser(u string) bool {
	if s.c.isSuperUser(u) {
		return true
	}
	// s.client.Repositories.IsCollaborator() requires *write* access to the
	// repository, which we really do not want here. So don't even try for now.
	return false
}
