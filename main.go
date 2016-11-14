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
	"bytes"
	"encoding/json"
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
	"path"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/bugsnag/osext"
	"github.com/google/go-github/github"
	"golang.org/x/oauth2"
)

type config struct {
	Port              int      // TCP port number for HTTP server.
	WebHookSecret     string   // https://developer.github.com/webhooks/
	Oauth2AccessToken string   // https://github.com/settings/tokens, check "repo:status" and "gist"
	UseSSH            bool     // Use ssh (instead of https) for checkout. Required for private repositories.
	Name              string   // Display name to use in the status report on Github.
	Check             []string // Command to run to test the repository. It is run from the repository's root.
}

func loadConfig() (*config, error) {
	c := &config{
		Port:              8080,
		WebHookSecret:     "Create a secret and set it at github.com/'name'/'repo'/settings/hooks",
		Oauth2AccessToken: "Get one at https://github.com/settings/tokens",
		UseSSH:            false,
		Name:              "sci",
		Check:             []string{"go", "test", "./..."},
	}
	b, err := ioutil.ReadFile("sci.json")
	if err != nil {
		b, err = json.MarshalIndent(c, "", "  ")
		if err != nil {
			return nil, err
		}
		if err = ioutil.WriteFile("sci.json", b, 0600); err != nil {
			return nil, err
		}
		return nil, errors.New("wrote new sci.json")
	}
	if err = json.Unmarshal(b, c); err != nil {
		return nil, err
	}
	d, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(b, d) {
		log.Printf("Updating sci.json in canonical format")
		if err := ioutil.WriteFile("sci.json", d, 0600); err != nil {
			return nil, err
		}
	}
	return c, nil
}

func run(cwd string, cmd ...string) (string, bool) {
	cmds := strings.Join(cmd, " ")
	log.Printf("- cwd=%s : %s", cwd, cmds)
	c := exec.Command(cmd[0], cmd[1:]...)
	c.Dir = cwd
	start := time.Now()
	out, err := c.CombinedOutput()
	duration := time.Since(start)
	// Assumes UTF-8.
	return fmt.Sprintf("$ %s\nin %s\n%s", cmds, duration, string(out)), err == nil
}

// runCheck syncs then runs the check and returns task's metadata and stdout.
func runCheck(cmd []string, repoName string, useSSH bool, commit, gopath string) (string, string, bool) {
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
		out1, ok := run(up, "git", "clone", "--quiet", url)
		metadata += out1
		if !ok {
			return metadata, "", ok
		}
	} else {
		out1, ok := run(base, "git", "fetch", "--prune", "--quiet")
		metadata += out1
		if !ok {
			return metadata, "", ok
		}
	}
	out1, ok := run(base, "git", "checkout", "--quiet", commit)
	metadata += out1
	if !ok {
		return metadata, "", ok
	}
	// TODO(maruel): update dependencies manually!
	out1, ok = run(base, "go", "get", "-v", "-d", "-t", "./...")
	metadata += out1
	if !ok {
		return metadata, "", ok
	}
	// Precompilation has a dramatic effect on a Raspberry Pi.
	out1, ok = run(base, "go", "test", "-i", "./...")
	metadata += out1
	if !ok {
		return metadata, "", ok
	}
	out, ok := run(base, cmd...)
	return metadata, out, ok
}

type server struct {
	c       *config
	client  *github.Client
	gopath  string
	mu      sync.Mutex
	collabs map[string]map[string]bool
}

