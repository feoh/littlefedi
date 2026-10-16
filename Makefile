GO ?= go
VERSION ?= $(shell cat VERSION)
TAGS ?=
DIST_DIR ?= dist
BINARY_PREFIX ?= littlefedi
BUILD_FLAGS := -trimpath -buildvcs=false
LINK_FLAGS := -s -w -buildid= -X brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/version.Version=$(VERSION)

.PHONY: build build-sqlite build-postgres build-s3 build-postgres-s3 build-blog build-all release allreleases licenses

build: build-sqlite

build-sqlite:
	CGO_ENABLED=0 $(GO) build $(BUILD_FLAGS) -ldflags="$(LINK_FLAGS)" -tags "$(TAGS)" -o littlefedi ./cmd/littlefedi

build-postgres:
	CGO_ENABLED=0 $(GO) build $(BUILD_FLAGS) -ldflags="$(LINK_FLAGS)" -tags "$(TAGS) postgres" -o littlefedi-postgres ./cmd/littlefedi

build-s3:
	CGO_ENABLED=0 $(GO) build $(BUILD_FLAGS) -ldflags="$(LINK_FLAGS)" -tags "$(TAGS) s3" -o littlefedi-s3 ./cmd/littlefedi

build-postgres-s3:
	CGO_ENABLED=0 $(GO) build $(BUILD_FLAGS) -ldflags="$(LINK_FLAGS)" -tags "$(TAGS) postgres s3" -o littlefedi-postgres-s3 ./cmd/littlefedi

build-blog:
	CGO_ENABLED=0 $(GO) build $(BUILD_FLAGS) -ldflags="$(LINK_FLAGS)" -tags "$(TAGS) blog" -o littlefedi-blog ./cmd/littlefedi

build-all: build-sqlite build-postgres build-s3 build-postgres-s3 build-blog

# Use the same targets as the repository's release builds.
RELEASE_COMMON := darwin/amd64 darwin/arm64 linux/amd64 linux/arm64 linux/arm \
	freebsd/amd64 freebsd/arm64 illumos/amd64 netbsd/amd64 netbsd/arm \
	openbsd/amd64 openbsd/arm64 windows/amd64
RELEASE_EXCLUDE := ios/% js/% wasip1/% android/386 android/amd64 android/arm
RELEASE_ALL = $(filter-out $(RELEASE_EXCLUDE),$(shell $(GO) tool dist list))

release: RELEASE_TARGETS = $(RELEASE_COMMON)
allreleases: RELEASE_TARGETS = $(RELEASE_ALL)
release allreleases:
	@mkdir -p "$(DIST_DIR)"
	@set -e; for target in $(RELEASE_TARGETS); do \
		goos=$${target%/*}; goarch=$${target#*/}; \
		if [ "$$goarch" = "arm" ]; then \
			for armv in 6 7; do \
				name="$(BINARY_PREFIX)-armv$${armv}-$${goos}-$${goarch}"; \
				echo "=> $$name"; \
				CGO_ENABLED=0 GOARM=$$armv GOOS=$$goos GOARCH=$$goarch \
					$(GO) build $(BUILD_FLAGS) -ldflags="$(LINK_FLAGS)" -tags "blog $(TAGS)" \
					-o "$(DIST_DIR)/$$name" ./cmd/littlefedi; \
			done; \
		else \
			ext=; [ "$$goos" = "windows" ] && ext=".exe"; \
			name="$(BINARY_PREFIX)-$${goos}-$${goarch}$${ext}"; \
			echo "=> $$name"; \
			CGO_ENABLED=0 GOOS=$$goos GOARCH=$$goarch \
				$(GO) build $(BUILD_FLAGS) -ldflags="$(LINK_FLAGS)" -tags "blog $(TAGS)" \
				-o "$(DIST_DIR)/$$name" ./cmd/littlefedi; \
		fi; \
	done
	@cp LICENSE NOTICE.md THIRD_PARTY_LICENSES.md "$(DIST_DIR)/"

licenses:
	$(GO) run ./tools/licenses > THIRD_PARTY_LICENSES.md.tmp
	mv THIRD_PARTY_LICENSES.md.tmp THIRD_PARTY_LICENSES.md
