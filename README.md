# trains

Tracks the on-time status of a few Italian trains. It notifies you at a set
time if a train is late or cancelled, and serves a page with every train's
delay and last known position.

It is a single static Go binary with no runtime dependencies. Data comes
from [ViaggiaTreno](https://www.viaggiatreno.it), which publishes RFI's live
circulation data for every train on the Italian network: Trenitalia, Italo,
Trenord and the rest.

## How it works

- Every minute, `trains run` checks which trains need polling. A train is
  polled every `poll_interval` (default 5 min), from 30 min before its
  scheduled departure until it arrives or is cancelled. Outside that window
  nothing is fetched.
- Every reading is stored in SQLite, including the raw API response. So are
  fetch errors and notification checks.
- At a train's notification time, it fetches a fresh reading. A message is
  sent if the train is at least `late_after` minutes late, cancelled or
  partly cancelled. On time means no message. If ViaggiaTreno can't be
  reached for 30 minutes after that time, a "status unknown" message is
  sent instead. While the service is running, silence means on time.
- The status page (`listen`, default `:8080`) shows today's delay, last
  position and schedule for each train (those running right now first), plus its last 14 runs, and when the
  data was last updated. It reloads itself every 5 minutes while a train is
  running, and not at all otherwise. **Refresh now** fetches every train
  running today straight away. The same data
  is at `/api/trains` as JSON.

## History

**History** next to each train charts its past runs, one column per day
(early runs go below the line), for the last 30, 90 or 365 days:

- **Delay at the end of the line:** the arrival delay at the last stop.
- **Delay at a stop:** pick any stop on the route to see the arrival delay
  there (the departure delay, for the first stop).

Above each chart: how many runs, the share on time (under the train's
"late from" minutes), the median and the worst delay, and cancellations.
Hovering, or the arrow keys, shows each run's value, and **Show as a
table** lists them all. Everything comes from the readings already stored,
so past days are included. A run whose polling stopped before the end of
the line has no final delay. The same data is at
`/api/history?key=2034&days=90&stop=S01708`.

## Stops

**Stops** next to each train lists every stop of a run: scheduled and
actual arrival and departure, the delay at each, the platform, cancelled
stops, and where the train was last seen. It opens on the most recent run;
the **Run** menu picks any earlier recorded day, and the dates in the
history table link to it. Times are as of the run's last reading.

## Trains

Trains are managed on the status page: **Add a train**, or **Edit** and
**Delete** next to each one. Deleting stops tracking; the train's history
stays in the database. Changes apply immediately.

- **Number:** the train number, e.g. 2034.
- **Departure station:** train numbers aren't unique, and different
  operators can use the same number on the same day. **Look up today's
  trains** lists today's trains with that number to choose from. You can
  also type the departure station's name (or part of it, like
  `Milano Centrale`) or its code (`S01700`). A number shared by several
  trains isn't tracked until one is chosen; the status page says so.
- **Board at:** the stop you get on at, the starting station by default.
  When ViaggiaTreno knows its departure platform, the status page shows it,
  marked "planned" until confirmed and noting any change (e.g. "Platform 5
  · changed from 3"). The other stops can be picked once a run has been
  recorded.
- **Notify on:** the weekdays to send notifications on. The train itself is
  tracked every day it runs, so its history is complete; on days
  ViaggiaTreno lists no run (weekends, holidays) the status page says "Not
  running today".
- **Notify at:** the time to send a message if the train is late or
  cancelled. Leave it empty for no notification.
- **Late from:** minutes of delay that count as late. Defaults to
  `late_after` in the config file.

On the first start, any `trains:` listed in the config file are imported.
After that the list in the file is ignored.

The pages have no login. Anyone who can open them can change the list, so
put them behind the dashboard's proxy (and its Cloudflare Access login) or
keep `listen` on a private address. Form posts from other sites are
refused.

## Configuration

See [`config.example.yaml`](config.example.yaml) for the settings: the
status page address, database, polling interval, default `late_after` and
the notifiers. The file is re-read when it changes, so notifier changes
apply without a restart; `listen` and `database` need one.

Notifications go to Telegram or [ntfy](https://ntfy.sh). They are optional:
a notifier whose settings are empty (say `${TELEGRAM_TOKEN}` isn't set) is
disabled with a warning in the log, and everything else keeps working. For
Telegram, create a bot with @BotFather and get your chat id from
@userinfobot. Other channels implement the `Notifier` interface in
`internal/notify`.

## Commands

```
trains run [-config FILE]           track trains and serve the page
trains check [-config FILE] 2034    print a train's live status
trains test-notify [-config FILE]   send a test message
trains version
```

The configuration file defaults to `/etc/trains/config.yaml`.

## Building

Building needs Go 1.26 or later. `go` fetches it automatically if an older
one is installed.

```sh
make test
make build          # ./trains for this machine
make dist           # dist/trains-linux-{amd64,arm64}
```

## Installing on a server

Each [release](https://github.com/emdioh/trains/releases) has a tarball and
a zip per architecture, both holding the binary, `deploy/` and the example
config. On the
server (an LXC container is fine), as root:

```sh
v=v0.2.5 arch=amd64
wget https://github.com/emdioh/trains/releases/download/trains-$v/trains-$v-linux-$arch.tar.gz
tar -xzf trains-$v-linux-$arch.tar.gz
deploy/install.sh trains-linux-$arch
```

It installs `/usr/local/bin/trains`, creates a `trains` user and
`/var/lib/trains` for the database, and writes `/etc/trains/config.yaml` and
`/etc/trains/env` (for the Telegram secrets) if they don't exist. It then
installs and starts a systemd or OpenRC service, whichever the system uses.
To update, do the same with a newer release: configuration and data are
kept.

From a git clone of this repository, `deploy/install-release.sh` does all of
that in one step for the version in `VERSION`, checking the download
against `SHA256SUMS`. The dashboard project's deploy step uses it.

## Releasing

Bump `VERSION` and push it to `master`. The `trains release`
workflow then tests and builds the binaries, and publishes them as the
GitHub release `trains-<version>`, creating the tag. Pushing a
`trains-v<version>` tag yourself does the same. The `trains-` prefix is
kept from when this project shared a repository with others.

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
