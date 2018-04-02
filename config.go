// Copyright 2018 Marc-Antoine Ruel. All rights reserved.
// Use of this source code is governed under the Apache License, Version 2.0
// that can be found in the LICENSE file.

package main

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	yaml "gopkg.in/yaml.v2"
)

// check is a single command to run.
type check struct {
	Cmd []string // Command to run.
	Env []string // Optional environment variables to use.
}

// project defines a github repository being tested.
type project struct {
	Org        string   // Organisation name (e.g. a user)
	Repo       string   // Project name
	AltPath    string   // Alternative package path to use. Defaults to the actual path.
	SuperUsers []string // List of github accounts that can trigger a run. In practice any user with write access is a super user but OAuth2 tokens with limited scopes cannot get this information.
	Checks     []check  // Commands to run to test the repository. They are run one after the other from the repository's root.
}

// name returns the "org/repo" identifier.
func (p *project) name() string {
	return p.Org + "/" + p.Repo
}

// path returns the path to checkout the repository into.
func (p *project) path() string {
	if len(p.AltPath) != 0 {
		return strings.Replace(p.AltPath, "/", string(os.PathSeparator), -1)
	}
	return filepath.Join("github.com/" + strings.Replace(p.name(), "/", string(os.PathSeparator), -1))
}

// isSuperUser returns true if the user can trigger tasks.
func (p *project) isSuperUser(u string) bool {
	for _, s := range p.SuperUsers {
		if s == u {
			return true
		}
	}
	return false
}

// cmds returns the list of commands to attach to the metadata gist as a single
// indented string.
func (p *project) cmds() string {
	cmds := ""
	for i, c := range p.Checks {
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

type config struct {
	Port              int       // TCP port number for HTTP server.
	WebHookSecret     string    // https://developer.github.com/webhooks/
	Oauth2AccessToken string    // https://github.com/settings/tokens, check "repo:status" and "gist"
	Name              string    // Display name to use in the status report on Github.
	Projects          []project // All the projects this workre handles.
}

// loadConfig loads the current config or returns the default one.
//
// It saves a reformatted version on disk if it was not in the canonical format.
func loadConfig(fileName string) (*config, error) {
	hostname, err := os.Hostname()
	if err != nil {
		hostname = "gohci"
	}
	// Create a dummy config file to make it easier to edit.
	c := &config{
		Port:              8080,
		WebHookSecret:     "Create a secret and set it at github.com/user/repo/settings/hooks",
		Oauth2AccessToken: "Get one at https://github.com/settings/tokens",
		Name:              hostname,
		Projects: []project{
			{
				Org:     "the user",
				Repo:    "the project",
				AltPath: "",
				// Since github user names cannot have a space in it, we know it
				// doesn't open a security hole.
				SuperUsers: []string{"admin user1", "admin user2"},
				Checks: []check{
					{Cmd: []string{"go", "test", "./..."}},
				},
			},
		},
	}
	b, err := ioutil.ReadFile(fileName)
	if err != nil {
		b, err = yaml.Marshal(c)
		if err != nil {
			return nil, err
		}
		// Makes it editable in notepad.exe.
		if runtime.GOOS == "windows" {
			b = bytes.Replace(b, []byte("\n"), []byte("\r\n"), -1)
		}
		if err = ioutil.WriteFile(fileName, b, 0600); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("wrote new %s", fileName)
	}
	if err = yaml.Unmarshal(b, c); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *config) getProject(org, repo string) *project {
	for i := range c.Projects {
		if c.Projects[i].Org == org && c.Projects[i].Repo == repo {
			return &c.Projects[i]
		}
	}
	// Allow the unconfigured project and only run go test on it, but do not
	// specify any super user.
	return &project{
		Org:  org,
		Repo: repo,
		Checks: []check{
			{Cmd: []string{"go", "test", "./..."}},
		},
	}
}
