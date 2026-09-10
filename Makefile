APP := fma
BUILD_DATETIME := $(shell date -u +%Y%m%dT%H%M%SZ)
VERSION ?= dev-$(BUILD_DATETIME)
COMMIT ?= $(shell git rev-parse HEAD 2>/dev/null || echo unknown)
RELEASE_TIME ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.releaseTime=$(RELEASE_TIME)
export FMA_BUILD_LDFLAGS := $(LDFLAGS)
ifeq ($(shell id -u),0)
PREFIX ?= /usr/local
SYSTEMD_USER_DIR := /etc/systemd/system
CONFIG_DIR := /etc/fma
SYSTEMD_FLAGS :=
else
PREFIX ?= $(HOME)/.local
SYSTEMD_USER_DIR := $(HOME)/.config/systemd/user
CONFIG_DIR := $(HOME)/.config/fma
SYSTEMD_FLAGS := --user
endif
BINDIR := $(PREFIX)/bin
GENERATED := deploy/generated
-include $(GENERATED)/install.mk
SERVICE := $(SYSTEMD_USER_DIR)/$(APP).service

.PHONY: build check test benchmark install install-config uninstall
build: check
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(APP) .

check:
	@test -z "$$(gofmt -l *.go)" || { echo 'Run gofmt -w *.go'; exit 1; }
	@test "$$(ls -1 *.go)" = "$$(printf 'main.go\nmain_test.go')" || { echo 'Keep Go code in main.go and main_test.go'; exit 1; }
	go vet ./...

test: check
	go test -race -ldflags "$(LDFLAGS)" ./...
	python3 scripts/verify.py
	python3 scripts/test_deploy.py
	python3 scripts/test_install.py
	python3 scripts/test_streaming.py --mail --size-mib 32

install-config:
	@for file in install.mk fma.service s3.env outbound.env nginx-http.conf nginx-https.conf nginx-stream.conf renew-hook.sh; do \
		test -s "$(GENERATED)/$$file" || { echo "配置文件没有找到: $(GENERATED)/$$file — 请先运行 bash deploy/gen.sh" >&2; exit 1; }; \
	done
	@test "$$(id -un)" = "$(DEPLOY_USER)" && test "$$(id -u)" = "$(DEPLOY_UID)" || { echo 'Run install as the user and UID selected in deploy/gen.sh' >&2; exit 1; }
	@command -v systemctl >/dev/null || { echo 'Installation requires Linux with systemd' >&2; exit 1; }

install: install-config
	@if test "$$(id -u)" = 0 && { test -e /bin/fma || test -L /bin/fma; }; then \
		test -L /bin/fma && test "$$(readlink /bin/fma)" = "$(BINDIR)/$(APP)" || { echo '/bin/fma is occupied by another installation' >&2; exit 1; }; \
	fi
	@echo "WARNING: UID $$(id -u); systemctl $(SYSTEMD_FLAGS); config $(CONFIG_DIR)"
	$(MAKE) build
	install -d "$(BINDIR)" "$(SYSTEMD_USER_DIR)"
	install -d -m 700 "$(CONFIG_DIR)"
	install -m 600 "$(GENERATED)/s3.env" "$(CONFIG_DIR)/s3.env"
	install -m 600 "$(GENERATED)/outbound.env" "$(CONFIG_DIR)/outbound.env"
	install -m 755 $(APP) "$(BINDIR)/$(APP).new"
	mv "$(BINDIR)/$(APP).new" "$(BINDIR)/$(APP)"
	@if test "$$(id -u)" = 0 && ! test -L /bin/fma; then ln -s "$(BINDIR)/$(APP)" /bin/fma; fi
	install -m 644 "$(GENERATED)/fma.service" "$(SERVICE)"
	systemctl $(SYSTEMD_FLAGS) daemon-reload
	systemctl $(SYSTEMD_FLAGS) enable $(APP).service
	systemctl $(SYSTEMD_FLAGS) restart $(APP).service

uninstall:
	-systemctl $(SYSTEMD_FLAGS) disable --now $(APP).service
	@if test "$$(id -u)" = 0 && test -L /bin/fma && test "$$(readlink /bin/fma)" = "$(BINDIR)/$(APP)"; then rm -f /bin/fma; fi
	rm -f "$(SERVICE)" "$(BINDIR)/$(APP)"
	systemctl $(SYSTEMD_FLAGS) daemon-reload
	@echo 'S3 bucket and connection configuration preserved'

benchmark:
	python3 scripts/test_streaming.py --mail --report /tmp/fma-streaming.json
