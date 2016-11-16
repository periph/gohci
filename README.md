# sci - The Stupid CI

## Genesis

All I wanted was to run `go test ./...` on a Raspberry Pi on both Pull Requests
and Pushes for a private repository.nd I realized that I could push the test's
stdout to a [Github Gist](https://gist.github.com/).

The result is the distilled essence of a Continuous Integration service.


## Design

It hardly can get any simpler:

- Only support one specific use case: *Golang project hosted on Github*.
- There is no server, the worker must be internet accessible and HTTPS must be
  proxied ([Caddy](https://caddyserver.com/) works great along
  https://letsencrypt.org).
- The worker has a configuration file that determines what command it runs to
  test the project. By default it is `go test ./...`.


## Installation

Install and create the default `sci.json`:

```
go get github.com/maruel/sci
sci
```

It will look like this:

```
{
  "Port": 8080,
  "WebHookSecret": "Create a secret and set it at github.com/'name'/'repo'/settings/hooks",
  "Oauth2AccessToken": "Get one at https://github.com/settings/tokens",
  "UseSSH": false,
  "Name": "sci",
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
`sci` is running, updating `sci.json` will make the process quit. It is assumed
that you use a service manager, like systemd.

To test a private repository, set `UseSSH` to true and use a read-only
deployment key so your worker has access to the repository.


### OAuth2 token

Visit https://github.com/settings/tokens, check `repo:status` and `gist`. Put
the hex string into `AccessToken` in `sci.json`. This is needed to create the
gits and put success/failure status on the Pull Requests.


### Webhook secret

Visit to `github.com/<name>/<repo>/settings/hooks` and create a new webhook.

- Use your worker IP address or hostname as the hook URL,
  `https://1.2.3.4/github/repoA`.
- Type a random string, that you will put in `WebHookSecret` in `sci.json`.
- Click `Let me select individual events` and check: ``Commit comment`, `Pull
  request`, `Pull request review comment` and `Push`.


### systemd

Setting up as systemd means it'll run automatically. The following is
preconfigured for a `pi` user. Edit as necessary, which is necessary if you run
your own Go version.

As root:

```
cp systemd/* /etc/systemd/system
systemctl daemon-reload
systemctl enable sci.service
systemctl enable sci_update.timer
systemctl start sci.service
systemctl start sci_update.timer
```

## Updating

Recompiling will trigger an automatic service restart, so simply run:

```
go get -u github.com/maruel/sci
```

but it is not necessary, as `sci_update.service` does it for you and the
`sci_update.timer` runs sci_update every 10 minutes.


## Testing

To test your hook, run:

```
sci -test maruel/sci
```

where `maruel/sci` is replaced with the repository you want to fetch and test at
`HEAD`. Use `-commit` and it'll create the gist and the status on the commit.


## Security

This is a remote execution engine so assume the host that is running `sci` will
be 0wned. That's it. Use a strong webhook secret.


## FAQ


### `sci` is so aewsome, I want to run it for multiple repositories. How?

- Copy paste sci.service multiple times
- Make each one use a different `WorkingDirectory` value.
- In each directory, create a `sci.json` and use a different `Port`.
- Register them to systemd and start them.
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


### `sci` doesn't have unit tests. Isn't that stupid?

I think you are missing the point.
