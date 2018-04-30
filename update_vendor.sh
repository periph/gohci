#!/bin/bash
# Copyright 2016 Marc-Antoine Ruel. All rights reserved.
# Use of this source code is governed under the Apache License, Version 2.0
# that can be found in the LICENSE file.

set -eu

go get -v github.com/golang/dep
dep init

# Trim google.golang.org dependency. 
rm vendor/golang.org/x/oauth2/internal/client_appengine.go

git add -f .
