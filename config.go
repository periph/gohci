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

// serverConfig is the configuration for the web server.
type serverConfig interface {
	getName() string
	getPort() int
	getWebHookSecret() []byte
	getProject(org, repo string) project
}

// project is the configuration for this project.
type project interface {
	getOrg() string
	getRepo() string
	// getPath returns the path to checkout the repository into. It may be
	// different than "github.com/<org>/<repo>".
	getPath() string
	isSuperUser(user string) bool
	getChecks() []check
}

// getID returns the "org/repo" identifier for a project.
func getID(p project) string {
	return p.getOrg() + "/" + p.getRepo()
}

// check is a single command to run.
type check struct {
	Cmd []string // Command to run.
	Env []string // Optional environment variables to use.
}

// Worker configuration via gohci.yml.

// projectDef defines a github repository being tested in the worker gohci.yml
// configuration file.
type projectDef struct {
	Org        string   // Organisation name (e.g. a user)
	Repo       string   // Project name
	AltPath    string   // Alternative package path to use. Defaults to the actual path.
	SuperUsers []string // List of github accounts that can trigger a run. In practice any user with write access is a super user but OAuth2 tokens with limited scopes cannot get this information.
	Checks     []check  // Commands to run to test the repository. They are run one after the other from the repository's root.
}

// getPath implements project.
func (p *projectDef) getPath() string {
	if len(p.AltPath) != 0 {
		return strings.Replace(p.AltPath, "/", string(os.PathSeparator), -1)
	}
	return filepath.Join("github.com", p.Org, p.Repo)
}

// isSuperUser returns true if the user can trigger tasks.
func (p *projectDef) isSuperUser(u string) bool {
	for _, s := range p.SuperUsers {
		if s == u {
			return true
		}
	}
	return false
}

func (p *projectDef) getOrg() string {
	return p.Org
}

func (p *projectDef) getRepo() string {
	return p.Repo
}

func (p *projectDef) getChecks() []check {
	return p.Checks
}

// workerConfig is a worker configuration.
type workerConfig struct {
	Port              int          // TCP port number for HTTP server.
	WebHookSecret     string       // https://developer.github.com/webhooks/
	Oauth2AccessToken string       // https://github.com/settings/tokens, check "repo:status" and "gist"
	Name              string       // Display name to use in the status report on Github.
	Projects          []projectDef // All the projects this workre handles.
}

// loadConfig loads the current config or returns the default one.
//
// It saves a reformatted version on disk if it was not in the canonical format.
func loadConfig(fileName string) (*workerConfig, error) {
	hostname, err := os.Hostname()
	if err != nil {
		hostname = "gohci"
	}
	// Create a dummy config file to make it easier to edit.
	c := &workerConfig{
		Port:              8080,
		WebHookSecret:     "Create a secret and set it at github.com/user/repo/settings/hooks",
		Oauth2AccessToken: "Get one at https://github.com/settings/tokens",
		Name:              hostname,
		Projects: []projectDef{
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

// getName implements serverConfig.
func (c *workerConfig) getName() string {
	return c.Name
}

// getPort implements serverConfig.
func (c *workerConfig) getPort() int {
	return c.Port
}

// getWebHookSecret implements serverConfig.
func (c *workerConfig) getWebHookSecret() []byte {
	return []byte(c.WebHookSecret)
}

// getProject implements serverConfig.
func (c *workerConfig) getProject(org, repo string) project {
	for i := range c.Projects {
		if c.Projects[i].Org == org && c.Projects[i].Repo == repo {
			return &c.Projects[i]
		}
	}
	// Allow the unconfigured project and only run go test on it, but do not
	// specify any super user.
	return &projectDef{Org: org, Repo: repo}
}

// Project configuration via ".gohci.yml".

type workerProjectConfig struct {
	Name   string  // Worker which this config belongs to.
	Checks []check // Commands to run to test the repository. They are run one after the other from the repository's root.
}

// projectConfig is a configuration file found in a project as ".gohci.yml" in
// the root directory of the repository.
type projectConfig struct {
	Version int                   // Current 1
	Workers []workerProjectConfig //
}

func loadProjectConfig(fileName string) *projectConfig {
	if b, err := ioutil.ReadFile(fileName); err == nil {
		p := &projectConfig{}
		if err = yaml.Unmarshal(b, p); err == nil && p.Version == 1 {
			return p
		}
	}
	return nil
}
