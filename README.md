[![Stand With Ukraine](https://raw.githubusercontent.com/vshymanskyy/StandWithUkraine/main/banner2-direct.svg)](https://stand-with-ukraine.pp.ua)

# Spoloh

Spoloh is a self-hosted appliance that announces air raid alerts and
scheduled events over an existing PA system or speaker. It runs on a
single-board ARM computer (Raspberry Pi 2/3/4, Orange Pi) that the operator
owns and installs themselves — no cloud, no account, no license key, no
vendor.

One device serves one site: a school, a community hall, a workshop, a
household.

## Installation

On a Raspberry Pi (2, 3, 4 or Zero 2) or a comparable ARM board running
Raspberry Pi OS Lite or Armbian:

```sh
curl -fsSL https://github.com/b-ovsepian/spoloh/releases/latest/download/install.sh | sudo sh
```

This needs a published release: the script and every file it installs are
release assets, so until the first `v*` tag is pushed the command has nothing
to fetch and will fail with a 404. Installing before then, or from a fork, is
described below.

The installer downloads the release for your board, verifies it, installs the
service, and prints the Wi-Fi access point password. **Write that password
down** — if the device ever cannot reach a network it raises an access point
called `Spoloh-Setup`, and that password is the only way back in. It is kept
in `/etc/spoloh/config.json` too, but that file is `0600` and owned by
`spoloh`, so reading it back takes a shell on the board — which is exactly
what you do not have on the day you need the password.

While `Spoloh-Setup` is up and the device has a Wi-Fi network saved, it takes
the access point down for 90 seconds after every five minutes up, to look for
that network — a router that came back after the device did. It waits while
someone has used the dashboard in the last two minutes, but for no more than
fifteen. If `Spoloh-Setup` vanishes briefly from your phone's list, that is
why; it comes back on its own. A device with no network saved never does this.

Then open `http://spoloh.local/` and set an administrator password. That, and
`sudo usermod -aG spoloh $USER` if you want the command line, are the only
things an operator has to do by hand; both are once, not once per update.

### Installing without a release

Before the first tag, and for a fork or a change you have not pushed, stage a
release of your own and point the installer at it. The download root is
overridable, so this runs the real script over real checksums and installs
the real units — only the host is a directory instead of GitHub:

```sh
git clone https://github.com/b-ovsepian/spoloh && cd spoloh
make dist        # armv6, armv7, arm64, every deploy file, and SHA256SUMS

sudo mkdir -p /srv/spoloh-release/latest/download
sudo cp dist/* /srv/spoloh-release/latest/download/
sudo SPOLOH_BASE_URL=file:///srv/spoloh-release sh deploy/install.sh
```

`latest/download` is not decoration: it is the path the installer builds when
no `SPOLOH_VERSION` is given, mirroring where GitHub's `/releases/latest`
redirect lands. `make dist` cross-compiles all three boards and needs Go 1.25
and nothing else, so it can run on the board or on a laptop with `dist/`
copied across.

A device installed this way is stamped with whatever `git describe` said, so
`spoloh update` will offer it the first real release as soon as one is
published. Cutting that release is [docs/RELEASING.md](docs/RELEASING.md).

## Updating

```sh
sudo spoloh update --check   # what is installed, and what is available
sudo spoloh update           # install it
sudo spoloh update --rollback
```

An update is verified against the checksum published with the release and is
proven to run on the board before it replaces anything. If the new version
will not start, the previous one is put back automatically.

## Command line

The daemon listens on a unix socket at `/run/spoloh/control.sock`, so the
device can be driven from a shell without the dashboard and without a second
password:

```sh
spoloh status                # alert, region, source, clock, output, what is playing
spoloh sounds                # the library, one name per line
spoloh play siren.wav        # through the real output, same queue an alert uses
spoloh stop                  # silence whatever is playing
spoloh log -n 50             # the last N event log entries, newest first
spoloh version               # what the running daemon is, not what is on disk
```

The socket's permissions are the whole authorisation: it is `0660`, owned
`spoloh:spoloh`, so **an account has to be in the `spoloh` group** to use any
of this.

```sh
sudo usermod -aG spoloh $USER
```

Log out and back in afterwards — group membership is read at login, so the
shell you typed that in still cannot reach the socket.

Output is plain lines with no colour, meant for `grep`, `cut` and `awk` as
much as for reading. `spoloh status` always prints every field, with `-` for
one that is empty, and `spoloh log` separates its columns with tabs.

`spoloh play` is not a shortcut past anything: it goes through the same engine,
the same single voice and the same event log as an announcement, so a real
alert is queued exactly as it would be against a preview started from the
dashboard.

None of this reaches the network — a unix socket cannot be connected to from
another machine — and none of it works if the daemon is not running, which
`systemctl status spoloh` will say.

## Radio

**Налаштування → Радіо** plays an internet radio station between
announcements, on the days and between the times you choose: one of a short
list of Ukrainian stations, or a stream address of your own (http or https
only). It is off until you switch it on.

The radio comes last. Any sound the device makes — an alert, a scheduled
event, listening to a sound on the settings pages — stops it at once, and it
picks up again afterwards; because it is a live broadcast, what played in
between is skipped, not paused. Quiet hours silence it. During an air raid it
plays on between announcements unless you tick the box that keeps it off from
the alert to the all-clear. **Зупинити звук** on the home screen stops the
radio as well, until you press **Відновити радіо** on its page or its window
next opens.

Its volume is a share of the master volume, and the share is applied in
software: the card itself stays at the master volume, so a fault while
setting an announcement's own volume can never leave a siren as quiet as the
radio was.

A stream uses the site's internet connection the whole time it plays — about
1.4 GB a day at 128 kbit/s — which matters on a mobile connection. Playing a
broadcast to the public may need the broadcaster's or a rights society's
permission; that is for whoever installs the device to check.

## Monitoring

`http://<device>/metrics` reports the device's health in the Prometheus text
format: whether an alert source has answered and how long ago, whether an
alert is on and how severe, whether the siren has been switched off in the
settings or quiet hours silence alerts, whether the clock is synchronised,
the network and the radio, and counts of announcements, playback errors,
silenced alerts, failed webhooks and event log writes that failed.

It is off until **Налаштування → Моніторинг → Створити токен**, and then
answers only a request carrying that token as `Authorization: Bearer <token>`.
The device keeps only the token's hash, so it shows the token once, when it
is made; a lost token is replaced by making a new one. Like the dashboard it
is plain HTTP, so keep the device and the monitoring system on a network you
trust.

- **Zabbix 6.0 or later:** import
  [deploy/monitoring/zabbix_template_spoloh.yaml](deploy/monitoring/zabbix_template_spoloh.yaml),
  link *Spoloh by HTTP* to a host whose interface is the device's address,
  and set `{$SPOLOH.TOKEN}` on that host. Its triggers cover a device that
  stops answering, no alert source answering, the siren switched off, quiet
  hours silencing alerts, an unsynchronised clock, an access point the device
  cannot lower once it is back on its network, playback and webhook failures,
  and an event log that can no longer be written. A device with no network at
  all cannot be reached from Zabbix, so it shows as one that stopped
  answering.
- **Prometheus and anything that reads its format:** scrape `/metrics` with
  `authorization: {credentials: <token>}`.

## Licence

[PolyForm Noncommercial 1.0.0](LICENSE.md). Use it, change it, run it,
redistribute it — for any noncommercial purpose.

"Noncommercial" is meant the way the licence spells it out, which is broader
than it first sounds: personal and hobby use, and use by any charity, school,
public research body, public safety or health organisation, environmental
group or government institution — regardless of how any of them are funded.
Selling this software, or selling a device or service built on it, is what it
does not allow.

A word on the label: this licence restricts the field of use, so it is not
"open source" as the OSI defines the term, and GitHub will not badge it as
such. The source is public, readable and modifiable; it is source-available
rather than OSI open source. If you want to use Spoloh commercially, ask.

## Stand with Ukraine

[![StandWithUkraine](https://raw.githubusercontent.com/vshymanskyy/StandWithUkraine/main/badges/StandWithUkraine.svg)](https://stand-with-ukraine.pp.ua/)

This software exists because of the war. If it is useful to you, consider
[supporting Ukraine](https://stand-with-ukraine.pp.ua/) — through
[humanitarian aid](https://savelife.in.ua/en/donate/), or by helping someone
who needs a siren they can hear.
