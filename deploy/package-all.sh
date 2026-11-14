#!/usr/bin/env bash
# @author: kevinkelly
# @license: Apache Licence
# @file: package-all.sh
# @time: 2026/10/28

# dbtool 一键打包（多平台）：一条命令生成各个平台的发布包，放在 dist/ 下。
#
# 用法：bash deploy/package-all.sh [平台 ...] [选项]
#   bash deploy/package-all.sh                          默认：linux-amd64 + linux-arm64
#   bash deploy/package-all.sh all                      全部平台
#   bash deploy/package-all.sh linux-amd64 windows-amd64
#   bash deploy/package-all.sh --list                   列出支持的平台
#
# 平台：
#   linux-amd64  linux-arm64    完整安装包（和 deploy/package-centos7.sh 一样：解压后 sudo ./install.sh install，CentOS / RHEL / Rocky 等带 systemd 的 Linux）
#   windows-amd64               二进制压缩包（dbtool.exe / dbt-admin.exe + 配置示例 + SQL + 文档），没有 install.sh（一键安装依赖 systemd，只支持 Linux）
#   darwin-amd64  darwin-arm64  同上，macOS（Intel / Apple 芯片）
#   linux 是 linux-amd64 + linux-arm64；all 是上面全部。
#
# 选项：
#   -v, --version X   显式指定版本号（不读也不改 deploy/RELEASE_VERSION）
#   --tests           打包前先跑一次 go vet ./... 与 go test -count=1 ./...（只跑一次，不是每个平台各跑一遍），失败就一个都不打
#   --run             Linux 包额外生成自解压单文件 .run
#   --skip-build      Linux 包复用 dist/.build-linux-<arch>/ 里上一次编译的二进制（只改了文档 / 脚本时用）
#   --no-soar         Linux 包都不带 SOAR（默认：plugins/soar 是哪个架构，就只放进对应架构的包；另一个架构的包自动不带并给出提示）
#   --no-bump         版本取自 deploy/RELEASE_VERSION 时，全部成功后也不把它加 0.01
#   -h, --help        显示本说明
#
# 版本号：和 package-centos7.sh 一样的规则：--version / 环境变量 VERSION → deploy/RELEASE_VERSION（格式 主.两位小数；
#   **所有平台都成功后才加 0.01 写回**，任何一个平台失败都不加）→ git describe → 0.0.0-<日期>。所有平台用同一个版本号。
#
# 产物（dist/ 下）：
#   dbtool-<版本>-linux-<arch>.tar.gz / .sha256        （--run 时还有 .run）
#   dbtool-<版本>-<windows|darwin>-<arch>.zip 或 .tar.gz / .sha256   （有 zip 命令用 zip，没有就用 tar.gz）
#   dbtool-<版本>-SHA256SUMS.txt                        这一次打出来的所有包的校验和汇总
#
# 需要：bash、go（1.25+，纯 Go 交叉编译，CGO_ENABLED=0）、GNU tar、gzip、sha256sum、sed、od；zip 可选。
# 在 Windows（Git Bash）/ macOS / Linux 上都能运行。

set -euo pipefail

PROJECT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DIST_DIR="${PROJECT_DIR}/dist"
VERSION_FILE="${PROJECT_DIR}/deploy/RELEASE_VERSION"
ALL_PLATFORMS="linux-amd64 linux-arm64 windows-amd64 darwin-amd64 darwin-arm64"

die()  { echo "✗ $*" >&2; exit 1; }
ok()   { echo "    ✓ $*"; }
warn() { echo "    ⚠ $*"; }

usage() { awk 'NR > 6 { if ($0 ~ /^#/) { sub(/^# ?/, ""); print } else exit }' "${BASH_SOURCE[0]}"; }

