VERSION ?= $(shell cat VERSION 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build dist package test clean

build:
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o trains .

dist:
	for arch in amd64 arm64; do \
		CGO_ENABLED=0 GOOS=linux GOARCH=$$arch go build -trimpath -ldflags '$(LDFLAGS)' -o dist/trains-linux-$$arch . ; \
	done

# package adds a tarball and a zip per architecture with the binary,
# deploy/ and the example config, plus checksums: what a release publishes.
package: dist
	for arch in amd64 arm64; do \
		tar -czf dist/trains-$(VERSION)-linux-$$arch.tar.gz --transform 's|^dist/||' \
			dist/trains-linux-$$arch deploy config.example.yaml ; \
		rm -f dist/trains-$(VERSION)-linux-$$arch.zip ; \
		zip -qr dist/trains-$(VERSION)-linux-$$arch.zip deploy config.example.yaml ; \
		(cd dist && zip -q trains-$(VERSION)-linux-$$arch.zip trains-linux-$$arch) ; \
	done
	cd dist && sha256sum trains-* > SHA256SUMS

test:
	go vet ./...
	go test ./...

clean:
	rm -rf trains dist
