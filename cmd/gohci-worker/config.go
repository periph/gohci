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

	yaml "gopkg.in/yaml.v3"
	"periph.io/x/gohci"
)

// loadConfig loads the current config or returns the default one.
//
// It saves a reformatted version on disk if it was not in the canonical format.
func loadConfig(fileName string) (*gohci.WorkerConfig, error) {
	// Create a dummy config file to make it easier to edit.
	c := &gohci.WorkerConfig{
		Port:              8080,
		Oauth2AccessToken: "Get one at https://github.com/settings/tokens",
	}
	b, err := ioutil.ReadFile(fileName)
	if err != nil {
		return nil, rewrite(fileName, c)
	}
	if err = yaml.Unmarshal(b, c); err != nil {
		_ = rewrite(fileName, c)
		return nil, err
	}
	if c.Name == "" || c.WebHookSecret == "" {
		return nil, rewrite(fileName, c)
	}
	return c, nil
}

func rewrite(fileName string, c *gohci.WorkerConfig) error {
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

func loadProjectConfig(fileName string) *gohci.ProjectConfig {
	if b, err := ioutil.ReadFile(fileName); err == nil {
		p := &gohci.ProjectConfig{}
		if err = yaml.Unmarshal(b, p); err == nil && p.Version == 1 {
			// TODO(maruel): Validate.
			return p
		}
	}
	return nil
}
