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
	"runtime"
	"strings"
	"time"

	"github.com/google/go-github/v31/github"
	fsnotify "gopkg.in/fsnotify.v1"
	"periph.io/x/gohci"
)

// runServer runs the web server.
func runServer(c *gohci.WorkerConfig, wkr worker, fileName string) error {
	thisFile, err := os.Executable()
	if err != nil {
		return err
	}
	log.Printf("Executable: %s", thisFile)
	log.Printf("Name: %s", c.Name)
	log.Printf("PATH: %s", os.Getenv("PATH"))

	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", c.Port))
	if err != nil {
		return err
	}
	a := ln.Addr().String()
	_ = ln.Close()
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

	_ = SetConsoleTitle(fmt.Sprintf("gohci - %s", a))
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
	c     *gohci.WorkerConfig
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
		// Return the uptime and Go version. This is a small enough information leak.
		w.Header().Add("Content-Type", "text/plain")
		_, _ = io.WriteString(w, time.Since(s.start).Round(time.Second).String())
		_, _ = io.WriteString(w, "\n")
		_, _ = io.WriteString(w, runtime.Version())
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
	altPath, superUsers, err := validateArgs(r.URL.Query())
	if err != nil {
		// Immediately return an error. This helps catch typos.
		log.Printf("Invalid query argument, check your webhook URL: %q; %v", r.URL.String(), err)
		http.Error(w, "Invalid query argument", http.StatusBadRequest)
		return
	}
	s.handleHook(github.WebHookType(r), payload, altPath, superUsers)
	w.Header().Add("Content-Type", "application/json")
	_, _ = io.WriteString(w, "{}")
}

// handleHook handles a validated github webhook.
func (s *server) handleHook(t string, payload []byte, altPath string, superUsers []string) {
	if t == "ping" {
		return
	}
	event, err := github.ParseWebHook(t, payload)
	if err != nil {
		log.Printf("- invalid payload for hook %s\n%s", t, payload)
		return
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

// Look explicitly at query arguments. Two are supported:
// - altPath
// - superUsers
// These defines additional settings.
func validateArgs(values url.Values) (string, []string, error) {
	// Make sure there is no unknown keys. This is to catch typos, as for example
	// it is easy to mistype 'altpath' instead of 'altPath'.
	for k := range values {
		if k != "altPath" && k != "superUsers" {
			return "", nil, fmt.Errorf("unexpected key %q", k)
		}
	}
	// Limit the allowed characters in altPath.
	altPath := values.Get("altPath")
	if strings.Contains(altPath, "//") || strings.Contains(altPath, "..") {
		return "", nil, fmt.Errorf("invalid altPath %q: contains invalid characters", altPath)
	}
	if len(altPath) > 0 {
		u, err := url.Parse("https://" + altPath)
		if err != nil {
			return "", nil, fmt.Errorf("invalid altPath %q: %v", altPath, err)
		}
		if u.Scheme != "https" || u.User != nil || u.Host == "" || u.Path == "" || u.RawQuery != "" || u.Fragment != "" {
			return "", nil, fmt.Errorf("invalid altPath %q: unexpected url format", altPath)
		}
	}
	var superUsers []string
	for _, v := range values["superUsers"] {
		for _, s := range strings.Split(v, ",") {
			if len(s) == 0 {
				return "", nil, fmt.Errorf("passing an empty superUser")
			}
			// From https://github.com/join:
			// "Username may only contain alphanumeric characters or single hyphens,
			// and cannot begin or end with a hyphen"
			if !isSubset(s, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-") {
				return "", nil, fmt.Errorf("superUser contains unexpected characters: %q", s)
			}
			if strings.HasPrefix(s, "-") || strings.HasSuffix(s, "-") {
				return "", nil, fmt.Errorf("superUser starts or ends with a dash: %q", s)
			}
			superUsers = append(superUsers, s)
		}
	}
	return altPath, superUsers, nil
}

// isSubset returns true if s is composed of characters from c and is not empty.
func isSubset(s, allowed string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if !strings.Contains(allowed, string(c)) {
			return false
		}
	}
	return true
}

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
