# Releasing Spoloh

The README tells a first-time visitor to run

```sh
curl -fsSL https://github.com/b-ovsepian/spoloh/releases/latest/download/install.sh | sudo sh
```

That URL is a 404 until a release exists, and so is every file the script
then goes on to fetch. `spoloh update` has nothing to compare itself against
either. Tagging is the one action only the maintainer can take; this
document is everything that has to be true around it.

## What a release contains

`deploy/install.sh` downloads, by name, whatever its `FILES=` line lists —
read that line rather than this paragraph, because it is the authority. As
of this writing it is the per-board binary plus:

```
spoloh.service  spoloh.sysusers  spoloh.tmpfiles
50-spoloh-networkmanager.rules
spoloh-timesync.service  50-spoloh-timedate.rules
```

On top of that a release carries all three binaries (`spoloh-linux-armv6`,
`spoloh-linux-armv7`, `spoloh-linux-arm64`) so any board can install from
it, `SHA256SUMS` covering the lot, and `install.sh` itself — the one file
that is in the release but is not in `FILES`, because it is the script doing
the downloading. The README fetches it from
`releases/latest/download/install.sh`, so a release without it is a 404 for
every new install.

A file added to `deploy/` has to be named in three places: `install.sh`'s
`FILES`, the Makefile's `cp` line, and the Makefile's `sha256sum` line.
`TestTheMakefileAndTheInstallerAgreeOnTheDeploymentFilenames` in
`deploy/install_test.go` fails when those three disagree, which is the only
thing standing between a rename and a field of devices that all 404 on it.
Keep the list above in step with them by hand — no test reads this file.

## Which number

Use `v0.1.0` for the first one.

The device works and is in daily use, so the temptation is `v1.0.0`. The
argument against it is the install surface: two polkit rules shipped wrong
in a single week, and one of them was missing an action the daemon needed,
so the access point could not rise — on a board that has fallen off the
network, the access point *is* the way back in. The code that makes a sound
has been steadier than the code that puts it on a board, and it is exactly
the second kind of mistake that nobody can fix remotely. A 0.x number says
that to an operator deciding how much to trust an unattended update. It is
not modesty; it is the accurate figure.

Tags are matched by the release workflow's `v*`, and a tag carrying a
hyphen (`v0.1.0-rc1`) publishes as a GitHub pre-release, which
`/releases/latest` skips. That is how to rehearse the whole path — workflow,
assets, checksums, a real install — without any device in the field
following it.

## The steps

**1. Tag first.** The version is `git describe`, evaluated at build time and
linked into all three binaries, and `spoloh update` compares that string
against the release tag literally. Build before tagging and every board
reports something like `v0.1.0-3-gdeadbee`, which names no release and
matches nothing.

```sh
git switch main && git pull
go test ./... -race
git tag -a v0.1.0 -m "first release"
```

**2. Push the tag.** `.github/workflows/release.yml` fires on `v*`. It
re-runs `go vet`, `gofmt`, the race suite — the tagged commit is not assumed
to have passed CI, because CI runs in a separate workflow with no dependency
on this one — then `make dist`, and uploads every file in `dist/`.

```sh
git push origin v0.1.0
```

**3. Read the assets on the release page.** Three binaries, the six
deployment files, `install.sh` and `SHA256SUMS` — eleven in all, and the
count changes whenever `deploy/` does. A partial upload is the
failure this step exists to catch: `install.sh` verifies what it downloads
against `SHA256SUMS` and stops, so a release missing one file is not
dangerous, but it is a device that does not get installed.

**4. Verify on a board that has never had Spoloh on it.** Not a re-install,
not the development board — the whole point is the path a stranger takes:

```sh
curl -fsSL https://github.com/b-ovsepian/spoloh/releases/latest/download/install.sh | sudo sh
```

It should print the access point password, then `http://spoloh.local/`
should answer and ask for an administrator password. On a board that already
has a release installed, `sudo spoloh update --check` should report the new
tag as available.

## Publishing by hand

If GitHub Actions is unavailable, `make release` produces the same `dist/`
the workflow uploads, and refuses to run unless `HEAD` is exactly a tag with
no modified tracked files — the mistake it is there to prevent is step 1's.
Then upload every file in `dist/`, `install.sh` and `SHA256SUMS` included.

```sh
make release
gh release create v0.1.0 --generate-notes dist/*
```
