VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
PLATFORMS := darwin/arm64 darwin/amd64 linux/amd64 linux/arm64 windows/amd64

.PHONY: build test vet dist clean

build:
	go build -ldflags "$(LDFLAGS)" -o bin/lt ./cmd/lt
	go build -o bin/testserver ./cmd/testserver

test:
	go test -race ./...

vet:
	go vet ./...
	@test -z "$$(gofmt -l .)" || (gofmt -l . && exit 1)

# Cross-compiled binaries in dist/.
dist:
	@mkdir -p dist
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; ext=""; [ $$os = windows ] && ext=.exe; \
		echo "dist/lt-$$os-$$arch$$ext"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -ldflags "$(LDFLAGS)" -o dist/lt-$$os-$$arch$$ext ./cmd/lt || exit 1; \
	done

clean:
	rm -rf bin dist
