# gohci - The Go on Hardware CI

[![Go Report Card](https://goreportcard.com/badge/github.com/periph/gohci)](https://goreportcard.com/report/github.com/periph/gohci)

*   [Genesis](#genesis)
*   [Pictures](#pictures)
*   [Design](#design)
*   [Features](#features)
*   [Installation](#installation)
*   [Private repository](#private-repository)
*   [Testing](#testing)
*   [Security](#security)
*   [FAQ](#faq)


## Genesis

All I wanted was to run `go test ./...` on a Raspberry Pi on both Pull Requests
and Pushes for a private repository. I realized that it is possible to store the
test's stdout to a [Github Gist](https://gist.github.com/) so I created a
_serverless_ CI.

The result is the distilled essence of a Continuous Integration service that
leans heavily toward testing Go projects on hardware, specifically low power
ones (Raspberry Pis, C.H.I.P., BeagleBone, ODROID, etc).


## Pictures

A gohci lab testing a Go project that interacts with a board:

![lab](https://raw.githubusercontent.com/wiki/periph/gohci/lab.jpg
"lab")

Not pictured above is the Windows 10 VM that ensures the code also compiles on
other platforms.


Here's how it looks like on a PR when the workers start to handle it:

![screen cast](https://raw.githubusercontent.com/wiki/periph/gohci/gohci.gif
"screen cast")


View of the status on commits:

![commits](https://raw.githubusercontent.com/wiki/periph/gohci/commits.png
"commits")


## Design

It hardly can get any simpler:

- Only support one specific use case: *Golang project hosted on Github*.
- There is no server, only a worker. The worker must be internet accessible and
  HTTPS must be proxied down to HTTP.
  - [Caddy](https://caddyserver.com/) works great along its native
    [letsencrypt.org](https://letsencrypt.org) support.
- The worker has a configuration file that determines what command it runs to
  test the project.
  - By default it is `go test ./...`. There's no configuration file in the
    repository itself.


## Features

- Each check's stdout is "_streamed_" to the gist as they complete.
- The commit's status is updated "_live_" on Github. This is pretty cool to see
  in action on a github PR.
- Trivial to run as a low maintenance systemd service.
- Designed to work great on a single core ARM CPU with minimal memory
  requirements.
- `gohci` exits whenever the executable or `gohci.yml` is updated; making it
  easy to use an auto-updating mechanism.
- Works on Windows.


## Installation

**`gohci` requires Go 1.8**

Install and create the default `gohci.yml`:

```
go get periph.io/x/gohci
gohci
```

It will look like this, with comments added here:

```
# The TCP port the HTTP server should listen on. It needs to be "frontend" by an
# HTTPS enabled proxy, like caddyserver.com
port: 8080
# The github webhook secret when receiving events:
webhooksecret: Create a secret and set it at github.com/user/repo/settings/hooks
# The github oauth2 client token when updating status and gist:
oauth2accesstoken: Get one at https://github.com/settings/tokens
# Name of the worker as presented on the status:
name: raspberrypi
# A single worker can run tests for multiple projects.
projects:
# Define one project per repository.
- org: user_name
  repo: repo_name
  # Alternative path to use to checkout the git repository, can be an
  # alternative name like "golang.org/x/tools".
  alt_path: ""
  # Users that can trigger a job on any commit by commenting "gohci: run".
  superusers:
  - maintainer1
  - maintainer2
  # Commands to run:
  checks:
  - cmd:
    - go
    - test
    - -race
    - ./...
    env:
    - CGO_ENABLED=0
  - cmd:
    - go
    - vet
    - -unsafeptr=false
    - ./...
    env: []
```

Edit based on your needs. Run `gohci` again and it will start a web server. When
`gohci` is running, updating `gohci.yml` will make the process quit. It is
assumed that you use a service manager, like systemd or a bash/batch file that
continuously restart the service.


### OAuth2 token

It is preferable to create a bot account any not use your personal account. For
example, all projects for [periph.io](https://periph.io) are tested with the
account [github.com/gohci-bot](https://github.com/gohci-bot).

- Visit https://github.com/settings/tokens while logged in with your bot
  account.
- Click `Personal access tokens` near the bottom in the left list
- Click `Generate new token`
- Add a description like `gohci`
- Check `gist` and `repo:status`
  - Do not give any write access to this token!
- Click `Generate token`
- Put the hex string into `AccessToken` in `gohci.yml`. This is needed to
  create the gists and put success/failure status on the Pull Requests.


### Bot access

The bot must have access to set a [commit
status](https://help.github.com/articles/about-statuses/).

- As your normal account, add the bot as a 'Write' collaborator.
  - Sadly 'Write' access is needed even for just status update.
- Login as the bot account on github and accept the invitation.


### Webhook

Visit to `github.com/user/repo/settings/hooks` and create a new webhook.

- Use your worker IP address or hostname as the hook URL,
  `https://1.2.3.4/github/repoA`.
- Type a random string, that you will put in `WebHookSecret` in `gohci.yml`.
- Click `Let me select individual events` and check: `Commit comment`, `Issue
  Comment`, `Pull request`, `Pull request review`, `Pull request review comment`
  and `Push`.


### systemd: Running automatically and auto-update

Setting up as systemd means it'll run automatically. The following is
preconfigured for a `pi` user. Edit as necessary, which is necessary if you run
your own Go version instead of the debian package.

As root:

```
cp systemd/* /etc/systemd/system
systemctl daemon-reload
systemctl enable gohci.service
systemctl enable gohci_update.timer
systemctl start gohci.service
systemctl start gohci_update.timer
```

`gohci_update.timer` runs `gohci_update.service` every 10 minutes.


### Windows


`gohci` works on Windows!


#### Windows: Running automatically

- First enable auto-login on a fresh low privilege account.
- Create a batch file `%APPDATA%\Microsoft\Windows\Start
  Menu\Programs\Startup\gohci.bat` that contains the following:

```
@echo off
cd c:\path\to\work\dir
:loop
gohci
goto loop
```

#### Windows: Auto update

Auto-update can be done via the task scheduler. The following command will
auto-update `gohci` every 10 minutes:

```
schtasks /create /tn "Update gohci" /tr "go get -v -u periph.io/x/gohci" /sc minute /mo 10
```

The task should show up with: `schtasks /query /fo table | more` or navigating
the GUI with `taskschd.msc`.


### OSX

Create a `gohci` standard account and set it to auto-login upon boot. Use the
included `gohci.yml` to set it to automatically start upon login:

```
mkdir -p ~/Library/LaunchAgents
cp osx/gohci.plist ~/Library/LaunchAgents
mkdir gohci
```

Create `~/gohci/gohci.yml` with the relevant configuration as described above.


## Private repository

`gohci` will automatically switch from HTTPS to SSH checkout when the repository
is private. For it to work you must:
- On your device, create a key via `ssh-keygen -C "raspberrypi"` and do not
  specify a password.
- Visit `github.com/user/repo/settings/keys`, click `Add deploy key`.
- Put a name of the device and paste the content of the public key at
  `$HOME/.ssh/id_rsa.pub`, `%USERPROFILE%\.ssh\id_rsa.pub` on Windows.
- Do not check `Allow write access`!
- Click `Add key`.


## Testing

To test your hook, run:

```
gohci -test periph/gohci
```

where `periph/gohci` is replaced with the repository you want to fetch and test
at `HEAD`. Use `-commit` and it'll create the gist and the status on the commit.
Useful when testing checks.

The github's "_Redeliver hook_" functionality is also very useful to test your
setup.


## Security

This is a remote execution engine so assume the host that is running `gohci`
will be 0wned. Still, use a strong randomly generated webhook secret.

The main problem is someone could steal the OAuth2 token which means the
attacker can:
- create gists under your name
- create or modify commit statuses


## FAQ


### Test on multiple kind of hardware simultaneously?

- Install `gohci` on each of your devices, e.g. a
  [C.H.I.P.](https://getchip.com/), a [Raspberry
  Pi](https://www.raspberrypi.org/), a [BeagleBone](https://beagleboard.org/),
  Windows, etc.
- Register multiple webhooks to your repository, one per device, using the
  [explanations](#webhook) above. For each hook, use URLs in the format
  `https://1.2.3.4/gohci/deviceX`.
- Setup your `Caddyfile` like this:

```
ci.example.com {
    log log/ci.example.com.log
    tls youremail@example.com
    proxy /gohci/chip chip:8080 {
      transparent
      without /gohci/chip
    }
    proxy /gohci/pine64 pine64:8080 {
      transparent
      without /gohci/pine64
    }
    proxy /gohci/rpi3 raspberrypi:8080 {
      transparent
      without /gohci/rpi3
    }
}
```


### What are the rules about which PRs are tested?

By default, `RunForPRsFromFork` is `false`, which means that PRs that do come
from a forked repository are not tested automatically. If `RunForPRsFromFork` is
set to `true`, all PRs are tested. This increase your attack surface as your
worker litterally run random code. This is not recommended.

The default rule is that only PRs coming from the own repo (not a fork) will be
automatically tested, plus any push to the repository.

You can specify `SuperUsers` to allow all PRs created by these users to be
tested automatically. These users can also comment `gohci: run` on any commit
which will trigger a test run.


### Won't the auto-updater break my CI when you push broken code?

Yes. I'll try to keep `gohci` always in a working condition but it can fail from
time to time. So feel free to fork the `gohci` repository and run from your
copy. Don't forget to update `gohci_update.timer` to pull from your repository
instead.


### What's the maximum testing rate per hour?

Github has a free quota of [5000 requests per
hour](https://developer.github.com/v3/#rate-limiting) for authenticated
requests. Each test run does:

- 1 create status request for pending
- 1 create status request at the start
- 1 gist create request including the 'metadata' pseudo-file
- For each stream (one stream per test plus the two 'setup' streams):
  - 1 gist edit request
  - 1 status create request

So a configuration defining 7 tests would sum for `3 + 1 + (2 * (7+2))` = 22
requests. 5000/13 = *227 test runs/hour*. If you have 3 workers, this means an
upper bound of *75 test runs/hour*. In practice, `gohci` throttles its requests
so the effective number of requests per build is lower.


### Can you add support for `gd`, `glide`, etc?

If there's enough interest, I'm open to adding support for more tools.


### What about when the device dies?

Micro computers tends to be unstable, so monitoring is recommended, even for a
one-off solution. A good option is to setup https://uptimerobot.com which has a
free plan with 50 monitored sites pinged at a 5 minutes interval. It supports
sending SMS via common email-to-SMS provider functionality.
