// Copyright 2018 Marc-Antoine Ruel. All rights reserved.
// Use of this source code is governed under the Apache License, Version 2.0
// that can be found in the LICENSE file.

package main

import (
	"testing"
	"time"
)

func TestRoundDuration(t *testing.T) {
	data := []struct {
		in       time.Duration
		expected string
	}{
		{time.Minute, "1m0s"},
		{time.Second, "1s"},
		{0, "0s"},
		{time.Nanosecond, "1ns"},
		{123450 * time.Nanosecond, "123.5µs"},
		{1234500 * time.Nanosecond, "1.235ms"},
		{12345000 * time.Nanosecond, "12.35ms"},
		{123450000 * time.Nanosecond, "123.5ms"},
		{1234499999 * time.Nanosecond, "1.234s"},
		{1234500000 * time.Nanosecond, "1.235s"},
		{12345000000 * time.Nanosecond, "12.345s"},
	}
	for _, l := range data {
		if s := roundDuration(l.in).String(); s != l.expected {
			t.Fatalf("roundDuration(%s) = %s; not %s", l.in, s, l.expected)
		}
	}
}

func TestRoundSize(t *testing.T) {
	data := []struct {
		in       uint64
		expected string
	}{
		{0, "0bytes"},
		{1, "1bytes"},
		{10, "10bytes"},
		{1023, "1023bytes"},
		{1024, "1Kib"},
		{1025, "1.0Kib"},
		{1024 * 1024, "1Mib"},
		{1024 * 1024 * 1024, "1Gib"},
		{1024*1024*1024 - 1, "1048576.0Kib"},
		{1024*1024*1024 + 1, "1048576.0Kib"},
		{1024 * 1024 * 1024 * 1024, "1Tib"},
		{1024 * 1024 * 1024 * 1024 * 1024, "1Pib"},
		{1024 * 1024 * 1024 * 1024 * 1024 * 1024, "1Eib"},
		{10 * 1024 * 1024 * 1024 * 1024 * 1024 * 1024, "10Eib"},
	}
	for _, l := range data {
		if s := roundSize(l.in); s != l.expected {
			t.Fatalf("roundSize(%d) = %s; not %s", l.in, s, l.expected)
		}
	}
}
