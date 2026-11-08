.DEFAULT_GOAL := help
.NOTPARALLEL:

GO ?= go
UV ?= uv
PYTHON ?= python3
ADB ?= adb
STATION_IP ?=
CHROME ?=
JOBS ?= 2

-include config.mk
export GO UV PYTHON JOBS MAKE

start restart: export VOICE := $(VOICE)
start restart: export BACKEND_MODEL := $(BACKEND_MODEL)
start restart: export BACKEND_THINKING_EFFORT := $(BACKEND_THINKING_EFFORT)
login: export CHROME := $(CHROME)

.PHONY: help setup fmt test vet check build clean connect status install start stop restart logs shell login rollback enable-autostart disable-autostart

help:
	@printf '%s\n' \
	  'setup             Prepare Go modules, pinned Zig and ARM Opus (no system packages)' \
	  'fmt               Format Go source in place' \
	  'test              Run native race tests (requires host Opus, pkg-config and cc)' \
	  'vet               Vet native source (requires host Opus, pkg-config and cc)' \
	  'check             Check formatting, helpers, tests and vet without formatting writes' \
	  'build             Build ARM build/yandex-station-chatgpt-voice' \
	  'clean             Remove build output; retain config, credentials and toolchain cache' \
	  'connect           Connect ADB to STATION_IP:5555' \
	  'status            Inspect root, install, process, session permissions and token expiry' \
	  'install           Build, verify, stop the old client and install; leave stopped' \
	  'login             Sign in to ChatGPT in a private Chrome window; the Station saves a renewable session' \
	  'start             Start the installed client with bounded process recovery' \
	  'stop              Stop the verified client and pause autostart until make start' \
	  'restart           Stop and start the installed client' \
	  'rollback          Restore client.previous without changing credentials; leave stopped' \
	  'enable-autostart  Enable boot startup with the last make start settings' \
	  'disable-autostart Remove our boot hook without stopping the running client' \
	  'logs              Follow /tmp/yandex-station-chatgpt-voice.log' \
	  'shell             Open the selected Station shell' \
	  '' \
	  'Copy config.example.mk to config.mk and set STATION_IP before device commands.' \
	  'First run: make install, make login, then make start.' \
	  'Use make install restart to install and explicitly activate an update.'

setup:
	$(GO) list -mod=readonly -tags=nolibopusfile -deps ./cmd/yandex-station-chatgpt-voice >/dev/null
	sh tools/build-arm.sh setup

fmt:
	"$$($(GO) env GOROOT)/bin/gofmt" -w cmd internal

define host-check
	@command -v cc >/dev/null || { echo 'Install a host C compiler (cc) before native checks.' >&2; exit 1; }
	@pkg-config --exists opus || { echo 'Install host Opus and pkg-config before native checks.' >&2; exit 1; }
endef

test:
	$(host-check)
	CGO_ENABLED=1 GOOS="$$($(GO) env GOHOSTOS)" GOARCH="$$($(GO) env GOHOSTARCH)" $(GO) test -mod=readonly -tags=nolibopusfile -race -count=1 -timeout=60s ./...

vet:
	$(host-check)
	CGO_ENABLED=1 GOOS="$$($(GO) env GOHOSTOS)" GOARCH="$$($(GO) env GOHOSTARCH)" $(GO) vet -mod=readonly -tags=nolibopusfile ./...

check:
	@files="$$("$$($(GO) env GOROOT)/bin/gofmt" -l cmd internal)"; test -z "$$files" || { printf 'Run make fmt for:\n%s\n' "$$files" >&2; exit 1; }
	sh -n tools/build-arm.sh
	$(PYTHON) -c 'import ast; from pathlib import Path; [ast.parse(p.read_text(), filename=str(p)) for p in Path("tools").glob("*.py")]'
	PYTHONDONTWRITEBYTECODE=1 $(PYTHON) -m unittest discover -s tools -p 'test_*.py'
	$(MAKE) test vet

build:
	sh tools/build-arm.sh build

clean:
	rm -rf build

connect status start stop restart logs shell login rollback enable-autostart disable-autostart:
	$(PYTHON) tools/station.py --adb "$(ADB)" --ip "$(STATION_IP)" $@

install: build
	$(PYTHON) tools/station.py --adb "$(ADB)" --ip "$(STATION_IP)" install
