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
	"periph.io/x/gohci"
)

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
		_ = os.Setenv("PATH", path)
		// Restore PATH.
		defer func() {
			_ = os.Setenv("PATH", oldpath)
		}()
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
	if commitHash != "" {
		env = append(env, "GIT_SHA="+commitHash)
	}

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

// findCommitHash tries to get the HEAD commit for the PR # or default branch.
func (j *jobRequest) findCommitHash() bool {
	if err := j.assertDir(); err != nil {
		return false
	}
	stdout, ok := j.run("", nil, []string{"git", "ls-remote", j.cloneURL()}, false)
	if !ok {
		log.Printf("  git ls-remote failed:\n%s", stdout)
		return false
	}
	p := "HEAD"
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
func (j *jobRequest) run(relwd string, env, cmd []string, pathOverride bool) (string, bool) {
	// Keep a copy of the one off environment variables, as we'll print them
	// later.
	dbg := strings.Join(env, " ")

	// Setup the environment variables.
	if len(env) != 0 {
		// TODO(maruel): Remove previous existing definition.
		env = append(append([]string(nil), j.env...), env...)
	} else {
		env = j.env
	}

	// Evaluate environment variables.
	cmd = append([]string(nil), cmd...)
	for i := range cmd {
		cmd[i] = os.Expand(cmd[i], func(key string) string {
			key += "="
			for _, e := range env {
				if strings.HasPrefix(e, key) {
					return e[len(key):]
				}
			}
			return ""
		})
	}
	// Log the final command.
	if len(dbg) != 0 {
		dbg += " "
	}
	dbg += strings.Join(cmd, " ")
	log.Printf("- relwd=%s : %s", relwd, dbg)

	var c *exec.Cmd
	if pathOverride {
		c = getCmd(j.path, cmd)
	} else {
		c = getCmd("", cmd)
	}
	c.Env = env
	c.Dir = filepath.Join(j.gopath, relwd)
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
		filepath.Join("$GOPATH/src", relwd), dbg, exit, roundDuration(duration), normalizeUTF8(out)), err == nil
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

// checkout is the first part of a job.
//
// It checkouts out the primary repository at the right commit.
func (j *jobRequest) checkout() (string, bool) {
	sha := j.commitHash
	if j.pullID != 0 {
		sha = fmt.Sprintf("pull/%d/head", j.pullID)
	}
	p := filepath.Join("src", j.getPath())
	if err := os.MkdirAll(filepath.Join(j.gopath, p), 0o700); err != nil {
		return err.Error(), false
	}
	// There's a trick to checkout a single exact commit which works on older git
	// clients.
	setupCmds := [][]string{
		{"git", "init", "--quiet"},
		{"git", "remote", "add", "origin", j.cloneURL()},
		{"git", "fetch", "--quiet", "--depth", "1", "origin", sha},
		{"git", "checkout", "--quiet", "FETCH_HEAD"},
	}
	out := ""
	ok := true
	for _, c := range setupCmds {
		stdout, ok2 := j.run(p, nil, c, false)
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
		d := filepath.Join("src", j.getPath())
		if c.Dir != "" {
			// TODO(maruel): Make sure it's still within the workspace. Including
			// symlinks. That said we can't do miracles without a proper namespace.
			d = filepath.Join(d, c.Dir)
		}
		stdout, ok2 := j.run(d, c.Env, c.Cmd, true)
		results <- gistFile{fmt.Sprintf("cmd%0*d", nb, i+1), stdout, ok2, time.Since(start)}
		// Still run the other tests.
		ok = ok && ok2
	}
	return ok
}

// cleanup is both the first and the last part of a job.
func (j *jobRequest) cleanup(name string, results chan<- gistFile) bool {
	start := time.Now()
	out := ""
	ok := true
	for _, x := range []string{"bin", "src"} {
		p := filepath.Join(j.gopath, x)
		if _, err := os.Stat(p); os.IsNotExist(err) {
			// Nothing was checked out, skip silently.
			continue
		}
		if err := os.RemoveAll(p); err != nil {
			out += err.Error() + "\n"
			ok = false
		} else {
			out += "Removed " + x + "\n"
		}
	}
	if out != "" {
		results <- gistFile{name, out, ok, time.Since(start)}
	}
	return ok
}
