// Copyright 2017 Marc-Antoine Ruel. All rights reserved.
// Use of this source code is governed under the Apache License, Version 2.0
// that can be found in the LICENSE file.

package main

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/google/go-github/github"
	"golang.org/x/oauth2"
)

// worker is the object that handles the queue of job requests.
type worker interface {
	// enqueueCheck immediately add the status that the test run is pending and
	// add the run in the queue. Ensures that the service doesn't restart until
	// the task is done.
	enqueueCheck(p project, useSSH bool, commitHash string, pullID int, blame []string)
	// wait waits until all enqueued worker job requests are done.
	wait()
}

// workerQueue is the task queue server.
type workerQueue struct {
	name   string // Copy of config.Name
	ctx    context.Context
	client *github.Client // Used to set commit status and create gists.
	wd     string

	mu sync.Mutex     // Set when a check is running in runJobRequest()
	wg sync.WaitGroup // Set for each pending task.
}

func newWorkerQueue(name, wd string, accessToken string) worker {
	tc := oauth2.NewClient(oauth2.NoContext, oauth2.StaticTokenSource(&oauth2.Token{AccessToken: accessToken}))
	return &workerQueue{
		name:   name,
		ctx:    context.Background(),
		client: github.NewClient(tc),
		wd:     wd,
	}
}

// enqueueCheck implements worker.
func (w *workerQueue) enqueueCheck(p project, useSSH bool, commitHash string, pullID int, blame []string) {
	w.wg.Add(1)
	defer w.wg.Done()

	j := newJobRequest(p, useSSH, w.wd, commitHash, pullID)
	// Immediately fetch the issue head commit inside the webhook, since
	// it's a race condition.
	if commitHash == "" && !j.findCommitHash() {
		log.Printf("- failed to get HEAD for issue #%d", pullID)
		return
	}
	log.Printf("- Enqueuing test for %s at %s", getID(j.p), j.commitHash)

	// https://developer.github.com/v3/repos/statuses/#create-a-status
	status := &github.RepoStatus{
		State:       github.String("pending"),
		Description: github.String("Tests pending"),
		Context:     &w.name,
	}
	if !w.status(j, status) {
		// Don't bother running the tests.
		return
	}
	// Enqueue and run.
	// TODO(maruel): It should be a buffered channel so it stays FIFO and can
	// deny when there's too many tasks enqueued.
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		w.runJobRequest(j, status, blame)
	}()
}

// wait implements worker.
func (w *workerQueue) wait() {
	w.wg.Wait()
}

// runJobRequest runs the check for the repository hosted on github at the
// specified commit.
//
// It will use the ssh protocol if "useSSH" is set, https otherwise.
// "status" is the github status to keep updating as progress is made.
// If "blame" is not empty, an issue is created on failure. This must be a list
// of github handles. These strings are different from what appears in the git
// commit log. Non-team members cannot be assigned an issue, in this case the
// API will silently drop them.
func (w *workerQueue) runJobRequest(j *jobRequest, status *github.RepoStatus, blame []string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	log.Printf("- Running test for %s at %s", getID(j.p), j.commitHash)
	// https://developer.github.com/v3/gists/#create-a-gist
	// It is accessible via the URL without authentication even if "private".
	gist := &github.Gist{
		Description: github.String(fmt.Sprintf("%s for %s", w.name, j)),
		Public:      github.Bool(false),
		Files: map[github.GistFilename]github.GistFile{
			"setup-0-metadata": {Content: github.String(j.metadata())},
		},
	}
	gist, _, err := w.client.Gists.Create(w.ctx, gist)
	if err != nil {
		// Don't bother running the tests.
		log.Printf("- Failed to create gist: %v", err)
		return
	}
	log.Printf("- Gist at %s", *gist.HTMLURL)

	status.TargetURL = gist.HTMLURL
	status.Description = github.String("Running tests")
	if !w.status(j, status) {
		return
	}

	failed := w.runJobRequestInner(j, gist, status)

	// This requires OAuth scope 'public_repo' or 'repo'. The problem is that
	// this gives full write access, not just issue creation and this is
	// problematic with the current security design of this project. Leave the
	// code there as this is harmless and still work is people do not care about
	// security.
	if failed && len(blame) != 0 {
		title := fmt.Sprintf("Build %q failed on %s", w.name, j.commitHash)
		log.Printf("- Failed: %s", title)
		log.Printf("- Blame: %v", blame)
		// https://developer.github.com/v3/issues/#create-an-issue
		issue := github.IssueRequest{
			Title: &title,
			// TODO(maruel): Add more than just the URL but that's a start.
			Body:      gist.HTMLURL,
			Assignees: &blame,
		}
		if issue, _, err := w.client.Issues.Create(w.ctx, j.p.getOrg(), j.p.getRepo(), &issue); err != nil {
			log.Printf("- failed to create issue: %v", err)
		} else {
			log.Printf("- created issue #%d", *issue.ID)
		}
	}
	log.Printf("- testing done: https://github.com/%s/commit/%s", getID(j.p), j.commitHash[:12])
}

