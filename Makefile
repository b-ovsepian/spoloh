VERSION ?= $(shell git describe --tags --always --dirty)
LDFLAGS := -s -w -X main.version=$(VERSION)
export CGO_ENABLED = 0

.PHONY: test lint build dist clean

test:
	go test ./... -race

build:
	go build -ldflags "$(LDFLAGS)" -o spoloh ./cmd/spoloh

# One binary per supported board family. CGO is off, so this needs no cross
# toolchain: a laptop builds every target.
dist:
	mkdir -p dist
	GOOS=linux GOARCH=arm   GOARM=6 go build -ldflags "$(LDFLAGS)" -o dist/spoloh-linux-armv6 ./cmd/spoloh
	GOOS=linux GOARCH=arm   GOARM=7 go build -ldflags "$(LDFLAGS)" -o dist/spoloh-linux-armv7 ./cmd/spoloh
	GOOS=linux GOARCH=arm64         go build -ldflags "$(LDFLAGS)" -o dist/spoloh-linux-arm64 ./cmd/spoloh
	cd dist && sha256sum spoloh-linux-* > SHA256SUMS

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
