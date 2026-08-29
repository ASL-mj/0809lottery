SHELL := /bin/sh
.DEFAULT_GOAL := help

PROJECT_DIR := $(abspath $(dir $(lastword $(MAKEFILE_LIST))))
LOCAL_ENV_FILE := $(PROJECT_DIR)/lottery-bot.env
LOCAL_STATE_FILE := $(PROJECT_DIR)/data/state.json
LOCAL_VAULT_FILE := $(PROJECT_DIR)/data/vault.json

APP ?= lottery-bot
CMD ?= ./cmd/lottery-bot
BIN ?= ./bin/$(APP)
ENV_FILE ?= $(LOCAL_ENV_FILE)
PID_FILE ?= ./data/$(APP).pid
LOG_FILE ?= ./data/$(APP).log
HOST ?= 127.0.0.1
PORT ?= 18090
HEALTH_URL ?= http://$(HOST):$(PORT)/api/health

.PHONY: help fmt test race vet check build env-check run serve migrate \
	start stop restart status health logs clean

help:
	@printf '%s\n' \
		'$(APP) workbench targets:' \
		'  make check                    run tests, vet and build' \
		'  make build                    build $(BIN)' \
		'  make run                    run serve in the foreground (project env)' \
		'  make migrate                migrate legacy accounts once (project env)' \
		'  make start                  start in the background (project env)' \
		'  make stop                     stop the managed background process' \
		'  make restart                restart the managed background process' \
		'  make status                   show PID and listener state' \
		'  make health                  call the authenticated health endpoint' \
		'  make logs                     follow the background log' \
		'  make fmt                      format Go sources' \
		'  make clean                    remove only the built binary'

fmt:
	gofmt -w $$(find . -type f -name '*.go' -not -path './.git/*' -print)

test:
	go test ./... -count=1

race:
	go test -race ./... -count=1

vet:
	go vet ./...

check: test vet build

build:
	mkdir -p "$$(dirname "$(BIN)")"
	go build -o "$(BIN)" "$(CMD)"

env-check:
	@set -e; \
	actual_env="$$(realpath "$(ENV_FILE)" 2>/dev/null || true)"; \
	expected_env="$$(realpath "$(LOCAL_ENV_FILE)" 2>/dev/null || true)"; \
	if [ -z "$$actual_env" ] || [ "$$actual_env" != "$$expected_env" ]; then \
		printf 'ENV_FILE must point to the project environment: %s\n' "$(LOCAL_ENV_FILE)" >&2; \
		exit 1; \
	fi; \
	test -f "$(LOCAL_ENV_FILE)" || { \
		printf 'missing environment file: %s\n' "$(LOCAL_ENV_FILE)" >&2; \
		printf 'copy config.example.env to lottery-bot.env in this project\n' >&2; \
		exit 1; \
	}; \
	set -a; . "$(LOCAL_ENV_FILE)"; set +a; \
	if [ "$$STATE_PATH" != "$(LOCAL_STATE_FILE)" ]; then \
		printf 'STATE_PATH must point to the project data file: %s\n' "$(LOCAL_STATE_FILE)" >&2; \
		exit 1; \
	fi; \
	if [ "$$LOTTERY_VAULT_PATH" != "$(LOCAL_VAULT_FILE)" ]; then \
		printf 'LOTTERY_VAULT_PATH must point to the project data file: %s\n' "$(LOCAL_VAULT_FILE)" >&2; \
		exit 1; \
	fi

run: build env-check
	@set -a; . "$(ENV_FILE)"; set +a; exec "$(BIN)" serve

serve: run

migrate: build env-check
	@set -a; . "$(ENV_FILE)"; set +a; exec "$(BIN)" migrate

start: build env-check
	@set -e; \
	mkdir -p "$$(dirname "$(PID_FILE)")" "$$(dirname "$(LOG_FILE)")"; \
	if [ -f "$(PID_FILE)" ]; then \
		pid="$$(sed -n '1p' "$(PID_FILE)")"; \
		case "$$pid" in \
			''|*[!0-9]*) rm -f "$(PID_FILE)" ;; \
			*) if kill -0 "$$pid" 2>/dev/null; then \
				printf '%s\n' "$(APP) is already running (pid $$pid)"; \
				exit 0; \
			fi; rm -f "$(PID_FILE)" ;; \
		esac; \
	fi; \
	set -a; . "$(ENV_FILE)"; set +a; \
	nohup "$(BIN)" serve >>"$(LOG_FILE)" 2>&1 & pid=$$!; \
	printf '%s\n' "$$pid" >"$(PID_FILE)"; \
	i=0; \
	while [ "$$i" -lt 5 ]; do \
		if ! kill -0 "$$pid" 2>/dev/null; then \
			printf '%s\n' "$(APP) failed to stay running; inspect $(LOG_FILE)" >&2; \
			rm -f "$(PID_FILE)"; \
			exit 1; \
		fi; \
		i=$$((i + 1)); sleep 1; \
	done; \
	printf '%s\n' "$(APP) started (pid $$pid, log $(LOG_FILE))"