// runJobRequestInner is the inner loop of runJobRequest. It updates gist as the
// checks are progressing.
//
// Returns true if it failed.
func (w *workerQueue) runJobRequestInner(j *jobRequest, gist *github.Gist, status *github.RepoStatus) bool {
	// The function exits once results is closed by the goroutine below.
	w.wg.Add(1)
	defer w.wg.Done()
	start := time.Now()
	results := make(chan gistFile, 16)
	type up struct {
		checks []check
		note   string
	}
	cc := make(chan up)
	go func() {
		defer close(results)

		// Phase 1: parallel sync.
		start := time.Now()
		content, ok := j.sync()
		results <- gistFile{"setup-1-sync", content, ok, time.Since(start)}
		if !ok {
			return
		}

		// Phase 2: checkout.
		start = time.Now()
		content, ok = j.checkout()
		results <- gistFile{"setup-2-get", content, ok, time.Since(start)}
		if !ok {
			return
		}

		// Phase 3: parse config.
		start = time.Now()
		chks, note := j.parseConfig(w.name)
		cc <- up{chks, note}

		// Phase 4: checks.
		j.runChecks(chks, results)
	}()

	// The check #0 is setup-3-checks.
	checkNum := 0
	failed := false
	total := 0
	var delay <-chan time.Time
	for {
		select {
		case <-delay:
			w.gist(gist)
			w.status(j, status)
			delay = nil

		case c := <-cc:
			total = len(c.checks)
			results <- gistFile{"setup-3-checks", c.note + "\nCommands to be run:\n" + cmds(c.checks), true, 0}

		case r, ok := <-results:
			if !ok {
				// The channel closed. Do one last update if necessary then quit.
				if delay != nil {
					w.gist(gist)
					w.status(j, status)
				}
				return failed
			}
			// https://developer.github.com/v3/gists/#edit-a-gist
			if len(r.content) == 0 {
				r.content = "<missing>"
			}

			firstFailure := false
			if !r.success {
				r.name += " (failed)"
				status.State = github.String("failure")
				if !failed {
					firstFailure = true
				}
				failed = true
			}
			r.name += " in " + roundDuration(r.d).String()

			// Update descriptions.
			suffix := ""
			statusDesc := "Running tests"
			if total != 0 {
				if checkNum != total {
					suffix = fmt.Sprintf(" (%d/%d)", checkNum, total)
					checkNum++
				} else {
					// Last check.
					statusDesc = "Ran tests"
					if !failed {
						suffix += " (success!)"
						status.State = github.String("success")
					}
				}
			}
			if failed {
				suffix += " (failed)"
			}
			suffix += " in " + roundDuration(time.Since(start)).String()
			gist.Files[github.GistFilename(r.name)] = github.GistFile{Content: &r.content}
			gist.Description = github.String(fmt.Sprintf("%s for %s%s", w.name, j, suffix))
			status.Description = github.String(statusDesc + suffix)

			// On first failure, do not wait.
			if firstFailure {
				w.gist(gist)
				w.status(j, status)
				delay = nil
			} else if delay == nil {
				// Otherwise, buffer for one second to reduce the number of RPCs. No
				// need to flush for the last item, since the channel will be
				// immediately closed right after.
				delay = time.After(time.Second)
			}
		}
	}
}

// status calls into w.client.Repositories.CreateStatus().
func (w *workerQueue) status(j *jobRequest, status *github.RepoStatus) bool {
	if _, _, err := w.client.Repositories.CreateStatus(w.ctx, j.p.getOrg(), j.p.getRepo(), j.commitHash, status); err != nil {
		if status.ID != nil {
			log.Printf("- failed to update status: %v", err)
		} else {
			log.Printf("- Failed to create status: %v", err)
		}
		return false
	}
	return true
}

// gist calls into w.client.Gists.Edit().
//
// It clears the file mapping to reduce I/O, since files are automatically
// carried over.
func (w *workerQueue) gist(gist *github.Gist) bool {
	if _, _, err := w.client.Gists.Edit(w.ctx, *gist.ID, gist); err != nil {
		log.Printf("- failed to update gist: %v", err)
		return false
	}
	gist.Files = map[github.GistFilename]github.GistFile{}
	return true
}

//

// cmds returns the list of commands to attach to the metadata gist as a single
// indented string.
func cmds(checks []check) string {
	cmds := ""
	for i, c := range checks {
		if i != 0 {
			cmds += "\n"
		}
		if len(c.Env) != 0 {
			cmds += "  " + strings.Join(c.Env, " ")
		}
		cmds += "  " + strings.Join(c.Cmd, " ")
	}
	return cmds
}
