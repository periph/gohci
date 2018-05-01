// Copyright 2018 Marc-Antoine Ruel. All rights reserved.
// Use of this source code is governed under the Apache License, Version 2.0
// that can be found in the LICENSE file.

package main

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io/ioutil"
	"os"
	"runtime"

	yaml "gopkg.in/yaml.v2"
)

// serverConfig is the configuration for the web server.
type serverConfig interface {
	getName() string
	getPort() int
	getWebHookSecret() []byte
}

// workerConfig is a worker configuration.
type workerConfig struct {
	Port              int    // TCP port number for HTTP server.
	WebHookSecret     string // https://developer.github.com/webhooks/
	Oauth2AccessToken string // https://github.com/settings/tokens, check "repo:status" and "gist"
	Name              string // Display name to use in the status report on Github. Defaults to the machine hostname
}

// loadConfig loads the current config or returns the default one.
//
// It saves a reformatted version on disk if it was not in the canonical format.
func loadConfig(fileName string) (*workerConfig, error) {
	// Create a dummy config file to make it easier to edit.
	c := &workerConfig{
		Port:              8080,
		Oauth2AccessToken: "Get one at https://github.com/settings/tokens",
	}
	b, err := ioutil.ReadFile(fileName)
	if err != nil {
		return nil, rewrite(fileName, c)
	}
	if err = yaml.Unmarshal(b, c); err != nil {
		rewrite(fileName, c)
		return nil, err
	}
	if c.Name == "" || c.WebHookSecret == "" {
		return nil, rewrite(fileName, c)
	}
	return c, nil
}

func rewrite(fileName string, c *workerConfig) error {
	// Defer these since they require actual work.
	if c.WebHookSecret == "" {
		var b [32]byte
		if _, err := rand.Read(b[:]); err != nil {
			return err
		}
		c.WebHookSecret = base64.RawURLEncoding.EncodeToString(b[:])
	}
	if c.Name == "" {
		if c.Name, _ = os.Hostname(); c.Name == "" {
			c.Name = "gohci"
		}
	}
	b, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	// Makes it editable in notepad.exe.
	if runtime.GOOS == "windows" {
		b = bytes.Replace(b, []byte("\n"), []byte("\r\n"), -1)
	}
	if err = ioutil.WriteFile(fileName, b, 0600); err != nil {
		return err
	}
	return fmt.Errorf("wrote new %s", fileName)
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