stop:
	@set -e; \
	if [ ! -f "$(PID_FILE)" ]; then \
		printf '%s\n' "$(APP) is not running (no PID file)"; \
		exit 0; \
	fi; \
	pid="$$(sed -n '1p' "$(PID_FILE)")"; \
	case "$$pid" in \
		''|*[!0-9]*) printf '%s\n' "invalid PID file: $(PID_FILE)" >&2; exit 1 ;; \
	esac; \
	if ! kill -0 "$$pid" 2>/dev/null; then \
		rm -f "$(PID_FILE)"; \
		printf '%s\n' "$(APP) is not running (stale PID file removed)"; \
		exit 0; \
	fi; \
	command_line="$$(ps -p "$$pid" -o command= 2>/dev/null || true)"; \
	case "$$command_line" in \
		*"$(APP)"*" serve"*) ;; \
		*) printf '%s\n' "PID $$pid is not owned by $(APP); refusing to stop it" >&2; exit 1 ;; \
	esac; \
	kill "$$pid"; \
	i=0; \
	while [ "$$i" -lt 10 ]; do \
		if ! kill -0 "$$pid" 2>/dev/null; then \
			rm -f "$(PID_FILE)"; \
			printf '%s\n' "$(APP) stopped"; \
			exit 0; \
		fi; \
		i=$$((i + 1)); sleep 1; \
	done; \
	printf '%s\n' "$(APP) did not stop within 10 seconds" >&2; exit 1

restart: stop start

status:
	@set -e; \
	if command -v lsof >/dev/null 2>&1; then \
		listener="$$(lsof -nP -iTCP:"$(PORT)" -sTCP:LISTEN 2>/dev/null || true)"; \
	else \
		listener=""; \
	fi; \
	if [ -f "$(PID_FILE)" ]; then \
		pid="$$(sed -n '1p' "$(PID_FILE)")"; \
		if [ -n "$$pid" ] && kill -0 "$$pid" 2>/dev/null; then \
			printf '%s\n' "process: running (pid $$pid)"; \
		else \
			printf '%s\n' 'process: stopped (stale or missing process)'; \
		fi; \
	elif [ -n "$$listener" ]; then \
		printf '%s\n' 'process: listener detected (foreground or externally managed)'; \
	else \
		printf '%s\n' 'process: stopped (no PID file)'; \
	fi; \
	if [ -n "$$listener" ]; then \
		printf '%s\n' "$$listener"; \
	elif command -v lsof >/dev/null 2>&1; then \
		printf '%s\n' "listener: none on $(HOST):$(PORT)"; \
	else \
		printf '%s\n' 'listener: lsof is unavailable'; \
	fi

health: env-check
	@set -e; \
	set -a; . "$(ENV_FILE)"; set +a; \
	if [ -z "$$WEB_USERNAME" ] || [ -z "$$WEB_PASSWORD" ]; then \
		printf '%s\n' "WEB_USERNAME and WEB_PASSWORD are required in $(ENV_FILE)" >&2; \
		exit 1; \
	fi; \
	tmpdir=/tmp; if [ -n "$$TMPDIR" ]; then tmpdir="$$TMPDIR"; fi; \
	netrc="$$(mktemp "$$tmpdir/$(APP)-health.XXXXXX")"; \
	trap 'rm -f "$$netrc"' EXIT HUP INT TERM; \
	chmod 600 "$$netrc"; \
	printf 'machine %s login %s password %s\n' "$(HOST)" "$$WEB_USERNAME" "$$WEB_PASSWORD" >"$$netrc"; \
	curl --fail --silent --show-error --netrc-file "$$netrc" "$(HEALTH_URL)"; \
	printf '\n'

logs:
	@test -f "$(LOG_FILE)" || { printf '%s\n' "log file does not exist: $(LOG_FILE)" >&2; exit 1; }; \
	tail -f "$(LOG_FILE)"

clean:
	rm -f "$(BIN)"
