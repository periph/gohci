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
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	fsnotify "gopkg.in/fsnotify.v1"
	yaml "gopkg.in/yaml.v2"

	"github.com/google/go-github/github"
	"golang.org/x/oauth2"
	"periph.io/x/gohci/lib"
)

var ctx = context.Background()
var start time.Time

// gohciBranch is a git branch name that doens't have an high likelihood of
// conflicting.
const gohciBranch = "_gohci"

type config struct {
	Port              int        // TCP port number for HTTP server.
	WebHookSecret     string     // https://developer.github.com/webhooks/
	Oauth2AccessToken string     // https://github.com/settings/tokens, check "repo:status" and "gist"
	Name              string     // Display name to use in the status report on Github.
	AltPath           string     // Alternative package path to use. Defaults to the actual path.
	SuperUsers        []string   // List of github accounts that can trigger a run. In practice any user with write access is a super user but OAuth2 tokens with limited scopes cannot get this information.
	Checks            [][]string // Commands to run to test the repository. They are run one after the other from the repository's root.
}

// loadConfig loads the current config or returns the default one.
//
// It saves a reformatted version on disk if it was not in the canonical format.
func loadConfig(fileName string) (*config, error) {
	hostname, err := os.Hostname()
	if err != nil {
		hostname = "gohci"
	}
	c := &config{
		Port:              8080,
		WebHookSecret:     "Create a secret and set it at github.com/user/repo/settings/hooks",
		Oauth2AccessToken: "Get one at https://github.com/settings/tokens",
		Name:              hostname,
		AltPath:           "",
		SuperUsers:        nil,
		Checks:            nil,
	}
	b, err := ioutil.ReadFile(fileName)
	if err != nil {
		b, err = yaml.Marshal(c)
		if err != nil {
			return nil, err
		}
		if len(c.Checks) == 0 {
			c.Checks = [][]string{{"go", "test", "./..."}}
		}
		if err = ioutil.WriteFile(fileName, b, 0600); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("wrote new %s", fileName)
	}
	if err = yaml.Unmarshal(b, c); err != nil {
		return nil, err
	}
	if len(c.Checks) == 0 {
		c.Checks = [][]string{{"go", "test", "./..."}}
	}
	return c, nil
}

// normalizeUTF8 returns valid UTF8 from potentially incorrectly encoded data
// from an untrusted process.
func normalizeUTF8(b []byte) []byte {
	if utf8.Valid(b) {
		return b
	}
	var out []byte
	for len(b) != 0 {
		r, size := utf8.DecodeRune(b)
		if r != utf8.RuneError {
			out = append(out, b[:size]...)
		}
		b = b[size:]
	}
	return out
}

// roundTime returns time rounded at a value that makes sense to display to the
// user.
func roundTime(t time.Duration) time.Duration {
	if t < time.Millisecond {
		// Precise at 1ns.
		return t
	}
	if t < time.Second {
		// Precise at 1µs.
		return (t + time.Microsecond/2) / time.Microsecond * time.Microsecond
	}
	// Round at 1ms
	return (t + time.Millisecond/2) / time.Millisecond * time.Millisecond
}

// run runs an executable and returns mangled merged stdout+stderr.
func run(cwd string, cmd ...string) (string, bool) {
	cmds := strings.Join(cmd, " ")
	log.Printf("- cwd=%s : %s", cwd, cmds)
	c := exec.Command(cmd[0], cmd[1:]...)
	c.Dir = cwd
	start := time.Now()
	out, err := c.CombinedOutput()
	duration := time.Since(start)
	exit := 0
	if err != nil {
		exit = -1
		if len(out) == 0 {
			out = []byte("<failure>\n" + err.Error() + "\n")
		}
		if exiterr, ok := err.(*exec.ExitError); ok {
			if status, ok := exiterr.Sys().(syscall.WaitStatus); ok {
				exit = status.ExitStatus()
			}
		}
	}
	return fmt.Sprintf("$ %s  (exit:%d in %s)\n%s", cmds, exit, roundTime(duration), normalizeUTF8(out)), err == nil
}

