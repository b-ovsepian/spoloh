VERSION ?= $(shell git describe --tags --always --dirty)
LDFLAGS := -s -w -X main.version=$(VERSION)
export CGO_ENABLED = 0

.PHONY: test lint build dist clean

test:
	go test ./... -race

build:
	go build -ldflags "$(LDFLAGS)" -o spoloh ./cmd/spoloh

# ARCH names the release artifact a binary is, which is not the same question
# as what the board can run: an armv6 binary runs happily on an armv7 board,
# so runtime.GOARCH would have an armv7 device quietly downgrade itself to the
# armv6 artifact on every update. Stamping it here is the only place that
# knows the answer.
ldflags_for = -s -w -X main.version=$(VERSION) -X main.arch=$(1)

dist:
	rm -rf dist
	mkdir -p dist
	GOOS=linux GOARCH=arm   GOARM=6 go build -ldflags "$(call ldflags_for,linux-armv6)" -o dist/spoloh-linux-armv6 ./cmd/spoloh
	GOOS=linux GOARCH=arm   GOARM=7 go build -ldflags "$(call ldflags_for,linux-armv7)" -o dist/spoloh-linux-armv7 ./cmd/spoloh
	GOOS=linux GOARCH=arm64         go build -ldflags "$(call ldflags_for,linux-arm64)" -o dist/spoloh-linux-arm64 ./cmd/spoloh
	# The deployment files ship in the release beside the binaries and are
	# covered by the same SHA256SUMS, so install.sh verifies the unit file the
	# way it verifies the binary, and always gets the one matching the version
	# it is installing.
	#
	# install.sh itself ships too. It is the only file here that is not
	# downloaded by install.sh — it is the file being run — but a release
	# that does not carry it leaves the README fetching the script from main
	# while the script installs a tagged release, so the two can drift for
	# the whole window between a merge and the next tag.
	cp deploy/install.sh deploy/spoloh.service deploy/spoloh.sysusers deploy/spoloh.tmpfiles deploy/50-spoloh-networkmanager.rules deploy/spoloh-timesync.service deploy/50-spoloh-timedate.rules deploy/spoloh-update.service deploy/50-spoloh-update.rules dist/
	cd dist && sha256sum spoloh-linux-* install.sh spoloh.service spoloh.sysusers spoloh.tmpfiles 50-spoloh-networkmanager.rules spoloh-timesync.service 50-spoloh-timedate.rules spoloh-update.service 50-spoloh-update.rules > SHA256SUMS

# `make release` is `make dist` plus the one thing a build cannot discover for
# itself: whether this commit is the tag being released. VERSION comes from
# `git describe`, which answers v0.1.0-3-gdeadbee or v0.1.0-dirty as readily
# as v0.1.0, and whatever it says is linked into all three binaries and
# reported by `spoloh version` on every board that installs them. `spoloh
# update` compares that string against the release tag literally, so a board
# stamped v0.1.0-3-gdeadbee answers `spoloh version` with something that names
# no release at all: whoever is standing in front of it, or reading its
# journal from somewhere else, has no way to say what is on it.
#
# Untracked files are deliberately not a failure. `git describe --dirty`
# ignores them too, and nothing untracked is in the build.
.PHONY: release
release:
	@tag=$$(git describe --exact-match --tags HEAD 2>/dev/null) || { \
	  echo "HEAD is not at a tag; this build would be stamped $(VERSION). Tag first - see docs/RELEASING.md" >&2; exit 1; }; \
	[ "$(VERSION)" = "$$tag" ] || { \
	  echo "tracked files are modified: this build would be stamped $(VERSION), not $$tag. Commit or stash first" >&2; exit 1; }
	$(MAKE) dist
	@echo
	@echo "dist/ is the release. Upload every file in it - the script and the checksums are assets too."

clean:
	rm -rf dist spoloh

# Regenerating the stylesheet needs Node; building the daemon does not, which is
# why internal/web/static/app.css is committed. Run this only when the design
# changes, or after any template gains a new class — Tailwind v4 emits only the
# classes it finds referenced in source, so a stale stylesheet renders quietly
# unstyled rather than failing anything. `npm ci` installs the exact toolchain
# pinned in package-lock.json (Tailwind v4 ships its CLI as the separate
# @tailwindcss/cli package, not bundled into tailwindcss itself) and this
# target then runs that local binary directly, so a clean clone always
# produces the same stylesheet instead of whatever is latest on the registry.
# First run needs nothing but Node — `npm ci` fetches everything else.
.PHONY: css
css:
	npm ci
	./node_modules/.bin/tailwindcss -i web/src/app.css -o internal/web/static/app.css --minify
