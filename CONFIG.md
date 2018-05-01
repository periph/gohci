## Initial Setup

- [Initial Setup](#initial-setup)
  - [OAuth2 token](#oauth2-token)
  - [Project access](#project-access)
  - [Webhook](#webhook)
- [Worker Setup](#worker-setup)
  - [Debian](#debian)
  - [Windows](#windows)
  - [macOS](#macos)
  - [Configuration](#configuration)
  - [Private repository](#private-repository)
- [Project config](#project-config)

Before starting the worker, some initial configuration is necessary.


### OAuth2 token

It is preferable to create a bot account and not use your personal account.
GitHub ToS calls it ['machine
account'](https://help.github.com/articles/github-terms-of-service/#2-account-requirements).
For example, all projects for [periph.io](https://periph.io) are tested with the
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


### Project access

The bot account must have access to set a [commit
status](https://help.github.com/articles/about-statuses/).

- As your normal account, add the bot account as a 'Write' collaborator.
  - Sadly 'Write' access is needed even for just status update.
- Login as the bot account on github and accept the invitation.


### Webhook

Visit to `github.com/user/repo/settings/hooks` and create a new webhook.

- Payload URL: Use your worker IP address or hostname as the hook URL,
  `https://1.2.3.4/gohci/workerA`.
- Content type: select 'application/json'.
- Type a random string, that you will put in `WebHookSecret` in `gohci.yml`.
- Click `Let me select individual events` and check: `Commit comment`, `Issue
  Comment`, `Pull request`, `Pull request review`, `Pull request review comment`
  and `Push`.
- Keep the tab open but don't enable it yet; enable the webhook once the worker
  is up and running.


## Worker Setup

Now it's time to setup the worker itself.

**`gohci` requires Go 1.8**


### Debian

This includes Raspbian and Ubuntu.

- Install [Go](https://golang.org/dl).
  - The Go version in packages is kinda old, so it's preferable to install a
    recent version.
  - See [official instructions](https://golang.org/doc/install#install) for
    help.
- Setup `$PATH` to include `~/go/bin`
- Install git.
- Install `gohci`.
- Create the directory `gohci`.
- Set up the system to run `gohci` automatically and update it every
  day via [`systemd/setup.sh`](systemd/setup.sh) .

Overall it looks like this:

```
sudo apt install git
export PATH="$PATH:$HOME/go/bin"
echo 'export PATH="$PATH:$HOME/go/bin"' >> ~/.bash_aliases
go get -u -v periph.io/x/gohci
mkdir -p ~/gohci
$HOME/go/src/periph.io/x/gohci/systemd/setup.sh
```


### Windows

- Install [Go](https://golang.org/dl).
  - Setup `PATH` to include `%USERPROFILE%\go\bin`
  - See [official instructions](https://golang.org/doc/install#install) for
    help.
- Install [git](https://git-scm.com)
- First enable auto-login (optionally on a fresh low privilege account).
  - Win-R
  - `netplwiz`
  - Uncheck _Users must enter a user name and password to use this computer_.
  - OK
  - Type password twice.
- Create a batch file `%APPDATA%\Microsoft\Windows\Start
  Menu\Programs\Startup\gohci.bat` that contains the following:
  ```
  @echo off
  title gohci
  cd %USERPROFILE%\gohci
  :loop
  gohci
  goto loop
  ```
- Auto-update can be done via the task scheduler. The following command will
  auto-update `gohci` every day:
  ```
  schtasks /create /tn "Update gohci" /tr "go get -v -u periph.io/x/gohci" /sc minute /mo 1439
  ```
  - The task should show up with: `schtasks /query /fo table | more` or
    navigating the GUI with `taskschd.msc`.
- Open `cmd` and run:
  ```
  go get -u -v periph.io/x/gohci
  mkdir %USERPROFILE%/gohci
  cd %USERPROFILE%/gohci
  ```
- Run `gohci` twice to make sure the firewall popup is shown and you allow the
  app.


### macOS

- Install [Xcode](https://developer.apple.com/xcode/) (which includes git).
- Install [Go](https://golang.org/dl).
  - See [official instructions](https://golang.org/doc/install#install) for
    help.
- Install [Homebrew](https://brew.sh) (optional).
- Create a `gohci` standard account (optional).
- Install `gohci` and setup for auto-start:
  ```
  go get -u -v periph.io/x/gohci
  mkdir -p ~/Library/LaunchAgents
  cp $HOME/go/src/periph.io/x/gohci/macos/gohci.plist ~/Library/LaunchAgents
  mkdir -p ~/gohci
  ```
- Enable auto-login via system preferences.


### Configuration

- Create `~/gohci/gohci.yml` with the default configuration:
  ```
  cd gohci
  gohci
  ```
- It will look like this, with comments added here:
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
    - cmd:
      - go
      - vet
      - -unsafeptr=false
      - ./...
      env: []
  ```
-  Edit based on your needs.
- Run `gohci` again and it will start a web server. When `gohci` is running,
  updating `gohci.yml` will make the process quit (after completing any enqueued
  checks).
- Reboot the host and make sure `gohci` starts correctly.
- Enable the webhook.
- Push a commit to confirm it works!


### Private repository

`gohci` will automatically switch from HTTPS to SSH checkout when the repository
is private. For it to work you must:
- On your device, create a key via `ssh-keygen -C "raspberrypi"` and do not
  specify a password.
- Visit `github.com/user/repo/settings/keys`, click `Add deploy key`.
- Put a name of the device and paste the content of the public key at
  `$HOME/.ssh/id_rsa.pub`, `%USERPROFILE%\.ssh\id_rsa.pub` on Windows.
- Do not check `Allow write access`!
- Click `Add key`.


## Project config

While you can add the checks on the worker itself, you can also add them to the
project in a file
[.gohci.yml](https://github.com/periph/gohci/blob/master/.gohci.yml):

When the worker name is not provided, this becomes the default checks. Checks on
the worker's `gohci.yml`, if defined, always override the project's
`.gohci.yml`.

If the project uses an alternate import path, like `periph.io/x/gohci` for
`github.com/periph/gohci`, this has to be defined on the worker's `gohci.yml`.

```
# See https://github.com/periph/gohci
version: 1
workers:
- name: win10
  checks:
  - cmd:
    - go
    - test
    - -race
    - ./...
  - cmd:
    - go
    - vet
    - ./...
- checks:
  - cmd:
    - go
    - test
    - -race
    - ./...
```


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