// gistFile is an item in the gist.
//
// It represents either the stdout of a command or metadata. They are created
// by processing fileToPush.
type gistFile struct {
	name, content string
	success       bool
	d             time.Duration
}

// metadata generates the pseudo-file to present information about the worker.
func metadata(commit, gopath string) string {
	out := fmt.Sprintf(
		"Commit:  %s\nCPUs:    %d\nVersion: %s\nGOROOT:  %s\nGOPATH:  %s\nPATH:    %s\n",
		commit, runtime.NumCPU(), runtime.Version(), runtime.GOROOT(), gopath, os.Getenv("PATH"))
	if runtime.GOOS != "windows" {
		if s, err := exec.Command("uname", "-a").CombinedOutput(); err == nil {
			out += "uname:   " + strings.TrimSpace(string(s)) + "\n"
		}
	}
	return out
}

// pullRepo tries to pull a repository if possible. If the pull failed, it
// deletes the checkout.
func pullRepo(repoPath string) (string, bool) {
	stdout, ok := run(repoPath, "git", "pull", "--prune", "--quiet")
	if !ok {
		// Give up and delete the repository. At worst "go get" will fetch
		// it below.
		if err := os.RemoveAll(repoPath); err != nil {
			// Deletion failed, that's a hard failure.
			return stdout + "<failure>\n" + err.Error() + "\n", false
		}
		return stdout + "<recovered failure>\nrm -rf " + repoPath + "\n", true
	}
	return stdout, ok
}

// jobRequest is the details to run a verification job.
type jobRequest struct {
	orgName    string // orgName is the user part
	repoName   string // repoName is the repo part
	useSSH     bool   // useSSH tells to use ssh instead of https
	commitHash string // commit hash, not a ref
	pullID     int    // pullID is the PR ID if relevant
}

func (j *jobRequest) String() string {
	if j.pullID != 0 {
		return fmt.Sprintf("https://github.com/%s/pull/%d at https://github.com/%s/commit/%s", j.repo(), j.pullID, j.repo(), j.commitHash[:12])
	}
	return fmt.Sprintf("https://github.com/%s/commit/%s", j.repo(), j.commitHash[:12])
}

func (j *jobRequest) repo() string {
	return j.orgName + "/" + j.repoName
}

func (j *jobRequest) cloneURL() string {
	if j.useSSH {
		return "git@github.com:" + j.repo()
	}
	return "https://github.com/" + j.repo()
}

// commitHashForPR tries to get the HEAD commit for the PR #.
func (j *jobRequest) commitHashForPR() bool {
	if j.pullID == 0 {
		return false
	}
	stdout, ok := run(".", "git", "ls-remote", j.cloneURL())
	if !ok {
		return false
	}
	p := fmt.Sprintf("refs/pull/%d/head", j.pullID)
	for _, l := range strings.Split(stdout, "\n") {
		if strings.HasSuffix(l, p) {
			j.commitHash = strings.SplitN(l, "\t", 2)[0]
			log.Printf("  Found %s for PR #%d", j.commitHash, j.pullID)
			return true
		}
	}
	return false
}

// fetchDetails is the details to run a verification job.
type fetchDetails struct {
	repoPath   string // repoPath is the absolute path to the repository
	cloneURL   string // cloneURL is the URL to clone for, either ssh or https
	commitHash string // commit hash, not a ref
	pullID     int    // pullID is the PR ID if relevant
}

// cloneOrFetch is meant to be used on the primary repository, making sure it
// is checked out.
func (f *fetchDetails) cloneOrFetch() (string, bool) {
	if _, err := os.Stat(f.repoPath); err == nil {
		if f.pullID != 0 {
			// For PRs, the commit has to be fetched manually.
			return run(f.repoPath, "git", "fetch", "--prune", "--quiet", "origin", fmt.Sprintf("pull/%d/head", f.pullID))
		}
		return run(f.repoPath, "git", "fetch", "--prune", "--quiet", "origin")
	} else if !os.IsNotExist(err) {
		return "<failure>\n" + err.Error() + "\n", false
	}
	if f.pullID != 0 {
		// TODO(maruel): Not sure this works.
		return run(filepath.Dir(f.repoPath), "git", "clone", "--quiet", f.cloneURL, "-b", fmt.Sprintf("pull/%d/head", f.pullID))
	}
	return run(filepath.Dir(f.repoPath), "git", "clone", "--quiet", f.cloneURL)
}

