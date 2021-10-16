// Copyright 2021 Marc-Antoine Ruel. All rights reserved.
// Use of this source code is governed under the Apache License, Version 2.0
// that can be found in the LICENSE file.

// gohci-check checks a project configuration.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
	"periph.io/x/gohci"
)

func mainImpl() error {
	flag.Parse()
	f := ""
	var err error
	switch flag.NArg() {
	case 0:
		if f, err = os.Getwd(); err != nil {
			return err
		}
	case 1:
		f = flag.Args()[0]
	default:
		return errors.New("pass only one argument")
	}

	b, err := ioutil.ReadFile(filepath.Join(f, ".gohci.yml"))
	if err != nil {
		return err
	}
	p := &gohci.ProjectConfig{}
	if err = yaml.Unmarshal(b, p); err != nil {
		return err
	}
	if p.Version != 1 {
		return errors.New("wrong version")
	}
	return nil
}

func main() {
	if err := mainImpl(); err != nil {
		fmt.Fprintf(os.Stderr, "gohci-check: %s.\n", err)
		os.Exit(1)
	}
}
