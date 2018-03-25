// Copyright 2018 Marc-Antoine Ruel. All rights reserved.
// Use of this source code is governed under the Apache License, Version 2.0
// that can be found in the LICENSE file.

package main

import (
	"fmt"
	"io/ioutil"
	"os"
	"strings"

	yaml "gopkg.in/yaml.v1"
)

type project struct {
	Org        string     // Organisation name (e.g. a user)
	Repo       string     // Project name
	AltPath    string     // Alternative package path to use. Defaults to the actual path.
	SuperUsers []string   // List of github accounts that can trigger a run. In practice any user with write access is a super user but OAuth2 tokens with limited scopes cannot get this information.
	Checks     [][]string // Commands to run to test the repository. They are run one after the other from the repository's root.
}

func (p *project) name() string {
	return p.Org + "/" + p.Repo
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
	for i, cmd := range p.Checks {
		if i != 0 {
			cmds += "\n"
		}
		cmds += "  " + strings.Join(cmd, " ")
	}
	return cmds
}

type config struct {
	Port              int       // TCP port number for HTTP server.
	WebHookSecret     string    // https://developer.github.com/webhooks/
	Oauth2AccessToken string    // https://github.com/settings/tokens, check "repo:status" and "gist"
	Name              string    // Display name to use in the status report on Github.
	Projects          []project // All the projects this workre handles.

	// Old style.
	AltPath    string     // Alternative package path to use. Defaults to the actual path.
	SuperUsers []string   // List of github accounts that can trigger a run. In practice any user with write access is a super user but OAuth2 tokens with limited scopes cannot get this information.
	Checks     [][]string // Commands to run to test the repository. They are run one after the other from the repository's root.
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
				Checks: [][]string{
					{"go", "test", "./..."},
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
	// Old style.
	return &project{
		Org:        org,
		Repo:       repo,
		AltPath:    c.AltPath,
		SuperUsers: c.SuperUsers,
		Checks:     c.Checks,
	}
}
