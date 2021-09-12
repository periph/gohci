// Copyright 2016 Marc-Antoine Ruel. All rights reserved.
// Use of this source code is governed under the Apache License, Version 2.0
// that can be found in the LICENSE file.

//go:build !windows
// +build !windows

package main

// SetConsoleTitle sets the console title.
func SetConsoleTitle(title string) error {
	// On other OSes, using systemd so it's not useful to print out escape codes.
	return nil
	//_, err := io.WriteString(os.Stdout, "\x1b]2;"+title+"\x07")
	//return err
}