// syncParallel checkouts out one repository if missing, and syncs all the
// other git repositories found under the root directory concurrently.
//
// Since fetching is a remote operation with potentially low CPU and I/O,
// reduce the total latency by doing all the fetches concurrently.
//
// The goal is to make "go get -t -d" as fast as possible, as all repositories
// are already synced to HEAD.
//
// cloneURL is fetched into repoPath.
func (f *fetchDetails) syncParallel(root string, c chan<- setupWorkResult) {
	// git clone / go get will have a race condition if the directory doesn't
	// exist.
	up := filepath.Dir(f.repoPath)
	err := os.MkdirAll(up, 0700)
	log.Printf("MkdirAll(%q) -> %v", up, err)
	if err != nil && !os.IsExist(err) {
		c <- setupWorkResult{"<failure>\n" + err.Error() + "\n", false}
		return
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		stdout, ok := f.cloneOrFetch()
		c <- setupWorkResult{stdout, ok}
	}()
	// Sync all the repositories concurrently.
	err = filepath.Walk(root, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if path == f.repoPath {
			// repoPath is handled specifically above.
			return filepath.SkipDir
		}
		if fi.Name() == ".git" {
			path = filepath.Dir(path)
			wg.Add(1)
			go func(p string) {
				defer wg.Done()
				stdout, ok := pullRepo(p)
				c <- setupWorkResult{stdout, ok}
			}(path)
			return filepath.SkipDir
		}
		return nil
	})
	wg.Wait()
	if err != nil {
		c <- setupWorkResult{"<directory walking failure>\n" + err.Error() + "\n", false}
	}
}

// setupWorkResult is metadata to add to the 'setup' pseudo-steps.
//
// It is used to track the work as all repositories are pulled concurrently,
// then used.
type setupWorkResult struct {
	content string
	ok      bool
}

func makeFetchDetails(j *jobRequest, altPath string, src string) *fetchDetails {
	repoPath := ""
	if len(altPath) != 0 {
		repoPath = filepath.Join(src, strings.Replace(altPath, "/", string(os.PathSeparator), -1))
	} else {
		repoURL := "github.com/" + j.repo()
		repoPath = filepath.Join(src, strings.Replace(repoURL, "/", string(os.PathSeparator), -1))
	}
	return &fetchDetails{repoPath: repoPath, cloneURL: j.cloneURL(), commitHash: j.commitHash, pullID: j.pullID}
}

// runChecks syncs then runs the checks and returns task's results.
//
// It aggressively concurrently fetches all repositories in `gopath` to
// accelerate the processing.
func runChecks(cmds [][]string, j *jobRequest, altPath string, gopath string, results chan<- gistFile) bool {
	start := time.Now()
	c := make(chan setupWorkResult)
	src := filepath.Join(gopath, "src")
	f := makeFetchDetails(j, altPath, src)
	go func() {
		defer close(c)
		f.syncParallel(src, c)
	}()
	setupSync := setupWorkResult{"", true}
	for i := range c {
		setupSync.content += i.content
		if !i.ok {
			setupSync.ok = false
		}
	}
	results <- gistFile{"setup-1-sync", setupSync.content, setupSync.ok, time.Since(start)}
	if !setupSync.ok {
		return false
	}

	start = time.Now()
	setupCmds := [][]string{
		// "go get" will try to pull and will complain if the checkout is not on a
		// branch.
		{"git", "checkout", "--quiet", "-B", gohciBranch, j.commitHash},
		// "git pull --ff-only" will fail if there's no tracking branch, and
		// it occasionally happen.
		{"git", "checkout", "--quiet", "-B", gohciBranch + "2", gohciBranch},
		// Pull add necessary dependencies.
		{"go", "get", "-v", "-d", "-t", "./..."},
		// Precompilation has a dramatic effect on a Raspberry Pi. YMMV.
		{"go", "test", "-i", "./..."},
	}
	setupGet := gistFile{name: "setup-2-get", success: true}
	for _, c := range setupCmds {
		stdout := ""
		stdout, setupGet.success = run(f.repoPath, c...)
		setupGet.content += stdout
		if !setupGet.success {
			break
		}
	}
	setupGet.d = time.Since(start)
	results <- setupGet
	if !setupGet.success {
		return false
	}
	ok := true
	// Finally run the checks!
	for i, cmd := range cmds {
		start = time.Now()
		stdout, ok2 := run(f.repoPath, cmd...)
		results <- gistFile{fmt.Sprintf("cmd%d", i+1), stdout, ok2, time.Since(start)}
		if !ok2 {
			// Still run the other tests.
			ok = false
		}
	}
	return ok
}

