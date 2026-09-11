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
to fetch and will fail with a 404.

The installer downloads the release for your board, verifies it, installs the
service, and prints the Wi-Fi access point password. **Write that password
down** — if the device ever cannot reach a network it raises an access point
called `Spoloh-Setup`, and that password is the only way back in.

Then open `http://spoloh.local/` and set an administrator password.

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

## Stand with Ukraine

[![StandWithUkraine](https://raw.githubusercontent.com/vshymanskyy/StandWithUkraine/main/badges/StandWithUkraine.svg)](https://stand-with-ukraine.pp.ua/)

This software exists because of the war. If it is useful to you, consider
[supporting Ukraine](https://stand-with-ukraine.pp.ua/) — through
[humanitarian aid](https://savelife.in.ua/en/donate/), or by helping someone
who needs a siren they can hear.
