# Configuration

- [Machine account](#machine-account)
  - [OAuth2 token](#oauth2-token)
- [Worker setup](#worker-setup)
  - [Debian](#debian)
  - [Windows](#windows)
  - [macOS](#macos)
  - [Worker configuration](#worker-configuration)
  - [Private repository](#private-repository)
- [Project](#project)
  - [Project access](#project-access)
  - [Webhook](#webhook)
  - [Project config](#project-config)
- [Testing](#testing)


## Machine account

It is preferable to create a ['machine
account'](https://help.github.com/articles/github-terms-of-service/#2-account-requirements)
and not use your personal account. For example, all projects for
[periph.io](https://periph.io) are tested with the account
[github.com/gohci-bot](https://github.com/gohci-bot).

- Visit [github.com/join](https://github.com/join) and create a new account,
  preferably with suffix `-bot`.
- Visit [github.com/settings/security](https://github.com/settings/security) and
  turn on `Two-factor authentication`.
  - You have it enabled with your personal account, right? Right?


### OAuth2 token

- Visit [github.com/settings/tokens](https://github.com/settings/tokens).
  - Click `Generate new token` button on the top right.
  - Add a description like `gohci`
  - Check `gist` and `repo:status`
    - Do not give any write access to this token!
  - Click `Generate token`.
- Save this `AccessToken` string, you'll need it later in the worker's
  `gohci.yml` at the `oauth2accesstoken` line.


## Worker setup

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


### Worker configuration

- Create `~/gohci/gohci.yml` with the default configuration:
  ```
  mkdir -p ~/gohci
  cd ~/gohci
  gohci
  ```
- It will look like this, with comments added here:
  ```
  # The TCP port the HTTP server should listen on. It needs to be "frontend" by an
  # HTTPS enabled proxy, like caddyserver.com
  port: 8080
  # The GitHub webhook secret when receiving events:
  webhooksecret: <random string>
  # The GitHub oauth2 client token when updating status and gist:
  oauth2accesstoken: Get one at https://github.com/settings/tokens
  # Name of the worker as presented on the status:
  name: raspberrypi
  ```
- Edit the values based on your needs.
  - `oauth2accesstoken` must be set to the `AccessToken` you created at the step
    [OAuth2 token](#oauth2-token).
- Run `gohci` again and it will start a web server. When `gohci` is running,
  updating `gohci.yml` will make the process quit (after completing any enqueued
  checks).
- Reboot the host and make sure `gohci` starts correctly.


### Private repository

`gohci` will automatically switch from HTTPS to SSH checkout when the repository
is private. For it to work you must:
- On your device, create a key via `ssh-keygen -C "raspberrypi"` and do not
  specify a password.
- Visit `github.com/<user>/<repo>/settings/keys`.
- Click `Add deploy key`.
- Put a name of the device and paste the content of the public key at
  `$HOME/.ssh/id_rsa.pub`, `%USERPROFILE%\.ssh\id_rsa.pub` on Windows.
- Do not check `Allow write access`!
  - This means the ssh key only works for this repository and grants read-only
    access.
- Click `Add key`.


## Project


### Project access

The machine account must have access to set a [commit
status](https://help.github.com/articles/about-statuses/):

- As your normal account, visit
  `github.com/<user>/<repo>/settings/collaboration`.
- Add the machine account as a `Write` collaborator.
  - Sadly `Write` access is needed even for just status update. This is fine
    because:
    - Your machine account doesn't have an ssh key setup.
    - Your machine account has 2FA enabled.
    - The OAuth2 token is read only.
- Login as the machine account on GitHub and accept the invitation.


### Webhook

Visit to `github.com/<user>/<repo>/settings/hooks` and create a new webhook.

- Payload URL: Use your worker IP address or hostname as the hook URL,
  `https://1.2.3.4/gohci/workerA?altPath=foo.io/x/project&superusers=user1,user2,user3`.
  - `altPath`: Set it when using [canonical import
    path](https://golang.org/doc/go1.4#canonicalimports). For example,
    `periph.io/x/gohci`. Leave it unspecified otherwise, which should be the
    general case.
  - `superUsers`: a comma separate list of GitHub user accounts. These users
    can trigger a check run by typing the comment `gohci` on a PR or a commit as
    explained in the
    [FAQ](FAQ.md#what-are-the-rules-about-which-prs-are-tested).
  - Both altPath and superUsers are optional.
- Content type: select `application/json`.
- Type the random string found in `webhooksecret` in `gohci.yml`.
- Click `Let me select individual events` and check:
  - `Commit comments`
  - `Issue Comments`
  - `Pull requests`
  - `Pull request review comments`
  - `Push`
  - All the items except the last one are for the magic `gohci` hotword by super
    users. The last one is for post merge testing.


### Project config

Now it's time to customize the checks run via a
[.gohci.yml](https://github.com/periph/gohci/blob/master/.gohci.yml) in the root
directory of your repository. When the worker name is not provided, this becomes
the default checks as in this
example:

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
    - ./...
```


## Testing

Push a new branch to your repository with a `.gohci.yml` file. Check the gohci
worker logs to see progress, and look at the commits to see status being
updated. You can see it at `github.com/<user>/<repo>/commits/<branch>`