// server is both the HTTP server and the task queue server.
type server struct {
	c      *config
	client *github.Client
	gopath string
	cmds   string
	mu     sync.Mutex     // Set when a check is running
	wg     sync.WaitGroup // Set for each pending task.
}

// isSuperUser returns true if the user can trigger tasks.
func (s *server) isSuperUser(u string) bool {
	for _, c := range s.c.SuperUsers {
		if c == u {
			return true
		}
	}
	// s.client.Repositories.IsCollaborator() requires *write* access to the
	// repository, which we really do not want here. So don't even try for now.
	return false
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
		io.WriteString(w, time.Since(start).String())
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
			j := &jobRequest{
				orgName:    *event.Repo.Owner.Login,
				repoName:   *event.Repo.Name,
				useSSH:     *event.Repo.Private,
				commitHash: *event.Comment.CommitID,
				pullID:     0,
			}
			s.runCheckAsync(j, nil)
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
					j := &jobRequest{
						orgName:    *event.Repo.Owner.Login,
						repoName:   *event.Repo.Name,
						useSSH:     *event.Repo.Private,
						commitHash: "",
						pullID:     *event.Issue.Number,
					}
					// Immediately fetch the issue head commit inside the webhook, since
					// it's a race condition.
					if !j.commitHashForPR() {
						log.Printf("- failed to get HEAD for issue #%d", *event.Issue.Number)
					} else {
						s.runCheckAsync(j, nil)
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
		case "opened", "synchronized":
			// TODO(maruel): If a reviewer is set, it has to be set by a repository
			// owner (?) If so, then it would be safe to run.
			if s.isSuperUser(*event.Sender.Login) {
				j := &jobRequest{
					orgName:    *event.Repo.Owner.Login,
					repoName:   *event.Repo.Name,
					useSSH:     *event.Repo.Private,
					commitHash: *event.PullRequest.Head.SHA,
					pullID:     *event.PullRequest.Number,
				}
				s.runCheckAsync(j, nil)
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
				j := &jobRequest{
					orgName:    *event.Repo.Owner.Login,
					repoName:   *event.Repo.Name,
					useSSH:     *event.Repo.Private,
					commitHash: *event.PullRequest.Head.SHA,
					pullID:     *event.PullRequest.Number,
				}
				s.runCheckAsync(j, nil)
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
				j := &jobRequest{
					orgName:    *event.Repo.Owner.Name,
					repoName:   *event.Repo.Name,
					useSSH:     *event.Repo.Private,
					commitHash: *event.HeadCommit.ID,
					pullID:     0,
				}
				s.runCheckAsync(j, blame)
			}
		}
	default:
		log.Printf("- ignoring hook type %s", reflect.TypeOf(event).Elem().Name())
	}
}

