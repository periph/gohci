# gohci - The Go on Hardware CI

[![Go Report Card](https://goreportcard.com/badge/github.com/periph/gohci)](https://goreportcard.com/report/github.com/periph/gohci)

- This doc:
  - [Genesis](#genesis)
  - [Pictures](#pictures)
  - [Design](#design)
  - [Features](#features)
- [Getting started and configuration](CONFIG.md): CONFIG.md
- [FAQ](FAQ.md): FAQ.md


## Genesis

All I wanted was to run `go test ./...` on a Raspberry Pi on both Pull Requests
and Pushes for a private repository. I realized that it is possible to store the
test's stdout to a [Github Gist](https://gist.github.com/) so I created a
_serverless_ CI.

The result is the distilled essence of a Continuous Integration service that
leans heavily toward testing Go projects on hardware, specifically low power
ones (Raspberry Pis, C.H.I.P., BeagleBone, ODROID, etc) but also works great on
Windows and macOS.

Part of the gohci lab testing https://periph.io:

![lab](https://raw.githubusercontent.com/wiki/periph/gohci/lab.jpg
"lab")

Here's how it looks like on a PR when the workers start to handle it:

![screen cast](https://raw.githubusercontent.com/wiki/periph/gohci/gohci.gif
"screen cast")


View of the status on commits:

![commits](https://raw.githubusercontent.com/wiki/periph/gohci/commits.png
"commits")


## Design

It hardly can get any simpler:

- Only support one specific use case: *Golang project hosted on Github*.
- There is no "server", only workers that you run yourself. Each worker must be
  internet accessible and HTTPS must be proxied down to HTTP.
  - [Caddy](https://caddyserver.com/) works great along its native
    [letsencrypt.org](https://letsencrypt.org) support.


## Features

- 100% free and open source.
  - Secure, you are in control. There's no third party service beside GitHub.
  - Enables free testing on macOS, Windows or single CPU ARM micro computer.
  - Low maintenance, run as systemd/launchd service.
- Each worker can test multiple repositories, each with custom checks.
- Each check's stdout is attached to the gist as they complete.
- The commit's status is updated "_live_" on Github. This is pretty cool to see
  in action on a GitHub PR.
- `gohci-worker` exits whenever the executable or `gohci.yml` is updated; making
  it easy to use an auto-updating mechanism.

Not convinced? Read the [FAQ.md](FAQ.md) for additional information.

Convinced? See [CONFIG.md](CONFIG.md) to get started!
