// Copyright 2016 Marc-Antoine Ruel. All rights reserved.
// Use of this source code is governed under the Apache License, Version 2.0
// that can be found in the LICENSE file.

package main

import (
	"os"
	"time"

	fsnotify "gopkg.in/fsnotify.v1"
)

func watchFiles(paths ...string) error {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer w.Close()
	m := map[string]time.Time{}
	for _, p := range paths {
		fi, err := os.Stat(p)
		if err != nil {
			return err
		}
		m[p] = fi.ModTime()
		if err = w.Add(p); err != nil {
			return err
		}
	}
	for {
		select {
		case err = <-w.Errors:
			return err
		case n := <-w.Events:
			if fi, err := os.Stat(n.Name); err != nil || !fi.ModTime().Equal(m[n.Name]) {
				return err
			}
		}
	}
}
