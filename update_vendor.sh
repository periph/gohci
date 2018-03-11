#!/bin/bash
# Copyright 2016 Marc-Antoine Ruel. All rights reserved.
# Use of this source code is governed under the Apache License, Version 2.0
# that can be found in the LICENSE file.

set -eu

go get -v github.com/golang/dep
dep ensure -update

# Trim google.golang.org dependency. 
rm vendor/golang.org/x/oauth2/internal/client_appengine.go
rm -rf vendor/google.golang.org/
# TODO(maruel): Update Gopkg.lock to remove reference to appengine.

git add -f .
