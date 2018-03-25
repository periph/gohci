// Copyright 2017 Marc-Antoine Ruel. All rights reserved.
// Use of this source code is governed under the Apache License, Version 2.0
// that can be found in the LICENSE file.

package main

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/google/go-github/github"
	"golang.org/x/oauth2"
)

// worker is the task queue server.
type worker struct {
	c      *config
	ctx    context.Context
	client *github.Client // Used to set commit status and create gists.
	gopath string         // Working environment.

	mu sync.Mutex     // Set when a check is running in runCheck()
	wg sync.WaitGroup // Set for each pending task.
}

func newWorker(c *config, gopath string) *worker {
	tc := oauth2.NewClient(oauth2.NoContext, oauth2.StaticTokenSource(&oauth2.Token{AccessToken: c.Oauth2AccessToken}))
	return &worker{
		c:      c,
		ctx:    context.Background(),
		client: github.NewClient(tc),
		gopath: gopath,
	}
}

// enqueueCheck immediately add the status that the test run is pending and
// add the run in the queue. Ensures that the service doesn't restart until the
// task is done.
func (w *worker) enqueueCheck(j *jobRequest, blame []string) {
	w.wg.Add(1)
	defer w.wg.Done()
	log.Printf("- Enqueuing test for %s at %s", j.p.name(), j.commitHash)
	// https://developer.github.com/v3/repos/statuses/#create-a-status
	status := &github.RepoStatus{
		State:       github.String("pending"),
		Description: github.String(fmt.Sprintf("Tests pending (0/%d)", len(w.c.Checks)+2)),
		Context:     &w.c.Name,
	}
	if _, _, err := w.client.Repositories.CreateStatus(w.ctx, j.p.Org, j.p.Repo, j.commitHash, status); err != nil {
		// Don't bother running the tests.
		log.Printf("- Failed to create status: %v", err)
		return
	}
	// Enqueue and run.
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		w.runCheck(j, status, blame)
	}()
}

// runCheck runs the check for the repository hosted on github at the specified
// commit.
//
// It will use the ssh protocol if "useSSH" is set, https otherwise.
// "status" is the github status to keep updating as progress is made.
// If "blame" is not empty, an issue is created on failure. This must be a list
// of github handles. These strings are different from what appears in the git
// commit log. Non-team members cannot be assigned an issue, in this case the
// API will silently drop them.
func (w *worker) runCheck(j *jobRequest, status *github.RepoStatus, blame []string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	log.Printf("- Running test for %s at %s", j.p.name(), j.commitHash)
	total := len(w.c.Checks) + 2
	gistDesc := fmt.Sprintf("%s for %s", w.c.Name, j)
	suffix := fmt.Sprintf(" (0/%d)", total)
	// https://developer.github.com/v3/gists/#create-a-gist
	// It is accessible via the URL without authentication even if "private".
	gist := &github.Gist{
		Description: github.String(gistDesc + suffix),
		Public:      github.Bool(false),
		Files: map[github.GistFilename]github.GistFile{
			"setup-0-metadata": {Content: github.String(metadata(j.commitHash, w.gopath) + "\nCommands to be run:\n" + w.c.cmds())},
		},
	}
	gist, _, err := w.client.Gists.Create(w.ctx, gist)
	if err != nil {
		// Don't bother running the tests.
		log.Printf("- Failed to create gist: %v", err)
		return
	}
	log.Printf("- Gist at %s", *gist.HTMLURL)

	statusDesc := "Running tests"
	status.TargetURL = gist.HTMLURL
	status.Description = github.String(statusDesc + suffix)
	if _, _, err = w.client.Repositories.CreateStatus(w.ctx, j.p.Org, j.p.Repo, j.commitHash, status); err != nil {
		log.Printf("- Failed to update status: %v", err)
		return
	}

	failed := w.runCheckInner(j, statusDesc, gistDesc, suffix, total, gist, status)

	// This requires OAuth scope 'public_repo' or 'repo'. The problem is that
	// this gives full write access, not just issue creation and this is
	// problematic with the current security design of this project. Leave the
	// code there as this is harmless and still work is people do not care about
	// security.
	if failed && len(blame) != 0 {
		title := fmt.Sprintf("Build %q failed on %s", w.c.Name, j.commitHash)
		log.Printf("- Failed: %s", title)
		log.Printf("- Blame: %v", blame)
		// https://developer.github.com/v3/issues/#create-an-issue
		issue := github.IssueRequest{
			Title: &title,
			// TODO(maruel): Add more than just the URL but that's a start.
			Body:      gist.HTMLURL,
			Assignees: &blame,
		}
		if issue, _, err := w.client.Issues.Create(w.ctx, j.p.Org, j.p.Repo, &issue); err != nil {
			log.Printf("- failed to create issue: %v", err)
		} else {
			log.Printf("- created issue #%d", *issue.ID)
		}
	}
	log.Printf("- testing done: https://github.com/%s/commit/%s", j.p.name(), j.commitHash[:12])
}

// runCheckInner is the inner loop of runCheck. It updates gist as the checks
// are progressing.
func (w *worker) runCheckInner(j *jobRequest, statusDesc, gistDesc, suffix string, total int, gist *github.Gist, status *github.RepoStatus) bool {
	start := time.Now()
	results := make(chan gistFile)
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		runChecks(w.c.Checks, j, w.c.AltPath, w.gopath, results)
		close(results)
	}()

	i := 1
	failed := false
	var delay <-chan time.Time
	for {
		select {
		case <-delay:
			if _, _, err := w.client.Gists.Edit(w.ctx, *gist.ID, gist); err != nil {
				log.Printf("- failed to update gist: %v", err)
			}
			gist.Files = map[github.GistFilename]github.GistFile{}
			if _, _, err := w.client.Repositories.CreateStatus(w.ctx, j.p.Org, j.p.Repo, j.commitHash, status); err != nil {
				log.Printf("- failed to update status: %v", err)
			}
			delay = nil

		case r, ok := <-results:
			if !ok {
				if delay != nil {
					if _, _, err := w.client.Gists.Edit(w.ctx, *gist.ID, gist); err != nil {
						log.Printf("- failed to update gist: %v", err)
					}
					gist.Files = map[github.GistFilename]github.GistFile{}
					if _, _, err := w.client.Repositories.CreateStatus(w.ctx, j.p.Org, j.p.Repo, j.commitHash, status); err != nil {
						log.Printf("- failed to update status: %v", err)
					}
				}
				return failed
			}

			// https://developer.github.com/v3/gists/#edit-a-gist
			if len(r.content) == 0 {
				r.content = "<missing>"
			}
			if !r.success {
				r.name += " (failed)"
				failed = true
				status.State = github.String("failure")
			}
			r.name += " in " + roundTime(r.d).String()
			suffix = ""
			if i != total {
				suffix = fmt.Sprintf(" (%d/%d)", i, total)
			} else {
				statusDesc = "Ran tests"
				if !failed {
					suffix += " (success!)"
					status.State = github.String("success")
				}
			}
			if failed {
				suffix += " (failed)"
			}
			suffix += " in " + roundTime(time.Since(start)).String()
			gist.Files[github.GistFilename(r.name)] = github.GistFile{Content: &r.content}
			gist.Description = github.String(gistDesc + suffix)
			status.Description = github.String(statusDesc + suffix)
			delay = time.After(500 * time.Millisecond)
			i++
		}
	}
}
