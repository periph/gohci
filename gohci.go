// Copyright 2018 Marc-Antoine Ruel. All rights reserved.
// Use of this source code is governed under the Apache License, Version 2.0
// that can be found in the LICENSE file.

// Package gohci defines the configuration schemas for 'gohci.yml' and
// '.gohci.yml'.
//
// '.gohci.yml' is found in the repository and defines the checks to run.
//
// 'gohci.yml' is found on the worker itself and defines the http port, webhook
// secret and OAuth2 access token.
package gohci

// WorkerConfig is a worker configuration.
//
// It is found as `gohci.yml` in the gohci-worker working directory.
type WorkerConfig struct {
	// TCP port number for the HTTP server.
	Port int
	// WebHookSecret is the shared secret that keeps people on the internet from
	// running tasks on your worker.
	//
	// gohci-worker generates a good secret by default.
	//
	// See https://developer.github.com/webhooks/ for more information.
	WebHookSecret string
	// Oauth2AccessToken is the OAuth2 Access Token to be able to create gist and
	// update commit status.
	//
	// https://github.com/settings/tokens, check "repo:status" and "gist"
	Oauth2AccessToken string
	// Display name to use in the status report on Github.
	//
	// Defaults to the machine hostname.
	Name string
}

// Check is a single command to run.
type Check struct {
	Cmd []string // Command to run.
	Env []string // Optional environment variables to use.
	Dir string   // Directory to run from. Defaults to the root of the checkout.
}

// ProjectWorkerConfig is the project configuration via ".gohci.yml" for a
// specific worker.
type ProjectWorkerConfig struct {
	// Name is the worker which this config belongs to.
	//
	// If empty, this is the default configuration to use.
	Name string
	// Checks are the commands to run to test the repository. They are run one
	// after the other from the repository's root.
	Checks []Check
}

// ProjectConfig is a configuration file found in a project as ".gohci.yml" in
// the root directory of the repository.
type ProjectConfig struct {
	Version int                   // Current 1
	Workers []ProjectWorkerConfig //
}