# ---------- 参数 ----------
platforms=""
version_arg=""
want_tests=0 want_run=0 skip_build=0 no_soar=0 no_bump=0
while [ $# -gt 0 ]; do
  case "$1" in
    -h|--help) usage; exit 0 ;;
    --list) for p in $ALL_PLATFORMS; do echo "$p"; done; exit 0 ;;
    -v|--version) [ $# -ge 2 ] || die "--version 需要一个版本号"; version_arg="$2"; shift ;;
    --tests) want_tests=1 ;;
    --run) want_run=1 ;;
    --skip-build) skip_build=1 ;;
    --no-soar) no_soar=1 ;;
    --no-bump) no_bump=1 ;;
    all) platforms="$platforms $ALL_PLATFORMS" ;;
    linux) platforms="$platforms linux-amd64 linux-arm64" ;;
    linux-amd64|linux-arm64|windows-amd64|darwin-amd64|darwin-arm64) platforms="$platforms $1" ;;
    -*) die "不认识的选项：$1（bash deploy/package-all.sh --help 看用法）" ;;
    *) die "不支持的平台：$1（支持：$ALL_PLATFORMS，或 all / linux）" ;;
  esac
  shift
done
[ -n "$platforms" ] || platforms="linux-amd64 linux-arm64"
# 去重并保持顺序
uniq_platforms=""
for p in $platforms; do
  case " $uniq_platforms " in *" $p "*) ;; *) uniq_platforms="$uniq_platforms $p" ;; esac
done
platforms="${uniq_platforms# }"

for t in tar gzip sha256sum sed od; do
  command -v "$t" >/dev/null 2>&1 || die "缺少命令：$t"
done
tar --version 2>/dev/null | head -n 1 | grep -qi 'GNU tar' || die "需要 GNU tar（用到 --owner / --mode）；macOS 请 brew install gnu-tar 并把 gtar 放到 PATH 的 tar 位置"
command -v go >/dev/null 2>&1 || [ "$skip_build" = "1" ] || die "没有找到 go（需要 Go 1.25+）"

cd "$PROJECT_DIR"
export GOTOOLCHAIN=local

# ---------- 版本号（只在这里决定一次，所有平台共用）----------
AUTO_BUMP=0
RAW_VERSION="${version_arg:-${VERSION:-}}"
if [ -z "$RAW_VERSION" ] && [ -f "$VERSION_FILE" ]; then
  RAW_VERSION="$(tr -d ' \r\n' < "$VERSION_FILE")"
  [[ "$RAW_VERSION" =~ ^[0-9]+\.[0-9]{2}$ ]] || die "deploy/RELEASE_VERSION 的内容必须是“主版本.两位小数”（如 1.15），现在是：${RAW_VERSION:-（空）}"
  AUTO_BUMP=1
fi
[ -n "$RAW_VERSION" ] || RAW_VERSION="$(git describe --tags --always --dirty 2>/dev/null || true)"
[ -n "$RAW_VERSION" ] || RAW_VERSION="0.0.0-$(date +%Y%m%d)"
DIST_VERSION="$(printf '%s' "$RAW_VERSION" | tr -c 'A-Za-z0-9._+-' '-')"
BUILD_TIME="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
COMMIT="$(git rev-parse --short HEAD 2>/dev/null || echo none)"
LDFLAGS="-s -w -X main.version=${DIST_VERSION}"

echo "==> 多平台打包"
echo "    版本号: ${DIST_VERSION}"
echo "    平台:   ${platforms}"
echo ""
mkdir -p "$DIST_DIR"

# plugins/soar 的 CPU 架构（读 ELF 头 e_machine）：amd64 / arm64 / 空（没有或不是 ELF）
soar_arch() {
  [ -f plugins/soar ] || return 0
  [ "$(od -An -tx1 -N4 plugins/soar | tr -d ' \n')" = "7f454c46" ] || return 0
  case "$(od -An -tx1 -j18 -N2 plugins/soar | tr -d ' \n')" in
    3e00) echo amd64 ;;
    b700) echo arm64 ;;
  esac
}

results="" # 每行：平台|状态|文件
artifacts="" # 成功打出来的文件（dist/ 下的文件名，空格分隔）
failed=0

