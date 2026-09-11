#!/bin/sh
# Install Spoloh on a Raspberry Pi or similar ARM board running Raspberry Pi
# OS Lite or Armbian. Run as root:
#
#   curl -fsSL https://raw.githubusercontent.com/b-ovsepian/spoloh/main/deploy/install.sh | sh
#
# Everything it fetches is verified against the SHA256SUMS published with the
# release. The variables below exist so the test can run this with fakes; an
# operator never sets them.
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

  FILES="spoloh-$ARCH spoloh.service spoloh.sysusers spoloh.tmpfiles 50-spoloh-networkmanager.rules"

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
    [ "$want" = "$got" ] || die "checksum mismatch for $f: the download is corrupt or tampered with, nothing was installed"
  done
  say "checksums verified"

  # 4. Dependencies. avahi-daemon is what makes spoloh.local resolve;
  #    network-manager is what lets the device raise its own access point.
  if command -v apt-get >/dev/null 2>&1; then
    say "installing packages"
    DEBIAN_FRONTEND=noninteractive apt-get update -qq
    DEBIAN_FRONTEND=noninteractive apt-get install -y -qq alsa-utils ffmpeg avahi-daemon network-manager
  else
    say "warning: no apt-get; install alsa-utils, ffmpeg, avahi-daemon and network-manager yourself"
  fi

  # 5. Files.
  install -D -m 0755 "spoloh-$ARCH"                "$PREFIX/usr/local/bin/spoloh"
  install -D -m 0644 spoloh.service                "$PREFIX/etc/systemd/system/spoloh.service"
  install -D -m 0644 spoloh.sysusers               "$PREFIX/usr/lib/sysusers.d/spoloh.conf"
  install -D -m 0644 spoloh.tmpfiles               "$PREFIX/usr/lib/tmpfiles.d/spoloh.conf"
  install -D -m 0644 50-spoloh-networkmanager.rules "$PREFIX/etc/polkit-1/rules.d/50-spoloh-networkmanager.rules"

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
