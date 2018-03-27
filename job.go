// Copyright 2018 Marc-Antoine Ruel. All rights reserved.
// Use of this source code is governed under the Apache License, Version 2.0
// that can be found in the LICENSE file.

package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"
)

// gohciBranch is a git branch name that doesn't have an high likelihood of
// conflicting.
const gohciBranch = "_gohci"

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
func run(cwd string, env []string, cmd []string) (string, bool) {
	cmds := strings.Join(env, " ")
	if len(cmds) != 0 {
		cmds += " "
	}
	cmds += strings.Join(cmd, " ")
	log.Printf("- cwd=%s : %s", cwd, cmds)
	c := exec.Command(cmd[0], cmd[1:]...)
	c.Dir = cwd
	if len(env) != 0 {
		oldenv := os.Environ()
		c.Env = make([]string, len(oldenv), len(oldenv)+len(env))
		copy(c.Env, oldenv)
		for _, e := range env {
			// TODO(maruel): Remove previous existing definition.
			c.Env = append(c.Env, e)
		}
	}
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
	stdout, ok := run(repoPath, nil, []string{"git", "pull", "--prune", "--quiet"})
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
	p          *project
	useSSH     bool   // useSSH tells to use ssh instead of https
	commitHash string // commit hash, not a ref
	pullID     int    // pullID is the PR ID if relevant
}

func newJobRequest(p *project, useSSH bool, commitHash string, pullID int) *jobRequest {
	return &jobRequest{
		p:          p,
		useSSH:     useSSH,
		commitHash: commitHash,
		pullID:     pullID,
	}
}

func (j *jobRequest) String() string {
	if j.pullID != 0 {
		return fmt.Sprintf("https://github.com/%s/pull/%d at https://github.com/%s/commit/%s", j.p.name(), j.pullID, j.p.name(), j.commitHash[:12])
	}
	return fmt.Sprintf("https://github.com/%s/commit/%s", j.p.name(), j.commitHash[:12])
}

func (j *jobRequest) cloneURL() string {
	if j.useSSH {
		return "git@github.com:" + j.p.name()
	}
	return "https://github.com/" + j.p.name()
}

// commitHashForPR tries to get the HEAD commit for the PR #.
func (j *jobRequest) commitHashForPR() bool {
	if j.pullID == 0 {
		return false
	}
	stdout, ok := run(".", nil, []string{"git", "ls-remote", j.cloneURL()})
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
// is checked out at the expected commit or Pull Request.
func (f *fetchDetails) cloneOrFetch(c chan<- setupWorkResult) {
	if _, err := os.Stat(f.repoPath); err != nil && !os.IsNotExist(err) {
		c <- setupWorkResult{"<failure>\n" + err.Error() + "\n", false}
		return
	} else if err != nil {
		// Directory doesn't exist, need to clone.
		stdout, ok := run(filepath.Dir(f.repoPath), nil, []string{"git", "clone", "--quiet", f.cloneURL})
		c <- setupWorkResult{stdout, ok}
		if f.pullID == 0 || !ok {
			// For PRs, the commit has to be fetched manually.
			return
		}
	}

	// Directory exists, need to fetch.
	args := []string{"git", "fetch", "--prune", "--quiet", "origin"}
	if f.pullID != 0 {
		args = append(args, fmt.Sprintf("pull/%d/head", f.pullID))
	}
	stdout, ok := run(f.repoPath, nil, args)
	c <- setupWorkResult{stdout, ok}
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
		f.cloneOrFetch(c)
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

func makeFetchDetails(j *jobRequest, src string) *fetchDetails {
	repoPath := ""
	if len(j.p.AltPath) != 0 {
		repoPath = filepath.Join(src, strings.Replace(j.p.AltPath, "/", string(os.PathSeparator), -1))
	} else {
		repoURL := "github.com/" + j.p.name()
		repoPath = filepath.Join(src, strings.Replace(repoURL, "/", string(os.PathSeparator), -1))
	}
	return &fetchDetails{repoPath: repoPath, cloneURL: j.cloneURL(), commitHash: j.commitHash, pullID: j.pullID}
}

// runChecks syncs then runs the checks and returns task's results.
//
// It aggressively concurrently fetches all repositories in `gopath` to
// accelerate the processing.
func runChecks(j *jobRequest, gopath string, results chan<- gistFile) bool {
	start := time.Now()
	c := make(chan setupWorkResult)
	src := filepath.Join(gopath, "src")
	f := makeFetchDetails(j, src)
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
	}
	setupGet := gistFile{name: "setup-2-get", success: true}
	for _, c := range setupCmds {
		stdout := ""
		stdout, setupGet.success = run(f.repoPath, nil, c)
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
	for i, c := range j.p.Checks {
		start = time.Now()
		stdout, ok2 := run(f.repoPath, c.Env, c.Cmd)
		results <- gistFile{fmt.Sprintf("cmd%d", i+1), stdout, ok2, time.Since(start)}
		if !ok2 {
			// Still run the other tests.
			ok = false
		}
	}
	return ok
}

// runLocal runs the checks run.
func runLocal(p *project, w *worker, gopath, commitHash string, update, useSSH bool) error {
	j := newJobRequest(p, useSSH, commitHash, 0)
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
		success := runChecks(j, gopath, results)
		close(results)
		wg.Wait()
		_, err := fmt.Printf("\nSuccess: %t\n", success)
		return err
	}
	// The reason for using the async version is that it creates the status.
	w.enqueueCheck(j, nil)
	w.wg.Wait()
	// TODO(maruel): Return any error that occurred.
	return nil
}
