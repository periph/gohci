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
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"
)

// gohciBranch is a git branch name that doesn't have an high likelihood of
// conflicting.
const gohciBranch = "_gohci"

var muCmd sync.Mutex

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

// Wrap the exec.Command() call with PATH value override.
//
// exec.Command() calls exec.Lookup() right away, and there is no way to
// override the PATH variable used by exec.Lookup(), so the process' value
// must be temporarily changed.
func getCmd(path string, cmd []string) *exec.Cmd {
	muCmd.Lock()
	defer muCmd.Unlock()
	if path != "" {
		oldpath := os.Getenv("PATH")
		os.Setenv("PATH", path)
		// Restore PATH.
		defer func() { os.Setenv("PATH", oldpath) }()
	}
	return exec.Command(cmd[0], cmd[1:]...)
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

// jobRequest is the details to run a verification job.
type jobRequest struct {
	p          project
	useSSH     bool     // useSSH tells to use ssh instead of https
	commitHash string   // commit hash, not a ref
	pullID     int      // pullID is the PR ID if relevant
	gopath     string   // Cache of GOPATH
	path       string   // Cache of PATH
	env        []string // Precomputed environment variables
}

// newJobRequest creates a new test request for project 'p' on commitHash
// and/or pullID.
func newJobRequest(p project, useSSH bool, wd, commitHash string, pullID int) *jobRequest {
	// Organization names cannot contain an underscore so it 'should' be fine.
	gopath := filepath.Join(wd, p.getOrg()+"_"+p.getRepo())
	path := filepath.Join(gopath, "bin") + string(os.PathListSeparator) + os.Getenv("PATH")
	// Setup the environment variables.
	oldenv := os.Environ()
	env := make([]string, 0, len(oldenv))
	for _, v := range oldenv {
		if strings.HasPrefix(v, "GOPATH=") || strings.HasPrefix(v, "PATH=") {
			continue
		}
		env = append(env, v)
	}
	// GOPATH may not be set especially when running from systemd, so use the
	// local GOPATH. This is safer as this doesn't modify the host environment.
	env = append(env, "GOPATH="+gopath)
	env = append(env, "PATH="+path)

	return &jobRequest{
		p:          p,
		useSSH:     useSSH,
		commitHash: commitHash,
		pullID:     pullID,
		gopath:     gopath,
		path:       path,
		env:        env,
	}
}

func (j *jobRequest) String() string {
	if j.pullID != 0 {
		return fmt.Sprintf("https://github.com/%s/pull/%d at https://github.com/%s/commit/%s", getID(j.p), j.pullID, getID(j.p), j.commitHash[:12])
	}
	return fmt.Sprintf("https://github.com/%s/commit/%s", getID(j.p), j.commitHash[:12])
}

func (j *jobRequest) cloneURL() string {
	if j.useSSH {
		return "git@github.com:" + getID(j.p)
	}
	return "https://github.com/" + getID(j.p)
}

// findCommitHash tries to get the HEAD commit for the PR # or master branch.
func (j *jobRequest) findCommitHash() bool {
	if err := j.assertDir(); err != nil {
		return false
	}
	stdout, ok := j.run("", nil, []string{"git", "ls-remote", j.cloneURL()}, false)
	if !ok {
		log.Printf("  git ls-remote failed:\n%s", stdout)
		return false
	}
	p := "refs/heads/master"
	if j.pullID != 0 {
		p = fmt.Sprintf("refs/pull/%d/head", j.pullID)
	}
	for _, l := range strings.Split(stdout, "\n") {
		if strings.HasSuffix(l, p) {
			j.commitHash = strings.SplitN(l, "\t", 2)[0]
			log.Printf("  Found %s for PR #%d", j.commitHash, j.pullID)
			return true
		}
	}
	log.Printf("  Didn't find remote")
	return false
}

// metadata generates the pseudo-file to present information about the worker.
func (j *jobRequest) metadata() string {
	out := fmt.Sprintf(
		"Commit:  %s\nCPUs:    %d\nVersion: %s\nGOROOT:  %s\nGOPATH:  %s\nPATH:    %s\n",
		j.commitHash, runtime.NumCPU(), runtime.Version(), runtime.GOROOT(), j.gopath, j.path)
	if runtime.GOOS != "windows" {
		if s, err := exec.Command("uname", "-a").CombinedOutput(); err == nil {
			out += "uname:   " + strings.TrimSpace(string(s)) + "\n"
		}
	}
	return out
}

// run runs an executable and returns mangled merged stdout+stderr.
//
// Use pathOverride when running checks.
func (j *jobRequest) run(relwd string, env []string, cmd []string, pathOverride bool) (string, bool) {
	cmds := strings.Join(env, " ")
	if len(cmds) != 0 {
		cmds += " "
	}
	cmds += strings.Join(cmd, " ")
	log.Printf("- relwd=%s : %s", relwd, cmds)
	var c *exec.Cmd
	if pathOverride {
		c = getCmd(j.path, cmd)
	} else {
		c = getCmd("", cmd)
	}
	c.Dir = filepath.Join(j.gopath, "src", relwd)
	// Setup the environment variables.
	if len(env) != 0 {
		c.Env = make([]string, 0, len(j.env)+len(env))
		// TODO(maruel): Remove previous existing definition.
		c.Env = append(c.Env, env...)
		c.Env = append(c.Env, j.env...)
	} else {
		c.Env = j.env
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
	return fmt.Sprintf("%s $ %s  (exit:%d in %s)\n%s",
		filepath.Join("$GOPATH/src", relwd), cmds, exit, roundTime(duration), normalizeUTF8(out)), err == nil
}

// fetchRepo tries to fetch a repository if possible and checks out
// origin/master as master.
//
// If the fetch failed, it deletes the checkout.
func (j *jobRequest) fetchRepo(repoRel string) (string, bool) {
	stdout, ok := j.run(repoRel, nil, []string{"git", "fetch", "--quiet", "--prune", "--all"}, false)
	if !ok {
		repoPath := filepath.Join(j.gopath, "src", repoRel)
		// Give up and delete the repository. At worst "go get" will fetch
		// it below.
		if err := os.RemoveAll(repoPath); err != nil {
			// Deletion failed, that's a hard failure.
			return stdout + "<failure>\n" + err.Error() + "\n", false
		}
		return stdout + "<recovered failure>\nrm -rf " + repoPath + "\n", true
	}
	stdout2, ok2 := j.run(repoRel, nil, []string{"git", "checkout", "--quiet", "-B", "master", "origin/master"}, false)
	return stdout + stdout2, ok && ok2
}

// cloneOrFetch is meant to be used on the primary repository, making sure it
// is checked out at the expected commit or Pull Request.
func (j *jobRequest) cloneOrFetch(c chan<- setupWorkResult) {
	p := filepath.Join(j.gopath, "src", j.p.getPath())
	if _, err := os.Stat(p); err != nil && !os.IsNotExist(err) {
		c <- setupWorkResult{"<failure>\n" + err.Error() + "\n", false}
		return
	} else if err != nil {
		// Directory doesn't exist, need to clone.
		stdout, ok := j.run(filepath.Dir(j.p.getPath()), nil, []string{"git", "clone", "--quiet", j.cloneURL()}, false)
		c <- setupWorkResult{stdout, ok}
		if j.pullID == 0 || !ok {
			// For PRs, the commit has to be fetched manually.
			return
		}
	}

	// Directory exists, need to fetch.
	args := []string{"git", "fetch", "--prune", "--quiet", "origin"}
	if j.pullID != 0 {
		args = append(args, fmt.Sprintf("pull/%d/head", j.pullID))
	}
	stdout, ok := j.run(j.p.getPath(), nil, args, false)
	c <- setupWorkResult{stdout, ok}
}

func (j *jobRequest) assertDir() error {
	repoPath := filepath.Join(j.gopath, "src", j.p.getPath())
	up := filepath.Dir(repoPath)
	err := os.MkdirAll(up, 0700)
	log.Printf("MkdirAll(%q) -> %v", up, err)
	if err != nil && !os.IsExist(err) {
		return err
	}
	return nil
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
func (j *jobRequest) syncParallel(c chan<- setupWorkResult) {
	// git clone / go get will have a race condition if the directory doesn't
	// exist.
	root := filepath.Join(j.gopath, "src")
	repoPath := filepath.Join(root, j.p.getPath())
	if err := j.assertDir(); err != nil {
		c <- setupWorkResult{"<failure>\n" + err.Error() + "\n", false}
		return
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		j.cloneOrFetch(c)
	}()

	// Sync all the repositories concurrently.
	err1 := filepath.Walk(root, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if path == repoPath {
			// repoPath is handled specifically above.
			return filepath.SkipDir
		}
		if fi.Name() == ".git" {
			wg.Add(1)
			go func(p string) {
				defer wg.Done()
				stdout, ok := j.fetchRepo(p)
				c <- setupWorkResult{stdout, ok}
			}(filepath.Dir(path)[len(root)+1:])
			return filepath.SkipDir
		}
		return nil
	})

	// Remove $GOPATH/bin unconditionally. Otherwise a 'go install' may fail yet
	// the execution afterwards 'succeeds' because a stale binary was left from a
	// previous run.
	if err2 := os.RemoveAll(filepath.Join(j.gopath, "bin")); err2 == nil {
		c <- setupWorkResult{"Removed $GOPATH/bin\n", true}
	} else {
		c <- setupWorkResult{"Removed $GOPATH/bin:" + err2.Error() + "\n", false}
	}

	wg.Wait()
	if err1 != nil {
		c <- setupWorkResult{"<directory walking failure>\n" + err1.Error() + "\n", false}
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

// runChecks syncs then runs the checks and returns task's results.
//
// It aggressively concurrently fetches all repositories in `gopath` to
// accelerate the processing.
func runChecks(j *jobRequest, results chan<- gistFile) bool {
	start := time.Now()
	c := make(chan setupWorkResult)
	go func() {
		defer close(c)
		j.syncParallel(c)
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
		stdout, setupGet.success = j.run(j.p.getPath(), nil, c, false)
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
	checks := j.p.getChecks()
	nb := len(strconv.Itoa(len(checks)))
	for i, c := range checks {
		start = time.Now()
		stdout, ok2 := j.run(j.p.getPath(), c.Env, c.Cmd, true)
		results <- gistFile{fmt.Sprintf("cmd%0*d", nb, i+1), stdout, ok2, time.Since(start)}
		if !ok2 {
			// Still run the other tests.
			ok = false
		}
	}
	return ok
}
