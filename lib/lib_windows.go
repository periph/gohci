// Copyright 2016 Marc-Antoine Ruel. All rights reserved.
// Use of this source code is governed under the Apache License, Version 2.0
// that can be found in the LICENSE file.

package lib

import (
	"syscall"
	"unsafe"
)

// SetConsoleTitle sets the console title.
func SetConsoleTitle(title string) error {
	h, err := syscall.LoadLibrary("kernel32.dll")
	if err != nil {
		return err
	}
	defer syscall.FreeLibrary(h)
	p, err := syscall.GetProcAddress(h, "SetConsoleTitleW")
	if err != nil {
		return err
	}
	_, _, errno := syscall.Syscall(p, 1, uintptr(unsafe.Pointer(syscall.StringToUTF16Ptr(title))), 0, 0)
	return syscall.Errno(errno)
}
