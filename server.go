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
	log.Printf("PATH: %s", os.Getenv("PATH"))

	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", c.Port))
	if err != nil {
		return err
	}
	a := ln.Addr().String()
	ln.Close()
	log.Printf("Listening on: %s", a)

	s := &server{c: c, w: wkr, wd: wd, start: time.Now()}
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
	wd    string
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
	s.handleHook(github.WebHookType(r), payload)
	io.WriteString(w, "{}")
}

// handleHook handles a validated github webhook.
func (s *server) handleHook(t string, payload []byte) {
	if t == "ping" {
		return
	}
	event, err := github.ParseWebHook(t, payload)
	if err != nil {
		log.Printf("- invalid payload for hook %s\n%s", t, payload)
		return
	}
	// Process the rest asynchronously so the hook doesn't take too long.
	switch e := event.(type) {
	case *github.CommitCommentEvent:
		s.handleCommitComment(e)
	case *github.IssueCommentEvent:
		s.handleIssueComment(e)
	case *github.PullRequestEvent:
		s.handlePullRequest(e)
	case *github.PullRequestReviewCommentEvent:
		s.handlePullRequestReviewComment(e)
	case *github.PushEvent:
		s.handlePush(e)
	default:
		log.Printf("- ignoring hook type %s", reflect.TypeOf(e).Elem().Name())
	}
}

// https://developer.github.com/v3/activity/events/types/#commitcommentevent
func (s *server) handleCommitComment(e *github.CommitCommentEvent) {
	p := s.c.getProject(*e.Repo.Owner.Login, *e.Repo.Name)
	if !p.isSuperUser(*e.Sender.Login) || strings.TrimSpace(*e.Comment.Body) != "gohci" {
		log.Printf("- ignoring commit comment from user %q", *e.Sender.Login)
		return
	}
	// TODO(maruel): The commit could be on a branch never fetched?
	j := newJobRequest(p, *e.Repo.Private, s.wd, *e.Comment.CommitID, 0)
	s.w.enqueueCheck(j, nil)
}

// https://developer.github.com/v3/activity/events/types/#issuecommentevent
func (s *server) handleIssueComment(e *github.IssueCommentEvent) {
	// We'd need the PR's commit head but it is not in the webhook payload.
	// This means we'd require read access to the issues, which the OAuth
	// token shouldn't have. This is because there is no read access to the
	// issue without write access.
	if e.Issue.PullRequestLinks == nil {
		log.Printf("- ignoring issue #%d", *e.Issue.Number)
		return
	}
	if *e.Action != "created" && *e.Action != "edited" {
		log.Printf("- ignoring PR #%d comment", *e.Issue.Number)
		return
	}
	// || *e.Issue.AuthorAssociation == "CONTRIBUTOR"
	p := s.c.getProject(*e.Repo.Owner.Login, *e.Repo.Name)
	if !p.isSuperUser(*e.Sender.Login) || strings.TrimSpace(*e.Comment.Body) != "gohci" {
		log.Printf("- ignoring issue #%d comment from user %q", *e.Issue.Number, *e.Sender.Login)
		return
	}
	// The commit hash is not provided. :(
	j := newJobRequest(p, *e.Repo.Private, s.wd, "", *e.Issue.Number)
	// Immediately fetch the issue head commit inside the webhook, since
	// it's a race condition.
	if !j.commitHashForPR() {
		log.Printf("- failed to get HEAD for issue #%d", *e.Issue.Number)
		return
	}
	s.w.enqueueCheck(j, nil)
}

// https://developer.github.com/v3/activity/events/types/#pullrequestevent
func (s *server) handlePullRequest(e *github.PullRequestEvent) {
	if *e.Action != "opened" && *e.Action != "synchronize" {
		log.Printf("- ignoring action %q for PR from %q", *e.Action, *e.Sender.Login)
		return
	}
	log.Printf("- PR %s #%d %s %s", *e.Repo.FullName, *e.PullRequest.Number, *e.Sender.Login, *e.Action)
	// TODO(maruel): If a reviewer is set, it has to be set by a repository
	// owner (?) If so, then it would be safe to run.
	p := s.c.getProject(*e.Repo.Owner.Login, *e.Repo.Name)
	if !p.isSuperUser(*e.Sender.Login) {
		log.Printf("- ignoring PR from not super user %q", *e.PullRequest.Head.Repo.FullName)
		return
	}
	j := newJobRequest(p, *e.Repo.Private, s.wd, *e.PullRequest.Head.SHA, *e.PullRequest.Number)
	s.w.enqueueCheck(j, nil)
}

// https://developer.github.com/v3/activity/events/types/#pullrequestreviewcommentevent
func (s *server) handlePullRequestReviewComment(e *github.PullRequestReviewCommentEvent) {
	if *e.Action != "created" && *e.Action != "edited" {
		log.Printf("- ignoring action %s for PR #%d comment", *e.Action, *e.PullRequest.Number)
		return
	}
	// || *e.PullRequest.AuthorAssociation == "CONTRIBUTOR"
	p := s.c.getProject(*e.Repo.Owner.Login, *e.Repo.Name)
	if !p.isSuperUser(*e.Sender.Login) || strings.TrimSpace(*e.Comment.Body) != "gohci" {
		log.Printf("- ignoring issue #%d comment from user %q", *e.PullRequest.Number, *e.Sender.Login)
		return
	}
	j := newJobRequest(p, *e.Repo.Private, s.wd, *e.PullRequest.Head.SHA, *e.PullRequest.Number)
	s.w.enqueueCheck(j, nil)
}

// https://developer.github.com/v3/activity/events/types/#pushevent
func (s *server) handlePush(e *github.PushEvent) {
	if e.HeadCommit == nil {
		log.Printf("- Push %s %s <deleted>", *e.Repo.FullName, *e.Ref)
		return
	}
	log.Printf("- Push %s %s %s", *e.Repo.FullName, *e.Ref, *e.HeadCommit.ID)
	// TODO(maruel): Potentially leverage e.Repo.DefaultBranch or
	// e.Repo.MasterBranch?
	if !strings.HasPrefix(*e.Ref, "refs/heads/") {
		log.Printf("- ignoring branch %q for push", *e.Ref)
		return
	}
	var blame []string
	if *e.Ref == "refs/heads/master" {
		author := *e.HeadCommit.Author.Login
		committer := *e.HeadCommit.Committer.Login
		if author != committer {
			blame = []string{author, committer}
		} else {
			blame = []string{author}
		}
	}
	p := s.c.getProject(*e.Repo.Owner.Name, *e.Repo.Name)
	j := newJobRequest(p, *e.Repo.Private, s.wd, *e.HeadCommit.ID, 0)
	s.w.enqueueCheck(j, blame)
}
