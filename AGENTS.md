# Working on Spoloh

Spoloh (Сполох) announces air raid alerts through a PA system or a speaker.
It runs on an ARM single-board computer, on someone's wall, and its job is to
be heard. Every rule below follows from that.

Read this before changing anything. It is not style advice — most of it was
learned by shipping a bug.

## The one rule everything else serves

**A device that stays silent during an air raid is the worst outcome this
project has.** When two choices conflict, take the one that still makes a
sound.

Concretely, and each of these is real code here:

- A missing alert event is restored on load with a loud log line, not
  rejected. Refusing to start over a repairable config trades a siren for a
  clean error.
- A stored poll interval below the minimum is raised on load, not rejected —
  a config written by an older version must never stop the daemon starting.
  The same value typed into a form *is* rejected, because a person asking
  for something impossible should be told so.
- A volume that cannot be set does not stop playback. Some cards have no
  mixer control at all; that is normal, not a fault.
- A dashboard that fails to bind retries with back-off. It does not take the
  daemon down, and it does not give up either.
- Quiet hours may silence a scheduled event. They may silence an alert too,
  but only because the operator typed a word to say so, and never without
  writing the suppression to the event log — a device that chooses silence
  during a raid and cannot say afterwards that it chose it is
  indistinguishable from a broken one.
- A preview is not an announcement. Listening to a sound, or testing an
  output, plays below everything real: the single voice must never be held
  by someone finding out what a file sounds like while a siren waits.

## Constraints

- **Go 1.25+, `CGO_ENABLED=0` always.** One laptop cross-compiles armv6,
  armv7 and arm64.
- **No new Go dependencies.** The module graph does not grow.
- **No runtime CDN.** Tailwind is compiled and the CSS is committed. The one
  exception is the alerts.in.ua map iframe on the dashboard, which is
  deliberately an external embed and which the page must survive not
  loading.
- **Server-rendered `html/template` + Tailwind + Basecoat + Alpine.js.**
  No SPA, no build step at runtime.
- **Nothing may depend on JavaScript being alive.** Progressive enhancement
  only. This is safety equipment: every control must work with scripts off.
- **All operator-visible text is Ukrainian. All code, comments, commit
  messages and test names are English.**

## Who this is for

Anyone who wants an air raid announcer — a community, a workshop, a
household, a school. When an example would make a comment concrete, vary it
or keep it generic: "the site", "the operator", "whoever installed it". Do
not make one setting the stock illustration for everything.

## Testing

**Every new test must be proven able to fail.** Break the code it covers,
watch that specific test fail with a message naming the real problem, then
restore. A test that passes against broken code is worse than no test,
because it is evidence that is not evidence.

This is not ceremony. A total, 100%-reproducible failure — every non-WAV
upload on every board since the feature shipped — hid behind a green suite
for weeks, because the fake converter in the tests wrote a valid WAV to any
destination name, and the real ffmpeg needs the name to end in `.wav`. **A
fake that is more permissive than the real thing is not a test double, it is
a blindfold.**

No test may:

- open a socket to the internet
- really run `nmcli`, `aplay`, `amixer`, `ffmpeg`, `timedatectl`,
  `systemctl`, `apt-get` or `curl`
- touch a network interface
- sleep on a real clock
- require root

Every external command goes through an injectable `RunFunc` seam. See
`internal/player/alsa.go`, `internal/netmgr/nmcli.go` and
`internal/clock/sync.go` and copy the shape.

Capture real output into `testdata/` rather than inventing it. There are
verbatim captures from a Raspberry Pi 4 in `internal/player/testdata/`.

`internal/web` takes about 30 seconds; run a focused `-run` while iterating
and the whole package once before committing.

## Hardware truths worth knowing

Learned on a Raspberry Pi 4 running Debian 13, and they surprise people:

- `default` resolves to **HDMI**. With nothing plugged into HDMI, every
  playback fails with `audio open error: Unknown error 524` (ENOTSUPP).
  The device ships pointed at the one output that usually cannot play.
- The HDMI card reports **no simple mixer controls at all**. The headphone
  card's control is called `PCM`, not `Master`.
- `aplay -L`'s output is merged with ALSA-lib diagnostics on stdout, so a
  line in column 0 containing whitespace is a warning, not a PCM name.
- `--dump-hw-params` cannot tell a working output from a broken one: it
  exits non-zero on both. Probing means playing a short silent WAV.

## Security

- `Auth.PasswordHash` and `Auth.SessionSecret` must never reach a page.
  `internal/web/leak_test.go` walks every route and enforces it.
- Session cookies are `SameSite=Lax`; that is the real CSRF defence.
  `sameOrigin` deliberately allows an opaque `Origin: null`, because real
  Chrome sends it for ordinary same-origin form posts to this device over
  plain HTTP.
- Any redirect target taken from a form goes through `returnPath` first. A
  `Location` built from user input is an open redirect the moment it can
  name a host.
- The AP password is logged exactly once, at generation, and nowhere else.
  Never put a PSK in an error string.
- polkit rules grant named actions only, and `manage-units` is pinned to one
  unit and one verb. A broad grant would let an HTTP request restart
  anything on the board.

## Conventions

- Comments say **why**, not what. If a line looks wrong but is right, the
  comment explaining that is the point of the line.
- Match the density and voice of the code around you.
- Commit messages: what changed, and what made it necessary. If a real
  device taught you something, say so.
