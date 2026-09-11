#!/bin/sh
# Install Spoloh on a Raspberry Pi or similar ARM board running Raspberry Pi
# OS Lite or Armbian. Run as root:
#
#   curl -fsSL https://github.com/b-ovsepian/spoloh/releases/latest/download/install.sh | sudo sh
#
# Everything it fetches is checked against the SHA256SUMS published with the
# release. That file comes over HTTPS from the same host as the files it
# describes and carries no signature, so it proves a download arrived as
# published - not that what was published is legitimate. The variables below
# exist so the test can run this with fakes; an operator never sets them.
set -eu

BASE_URL="${SPOLOH_BASE_URL:-https://github.com/b-ovsepian/spoloh/releases}"
VERSION="${SPOLOH_VERSION:-latest}"
PREFIX="${SPOLOH_PREFIX:-}"

say() { printf '%s\n' "$*"; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

# Everything below runs inside main(), which the last line of the file calls.
# This script is delivered as `curl ... | sudo sh`, and sh executes whatever
# bytes arrive: a straight-line script whose download is cut short runs its
# first half and exits 0 - installing packages, placing no files, and
# reporting success. Inside a function, a truncated copy is either a syntax
# error or a definition nobody calls. Either way it changes nothing.
main() {
  [ "$(id -u)" = 0 ] || [ -n "$PREFIX" ] || die "run this as root (try: curl ... | sudo sh)"

  # 1. Which board is this?
  case "$(uname -m)" in
    armv6l)          ARCH=linux-armv6 ;;
    armv7l|armv8l)   ARCH=linux-armv7 ;;
    aarch64|arm64)   ARCH=linux-arm64 ;;
    *) die "unsupported architecture $(uname -m); spoloh ships armv6, armv7 and arm64 builds" ;;
  esac
  say "board: $(uname -m) -> $ARCH"

  # 2. Which release? /releases/latest redirects to the tag, so this needs no
  #    API token and no JSON parser.
  if [ "$VERSION" = latest ]; then
    DOWNLOAD="$BASE_URL/latest/download"
  else
    DOWNLOAD="$BASE_URL/download/$VERSION"
  fi

  # 3. Fetch everything into one directory and verify it before touching the
  #    system. A half-installed device is worse than one that was never touched.
  TMP="$(mktemp -d)"
  # Split rather than one `trap ... EXIT INT TERM`: a handler for INT or TERM
  # that never calls exit does not terminate the process under POSIX, it just
  # resumes at the next statement. That would let a volunteer's Ctrl-C during
  # the apt-get step delete TMP out from under the still-running script and let
  # it fall through to the file-placement step with its downloads gone -
  # packages installed, binary not, the half-installed device this section's
  # own comment exists to prevent. Each signal handler exits explicitly, which
  # fires the EXIT trap in turn, so cleanup still happens in exactly one place.
  trap 'rm -rf "$TMP"' EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM
  cd "$TMP"

  FILES="spoloh-$ARCH spoloh.service spoloh.sysusers spoloh.tmpfiles 50-spoloh-networkmanager.rules spoloh-timesync.service 50-spoloh-timedate.rules"

  for f in $FILES SHA256SUMS; do
    say "downloading $f"
    curl -fsSL -o "$f" "$DOWNLOAD/$f" || die "could not download $f from $DOWNLOAD"
  done

  # Checked one file at a time against its own line, rather than with
  # `sha256sum -c`: the release also carries the artifacts for the other two
  # boards, which are not downloaded here, and the flag that would skip them
  # (--ignore-missing) is a GNU coreutils extension. Single-file mode is the
  # part every sha256sum agrees on.
  for f in $FILES; do
    want=$(awk -v n="$f" '$2 == n { print $1 }' SHA256SUMS)
    [ -n "$want" ] || die "the release publishes no checksum for $f; it may not have finished uploading"
    got=$(sha256sum "$f" | awk '{ print $1 }')
    # "corrupt" and not "tampered with": the checksum came over HTTPS from
    # the same host as the file, so a mismatch proves the download does not
    # match what that host published, and nothing about whether what it
    # published is legitimate.
    [ "$want" = "$got" ] || die "checksum mismatch for $f: it does not match what the release publishes, so the download did not arrive intact; nothing was installed"
  done
  say "checksums verified"

  # 4. Dependencies. avahi-daemon is what makes spoloh.local resolve;
  #    network-manager is what lets the device raise its own access point;
  #    systemd-timesyncd is what the /time page switches on and points at an
  #    NTP server, and it is a separate package on Debian rather than part of
  #    systemd itself - a board without it shows the time settings and has
  #    nothing behind the synchronisation switch.
  if command -v apt-get >/dev/null 2>&1; then
    say "installing packages"
    DEBIAN_FRONTEND=noninteractive apt-get update -qq
    DEBIAN_FRONTEND=noninteractive apt-get install -y -qq alsa-utils ffmpeg avahi-daemon network-manager systemd-timesyncd
  else
    say "warning: no apt-get; install alsa-utils, ffmpeg, avahi-daemon, network-manager and systemd-timesyncd yourself"
  fi

  # 5. Files.
  install -D -m 0755 "spoloh-$ARCH"                "$PREFIX/usr/local/bin/spoloh"
  install -D -m 0644 spoloh.service                "$PREFIX/etc/systemd/system/spoloh.service"
  install -D -m 0644 spoloh.sysusers               "$PREFIX/usr/lib/sysusers.d/spoloh.conf"
  install -D -m 0644 spoloh.tmpfiles               "$PREFIX/usr/lib/tmpfiles.d/spoloh.conf"
  install -D -m 0644 50-spoloh-networkmanager.rules "$PREFIX/etc/polkit-1/rules.d/50-spoloh-networkmanager.rules"
  # The oneshot that carries the configured NTP server into
  # /etc/systemd/timesyncd.conf.d/, and the polkit rule that lets the daemon
  # set the clock and start that one unit by name. Neither is enabled: the
  # daemon starts the unit itself when the operator saves a time server.
  install -D -m 0644 spoloh-timesync.service       "$PREFIX/etc/systemd/system/spoloh-timesync.service"
  install -D -m 0644 50-spoloh-timedate.rules      "$PREFIX/etc/polkit-1/rules.d/50-spoloh-timedate.rules"

  # 6. The user and the directories. Without systemd-tmpfiles --create the
  #    unit's ReadWritePaths=-/etc/spoloh silently skips its write exception and
  #    the daemon crash-loops on a read-only /etc.
  systemd-sysusers
  systemd-tmpfiles --create

  # 7. The name people type. avahi publishes the hostname, so this is what makes
  #    http://spoloh.local work.
  if [ "$(hostname)" != spoloh ] && command -v hostnamectl >/dev/null 2>&1; then
    say "setting hostname to spoloh"
    hostnamectl set-hostname spoloh
    # avahi is already running on a Raspberry Pi OS Lite image, already
    # publishing raspberrypi.local, and is not reliably notified of a
    # hostname change - so without this the banner below would print a
    # spoloh.local that does not resolve. try-restart does nothing at all if
    # avahi is not installed, so it costs nothing to always run it.
    systemctl try-restart avahi-daemon
  fi

  # Raspberry Pi OS ships `127.0.1.1 raspberrypi`, and a hostname that line
  # does not name makes every later sudo print "unable to resolve host".
  # Harmless to the daemon, alarming to a volunteer who has just set the
  # device up and has no way to tell a cosmetic warning from a real one.
  #
  # Deliberately outside the rename above rather than part of it. A device
  # installed by an earlier version of this script is already called spoloh
  # and still has raspberrypi in its hosts file, because that version never
  # touched it; a repair that only ran on the renaming pass would skip
  # exactly the devices that have been in the field longest. So this asks
  # what the hostname is now - whoever set it - and makes the line agree.
  #
  # What it will not do: change anything when the line already names the
  # host (the second grep is what makes a re-run a true no-op, not a rewrite
  # with the same bytes), touch a hosts file that does not have exactly one
  # 127.0.1.1 line (none means somebody removed it on purpose; more than one
  # means an arrangement where picking "the" host entry is a guess, and this
  # is a file an operator may have configured deliberately), drop whatever
  # follows the name on that line (aliases are somebody's addition and only
  # the name is replaced), move the line (hence sed in place rather than a
  # filter and an append), or write an entry with no name on it at all.
  #
  # The replacement carries a literal tab rather than \t: POSIX does not say
  # what a backslash escape means in a sed replacement, so implementations
  # are free to differ and a literal tab is the spelling that cannot be read
  # two ways.
  #
  # It is copied over the existing file rather than renamed into place, so
  # /etc/hosts keeps the mode and owner it has instead of whatever this
  # shell's umask would have given it.
  #
  # `hostname` is read with its failure absorbed. This is a bare assignment,
  # not an `if` condition, so under `set -eu` a board where hostname is
  # missing or fails would otherwise kill the script here - after every file
  # is placed and the host renamed, and before the unit is ever enabled. A
  # cosmetic repair must not be able to produce a half-installed device.
  HOSTS="$PREFIX/etc/hosts"
  NAME="$(hostname 2>/dev/null || true)"
  if [ -n "$NAME" ] && [ -f "$HOSTS" ] &&
     [ "$(grep -c '^127\.0\.1\.1[[:space:]]' "$HOSTS")" = 1 ] &&
     ! grep -q "^127\.0\.1\.1[[:space:]][[:space:]]*$NAME\([[:space:]]\|$\)" "$HOSTS"; then
    say "pointing the 127.0.1.1 entry in /etc/hosts at $NAME"
    sed "s|^\(127\.0\.1\.1\)[[:space:]][[:space:]]*[^[:space:]]*|\1	$NAME|" "$HOSTS" > "$TMP/hosts"
    cat "$TMP/hosts" > "$HOSTS"
  fi

  # 8. Start it. `enable --now` is not enough on a re-run: on an already-active
  #    unit it is a no-op, so the new binary and unit file would sit on disk
  #    while the old process kept executing the old inode, indefinitely - and
  #    `spoloh update` execs the on-disk binary to learn its version, so the
  #    device would report itself up to date forever while running old code.
  #    `restart` starts a stopped unit too, so this is also correct on a first
  #    install.
  systemctl daemon-reload
  systemctl enable spoloh
  systemctl restart spoloh

  # 9. The access point password. The daemon generates it on first start, so it
  #    is read back rather than generated here: a second implementation of the
  #    same rule would drift from the first. Until this line prints it, that
  #    password exists only in the journal, which is exactly where a volunteer
  #    with no monitor cannot reach it.
  CONFIG="$PREFIX/etc/spoloh/config.json"
  WAIT="${SPOLOH_AP_WAIT:-30}"
  AP=""
  i=0
  while :; do
    if [ -f "$CONFIG" ]; then
      AP=$(sed -n 's/.*"ap_password"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$CONFIG")
      [ -n "$AP" ] && break
    fi
    i=$((i + 1))
    [ "$i" -gt "$WAIT" ] && break
    sleep 1
  done

  say ""
  say "================================================================"
  say " Spoloh is installed."
  say ""
  if [ -n "$AP" ]; then
    say " Wi-Fi access point:  Spoloh-Setup"
    say " Password:            $AP"
    # nmcli device wifi hotspot uses NetworkManager's shared mode, whose
    # gateway is 10.42.0.1. Without this the banner names a network to join
    # and no address to open once you are on it.
    say " Then open:           http://10.42.0.1/"
    say ""
    say " Write this down. If the device cannot reach a network it raises"
    say " this access point, and it is the only way back in."
  else
    say " Could not read the access point password from $CONFIG."
    say " Check: systemctl status spoloh"
  fi
  say ""
  say " Dashboard:           http://spoloh.local/"
  for ip in $(hostname -I 2>/dev/null || true); do
    say "                      http://$ip/"
  done
  say "================================================================"
}

main "$@"
