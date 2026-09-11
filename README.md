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
