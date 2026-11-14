#!/bin/bash
# dbtool 自解压安装包（单文件 .run）。由 make dist 生成：本头部（占位符 @...@ 在打包时替换）+ 下面 __ARCHIVE__ 之后的 tar.gz 载荷。
#
#   sudo ./dbtool-<版本>-linux-<arch>.run install --gen-secrets --bootstrap --admin-user admin --database-url '...' --public-url https://dba.example.com --open-firewall
#   ./dbtool-<版本>-linux-<arch>.run --info            显示版本 / 构建信息 / 载荷 sha256 / 包内文件清单
#   ./dbtool-<版本>-linux-<arch>.run --check           校验载荷 sha256 与包内 SHA256SUMS（不安装）
#   ./dbtool-<版本>-linux-<arch>.run --extract DIR     只解包到 DIR（得到 DIR/dbtool/{install.sh,dbtool,dbt-admin,...}）
#   其它参数（install / upgrade / rollback / uninstall / status / doctor / init-db / reconfigure / version / help 及各选项）
#   原样传给包内的 install.sh；用法见 "$0 help" 与 docs/one-click-deploy.md。
#
# 环境变量 DBT_RUN_SKIP_CHECK=1 可跳过运行前的载荷 sha256 校验（不推荐）。DBT_ADMIN_PASSWORD 等环境变量会原样传给 install.sh。
ARCHIVE_SHA256="@ARCHIVE_SHA256@"
# @author: kevinkelly
# @license: Apache Licence
# @file: run-header.sh
# @time: 2026/10/28

PKG_VERSION="@VERSION@"
PKG_ARCH="@ARCH@"
PKG_BUILD_TIME="@BUILD_TIME@"
PKG_COMMIT="@COMMIT@"

self=${BASH_SOURCE[0]:-$0}
fail() { echo "错误: $*" >&2; exit 1; }

offset=$(awk '/^__ARCHIVE__$/ { print NR + 1; exit }' "$self") || offset=""
[ -n "$offset" ] || fail "找不到载荷标记（文件被截断或被改动）"

payload() { tail -n +"$offset" "$self"; }

payload_sha() {
  local h=""
  read -r h _ < <(payload | sha256sum)
  printf '%s' "$h"
}

verify_payload() {
  local got
  got=$(payload_sha)
  if [ "$got" != "$ARCHIVE_SHA256" ]; then
    echo "载荷 sha256 不一致：期望 $ARCHIVE_SHA256，实际 $got（文件下载不完整或被改动）" >&2
    return 1
  fi
  return 0
}

extract_to() {
  command -v tar >/dev/null 2>&1 || fail "需要 tar 命令"
  mkdir -p "$1" || fail "无法创建目录 $1"
  payload | tar -xzf - -C "$1" || fail "解包失败"
}

case "${1:-}" in
  --info)
    echo "名称:       dbtool-${PKG_VERSION}-linux-${PKG_ARCH}.run"
    echo "版本:       $PKG_VERSION"
    echo "架构:       linux/$PKG_ARCH"
    echo "构建时间:   $PKG_BUILD_TIME"
    echo "提交:       $PKG_COMMIT"
    echo "载荷 sha256: $ARCHIVE_SHA256"
    echo "载荷偏移行: $offset"
    echo "包内文件（前 40 项）:"
    if command -v tar >/dev/null 2>&1; then payload | tar -tzf - 2>/dev/null | head -n 40 | sed 's/^/  /'; fi
    exit 0
    ;;
  --check)
    command -v sha256sum >/dev/null 2>&1 || fail "需要 sha256sum 命令"
    verify_payload || exit 1
    echo "载荷 sha256 校验通过"
    tmp=$(mktemp -d "${TMPDIR:-/tmp}/dbtool-check.XXXXXX") || fail "无法创建临时目录"
    trap 'rm -rf "$tmp"' EXIT
    extract_to "$tmp"
    if [ -f "$tmp/dbtool/SHA256SUMS" ]; then
      (cd "$tmp/dbtool" && sha256sum -c --quiet SHA256SUMS) || fail "包内 SHA256SUMS 校验失败"
      echo "包内 SHA256SUMS 校验通过（$(wc -l < "$tmp/dbtool/SHA256SUMS") 个文件）"
    else
      fail "包内没有 SHA256SUMS"
    fi
    echo "OK"
    exit 0
    ;;
  --extract)
    [ -n "${2:-}" ] || fail "--extract 需要目标目录"
    extract_to "$2"
    echo "已解包到 $2/dbtool"
    exit 0
    ;;
  -h|--help)
    sed -n '2,/^ARCHIVE_SHA256=/p' "$self" | sed '/^ARCHIVE_SHA256=/d' | sed 's/^# \{0,1\}//'
    exit 0
    ;;
esac

# 其余参数交给包内的 install.sh：先校验载荷，再解到临时目录运行，结束后清理
if [ "${DBT_RUN_SKIP_CHECK:-0}" != 1 ]; then
  command -v sha256sum >/dev/null 2>&1 || fail "需要 sha256sum 命令（或设置 DBT_RUN_SKIP_CHECK=1 跳过校验）"
  verify_payload || exit 1
fi
tmp=$(mktemp -d "${TMPDIR:-/tmp}/dbtool-run.XXXXXX") || fail "无法创建临时目录"
trap 'rm -rf "$tmp"' EXIT
extract_to "$tmp"
[ -f "$tmp/dbtool/install.sh" ] || fail "包内没有 install.sh"
# 不能 exec：需要在子进程结束后清理临时目录
bash "$tmp/dbtool/install.sh" "$@"
exit $?
__ARCHIVE__
