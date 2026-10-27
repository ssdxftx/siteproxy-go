# siteproxy-go — native Go build
BINARY     := siteproxy-go
VERSION    := $(shell date +%Y%m%d)
GO         ?= go
LDFLAGS    := -s -w

.PHONY: all build test vet clean cross

all: build

build:
	$(GO) build -ldflags "$(LDFLAGS)" -o $(BINARY) .

test:
	$(GO) test -timeout 120s ./...

vet:
	$(GO) vet ./...

clean:
	rm -rf dist $(BINARY)

# Cross-compile static binaries (CGO disabled => single self-contained file).
TARGETS := \
	linux/amd64 linux/arm64 \
	darwin/amd64 darwin/arm64 \
	windows/amd64 \
	freebsd/amd64

cross:
	@mkdir -p dist
	@set -e; for t in $(TARGETS); do \
		os=$${t%%/*}; arch=$${t##*/}; \
		suffix=""; [ "$$os" = "windows" ] && suffix=".exe"; \
		echo "== $${os}/$${arch} =="; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch \
			$(GO) build -trimpath -ldflags "$(LDFLAGS)" \
			-o dist/$(BINARY)-$${os}-$${arch}$$suffix .; \
	done
	@ls -lh dist/
