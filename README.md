# trains

Tracks the on-time status of a few Italian trains. It notifies you at a set
time if a train is late or cancelled, and serves a page with every train's
delay and last known position.

It is a single static Go binary with no runtime dependencies. Data comes
from [ViaggiaTreno](https://www.viaggiatreno.it), which publishes RFI's live
circulation data for every train on the Italian network: Trenitalia, Italo,
Trenord and the rest.

## How it works

- Every minute, `trains run` checks which configured trains need polling. A
  train is polled every `poll_interval` (default 5 min), starting 30 min
  before its scheduled departure and stopping once it arrives or is
  cancelled. Outside that window it isn't polled at all.
- Every reading is stored in SQLite, including the raw API response. So are
  fetch errors and notification checks.
- At a train's `notify_at` time, it fetches a fresh reading. A message is
  sent if the train is at least `late_after` minutes late, cancelled or
  partly cancelled. On time means no message. If ViaggiaTreno can't be
  reached for 30 minutes after `notify_at`, a "status unknown" message is
  sent instead. While the service is running, silence means on time.
- The status page (`listen`, default `:8080`) shows today's delay, last
  position and schedule for each train, plus its last 14 runs. It
  refreshes every minute. The same data is at `/api/trains` as JSON.
- The configuration is re-read when it changes, so trains and notifiers can
  be edited while the service runs. `listen` and `database` need a restart.

## Configuration

See [`config.example.yaml`](config.example.yaml). A train needs only its
number:

```yaml
trains:
  - number: 2034
    label: Morning commute
    days: mon-fri
    notify_at: "07:20"
```

Some numbers run from more than one origin. `trains check NUMBER` lists the
runs and their origin codes; set `origin` to the one you want.

Notifications go to Telegram or [ntfy](https://ntfy.sh). They are optional:
a notifier whose settings are empty (say `${TELEGRAM_TOKEN}` isn't set) is
disabled with a warning in the log, and everything else keeps working. For Telegram,
create a bot with @BotFather and get your chat id from @userinfobot. Other
channels implement the `Notifier` interface in `internal/notify`.

## Commands

```
trains run [-config trains.yaml]          track trains and serve the page
trains check [-config trains.yaml] 2034   print a train's live status
trains test-notify [-config trains.yaml]  send a test message
trains version
```

## Building

Building needs Go 1.26 or later. `go` fetches it automatically if an older
one is installed.

```sh
make test
make build          # ./trains for this machine
make dist           # dist/trains-linux-{amd64,arm64}
```

## Installing on a server

Each [release](https://github.com/emdioh/things/releases) has a tarball and
a zip per architecture, both holding the binary, `deploy/` and the example
config. On the
server (an LXC container is fine), as root:

```sh
v=v0.1.2 arch=amd64
wget https://github.com/emdioh/things/releases/download/trains-$v/trains-$v-linux-$arch.tar.gz
tar -xzf trains-$v-linux-$arch.tar.gz
deploy/install.sh trains-linux-$arch
```

It installs `/usr/local/bin/trains`, creates a `trains` user and
`/var/lib/trains` for the database, and writes `/etc/trains/trains.yaml` and
`/etc/trains/env` (for the Telegram secrets) if they don't exist. It then
installs and starts a systemd or OpenRC service, whichever the system uses.
To update, do the same with a newer release: configuration and data are
kept.

From a git clone of this branch, `deploy/install-release.sh` does all of
that in one step for the version in `VERSION`, checking the download
against `SHA256SUMS`. The dashboard project's deploy step uses it.

## Releasing

Bump `VERSION` and push it to the `trains` branch. The `trains release`
workflow then tests and builds the binaries, and publishes them as the
GitHub release `trains-<version>`, creating the tag. Pushing a
`trains-v<version>` tag yourself does the same. The `trains-` prefix keeps
the tags apart from other projects in this repository.

## Caveats

- ViaggiaTreno's API is unofficial and undocumented. It can change without
  notice, and the field meanings here come from community projects. In
  particular, cancellations are recognised by `tipoTreno` (`ST` cancelled;
  `PP`, `SI` and `SF` partly cancelled), and that needs checking against a
  real cancelled train. If a field changes type, it reads as empty rather
  than failing the whole response.
- There is no GPS: the position is the last station where the train was
  detected.
- Tracking is per calendar day: a train still running at midnight stops
  being tracked then. Night trains aren't handled yet.