// runCheckAsync immediately add the status that the test run is pending and
// add the run in the queue. Ensures that the service doesn't restart until the
// task is done.
func (s *server) runCheckAsync(j *jobRequest, blame []string) {
	s.wg.Add(1)
	defer s.wg.Done()
	log.Printf("- Enqueuing test for %s at %s", j.repo(), j.commitHash)
	// https://developer.github.com/v3/repos/statuses/#create-a-status
	status := &github.RepoStatus{
		State:       github.String("pending"),
		Description: github.String(fmt.Sprintf("Tests pending (0/%d)", len(s.c.Checks)+2)),
		Context:     &s.c.Name,
	}
	if _, _, err := s.client.Repositories.CreateStatus(ctx, j.orgName, j.repoName, j.commitHash, status); err != nil {
		// Don't bother running the tests.
		log.Printf("- Failed to create status: %v", err)
		return
	}
	// Enqueue and run.
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.runCheckSync(j, status, blame)
	}()
}

// runCheckSync runs the check for the repository hosted on github at the
// specified commit.
//
// It will use the ssh protocol if "useSSH" is set, https otherwise.
// "status" is the github status to keep updating as progress is made.
// If "blame" is not empty, an issue is created on failure. This must be a list
// of github handles. These strings are different from what appears in the git
// commit log. Non-team members cannot be assigned an issue, in this case the
// API will silently drop them.
func (s *server) runCheckSync(j *jobRequest, status *github.RepoStatus, blame []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	log.Printf("- Running test for %s at %s", j.repo(), j.commitHash)
	total := len(s.c.Checks) + 2
	gistDesc := fmt.Sprintf("%s for %s", s.c.Name, j)
	suffix := fmt.Sprintf(" (0/%d)", total)
	// https://developer.github.com/v3/gists/#create-a-gist
	// It is accessible via the URL without authentication even if "private".
	gist := &github.Gist{
		Description: github.String(gistDesc + suffix),
		Public:      github.Bool(false),
		Files: map[github.GistFilename]github.GistFile{
			"setup-0-metadata": {Content: github.String(metadata(j.commitHash, s.gopath) + "\nCommands to be run:\n" + s.cmds)},
		},
	}
	gist, _, err := s.client.Gists.Create(ctx, gist)
	if err != nil {
		// Don't bother running the tests.
		log.Printf("- Failed to create gist: %v", err)
		return
	}
	log.Printf("- Gist at %s", *gist.HTMLURL)

	statusDesc := "Running tests"
	status.TargetURL = gist.HTMLURL
	status.Description = github.String(statusDesc + suffix)
	if _, _, err = s.client.Repositories.CreateStatus(ctx, j.orgName, j.repoName, j.commitHash, status); err != nil {
		log.Printf("- Failed to update status: %v", err)
		return
	}

	failed := s.runCheckSyncLoop(j, statusDesc, gistDesc, suffix, total, gist, status)

	// This requires OAuth scope 'public_repo' or 'repo'. The problem is that
	// this gives full write access, not just issue creation and this is
	// problematic with the current security design of this project. Leave the
	// code there as this is harmless and still work is people do not care about
	// security.
	if failed && len(blame) != 0 {
		title := fmt.Sprintf("Build %q failed on %s", s.c.Name, j.commitHash)
		log.Printf("- Failed: %s", title)
		log.Printf("- Blame: %v", blame)
		// https://developer.github.com/v3/issues/#create-an-issue
		issue := github.IssueRequest{
			Title: &title,
			// TODO(maruel): Add more than just the URL but that's a start.
			Body:      gist.HTMLURL,
			Assignees: &blame,
		}
		if issue, _, err := s.client.Issues.Create(ctx, j.orgName, j.repoName, &issue); err != nil {
			log.Printf("- failed to create issue: %v", err)
		} else {
			log.Printf("- created issue #%d", *issue.ID)
		}
	}
	log.Printf("- testing done: https://github.com/%s/commit/%s", j.repo(), j.commitHash[:12])
}

