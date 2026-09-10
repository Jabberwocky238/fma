APP := fma
BUILD_DATETIME := $(shell date -u +%Y%m%dT%H%M%SZ)
VERSION ?= dev-$(BUILD_DATETIME)
COMMIT ?= $(shell git rev-parse HEAD 2>/dev/null || echo unknown)
RELEASE_TIME ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.releaseTime=$(RELEASE_TIME)
export FMA_BUILD_LDFLAGS := $(LDFLAGS)
PREFIX ?= $(HOME)/.local
BINDIR := $(PREFIX)/bin
SYSTEMD_USER_DIR := $(HOME)/.config/systemd/user
CONFIG_DIR := $(HOME)/.config/fma
GENERATED := deploy/generated
-include $(GENERATED)/install.mk
SERVICE := $(SYSTEMD_USER_DIR)/$(APP).service

.PHONY: build check test install install-config uninstall
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

install-config:
	@for file in install.mk fma.service s3.env outbound.env nginx-http.conf nginx-https.conf nginx-stream.conf renew-hook.sh; do \
		test -s "$(GENERATED)/$$file" || { echo "配置文件没有找到: $(GENERATED)/$$file — 请先运行 bash deploy/gen.sh" >&2; exit 1; }; \
	done
	@test "$$(id -un)" = "$(DEPLOY_USER)" && test "$$(id -u)" = "$(DEPLOY_UID)" || { echo 'Run install as the user and UID selected in deploy/gen.sh' >&2; exit 1; }
	@command -v systemctl >/dev/null || { echo 'Installation requires Linux with systemd' >&2; exit 1; }

install: install-config
	$(MAKE) build
	install -d "$(BINDIR)" "$(SYSTEMD_USER_DIR)"
	install -d -m 700 "$(CONFIG_DIR)"
	install -m 600 "$(GENERATED)/s3.env" "$(CONFIG_DIR)/s3.env"
	install -m 600 "$(GENERATED)/outbound.env" "$(CONFIG_DIR)/outbound.env"
	install -m 755 $(APP) "$(BINDIR)/$(APP).new"
	mv "$(BINDIR)/$(APP).new" "$(BINDIR)/$(APP)"
	install -m 644 "$(GENERATED)/fma.service" "$(SERVICE)"
	systemctl --user daemon-reload
	systemctl --user enable $(APP).service
	systemctl --user restart $(APP).service

uninstall:
	-systemctl --user disable --now $(APP).service
	rm -f "$(SERVICE)" "$(BINDIR)/$(APP)"
	systemctl --user daemon-reload
	@echo 'S3 bucket and connection configuration preserved'
