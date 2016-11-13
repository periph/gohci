// Copyright 2016 Marc-Antoine Ruel. All rights reserved.
// Use of this source code is governed under the Apache License, Version 2.0
// that can be found in the LICENSE file.

// sci is a shameful CI.
//
// It is a simple Github webhook that runs a Go build and an hardcoded
// command upon PR or push from whitelisted users.
//
// It posts the stdout to a Github gist and updates the PR status.
//
// It doesn't stream data so it cannot be used for slow task.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"

	"github.com/google/go-github/github"
)

type config struct {
	Port          int
	WebHookSecret string // https://developer.github.com/webhooks/
	AccessToken   string // https://github.com/settings/tokens, check "repo:status" and "gist"
	UseSSH        bool
	Owners        []string
	Branches      []string
	Repo          string
	Name          string // Name to use in the status.
	Check         []string
	GOPATH        string
}

func loadConfig() (*config, error) {
	c := &config{
		Port:          8080,
		WebHookSecret: "secret",
		AccessToken:   "token",
		Owners:        []string{"joe"},
		Branches:      []string{"refs/heads/master"},
		Repo:          "github.com/example/todo",
		Name:          "sci",
		Check:         []string{"go", "test"},
		GOPATH:        "sci-tmp",
	}
	f, err := os.Open("sci.json")
	if err != nil {
		// Write a default file and exit.
		f, err := os.Create("sci.json")
		if err != nil {
			return nil, err
		}
		defer f.Close()
		b, _ := json.MarshalIndent(c, "", "  ")
		if _, err := f.Write(b); err != nil {
			return nil, err
		}
		return nil, errors.New("wrote new sci.json")
	}
	defer f.Close()
	if err := json.NewDecoder(f).Decode(c); err != nil {
		return nil, err
	}
	if c.GOPATH, err = filepath.Abs(c.GOPATH); err != nil {
		return nil, err
	}
	return c, nil
}

func isInList(i string, list []string) bool {
	// Brute force is fine for short list.
	for _, s := range list {
		if i == s {
			return true
		}
	}
	return false
}

func newBool(b bool) *bool {
	return &b
}

func newString(s string) *string {
	return &s
}

func run(cwd string, env []string, cmd ...string) (string, bool) {
	cmds := strings.Join(cmd, " ")
	log.Printf("- cwd=%s : %s", cwd, cmds)
	c := exec.Command(cmd[0], cmd[1:]...)
	if cwd != "" {
		c.Dir = cwd
	}
	start := time.Now()
	out, err := c.CombinedOutput()
	duration := time.Since(start)
	// Assumes UTF-8.
	return fmt.Sprintf("$ %s\nin %s\n%s", cmds, duration, string(out)), err == nil
}

// runC runs the check.
//
// It runs the check in a temporary GOPATH at the specified commit.
func runC(env, cmd []string, repoName string, useSSH bool, commit, gopath string) (string, string, bool) {
	metadata := fmt.Sprintf("Commit: %s\nVersion: %s\nGOROOT: %s\nGOPATH: %s\nCPUs: %d\n---\n",
		commit, runtime.Version(), runtime.GOROOT(), gopath, runtime.NumCPU())
	repoPath := "github.com/" + repoName
	base := filepath.Join(gopath, "src", repoPath)
	if _, err := os.Stat(base); err != nil {
		up := path.Dir(base)
		if err := os.MkdirAll(up, 0700); err != nil && !os.IsExist(err) {
			log.Printf("- %v", err)
		}
		url := "https://" + repoPath
		if useSSH {
			url = "git@github.com:" + repoName
		}
		out1, ok := run(up, env, "git", "clone", "--quiet", url)
		metadata += out1
		if !ok {
			return metadata, "", ok
		}
	} else {
		out1, ok := run(base, env, "git", "fetch", "--prune", "--quiet")
		metadata += out1
		if !ok {
			return metadata, "", ok
		}
	}
	out1, ok := run(base, env, "git", "checkout", "--quiet", commit)
	metadata += out1
	if !ok {
		return metadata, "", ok
	}
	// TODO(maruel): update dependencies manually.
	out1, ok = run(gopath, env, "go", "get", "-v", "-d", repoPath)
	metadata += out1
	if !ok {
		return metadata, "", ok
	}
	out, ok := run(base, env, cmd...)
	return metadata, out, ok
}