# ---------- 先跑一次测试 ----------
if [ "$want_tests" = "1" ]; then
  echo "==> 运行 go vet 与单元测试（只跑一次）"
  go vet ./... || die "go vet 失败，已停止打包"
  go test -count=1 ./... || die "单元测试失败，已停止打包"
  ok "go vet / go test 通过"
  echo ""
fi

# ---------- Linux：完整安装包（复用 package-centos7.sh）----------
build_linux() { # 架构
  local arch="$1" nosoar="$no_soar" sa
  if [ "$nosoar" != "1" ]; then
    sa="$(soar_arch)"
    if [ -n "$sa" ] && [ "$sa" != "$arch" ]; then
      warn "plugins/soar 是 ${sa} 版，linux-${arch} 包不带 SOAR（要带就放一个 ${arch} 版的 plugins/soar 再打）"
      nosoar=1
    fi
  fi
  ARCH="$arch" VERSION="$DIST_VERSION" NO_SOAR="$nosoar" MAKE_RUN="$want_run" SKIP_BUILD="$skip_build" \
    bash deploy/package-centos7.sh || return 1
  artifacts="$artifacts dbtool-${DIST_VERSION}-linux-${arch}.tar.gz"
  [ "$want_run" = "1" ] && artifacts="$artifacts dbtool-${DIST_VERSION}-linux-${arch}.run"
  return 0
}

