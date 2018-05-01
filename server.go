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
	"net/url"
	"os"
	"reflect"
	"strings"
	"time"

	"github.com/google/go-github/github"
	fsnotify "gopkg.in/fsnotify.v1"
	"periph.io/x/gohci/lib"
)

// runServer runs the web server.
func runServer(c serverConfig, wkr worker, fileName string) error {
	thisFile, err := os.Executable()
	if err != nil {
		return err
	}
	log.Printf("Executable: %s", thisFile)
	log.Printf("Name: %s", c.getName())
	log.Printf("PATH: %s", os.Getenv("PATH"))

	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", c.getPort()))
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

	lib.SetConsoleTitle(fmt.Sprintf("gohci - %s", a))
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
	s.w.wait()
	return err
}

// server is the HTTP server and manages the task queue server.
type server struct {
	c     serverConfig
	w     worker
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
	payload, err := github.ValidatePayload(r, s.c.getWebHookSecret())
	if err != nil {
		http.Error(w, "Invalid secret", http.StatusUnauthorized)
		log.Printf("- invalid secret")
		return
	}
	s.handleHook(github.WebHookType(r), payload, r.URL.Query())
	io.WriteString(w, "{}")
}

// handleHook handles a validated github webhook.
func (s *server) handleHook(t string, payload []byte, values url.Values) {
	if t == "ping" {
		return
	}
	event, err := github.ParseWebHook(t, payload)
	if err != nil {
		log.Printf("- invalid payload for hook %s\n%s", t, payload)
		return
	}
	// Look explicitly at query arguments. Two are supported:
	// - altPath
	// - superUsers
	// These defines additional settings.
	altPath := values.Get("altPath")
	var superUsers []string
	if s := values.Get("superUsers"); s != "" {
		superUsers = strings.Split(s, ",")
	}
	log.Printf("altPath=%s; superUsers=%s", altPath, strings.Join(superUsers, ","))
	// Process the rest asynchronously so the hook doesn't take too long.
	switch e := event.(type) {
	case *github.CommitCommentEvent:
		s.handleCommitComment(e, altPath, superUsers)
	case *github.IssueCommentEvent:
		s.handleIssueComment(e, altPath, superUsers)
	case *github.PullRequestEvent:
		s.handlePullRequest(e, altPath, superUsers)
	case *github.PullRequestReviewCommentEvent:
		s.handlePullRequestReviewComment(e, altPath, superUsers)
	case *github.PushEvent:
		s.handlePush(e, altPath)
	default:
		log.Printf("- ignoring hook type %s", reflect.TypeOf(e).Elem().Name())
	}
}

// https://developer.github.com/v3/activity/events/types/#commitcommentevent
func (s *server) handleCommitComment(e *github.CommitCommentEvent, altPath string, superUsers []string) {
	if strings.TrimSpace(*e.Comment.Body) != "gohci" {
		log.Printf("- ignoring non 'gohci' commit comment")
		return
	}
	if !isSuperUser(*e.Sender.Login, superUsers) {
		log.Printf("- ignoring commit comment from user %q", *e.Sender.Login)
		return
	}
	// TODO(maruel): The commit could be on a branch never fetched?
	s.w.enqueueCheck(*e.Repo.Owner.Login, *e.Repo.Name, altPath, *e.Comment.CommitID, *e.Repo.Private, 0, nil)
}

// https://developer.github.com/v3/activity/events/types/#issuecommentevent
func (s *server) handleIssueComment(e *github.IssueCommentEvent, altPath string, superUsers []string) {
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
	if strings.TrimSpace(*e.Comment.Body) != "gohci" {
		log.Printf("- ignoring non 'gohci' issue #%d comment", *e.Issue.Number)
		return
	}
	// || *e.Issue.AuthorAssociation == "CONTRIBUTOR"
	if !isSuperUser(*e.Sender.Login, superUsers) {
		log.Printf("- ignoring issue #%d comment from user %q", *e.Issue.Number, *e.Sender.Login)
		return
	}
	// The commit hash is not provided. :(
	s.w.enqueueCheck(*e.Repo.Owner.Login, *e.Repo.Name, altPath, "", *e.Repo.Private, *e.Issue.Number, nil)
}

// https://developer.github.com/v3/activity/events/types/#pullrequestevent
func (s *server) handlePullRequest(e *github.PullRequestEvent, altPath string, superUsers []string) {
	if *e.Action != "opened" && *e.Action != "synchronize" {
		log.Printf("- ignoring action %q for PR from %q", *e.Action, *e.Sender.Login)
		return
	}
	log.Printf("- PR %s #%d %s %s", *e.Repo.FullName, *e.PullRequest.Number, *e.Sender.Login, *e.Action)
	// TODO(maruel): If a reviewer is set, it has to be set by a repository
	// owner (?) If so, then it would be safe to run.
	if !isSuperUser(*e.Sender.Login, superUsers) {
		log.Printf("- ignoring PR from not super user %q", *e.PullRequest.Head.Repo.FullName)
		return
	}
	s.w.enqueueCheck(*e.Repo.Owner.Login, *e.Repo.Name, altPath, *e.PullRequest.Head.SHA, *e.Repo.Private, *e.PullRequest.Number, nil)
}

// https://developer.github.com/v3/activity/events/types/#pullrequestreviewcommentevent
func (s *server) handlePullRequestReviewComment(e *github.PullRequestReviewCommentEvent, altPath string, superUsers []string) {
	if *e.Action != "created" && *e.Action != "edited" {
		log.Printf("- ignoring action %s for PR #%d comment", *e.Action, *e.PullRequest.Number)
		return
	}
	if strings.TrimSpace(*e.Comment.Body) != "gohci" {
		log.Printf("- ignoring non 'gohci' issue #%d comment", *e.PullRequest.Number)
		return
	}
	// || *e.PullRequest.AuthorAssociation == "CONTRIBUTOR"
	if !isSuperUser(*e.Sender.Login, superUsers) {
		log.Printf("- ignoring issue #%d comment from user %q", *e.PullRequest.Number, *e.Sender.Login)
		return
	}
	s.w.enqueueCheck(*e.Repo.Owner.Login, *e.Repo.Name, altPath, *e.PullRequest.Head.SHA, *e.Repo.Private, *e.PullRequest.Number, nil)
}

// https://developer.github.com/v3/activity/events/types/#pushevent
func (s *server) handlePush(e *github.PushEvent, altPath string) {
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
	s.w.enqueueCheck(*e.Repo.Owner.Name, *e.Repo.Name, altPath, *e.HeadCommit.ID, *e.Repo.Private, 0, blame)
}

//

// isSuperUser returns true if the user can trigger tasks.
//
// superUsers is a list of github accounts that can trigger a run. In practice
// any user with write access is a super user but OAuth2 tokens with limited
// scopes cannot get this information. :/
func isSuperUser(u string, superUsers []string) bool {
	for _, s := range superUsers {
		if s == u {
			return true
		}
	}
	return false
}
