.PHONY: build run test fmt vet dist dist-all

build:
	go build -o bin/dbtool ./cmd/server

run:
	go run ./cmd/server

test:
	go test ./...

fmt:
	gofmt -w cmd internal

# 多平台一键打包：make dist-all [PLATFORMS="linux-amd64 linux-arm64 windows-amd64 darwin-arm64"|all] [VERSION=...]（默认 linux-amd64 + linux-arm64，详见 deploy/package-all.sh）
PLATFORMS ?=
dist-all:
	bash deploy/package-all.sh $(PLATFORMS) $(if $(VERSION),--version '$(VERSION)')

vet:
	go vet ./...

# 生成 CentOS/Linux 安装包（make dist [ARCH=arm64] [VERSION=...]）：实际工作由 deploy/package-centos7.sh 完成
# （没有 make 的机器，如 Windows Git Bash，直接 bash deploy/package-centos7.sh [版本号]；RUN_TESTS=1 / SKIP_BUILD=1 / ARCH=arm64 见脚本头部与 deploy/PACKAGING.md）。产物：
#   dist/dbtool-<版本>-linux-$(ARCH).tar.gz        解压后运行 install.sh
#   dist/dbtool-<版本>-linux-$(ARCH).run           默认不生成；make dist MAKE_RUN=1 才额外生成自解压单文件
#   dist/dbtool-<版本>-linux-$(ARCH).sha256        上面两个文件的 sha256
# 版本号：make dist VERSION=x.y 显式指定；不指定时取 deploy/RELEASE_VERSION（打包成功后自动加 0.01），再回退 git describe / 0.0.0-<日期>；
# 并用 -ldflags -X main.version 注入三个二进制。
ARCH ?= amd64
VERSION ?=
dist:
	ARCH=$(ARCH) VERSION='$(VERSION)' bash deploy/package-centos7.sh