# ---------- Windows / macOS：二进制压缩包 ----------
build_binary_bundle() { # 系统 架构
  local os="$1" arch="$2" ext="" name stage cache base
  [ "$os" = "windows" ] && ext=".exe"
  name="dbtool-${DIST_VERSION}-${os}-${arch}"
  stage="${DIST_DIR}/.stage-${os}-${arch}"
  cache="${DIST_DIR}/.build-${os}-${arch}"
  rm -rf "$stage"
  mkdir -p "$stage/dbtool/bootstrap" "$stage/dbtool/sql" "$stage/dbtool/docs" "$cache"

  echo "==> ${os}/${arch}：编译（静态，去符号表）"
  GOOS="$os" GOARCH="$arch" CGO_ENABLED=0 go build -trimpath -ldflags="${LDFLAGS}" -o "$cache/dbtool${ext}" ./cmd/server || return 1
  GOOS="$os" GOARCH="$arch" CGO_ENABLED=0 go build -trimpath -ldflags="${LDFLAGS}" -o "$cache/dbt-admin${ext}" ./cmd/dbt-admin || return 1
  cp "$cache/dbtool${ext}" "$cache/dbt-admin${ext}" "$stage/dbtool/"
  ok "dbtool${ext} ($(du -h "$stage/dbtool/dbtool${ext}" | cut -f1))、dbt-admin${ext} ($(du -h "$stage/dbtool/dbt-admin${ext}" | cut -f1))"

  cp deploy/bootstrap/*.sql "$stage/dbtool/bootstrap/"
  cp deploy/sql/*.sql "$stage/dbtool/sql/"
  cp configs/env.example configs/dbtool.example.toml "$stage/dbtool/"
  cp docs/deployment.md docs/user-guide.md docs/ops-manual.md docs/release-notes.md "$stage/dbtool/docs/"
  printf 'version=%s\nbuild_time=%s\ncommit=%s\nos=%s\narch=%s\n' "$DIST_VERSION" "$BUILD_TIME" "$COMMIT" "$os" "$arch" > "$stage/dbtool/VERSION"
  cat > "$stage/dbtool/README.txt" <<README
dbtool ${DIST_VERSION}（${os}/${arch}，二进制包）

这个包只有二进制和配置 / SQL / 文档，没有 install.sh：一键安装、升级、回滚依赖 systemd，只支持 Linux 安装包。
  dbtool${ext}           服务本体（前端页面已嵌入）
  dbt-admin${ext}        管理命令（初始化元数据库、设置、权限迁移等，用法见 docs/admin-cli.md 或 dbt-admin help）
  env.example / dbtool.example.toml   配置示例（环境变量 / 配置文件两种方式，见 docs/deployment.md）
  bootstrap/  sql/       元数据库的基础表、基础数据与扩展表脚本
  docs/                  部署 / 用户 / 运维手册、发布说明
README
  (cd "$stage/dbtool" && find . -type f ! -name SHA256SUMS -print0 | sort -z | xargs -0 sha256sum > SHA256SUMS)

  echo "==> ${os}/${arch}：打包"
  if [ "$os" = "windows" ] && command -v zip >/dev/null 2>&1; then
    base="${name}.zip"
    rm -f "$DIST_DIR/$base"
    (cd "$stage" && zip -qr "$DIST_DIR/$base" dbtool) || return 1
  else
    base="${name}.tar.gz"
    [ "$os" = "windows" ] && warn "没有 zip 命令，Windows 包用 .tar.gz（Windows 10+ 自带 tar 可以解开）"
    tar --owner=0 --group=0 --numeric-owner -C "$stage" -czf "$DIST_DIR/$base" dbtool || return 1
  fi
  (cd "$DIST_DIR" && sha256sum "$base" > "${name}.sha256")
  rm -rf "$stage"
  ok "$base ($(du -h "$DIST_DIR/$base" | cut -f1))"
  artifacts="$artifacts $base"
  return 0
}

for p in $platforms; do
  os="${p%%-*}"
  arch="${p##*-}"
  echo "################ ${p} ################"
  rc=0
  case "$os" in
    linux) build_linux "$arch" || rc=$? ;;
    windows|darwin) build_binary_bundle "$os" "$arch" || rc=$? ;;
  esac
  if [ "$rc" = "0" ]; then results="${results}${p}|成功"$'\n'; else results="${results}${p}|失败"$'\n'; failed=$((failed + 1)); echo "✗ ${p} 打包失败" >&2; fi
  echo ""
done

# ---------- 汇总 ----------
echo "==> 汇总"
while IFS='|' read -r p st; do
  if [ -n "$p" ]; then printf '    %-14s %s\n' "$p" "$st"; fi # 不要写成 [ -n ] && …：最后一个空行会让 set -e 把脚本杀掉
done <<< "$results"
if [ -n "$artifacts" ]; then
  SUMS="${DIST_DIR}/dbtool-${DIST_VERSION}-SHA256SUMS.txt"
  : > "$SUMS"
  for f in $artifacts; do
    [ -f "$DIST_DIR/$f" ] && (cd "$DIST_DIR" && sha256sum "$f" >> "$SUMS")
  done
  echo ""
  echo "产物（${DIST_DIR}）："
  for f in $artifacts; do
    [ -f "$DIST_DIR/$f" ] && printf '    %-48s %6s  %s\n' "$f" "$(du -h "$DIST_DIR/$f" | cut -f1)" "$(sha256sum "$DIST_DIR/$f" | cut -c1-16)…"
  done
  echo "    校验和汇总：dbtool-${DIST_VERSION}-SHA256SUMS.txt"
fi
echo ""

if [ "$failed" != "0" ]; then
  echo "✗ 有 ${failed} 个平台失败；版本号未改动" >&2
  exit 1
fi
if [ "$AUTO_BUMP" = "1" ] && [ "$no_bump" != "1" ]; then
  _n=$(( 10#${DIST_VERSION%%.*} * 100 + 10#${DIST_VERSION##*.} + 1 ))
  NEXT_VERSION="$(printf '%d.%02d' $(( _n / 100 )) $(( _n % 100 )))"
  printf '%s\n' "$NEXT_VERSION" > "$VERSION_FILE"
  echo "✓ 全部完成。版本号：本次 ${DIST_VERSION}，下次打包将是 ${NEXT_VERSION}（deploy/RELEASE_VERSION 已更新）"
else
  echo "✓ 全部完成。版本号：本次 ${DIST_VERSION}，deploy/RELEASE_VERSION 未改动"
fi
