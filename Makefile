# node_exporter 构建脚本
#
# 常用目标：
#   make build          编译到 ./node_exporter
#   make test           运行全部单元测试
#   make lint           gofmt 检查 + go vet
#   make install        安装到 $(PREFIX)/bin（默认 /usr/local）
#   make install-service 安装 systemd 服务并启用
#   make run            本地前台运行（开启全部模块）

BINARY      := node_exporter
PREFIX      ?= /usr/local
BINDIR      := $(PREFIX)/bin
SERVICEDIR  := /etc/systemd/system

VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT      ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE        ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

PKG         := github.com/prometheus/client_golang/prometheus
LDFLAGS     := -s -w \
	-X 'main.buildVersion=$(VERSION)' \
	-X 'main.buildCommit=$(COMMIT)' \
	-X 'main.buildDate=$(DATE)'

GO          ?= go
GOFILES     := $(shell find . -name '*.go' -not -path './vendor/*')

.PHONY: all build test test-race cover lint fmt vet tidy clean install uninstall install-service run help

all: lint test build

## build: 编译二进制
build:
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) .

## test: 运行单元测试
test:
	$(GO) test ./...

## test-race: 带竞态检测运行测试
test-race:
	$(GO) test -race ./...

## cover: 生成覆盖率报告
cover:
	$(GO) test -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out | tail -1

## lint: gofmt 检查与 go vet
lint: fmt vet

## fmt: 检查 gofmt（不改写文件）
fmt:
	@bad=$$(gofmt -l $(GOFILES)); \
	if [ -n "$$bad" ]; then \
		echo "以下文件未格式化，请执行 gofmt -w:"; echo "$$bad"; exit 1; \
	fi

## vet: 静态检查
vet:
	$(GO) vet ./...

## tidy: 整理依赖
tidy:
	$(GO) mod tidy

## install: 安装二进制与 systemd 服务文件
install: build
	install -d $(BINDIR)
	install -m 0755 $(BINARY) $(BINDIR)/$(BINARY)
	install -m 0644 node_exporter.service $(SERVICEDIR)/$(BINARY).service
	@echo "已安装到 $(BINDIR)/$(BINARY)"

## install-service: 安装并启动服务
install-service: install
	systemctl daemon-reload
	systemctl enable --now $(BINARY).service
	systemctl status $(BINARY).service

## uninstall: 卸载服务与二进制
uninstall:
	systemctl disable --now $(BINARY).service 2>/dev/null || true
	rm -f $(SERVICEDIR)/$(BINARY).service
	rm -f $(BINDIR)/$(BINARY)
	systemctl daemon-reload

## run: 前台运行并开启全部模块（用于调试）
run: build
	./$(BINARY) \
		-web.listen-address=:9100 \
		-enable-smart \
		-systemd.detail=true

## clean: 清理构建产物
clean:
	rm -f $(BINARY) coverage.out

## help: 显示帮助
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/^## //'
