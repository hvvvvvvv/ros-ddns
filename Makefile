ifeq ($(OS),Windows_NT)
GO ?= scripts\go.cmd
BIN ?= build/bin/ros-ddns.exe
RM = cmd /c rmdir /s /q
else
GO ?= go
BIN ?= build/bin/ros-ddns
RM = rm -rf
endif

VERSION ?= dev
COMMIT ?= unknown
BUILD_DATE ?= unknown
LDFLAGS = -X ros-ddns/internal/version.Version=$(VERSION) -X ros-ddns/internal/version.Commit=$(COMMIT) -X ros-ddns/internal/version.BuildDate=$(BUILD_DATE)
LINUX_BIN ?= build/bin/ros-ddns-linux-amd64
DOCKER ?= docker
IMAGE ?= ros-ddns:local

.PHONY: build build-linux docker-build test vet fmt check run clean clean-tmp clean-cache

build:
	$(GO) build -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/ros-ddns

# Cross-build without a C toolchain. These targets are intended for Linux/CI;
# the Windows recipe also works with a Windows make using cmd.exe.
build-linux:
ifeq ($(OS),Windows_NT)
	set "GOOS=linux" && set "GOARCH=amd64" && set "CGO_ENABLED=0" && $(GO) build -trimpath -buildvcs=false -ldflags "$(LDFLAGS)" -o $(LINUX_BIN) ./cmd/ros-ddns
else
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 $(GO) build -trimpath -buildvcs=false -ldflags "$(LDFLAGS)" -o $(LINUX_BIN) ./cmd/ros-ddns
endif

docker-build:
	$(DOCKER) build --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) --build-arg BUILD_DATE=$(BUILD_DATE) -t $(IMAGE) .

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

fmt:
	gofmt -l -w cmd internal

check: fmt vet test

run: build
	$(BIN) -config configs/config.example.yaml

# All non-source artifacts live under build/. Never blanket-delete build/: it
# also holds local config, runtime state, logs and credentials.
clean:
	-$(RM) build/bin

# Disposable debug output only; keeps config, state and credentials intact.
clean-tmp:
	-$(RM) build/tmp
	-$(RM) build/logs

# Go caches; recreated on the next build. No config, state or credentials.
clean-cache:
	-$(RM) build/.go-cache
