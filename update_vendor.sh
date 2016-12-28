#!/bin/bash
# Copyright 2016 Marc-Antoine Ruel. All rights reserved.
# Use of this source code is governed under the Apache License, Version 2.0
# that can be found in the LICENSE file.

set -eu

go get -v github.com/govend/govend
govend -v -u -l --prune --skipTestFiles

# Trim.
rm vendor/golang.org/x/oauth2/client_appengine.go
rm -rf vendor/google.golang.org/
# Cheezy way to remove google.golang.org dependency.
grep -v -f <(grep -A1 google.golang.org vendor.yml) < vendor.yml > vendor2.yml
mv vendor2.yml vendor.yml

git add .
