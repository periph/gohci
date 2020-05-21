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

	"github.com/pbnjay/memory"
	"periph.io/x/gohci/v0"
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

// roundDuration returns rounded time with approximatively 4~5 digits.
func roundDuration(t time.Duration) time.Duration {
	// Cheezy but good enough for now.
	for r := time.Second; r > time.Microsecond; r /= 10 {
		if t >= r {
			d := r / 1000
			return (t + d/2) / d * d
		}
	}
	return t
}

// roundSize rounds a size to something reasonable.
func roundSize(t uint64) string {
	orders := []string{"bytes", "Kib", "Mib", "Gib", "Tib", "Pib", "Eib"}
	i := 0
	for ; i < len(orders)-1; i++ {
		if t/1024*1024 != t || t == 0 {
			break
		}
		t /= 1024
	}
	if t > 1024 {
		return fmt.Sprintf("%.1f%s", float64(t)/1024., orders[i+1])
	}
	return fmt.Sprintf("%d%s", t, orders[i])
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

//

// jobRequest is the details to run a verification job.
//
// It defines a github repository being tested in the worker gohci.yml
// configuration file, along the alternate path to use and the checks to run.
type jobRequest struct {
	org        string // Organisation name (e.g. a user)
	repo       string // Project name
	altPath    string // Alternative package path to use. Defaults to the github canonical path.
	commitHash string // commit hash, not a ref
	useSSH     bool   // useSSH tells to use ssh instead of https
	pullID     int    // pullID is the PR ID if relevant

	gopath string   // Cache of GOPATH
	path   string   // Cache of PATH
	env    []string // Precomputed environment variables

	checks []gohci.Check // Commands to run to test the repository. They are run one after the other from the repository's root.
}

// newJobRequest creates a new test request for project 'org/repo' on commitHash
// and/or pullID.
func newJobRequest(org, repo, altPath, commitHash string, useSSH bool, pullID int, wd string) *jobRequest {
	// Organization names cannot contain an underscore so it 'should' be fine.
	gopath := filepath.Join(wd, org+"_"+repo)
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
		org:        org,
		repo:       repo,
		altPath:    altPath,
		commitHash: commitHash,
		useSSH:     useSSH,
		pullID:     pullID,
		gopath:     gopath,
		path:       path,
		env:        env,
	}
}

func (j *jobRequest) String() string {
	if j.pullID != 0 {
		return fmt.Sprintf("https://github.com/%s/pull/%d at https://github.com/%s/commit/%s", j.getID(), j.pullID, j.getID(), j.commitHash[:12])
	}
	return fmt.Sprintf("https://github.com/%s/commit/%s", j.getID(), j.commitHash[:12])
}

// getPath returns the path to checkout the repository into. It may be
// different than "github.com/<org>/<repo>".
func (j *jobRequest) getPath() string {
	if len(j.altPath) != 0 {
		return strings.Replace(j.altPath, "/", string(os.PathSeparator), -1)
	}
	return filepath.Join("github.com", j.org, j.repo)
}

func (j *jobRequest) cloneURL() string {
	if j.useSSH {
		return "git@github.com:" + j.getID()
	}
	return "https://github.com/" + j.getID()
}

// getID returns the "org/repo" identifier for a project.
func (j *jobRequest) getID() string {
	return j.org + "/" + j.repo
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
		"Commit:  %s\nCPUs:    %d\nRAM:     %s\nVersion: %s\nGOROOT:  %s\nGOPATH:  %s\nPATH:    %s\n",
		j.commitHash, runtime.NumCPU(), roundSize(memory.TotalMemory()), runtime.Version(), runtime.GOROOT(), j.gopath, j.path)
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
		filepath.Join("$GOPATH/src", relwd), cmds, exit, roundDuration(duration), normalizeUTF8(out)), err == nil
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
	p := filepath.Join(j.gopath, "src", j.getPath())
	if _, err := os.Stat(p); err != nil && !os.IsNotExist(err) {
		c <- setupWorkResult{"<failure>\n" + err.Error() + "\n", false}
		return
	} else if err != nil {
		// Directory doesn't exist, need to clone.
		stdout, ok := j.run(filepath.Dir(j.getPath()), nil, []string{"git", "clone", "--quiet", j.cloneURL()}, false)
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
	stdout, ok := j.run(j.getPath(), nil, args, false)
	c <- setupWorkResult{stdout, ok}
}

func (j *jobRequest) assertDir() error {
	repoPath := filepath.Join(j.gopath, "src", j.getPath())
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
	repoPath := filepath.Join(root, j.getPath())
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

// sync is the first part of a job.
//
// It aggressively concurrently fetches all repositories in `gopath` to
// accelerate the processing.
func (j *jobRequest) sync() (string, bool) {
	c := make(chan setupWorkResult)
	go func() {
		defer close(c)
		j.syncParallel(c)
	}()
	out := ""
	ok := true
	for i := range c {
		out += i.content
		ok = ok && i.ok
	}
	return out, ok
}

// checkout is the second part of a job.
//
// It checkouts out the primary repository at the right commit and runs "go
// get".
func (j *jobRequest) checkout() (string, bool) {
	// Second part: checkout the right commit, run go get.
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
	out := ""
	ok := true
	for _, c := range setupCmds {
		stdout, ok2 := j.run(j.getPath(), nil, c, false)
		out += stdout
		if ok = ok && ok2; !ok {
			break
		}
	}
	return out, ok
}

// parseConfig is the third part of a job.
//
// It reads the ".gohci.yml" if there's one.
func (j *jobRequest) parseConfig(name string) ([]gohci.Check, string) {
	if p := loadProjectConfig(filepath.Join(j.gopath, "src", j.getPath(), ".gohci.yml")); p != nil {
		for _, w := range p.Workers {
			if w.Name == name {
				return w.Checks, "Using worker specific checks from the repo's .gohci.yml"
			}
		}
		for _, w := range p.Workers {
			if w.Name == "" {
				return w.Checks, "Using generic checks from the repo's .gohci.yml"
			}
		}
	}
	// Returns the default.
	return []gohci.Check{{Cmd: []string{"go", "test", "./..."}}}, "Using default check"
}

// runChecks is the fourth part of a job.
func (j *jobRequest) runChecks(checks []gohci.Check, results chan<- gistFile) bool {
	ok := true
	nb := len(strconv.Itoa(len(checks)))
	for i, c := range checks {
		start := time.Now()
		stdout, ok2 := j.run(j.getPath(), c.Env, c.Cmd, true)
		results <- gistFile{fmt.Sprintf("cmd%0*d", nb, i+1), stdout, ok2, time.Since(start)}
		// Still run the other tests.
		ok = ok && ok2
	}
	return ok
}
