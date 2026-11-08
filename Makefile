.DEFAULT_GOAL := help
.NOTPARALLEL:

SHELL := /bin/bash -euo pipefail

GO ?= go
UV ?= uv
PYTHON ?= python3
ADB ?= adb
STATION_IP ?=
CHROME ?=
JOBS ?= 2

-include config.mk
export GO UV PYTHON JOBS MAKE

ACTIONLINT := bunx github-actionlint
TAPLO := bunx @taplo/cli
SHELLCHECK := bunx shellcheck
SHFMT ?= shfmt
C_FILES := $(wildcard internal/voice/*.c internal/voice/*.h internal/voice/testdata/*.c)
GO_HOST := CGO_ENABLED=1 GOOS="$$($(GO) env GOHOSTOS)" GOARCH="$$($(GO) env GOHOSTARCH)"

start restart: export VOICE := $(VOICE)
start restart: export BACKEND_MODEL := $(BACKEND_MODEL)
start restart: export BACKEND_THINKING_EFFORT := $(BACKEND_THINKING_EFFORT)
login: export CHROME := $(CHROME)

.PHONY: help
help:
	@printf '%s\n' \
	  'install-deps      Install Go modules, Python helpers and developer tools' \
	  'lint              Check formatting and Go, Python, C and shell lint' \
	  'lint-fix          Apply supported formatting and lint fixes' \
	  'check-fix         Apply supported fixes, then run all checks' \
	  'test              Run Python helper and Go race tests (requires uv, host Opus, pkg-config and cc)' \
	  'vet               Vet native source (requires host Opus, pkg-config and cc)' \
	  'check             Run local lint, types, dependencies, security, tests, build and CI checks' \
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

.PHONY: install-deps
install-deps:
	$(GO) mod download
	$(UV) sync --all-groups --locked

.PHONY: lint
lint:
	$(host-check)
	bun fmt --check
	$(TAPLO) fmt --check
	$(GO_HOST) golangci-lint fmt --diff
	$(GO_HOST) golangci-lint run
	$(UV) run --locked ruff check
	$(UV) run --locked ruff format --check
	$(UV) run --locked clang-format --dry-run --Werror $(C_FILES)
	$(SHFMT) -ln posix -i 2 -ci -sr -d tools/build-arm.sh
	$(SHELLCHECK) -s sh tools/build-arm.sh

.PHONY: lint-fix
lint-fix:
	bun fmt
	$(TAPLO) fmt
	$(GO_HOST) golangci-lint fmt
	$(UV) run --locked ruff check --fix
	$(UV) run --locked ruff format
	$(UV) run --locked clang-format -i $(C_FILES)
	$(SHFMT) -ln posix -i 2 -ci -sr -w tools/build-arm.sh

.PHONY: check-git
check-git:
	$(UV) run --locked prek validate-config prek.toml

.PHONY: check-types
check-types:
	$(UV) run --locked ty check .

.PHONY: check-deps
check-deps:
	$(GO) mod tidy -diff
	$(GO) mod verify
	$(UV) lock --check
	$(UV) run --locked deptry .

.PHONY: check-vulns
check-vulns:
	$(host-check)
	$(GO_HOST) $(GO) tool govulncheck -tags=nolibopusfile ./...
	$(UV) run --locked pysentry-rs .

.PHONY: check-unused
check-unused:
	$(UV) run --locked vulture

.PHONY: check-security
check-security:
	git ls-files --cached --others --exclude-standard -z -- '*.py' | xargs -0 $(UV) run --locked bandit -c pyproject.toml

.PHONY: check-c
check-c:
	@if [ "$$(uname -s)" = Darwin ]; then \
		$(UV) run --locked clang-tidy internal/voice/station_native.c internal/voice/testdata/spotter_abi.c -- -std=c11 -Wall -Wextra -Werror -isysroot "$$(xcrun --show-sdk-path)"; \
	else \
		$(UV) run --locked clang-tidy internal/voice/station_native.c internal/voice/testdata/spotter_abi.c -- -std=c11 -Wall -Wextra -Werror; \
	fi

.PHONY: check-renovate
check-renovate:
	bunx --package renovate renovate-config-validator --strict --no-global renovate.json

.PHONY: check-workflows
check-workflows:
	$(ACTIONLINT)

define host-check
	@command -v cc >/dev/null || { echo 'Install a host C compiler (cc) before native checks.' >&2; exit 1; }
	@pkg-config --exists opus || { echo 'Install host Opus and pkg-config before native checks.' >&2; exit 1; }
endef

.PHONY: test
test:
	PYTHONDONTWRITEBYTECODE=1 $(UV) run --locked python -m unittest discover -s tools -p 'test_*.py'
	$(host-check)
	$(GO_HOST) $(GO) test -mod=readonly -tags=nolibopusfile -race -count=1 -timeout=60s ./...

.PHONY: vet
vet:
	$(host-check)
	$(GO_HOST) $(GO) vet -mod=readonly -tags=nolibopusfile ./...

.PHONY: check
check: lint check-git check-types check-deps check-vulns check-unused check-security check-c test vet build check-renovate check-workflows

.PHONY: check-fix
check-fix: lint-fix
	$(MAKE) check

.PHONY: build
build:
	sh tools/build-arm.sh build

.PHONY: clean
clean:
	rm -rf build

.PHONY: connect status start stop restart logs shell login rollback enable-autostart disable-autostart
connect status start stop restart logs shell login rollback enable-autostart disable-autostart:
	$(PYTHON) tools/station.py --adb "$(ADB)" --ip "$(STATION_IP)" $@

.PHONY: install
install: build
	$(PYTHON) tools/station.py --adb "$(ADB)" --ip "$(STATION_IP)" install