// runCheckSyncLoop is the inner loop of runCheckSync. It updates gist as the
// checks are progressing.
func (s *server) runCheckSyncLoop(j *jobRequest, statusDesc, gistDesc, suffix string, total int, gist *github.Gist, status *github.RepoStatus) bool {
	start := time.Now()
	results := make(chan gistFile)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		runChecks(s.c.Checks, j, s.c.AltPath, s.gopath, results)
		close(results)
	}()

	i := 1
	failed := false
	var delay <-chan time.Time
	for {
		select {
		case <-delay:
			if _, _, err := s.client.Gists.Edit(ctx, *gist.ID, gist); err != nil {
				log.Printf("- failed to update gist: %v", err)
			}
			gist.Files = map[github.GistFilename]github.GistFile{}
			if _, _, err := s.client.Repositories.CreateStatus(ctx, j.orgName, j.repoName, j.commitHash, status); err != nil {
				log.Printf("- failed to update status: %v", err)
			}
			delay = nil

		case r, ok := <-results:
			if !ok {
				if delay != nil {
					if _, _, err := s.client.Gists.Edit(ctx, *gist.ID, gist); err != nil {
						log.Printf("- failed to update gist: %v", err)
					}
					gist.Files = map[github.GistFilename]github.GistFile{}
					if _, _, err := s.client.Repositories.CreateStatus(ctx, j.orgName, j.repoName, j.commitHash, status); err != nil {
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

// runLocal runs the checks run.
func runLocal(s *server, c *config, gopath, commitHash, test string, update, useSSH bool) error {
	parts := strings.SplitN(test, "/", 2)
	j := &jobRequest{
		orgName:    parts[0],
		repoName:   parts[1],
		useSSH:     useSSH,
		commitHash: commitHash,
		pullID:     0,
	}
	if !update {
		results := make(chan gistFile)
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range results {
				if !i.success {
					i.name += " (failed)"
				}
				fmt.Printf("--- %s\n%s", i.name, i.content)
			}
		}()
		fmt.Printf("--- setup-0-metadata\n%s", metadata(commitHash, gopath))
		success := runChecks(c.Checks, j, c.AltPath, gopath, results)
		close(results)
		wg.Wait()
		_, err := fmt.Printf("\nSuccess: %t\n", success)
		return err
	}
	s.runCheckAsync(j, nil)
	s.wg.Wait()
	// TODO(maruel): Return any error that occurred.
	return nil
}

// runServer runs the web server.
func runServer(s *server, c *config, wd, fileName string) error {
	http.Handle("/", s)
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
	s.wg.Wait()
	return err
}

// useGT replaces the "go test" calls with "gt".
func useGT(c *config, wd, gopath string) {
	hasTest := false
	for _, cmd := range c.Checks {
		if len(cmd) >= 2 && cmd[0] == "go" && cmd[1] == "test" {
			hasTest = true
			break
		}
	}
	if !hasTest {
		return
	}
	stdout, useGT := run(wd, "go", "get", "rsc.io/gt")
	if !useGT {
		log.Print(stdout)
		return
	}
	log.Print("Using gt")
	os.Setenv("CACHE", gopath)
	for i, cmd := range c.Checks {
		if len(cmd) >= 2 && cmd[0] == "go" && cmd[1] == "test" {
			cmd[1] = "gt"
			c.Checks[i] = cmd[1:]
		}
	}
}

func mainImpl() error {
	start = time.Now()
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
	// local GOPATH to install gt. This is safer as this doesn't modify the host
	// environment.
	os.Setenv("GOPATH", gopath)
	os.Setenv("PATH", filepath.Join(gopath, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"))
	useGT(c, wd, gopath)
	cmds := ""
	for i, cmd := range c.Checks {
		if i != 0 {
			cmds += "\n"
		}
		cmds += "  " + strings.Join(cmd, " ")
	}
	tc := oauth2.NewClient(oauth2.NoContext, oauth2.StaticTokenSource(&oauth2.Token{AccessToken: c.Oauth2AccessToken}))
	s := &server{c: c, client: github.NewClient(tc), gopath: gopath, cmds: cmds}
	if len(*test) != 0 {
		return runLocal(s, c, gopath, *commit, *test, *update, *useSSH)
	}
	return runServer(s, c, wd, fileName)
}

func main() {
	if err := mainImpl(); err != nil {
		fmt.Fprintf(os.Stderr, "gohci: %s.\n", err)
		os.Exit(1)
	}
}
