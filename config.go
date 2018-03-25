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

type config struct {
	Port              int        // TCP port number for HTTP server.
	WebHookSecret     string     // https://developer.github.com/webhooks/
	Oauth2AccessToken string     // https://github.com/settings/tokens, check "repo:status" and "gist"
	Name              string     // Display name to use in the status report on Github.
	AltPath           string     // Alternative package path to use. Defaults to the actual path.
	SuperUsers        []string   // List of github accounts that can trigger a run. In practice any user with write access is a super user but OAuth2 tokens with limited scopes cannot get this information.
	Checks            [][]string // Commands to run to test the repository. They are run one after the other from the repository's root.
}

// loadConfig loads the current config or returns the default one.
//
// It saves a reformatted version on disk if it was not in the canonical format.
func loadConfig(fileName string) (*config, error) {
	hostname, err := os.Hostname()
	if err != nil {
		hostname = "gohci"
	}
	c := &config{
		Port:              8080,
		WebHookSecret:     "Create a secret and set it at github.com/user/repo/settings/hooks",
		Oauth2AccessToken: "Get one at https://github.com/settings/tokens",
		Name:              hostname,
		AltPath:           "",
		SuperUsers:        nil,
		Checks:            nil,
	}
	b, err := ioutil.ReadFile(fileName)
	if err != nil {
		b, err = yaml.Marshal(c)
		if err != nil {
			return nil, err
		}
		if len(c.Checks) == 0 {
			c.Checks = [][]string{{"go", "test", "./..."}}
		}
		if err = ioutil.WriteFile(fileName, b, 0600); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("wrote new %s", fileName)
	}
	if err = yaml.Unmarshal(b, c); err != nil {
		return nil, err
	}
	if len(c.Checks) == 0 {
		c.Checks = [][]string{{"go", "test", "./..."}}
	}
	return c, nil
}

// isSuperUser returns true if the user can trigger tasks.
func (c *config) isSuperUser(u string) bool {
	for _, s := range c.SuperUsers {
		if s == u {
			return true
		}
	}
	// s.client.Repositories.IsCollaborator() requires *write* access to the
	// repository, which we really do not want here. So don't even try for now.
	return false
}

// cmds returns the list of commands to attach to the metadata gist as a single
// indented string.
func (c *config) cmds() string {
	cmds := ""
	for i, cmd := range c.Checks {
		if i != 0 {
			cmds += "\n"
		}
		cmds += "  " + strings.Join(cmd, " ")
	}
	return cmds
}
