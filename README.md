# gohci - The Go on Hardware CI

![screen cast](https://raw.githubusercontent.com/wiki/maruel/gohci/gohci.gif
"screen cast")

## Genesis

All I wanted was to run `go test ./...` on a Raspberry Pi on both Pull Requests
and Pushes for a private repository. I realized that it is possible to store the
test's stdout to a [Github Gist](https://gist.github.com/) so I created a
_serverless_ CI.

The result is the distilled essence of a Continuous Integration service that is
heavily towards testing Go projects on hardware, specifically low power ones
(Raspberry Pis, C.H.I.P., etc).


## Design

It hardly can get any simpler:

- Only support one specific use case: *Golang project hosted on Github*.
- There is no server, the worker must be internet accessible and HTTPS must be
  proxied down to HTTP.
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


## Installation

Install and create the default `gohci.json`:

```
go get github.com/maruel/gohci
gohci
```

It will look like this:

```
{
  "Port": 8080,
  "WebHookSecret": "Create a secret and set it at github.com/user/repo/settings/hooks",
  "Oauth2AccessToken": "Get one at https://github.com/settings/tokens",
  "Name": "<the hostname by default>",
  "Checks": [
    [
      "go",
      "test",
      "./..."
    ]
  ]
}
```

Edit it based on your needs. Run again and it will start a web server. When
`gohci` is running, updating `gohci.json` will make the process quit. It is
assumed that you use a service manager, like systemd.


### OAuth2 token

- Visit https://github.com/settings/tokens
- Click `Personal access tokens` near the bottom in the left list
- Click `Generate new token`
- Add a description like `gohci`
- Check `gist` and `repo:status`
  - Do not give any write access to this token!
- Click `Generate token`
- Put the hex string into `AccessToken` in `gohci.json`. This is needed to create
  the gits and put success/failure status on the Pull Requests.


### Webhook

Visit to `github.com/user/repo/settings/hooks` and create a new webhook.

- Use your worker IP address or hostname as the hook URL,
  `https://1.2.3.4/github/repoA`.
- Type a random string, that you will put in `WebHookSecret` in `gohci.json`.
- Click `Let me select individual events` and check: `Commit comment`, `Pull
  request`, `Pull request review comment` and `Push`.


### systemd

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


### Testing a private repository

`gohci` will automatically switch from HTTPS to SSH checkout when the repository
is private. For it to work you must:
- On your device, create a key via `ssh-keygen -C "raspberrypi"` and do not
  specify a password.
- Visit `github.com/user/repo/settings/keys`, click `Add deploy key`.
- Put a name of the device and paste the content of the public key at
  `/home/pi/.ssh/id_rsa.pub`.
- Do not check `Allow write access`!
- Click `Add key`.


## Updating

Recompiling will trigger an automatic service restart, so simply run:

```
go get -u github.com/maruel/gohci
```

but it is not necessary, as `gohci_update.service` does it for you and
`gohci_update.timer` runs gohci_update every 10 minutes.


## Testing locally

To test your hook, run:

```
gohci -test maruel/gohci
```

where `maruel/gohci` is replaced with the repository you want to fetch and test
at `HEAD`. Use `-commit` and it'll create the gist and the status on the commit.
Useful when testing checks.

The github's "_Redeliver hook_" functionality is also very useful to test your
setup.


## Security

This is a remote execution engine so assume the host that is running `gohci`
will be 0wned. That's it. Use a strong webhook secret.

The main problem is someone could steal the OAuth2 token which means the
attacker can:
- create gists under your name
- create or modify commit statuses


## FAQ


### Run `gohci` for multiple repositories on my device?

- Copy paste `gohci.service` multiple times. Don't duplicate
  `gohci_update.service` and `gohci_update.timer`, just `gohci.service`!
- Make each one use a *different* `WorkingDirectory=` value.
- In each directory, create a `gohci.json` and use a different `Port`.
- Register and start the services via systemd via `systemctl` commands [listed
  above](#systemd).
- Your `Caddyfile` file should look like the following. You can also run Caddy
  directly from your Raspberry Pi if you want.

```
ci.example.com {
    gzip
    log log/ci.example.com.log
    tls youremail@example.com
    proxy /github/repoA raspberry:8080
    proxy /github/repoB raspberry:8081
}
```


### Can you add support for node.js, ruby, C++, etc?

I think you are missing the point. That said, forking this code and updating
`runChecks()` accordingly would do just fine.


### Can you add support for `gd`, `glide`, etc?

The project's goal is to be very simple. Forking this code and updating
`runChecks()` accordingly would do just fine. That said, if there's enough
interest, I'm open to adding more suppotr.


### Test on multiple kind of hardware simultaneously?

- Install `gohci` on each of your devices, e.g. a
  [C.H.I.P.](https://getchip.com/), a [Raspberry
  Pi](https://www.raspberrypi.org/), a [Pine64](https://www.pine64.org/), etc.
- Register multiple webhooks to your repository, one per device, using the
  [explanations](#webhook) above. For each hook, use URLs in the format
  `https://1.2.3.4/github/repoA/deviceX`.
- Setup your `Caddyfile` like this:

```
ci.example.com {
    gzip
    log log/ci.example.com.log
    tls youremail@example.com
    proxy /github/repoA/chip chip:8080
    proxy /github/repoA/pine64 pine64:8080
    proxy /github/repoA/rpi3 raspberrypi:8080
}
```


### Won't the auto-updater break my CI when you push broken code?

Yes. I'll try to keep `gohci` always in a working condition but it can fail from
time to time. So feel free to fork the `gohci` repository and run from your
copy.  Don't forget to update `gohci_update.timer` to pull from your repository
instead.  It'll work just fine.


### Why `gohci` doesn't have unit tests?

Because I like ironic projects.


### What's the maximum testing rate per hour?

Github enforces [5000 requests per
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
upper bound of *75 test runs/hour*.

If this becomes a problem, the number of requests can be lowered up to:

- 1 create status request for pending
- 1 create status request at the start
- 1 create status request at the end
- 1 gists create request at the end

at the cost of having no updates while the task is running. This would enable
1250 test runs/hour.
