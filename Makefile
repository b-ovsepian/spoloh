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