type server struct {
	c      *config
	client *github.Client
	env    []string
	mu     sync.Mutex
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	if r.Method != "POST" {
		http.Error(w, "Invalid method", http.StatusMethodNotAllowed)
		log.Printf("Invalid method")
		return
	}
	payload, err := github.ValidatePayload(r, []byte(s.c.WebHookSecret))
	if err != nil {
		http.Error(w, "Invalid secret", http.StatusUnauthorized)
		log.Printf("Invalid secret")
		return
	}
	event, err := github.ParseWebHook(github.WebHookType(r), payload)
	if err != nil {
		http.Error(w, "Invalid payload", http.StatusBadRequest)
		log.Printf("Invalid payload")
		return
	}
	log.Printf("%s", reflect.TypeOf(event).Elem().Name())
	switch event := event.(type) {
	case *github.PullRequestEvent:
		if *event.Action != "opened" && *event.Action != "synchronized" {
			log.Printf("- ignoring action %q for PR from %q", *event.Action, *event.Sender.Login)
			io.WriteString(w, "{}")
			return
		}
		if !isInList(*event.Sender.Login, s.c.Owners) {
			log.Printf("- ignoring owner %q for PR", *event.Sender.Login)
			io.WriteString(w, "{}")
			return
		}
		if err := s.runCheck(*event.Repo.FullName, *event.PullRequest.Head.SHA); err != nil {
			log.Printf("- %v")
		}
	case *github.PushEvent:
		if !isInList(*event.Ref, s.c.Branches) {
			log.Printf("- ignoring branch %q for push", *event.Ref)
			io.WriteString(w, "{}")
			return
		}
		if err := s.runCheck(*event.Repo.FullName, *event.HeadCommit.ID); err != nil {
			log.Printf("- %v")
		}
	default:
		log.Printf("Invalid payload")
	}
	io.WriteString(w, "{}")
}

func (s *server) runCheck(repo, commit string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	log.Printf("- Running test for %s at %s", repo, commit)
	metadata, out, success := runC(s.env, s.c.Check, repo, s.c.UseSSH, commit, s.c.GOPATH)

	// https://developer.github.com/v3/gists/#create-a-gist
	gist := &github.Gist{
		Description: newString("output for https://github.com/" + repo + "/commit/" + commit),
		// It is still accessible via the URL;
		Public: newBool(false),
		Files: map[github.GistFilename]github.GistFile{
			"metadata": github.GistFile{Content: &metadata},
			"stdout":   github.GistFile{Content: &out},
		},
	}
	var err error
	if gist, _, err = s.client.Gists.Create(gist); err != nil {
		return err
	}
	log.Printf("- Gist at %s", *gist.HTMLURL)
	// https://developer.github.com/v3/repos/statuses/#create-a-status
	status := &github.RepoStatus{
		State:       newString("success"),
		TargetURL:   gist.HTMLURL,
		Description: newString("ran test"),
		Context:     newString("sci"),
	}
	if !success {
		status.State = newString("failure")
	}
	parts := strings.SplitN(repo, "/", 2)
	status, _, err = s.client.Repositories.CreateStatus(parts[0], parts[1], commit, status)
	return err
}

func mainImpl() error {
	fake := flag.Bool("fake", false, "fake an event for testing")
	flag.Parse()
	c, err := loadConfig()
	if err != nil {
		return err
	}
	env := os.Environ()
	for i, s := range env {
		if strings.HasPrefix(s, "GOPATH=") {
			env[i] = "GOPATH=" + c.GOPATH
		}
	}
	if err := os.Mkdir(c.GOPATH, 0700); err != nil && !os.IsExist(err) {
		return err
	}

	if *fake {
		metadata, out, success := runC(env, c.Check, c.Repo, c.UseSSH, "HEAD", c.GOPATH)
		io.WriteString(os.Stdout, metadata)
		io.WriteString(os.Stdout, "---\n")
		io.WriteString(os.Stdout, out)
		fmt.Printf("Success: %t\n", success)
		return nil
	}

	tc := oauth2.NewClient(oauth2.NoContext, oauth2.StaticTokenSource(&oauth2.Token{AccessToken: c.AccessToken}))
	client := github.NewClient(tc)
	s := server{c: c, client: client, env: env}
	http.Handle("/", &s)
	log.Printf("Listening on %d", c.Port)
	return http.ListenAndServe(fmt.Sprintf(":%d", c.Port), nil)
}

func main() {
	if err := mainImpl(); err != nil {
		fmt.Fprintf(os.Stderr, "sci: %s.\n", err)
		os.Exit(1)
	}
}