func (s *server) canCollab(owner, repo, user string) bool {
	key := owner + "/" + repo
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.collabs[key]; !ok {
		s.collabs[key] = map[string]bool{}
	}
	if v, ok := s.collabs[key][user]; ok {
		return v
	}
	v, _, _ := s.client.Repositories.IsCollaborator(owner, repo, user)
	if v {
		// Only cache hits because otherwise adding a collaborator would mean
		// restarting every sci instances.
		s.collabs[key][user] = v
	}
	log.Printf("- %s: %s access: %t", key, user, v)
	return v
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	log.Printf("HTTP: %s %s", r.RemoteAddr, r.URL.Path)
	defer r.Body.Close()
	if r.Method != "POST" {
		http.Error(w, "Invalid method", http.StatusMethodNotAllowed)
		log.Printf("- invalid method")
		return
	}
	payload, err := github.ValidatePayload(r, []byte(s.c.WebHookSecret))
	if err != nil {
		http.Error(w, "Invalid secret", http.StatusUnauthorized)
		log.Printf("- invalid secret")
		return
	}
	if t := github.WebHookType(r); t != "ping" {
		event, err := github.ParseWebHook(t, payload)
		if err != nil {
			http.Error(w, "Invalid payload", http.StatusBadRequest)
			log.Printf("- invalid payload")
			return
		}
		// Process the rest asynchronously so the hook doesn't take too long.
		go func() {
			switch event := event.(type) {
			// TODO(maruel): For *github.CommitCommentEvent and
			// *github.IssueCommentEvent, when the comment is 'run tests' from a
			// collaborator, run the tests.
			case *github.PullRequestEvent:
				log.Printf("- PR %s #%d %s %s", *event.Repo.FullName, *event.PullRequest.ID, *event.Sender.Login, *event.Action)
				if *event.Action != "opened" && *event.Action != "synchronized" {
					log.Printf("- ignoring action %q for PR from %q", *event.Action, *event.Sender.Login)
				} else if !s.canCollab(*event.Repo.Owner.Login, *event.Repo.Name, *event.Sender.Login) {
					log.Printf("- ignoring owner %q for PR", *event.Sender.Login)
				} else if err = s.runCheck(*event.Repo.FullName, *event.PullRequest.Head.SHA); err != nil {
					log.Printf("- %v", err)
				}
			case *github.PushEvent:
				if event.HeadCommit == nil {
					log.Printf("- Push %s %s <deleted>", *event.Repo.FullName, *event.Ref)
				} else {
					log.Printf("- Push %s %s %s", *event.Repo.FullName, *event.Ref, *event.HeadCommit.ID)
					if !strings.HasPrefix(*event.Ref, "refs/heads/") {
						log.Printf("- ignoring branch %q for push", *event.Ref)
					} else if err = s.runCheck(*event.Repo.FullName, *event.HeadCommit.ID); err != nil {
						log.Printf("- %v", err)
					}
				}
			default:
				log.Printf("- ignoring hook type %s", reflect.TypeOf(event).Elem().Name())
			}
		}()
	}
	io.WriteString(w, "{}")
}

func (s *server) runCheck(repo, commit string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	log.Printf("- Running test for %s at %s", repo, commit)
	// TODO(maruel): Update the gist as the task is running;
	// https://developer.github.com/v3/gists/#edit-a-gist
	metadata, out, success := runCheck(s.c.Check, repo, s.c.UseSSH, commit, s.gopath)
	if metadata == "" {
		metadata = "<missing>"
	}
	if out == "" {
		out = "<missing>"
	}
	// https://developer.github.com/v3/gists/#create-a-gist
	gist := &github.Gist{
		Description: github.String("Output for https://github.com/" + repo + "/commit/" + commit),
		// It is still accessible via the URL;
		Public: github.Bool(false),
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
		State:       github.String("success"),
		TargetURL:   gist.HTMLURL,
		Description: github.String(fmt.Sprintf("Ran: %s", strings.Join(s.c.Check, " "))),
		Context:     github.String("sci"),
	}
	if !success {
		status.State = github.String("failure")
	}
	parts := strings.SplitN(repo, "/", 2)
	_, _, err = s.client.Repositories.CreateStatus(parts[0], parts[1], commit, status)
	return err
}

func mainImpl() error {
	test := flag.String("test", "", "runs a simulation locally, specify the git repository name (not URL) to test, e.g. 'maruel/sci'")
	commit := flag.String("commit", "HEAD", "commit ID to test and update; will only update if not 'HEAD'")
	flag.Parse()
	c, err := loadConfig()
	if err != nil {
		return err
	}
	wd, err := os.Getwd()
	if err != nil {
		return err
	}
	gopath := filepath.Join(wd, "sci-gopath")
	os.Setenv("GOPATH", gopath)
	tc := oauth2.NewClient(oauth2.NoContext, oauth2.StaticTokenSource(&oauth2.Token{AccessToken: c.Oauth2AccessToken}))
	s := server{c: c, client: github.NewClient(tc), gopath: gopath, collabs: map[string]map[string]bool{}}
	if len(*test) != 0 {
		if *commit == "HEAD" {
			// Only run locally.
			metadata, out, success := runCheck(c.Check, *test, c.UseSSH, *commit, gopath)
			_, err := fmt.Printf("%s---\n%sSuccess: %t\n", metadata, out, success)
			return err

		}
		return s.runCheck(*test, *commit)
	}
	http.Handle("/", &s)
	thisFile, err := osext.Executable()
	if err != nil {
		return err
	}
	log.Printf("Running in: %s", wd)
	log.Printf("Executable: %s", thisFile)
	log.Printf("Listening on: %d", c.Port)
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", c.Port))
	if err != nil {
		return err
	}
	server := &http.Server{Addr: ln.Addr().String()}
	go server.ListenAndServe()
	err = watchFile(thisFile)
	// Ensures no task is running.
	s.mu.Lock()
	return err
}

func main() {
	if err := mainImpl(); err != nil {
		fmt.Fprintf(os.Stderr, "sci: %s.\n", err)
		os.Exit(1)
	}
}
