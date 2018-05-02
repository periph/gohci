# FAQ


## What are the rules about which PRs are tested?

By default, only commits in branches on the repository are tested but not PRs.

You have to specify [`superUsers` on the
webhook](https://github.com/periph/gohci/blob/query_arg/CONFIG.md#webhook) to
grant 'super user' access. This allows:
- All PRs created by these users to be tested automatically.
- These users can comment `gohci` on any commit or PR to trigger a test run!


## What's the security story?

This is a remote execution engine. It is designed to run code on it. At least,
you get to decide what code runs on it.

The main problem with the current design is someone could steal the OAuth2 token
which means the attacker can:
- create gists under your machine account
- create or modify commit statuses for the lolz


## Test on multiple kind of hardware simultaneously?

- Install `gohci-worker` on each of your devices, e.g. a
  [C.H.I.P.](https://getchip.com/), a [Raspberry
  Pi](https://www.raspberrypi.org/), a [BeagleBone](https://beagleboard.org/),
  macOS, Windows, etc.
- Register [one webhook](CONFIG.md#webhook) on your repository per device. For
  each hook, use URLs in the format `https://1.2.3.4/gohci/deviceX` or as your
  choosing.
- Each `gohci-worker` worker is completely independent. They do not need to be
  all located at the same physical location.
- Setup your `Caddyfile` like this:

```
ci.example.com {
    log log/ci.example.com.log
    tls youremail@example.com
    proxy /gohci/chip chip:8080 {
        transparent
        without /gohci/chip
    }
    proxy /gohci/rpi3 raspberrypi:8080 {
        transparent
        without /gohci/rpi3
    }
    proxy /gohci/win10 win10:8080 {
        transparent
        without /gohci/win10
    }
}
```


## Won't the auto-updater break my CI when you push broken code?

Maybe. I'll try to keep `gohci-worker` always in a working condition but it can
fail from time to time. So feel free to fork the `gohci` repository and run from
your copy. Don't forget to update `gohci_update.timer` to pull from your
repository instead.


## What's the maximum testing rate per hour?

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
upper bound of *75 test runs/hour*. In practice, `gohci-worker` throttles its
requests by buffering all the updates that happen within a second so the
effective number of requests per build is lower, i.e. you can run more tests in
practice.


## Can you add support for `gd`, `glide`, `vgo`, etc?

If there's enough interest, I'm open to adding support for more tools.


## What about when the device dies?

Micro computers tends to be unstable, so monitoring is recommended, even for a
one-off solution. A good option is to setup https://uptimerobot.com which has a
free plan with 50 monitored sites pinged at a 5 minutes interval. It supports
sending SMS via common email-to-SMS provider functionality.


## What's the difference with a GitHub Apps

[GitHub
Apps](https://developer.github.com/apps/differences-between-apps/#about-github-apps/)
are generally provided by companies. In contrast, `gohci` is purely open source
and you are in control of the whole security, including the [machine
account](CONFIG.md#machine-account) being used. As an added benefit, it is 100%
free.
