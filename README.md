# sci - The Shameful CI

## Genesis

All I wanted was to run `go test ./...` on a Raspberry Pi on both Pull Requests
and Pushes.

The result is the distilled essence of a Continuous Integration service.


## Design

It hardly can get any simpler:

- Only support one specific use case: *Golang project hosted on Github*.
- As simple as it can be; the initial version is less than 300 lines.
- There is no server, the worker must be internet accessible and HTTPS must be
  proxied ([Caddy](https://caddyserver.com/) works great along
  https://letsencrypt.org).
- Metadata and stdout is saved as a gist on Github.
- The worker has a configuration file that determines what command it runs to
  test the project. By default it is `go test ./...`.


## Configuration

```
go get github.com/maruel/sci
sci
```

This  will create `sci.json` similar to the following:

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

Edit it based on your needs. Run again and it will start a web server.


### OAuth2 token

Visit https://github.com/settings/tokens, check `repo:status` and `gist`. Put
the hex string into `AccessToken` in `sci.json`. This is needed to put
success/failure status on the Pull Requests.


### Webhook secret

Visit to `github.com/<name>/<repo>/settings/hooks` and create a new webhook.

- Use your worker IP address as the hook URL, `https://1.2.3.4/postreceive`.
- Type a random string, that you will put in `WebHookSecret` in `sci.json`.
- Click `Let me select individual events` and check: ``Commit comment`, `Pull
  request`, `Pull request review comment` and `Push`.


### systemd

Setting up as systemd means it'll run automatically. The following is
preconfigured for a `pi` user. Edit as necessary.

```
cp sci.service /etc/systemd/system/sci.service
systemctl daemon-reload
systemctl enable sci.service
systemctl start sci.service
```

If you use your own Go version instead of the debian package, you need to add
this in the `[Service]` section of `sci.service`:

```
Environment=PATH=/home/pi/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin GOROOT=/home/pi/go
```


## Updating

Recompiling will trigger an automatic service restart, so simply run:

```
go get -u github.com/maruel/sci
```


## Testing

To test your hook, run:

```
sci -test maruel/sci
```

where `maruel/sci` is replaced with the repository you want to fetch and test at
`HEAD`.


## Security

This is a remote execution engine so assume the host that is running `sci` will
be 0wned. That's it.
