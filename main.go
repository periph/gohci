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
	"errors"
	"flag"
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
// is checked out at the expected commit or Pull Request.
func (f *fetchDetails) cloneOrFetch(c chan<- setupWorkResult) {
	if _, err := os.Stat(f.repoPath); err != nil && !os.IsNotExist(err) {
		c <- setupWorkResult{"<failure>\n" + err.Error() + "\n", false}
		return
	} else if err != nil {
		// Directory doesn't exist, need to clone.
		stdout, ok := run(filepath.Dir(f.repoPath), "git", "clone", "--quiet", f.cloneURL)
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
	stdout, ok := run(f.repoPath, args...)
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

// runLocal runs the checks run.
func runLocal(w *worker, gopath, commitHash, test string, update, useSSH bool) error {
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
		success := runChecks(w.c.Checks, j, w.c.AltPath, gopath, results)
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

func mainImpl() error {
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
	// local GOPATH. This is safer as this doesn't modify the host environment.
	os.Setenv("GOPATH", gopath)
	os.Setenv("PATH", filepath.Join(gopath, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"))
	w := newWorker(c, gopath)
	if len(*test) != 0 {
		return runLocal(w, gopath, *commit, *test, *update, *useSSH)
	}
	return runServer(c, w, wd, fileName)
}

func main() {
	if err := mainImpl(); err != nil {
		fmt.Fprintf(os.Stderr, "gohci: %s.\n", err)
		os.Exit(1)
	}
}
