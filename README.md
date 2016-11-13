# sci - The Shameful CI

This is the bare bone essence of a Continuous Integration server. It hardly can
get any simpler.

- As simple as it can be; the initial version is less than 300 lines.
- Only support one specific use case: golang projects hosted on Github.
- There is no server, the worker must be internet accessible and HTTPS must be
  proxied ([Caddy](https://caddyserver.com/) works great).
- Metadata and stdout is saved as a gist on Github.
- The worker has a configuration file that determines what command it runs to
  test the project. By default it is `go test`.


## Configuration

Create a new directory and start `sci`. It will create `sci.json` similar to the following:

```
{
  "Port": 8080,
  "AccessToken": "put your access token here",
  "WebHookSecret": "generate a random string",
  "UseSSH": true,
  "Owners": [
    "maruel"
  ],
  "Branches": [
    "refs/heads/master"
  ],
  "Repo": "github.com/maruel/sci",
  "Check": [
    "go",
    "test"
  ],
  "Name": "sci",
  "GOPATH": "sci-tmp"
}
```


### OAuth2 token

Visit https://github.com/settings/tokens, check "repo:status" and "gist". Put
the hex string into `AccessToken` in `sci.json`. This is needed to put
success/failure status on the PR.


### Webhook secret

Visit to `github.com/<name>/<repo>/settings/hooks` and create a new webhook.

- Use your worker IP address as the hook URL, `https://1.2.3.4/postreceive`.
- Type a random string, that you will put in `WebHookSecret` in `sci.json`.
- Click `Let me select individual events` and check only `Pull request` and
  `Push`.
