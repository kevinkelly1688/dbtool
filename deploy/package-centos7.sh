#!/usr/bin/env bash
# @author: kevinkelly
# @license: Apache Licence
# @file: package-centos7.sh
# @time: 2026/10/28

# dbtool 发布包打包脚本（目标：CentOS 7 / 8 / 9、RHEL、Rocky、AlmaLinux 等带 systemd 的 Linux）
#
# 用法：bash deploy/package-centos7.sh [版本号]
#   示例：bash deploy/package-centos7.sh 2.0      生成 dist/dbtool-2.0-linux-amd64.tar.gz
#   不给版本号时按下面的顺序取：环境变量 VERSION → deploy/RELEASE_VERSION（本次要打的版本号，格式 主.两位小数，如 1.15；
#   **打包成功后自动加 0.01 写回该文件**，下次打包就是 1.16；失败不加）→ git describe --tags --always --dirty → 0.0.0-<日期>。
#   显式给了版本号（参数或 VERSION）时不读也不改 deploy/RELEASE_VERSION。
# 环境变量：
#   ARCH=amd64|arm64   目标架构，默认 amd64（x86_64）；arm64 服务器用 ARCH=arm64
#   RUN_TESTS=1        打包前先跑 go vet ./... 与 go test -count=1 ./...，失败就不打包
#   MAKE_RUN=1         额外生成自解压单文件 .run（默认不生成：部署用 tar.gz 解压后运行 install.sh）
#   NO_SOAR=1          不把 plugins/ 里的 SOAR 打进包（plugins/soar 只有 amd64 版时，arm64 包用它；deploy/package-all.sh 会自动判断）
#   SKIP_BUILD=1       不重新编译，复用 dist/.build-linux-<arch>/ 里上一次编译的三个二进制
#                      （只改了文档 / 脚本时可以加快打包；改了 Go 代码、前端模板、静态文件不要用）
#
# 产物（都在 dist/ 下，make dist 调用的就是本脚本）：
#   dbtool-<版本>-linux-<arch>.tar.gz     发布包：解压后运行 dbtool/install.sh（部署方式：tar xzf … && cd dbtool && sudo ./install.sh install）
#   dbtool-<版本>-linux-<arch>.sha256     tar.gz 的 sha256
#   dbtool-<版本>-linux-<arch>.run        仅 MAKE_RUN=1 时生成：自解压单文件（deploy/run-header.sh + tar.gz），sha256 文件里同时记录它
#   发布包文件名统一用 dbtool（不带 -）；包内目录 dbtool/、二进制 dbtool、安装目录 /opt/dbtool 不变。
#
# 包内（解压后目录 dbtool/）：
#   dbtool / dbt-admin                   两个 Linux 静态二进制（CGO_ENABLED=0，不依赖 C 库；前端模板与静态文件已嵌入 dbtool）
#   install.sh                             一键安装 / 升级 / 回滚 / 卸载 / 体检 / 初始化库 / 改配置
#   bootstrap/  sql/                       基础表与基础数据、扩展表脚本
#   env.example  dbtool.example.toml     配置示例
#   goinception.config.toml              goInception 初始配置（install.sh 部署到 <安装目录>/goinception/config.toml，已存在不覆盖）
#   plugins/soar  plugins/soar.yaml      SOAR（SQL 优化工具）及其配置，来自仓库根目录的 plugins/（可选：没有这个目录就不带）。
#                                          install.sh 每次 install / upgrade 都会部署到 <安装目录>/plugins/ 并把系统设置指向它
#   docs/                                  部署 / 用户 / 运维手册、发布说明等
#   Dockerfile                             发布包版：直接 COPY 包内的 dbtool，不需要源码和 Go 环境（CentOS 上直接用 install.sh，不需要它）
#   VERSION  SHA256SUMS                    版本信息；包内所有文件的 sha256
#
# 在 Windows（Git Bash）/ macOS 上也可以运行：交叉编译、纯 Go；需要 bash、tar（GNU）、gzip、sha256sum、sed、od。

set -euo pipefail

PROJECT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DIST_DIR="${PROJECT_DIR}/dist"
PKG_NAME="dbtool"
OUT="${DIST_DIR}/${PKG_NAME}"
ARCH="${ARCH:-amd64}"

die()  { echo "✗ $*" >&2; exit 1; }
ok()   { echo "    ✓ $*"; }
warn() { echo "    ⚠ $*"; }

case "$ARCH" in
  amd64) ELF_MACHINE="x86-64|x86_64" ;;
  arm64) ELF_MACHINE="aarch64|ARM aarch64" ;;
  *) die "不支持的 ARCH=$ARCH（只支持 amd64 / arm64）" ;;
esac

cd "$PROJECT_DIR"
export GOTOOLCHAIN=local

# ---------- 版本号 ----------
VERSION_FILE="${PROJECT_DIR}/deploy/RELEASE_VERSION"
AUTO_BUMP=0
RAW_VERSION="${1:-${VERSION:-}}"
if [ -z "$RAW_VERSION" ] && [ -f "$VERSION_FILE" ]; then
  RAW_VERSION="$(tr -d ' \r\n' < "$VERSION_FILE")"
  [[ "$RAW_VERSION" =~ ^[0-9]+\.[0-9]{2}$ ]] || die "deploy/RELEASE_VERSION 的内容必须是“主版本.两位小数”（如 1.15），现在是：${RAW_VERSION:-（空）}"
  AUTO_BUMP=1
fi
if [ -z "$RAW_VERSION" ]; then
  RAW_VERSION="$(git describe --tags --always --dirty 2>/dev/null || true)"
fi
[ -n "$RAW_VERSION" ] || RAW_VERSION="0.0.0-$(date +%Y%m%d)"
# 版本号会进文件名与 sed 替换：只留 A-Za-z0-9._+-
DIST_VERSION="$(printf '%s' "$RAW_VERSION" | tr -c 'A-Za-z0-9._+-' '-')"
BUILD_TIME="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
COMMIT="$(git rev-parse --short HEAD 2>/dev/null || echo none)"
LDFLAGS="-s -w -X main.version=${DIST_VERSION}"

ART_NAME="dbtool" # 发布包文件名前缀（不带 -）
RUN_FILE="${ART_NAME}-${DIST_VERSION}-linux-${ARCH}.run"
MAKE_RUN="${MAKE_RUN:-0}"
TAR_FILE="${ART_NAME}-${DIST_VERSION}-linux-${ARCH}.tar.gz"
SHA_FILE="${ART_NAME}-${DIST_VERSION}-linux-${ARCH}.sha256"
BUILD_CACHE="${DIST_DIR}/.build-linux-${ARCH}"

echo "==> 打包发布包"
echo "    版本号:   ${DIST_VERSION}"
echo "    目标:     linux/${ARCH}"
if [ "$MAKE_RUN" = "1" ]; then echo "    输出文件: ${DIST_DIR}/${TAR_FILE}、${DIST_DIR}/${RUN_FILE}"; else echo "    输出文件: ${DIST_DIR}/${TAR_FILE}"; fi
echo ""

# ========== 步骤 0：打包前检查 ==========
echo "==> 步骤 0：检查必要文件与脚本语法"
need_files=(
  cmd/server/main.go cmd/dbt-admin/main.go
  deploy/install.sh deploy/run-header.sh
  deploy/bootstrap/001_base_schema.sql deploy/bootstrap/002_base_data.sql
  configs/env.example configs/dbtool.example.toml configs/goinception.config.toml
  docs/deployment.md docs/user-guide.md docs/ops-manual.md
)
for f in "${need_files[@]}"; do
  [ -f "$f" ] || die "缺少必要文件：$f（请在项目根目录运行）"
done
ls deploy/sql/*.sql >/dev/null 2>&1 || die "deploy/sql/ 里没有 .sql 扩展表脚本"
for t in tar gzip sha256sum sed od; do
  command -v "$t" >/dev/null 2>&1 || die "缺少命令：$t"
done
tar --version 2>/dev/null | head -n 1 | grep -qi 'GNU tar' || die "需要 GNU tar（用到 --owner / --mode）；macOS 请 brew install gnu-tar 并把 gtar 放到 PATH 的 tar 位置"
command -v go >/dev/null 2>&1 || [ "${SKIP_BUILD:-0}" = "1" ] || die "没有找到 go（需要 Go 1.25+）"
for s in deploy/install.sh deploy/run-header.sh; do
  bash -n "$s" || die "$s 语法错误"
done
# 脚本在 CentOS 上运行，必须是 LF 换行：CRLF 会让 shebang 报 "bad interpreter"
for s in deploy/install.sh deploy/run-header.sh deploy/package-centos7.sh; do
  if grep -q $'\r' "$s"; then die "$s 含有 CRLF 换行，CentOS 上无法执行：先转成 LF（sed -i 's/\\r\$//' $s）"; fi
done
ok "必要文件齐全，脚本语法与换行检查通过"
echo ""

if [ "${RUN_TESTS:-0}" = "1" ]; then
  echo "==> 运行 go vet 与单元测试"
  go vet ./... || die "go vet 失败，已停止打包"
  go test -count=1 ./... || die "单元测试失败，已停止打包"
  ok "go vet / go test 通过"
  echo ""
fi

# ========== 步骤 1：编译 ==========
echo "==> 步骤 1：编译（linux/${ARCH}，静态，去符号表）"
rm -rf "$OUT"
mkdir -p "$OUT/sql" "$BUILD_CACHE"
gobuild() { # 输出文件 包路径
  GOOS=linux GOARCH="$ARCH" CGO_ENABLED=0 go build -trimpath -ldflags="${LDFLAGS}" -o "$1" "$2"
}
if [ "${SKIP_BUILD:-0}" = "1" ] && [ -s "$BUILD_CACHE/dbtool" ] && [ -s "$BUILD_CACHE/dbt-admin" ]; then
  warn "SKIP_BUILD=1：复用 ${BUILD_CACHE} 里的二进制（其中的版本号是上次编译时的值）"
else
  [ "${SKIP_BUILD:-0}" != "1" ] || warn "SKIP_BUILD=1 但没有可复用的二进制，重新编译"
  gobuild "$BUILD_CACHE/dbtool"    ./cmd/server
  gobuild "$BUILD_CACHE/dbt-admin" ./cmd/dbt-admin
fi
is_elf() { # 文件：ELF 魔数 7f 45 4c 46；在 Windows 上编译出 .exe 或误用本机架构时能立刻发现
  [ "$(od -An -tx1 -N4 "$1" | tr -d ' \n')" = "7f454c46" ]
}
for b in dbtool dbt-admin; do
  is_elf "$BUILD_CACHE/$b" || die "$b 不是 Linux 可执行文件（ELF）：检查 go env 与 GOOS"
  cp "$BUILD_CACHE/$b" "$OUT/$b"
  ok "$b ($(du -h "$OUT/$b" | cut -f1))"
done
echo ""

# ========== 步骤 2：复制文件 ==========
echo "==> 步骤 2：复制文件到打包目录"
cp deploy/install.sh "$OUT/install.sh"
cp deploy/sql/*.sql "$OUT/sql/"
mkdir -p "$OUT/bootstrap" && cp deploy/bootstrap/*.sql "$OUT/bootstrap/"
cp configs/env.example "$OUT/env.example"
cp configs/goinception.config.toml "$OUT/goinception.config.toml"
cp configs/dbtool.example.toml "$OUT/dbtool.example.toml"
mkdir -p "$OUT/docs"
cp docs/deployment.md docs/user-guide.md docs/ops-manual.md "$OUT/docs/"
for f in docs/index.md docs/bootstrap-design.md docs/admin-cli.md docs/one-click-deploy.md docs/charset-policy.md docs/release-notes.md docs/settings-design.md docs/settings-reference.md; do
  [ -f "$f" ] && cp "$f" "$OUT/docs/"
done
# 发布包版 Dockerfile：用包内已编译的二进制，不需要源码 / Go 环境（仓库里 deploy/Dockerfile 是源码多阶段构建版，两者不要混用）
cat > "$OUT/Dockerfile" <<'DOCKERFILE'
# 发布包版 Dockerfile：直接使用包内已编译的 dbtool（不需要源码和 Go 环境）。
# 构建：docker build -t dbtool:latest .    （在解压后的 dbtool/ 目录里执行）
# 运行：docker run -d --name dbtool -p 8000:8000 --env-file ./env.example dbtool:latest    （env.example 里填好 DATABASE_URL 等）
# 端口默认 8000；要改端口：挂载配置文件并指定  -v ./dbtool.toml:/dbtool.toml  -e DB_TOOL_CONFIG=/dbtool.toml
# （见 dbtool.example.toml），或直接设环境变量 LISTEN=:9000；同时改 EXPOSE / -p 映射。
# 在 CentOS 上正式部署请直接用 install.sh（systemd 管理、升级回滚、体检），不需要本文件。
FROM gcr.io/distroless/static-debian12:nonroot
COPY dbtool /dbtool
EXPOSE 8000
ENTRYPOINT ["/dbtool"]
DOCKERFILE
printf 'version=%s\nbuild_time=%s\ncommit=%s\narch=%s\n' "$DIST_VERSION" "$BUILD_TIME" "$COMMIT" "$ARCH" > "$OUT/VERSION"
ok "install.sh / sql($(ls "$OUT/sql" | wc -l)) / bootstrap / docs($(ls "$OUT/docs" | wc -l)) / Dockerfile / VERSION"
# 可选：SOAR（plugins/soar 必须是目标架构的 Linux 可执行文件，否则服务器上跑不起来，宁可停下来）
if [ "${NO_SOAR:-0}" = "1" ]; then
  warn "NO_SOAR=1：本次发布包不带 SOAR（plugins/ 不打进包）"
elif [ -f plugins/soar ]; then
  is_elf plugins/soar || die "plugins/soar 不是 Linux 可执行文件（ELF）：请换成 linux 版的 soar"
  if command -v file >/dev/null 2>&1; then
    soar_desc="$(file -b plugins/soar)"
    printf '%s' "$soar_desc" | grep -Eq "ELF 64-bit.*(${ELF_MACHINE})" || die "plugins/soar 的架构与 ARCH=${ARCH} 不一致：${soar_desc}（换成 ${ARCH} 版的 soar，或暂时移走 plugins/）"
  fi
  # 内容核对：dbtool 调用 SOAR 时会传 -online-dsn / -test-dsn / -allow-online-as-test，真正的 SOAR（XiaoMi/soar）程序里一定有这些参数名；
  # 名字也叫 soar 的其他程序（比如同名的软件包管理器 soar）不能当 SQL 优化工具打进包里
  for flag in allow-online-as-test online-dsn test-dsn; do
    grep -aq -e "$flag" plugins/soar || die "plugins/soar 不像 SQL 优化工具 SOAR（程序里没有参数名 -${flag}）。XiaoMi/soar 的 linux 版才可以；名字同为 soar 的软件包管理器不行"
  done
  mkdir -p "$OUT/plugins"
  cp plugins/soar "$OUT/plugins/soar"
  if [ -f plugins/soar.yaml ]; then cp plugins/soar.yaml "$OUT/plugins/soar.yaml"; else warn "plugins/ 里没有 soar.yaml：只带 SOAR 程序，服务器上用 SOAR 内置的默认配置"; fi
  ok "plugins/soar ($(du -h "$OUT/plugins/soar" | cut -f1)，sha256 $(sha256sum plugins/soar | cut -c1-12))${soar_desc:+：$soar_desc}"
else
  warn "没有 plugins/soar：本次发布包不带 SOAR（把 linux 版 soar 放进 plugins/ 即可随包部署）"
fi
echo ""

# ========== 步骤 3：自检（打包前） ==========
echo "==> 步骤 3：包目录自检"
for f in dbtool dbt-admin install.sh VERSION env.example dbtool.example.toml goinception.config.toml \
         bootstrap/001_base_schema.sql bootstrap/002_base_data.sql docs/release-notes.md; do
  [ -e "$OUT/$f" ] || die "发布包缺少：$f"
done
# 不能带进包里的东西：密钥 / 证书 / 本机配置 / 安装状态
leaked="$(cd "$OUT" && find . -type f \( -name '*.pem' -o -name '*.key' -o -name '.env' -o -name '.env.*' -o -name '*.log' -o -name '.install-state' -o -name 'dbtool.toml' \) -print)"
[ -z "$leaked" ] || die "发布包里出现了不该有的文件：$leaked"
bash -n "$OUT/install.sh" || die "install.sh 语法错误"
ok "关键文件齐全，没有密钥 / 本机配置，脚本语法通过"
(cd "$OUT" && find . -type f ! -name SHA256SUMS | sed 's|^\./||' | LC_ALL=C sort | xargs sha256sum > SHA256SUMS)
ok "SHA256SUMS（$(wc -l < "$OUT/SHA256SUMS") 个文件）"
echo ""

# ========== 步骤 4：创建 tar.gz ==========
echo "==> 步骤 4：创建 ${TAR_FILE}"
HAS_SOAR=""
if [ -f "$OUT/plugins/soar" ]; then HAS_SOAR=1; fi
cd "$DIST_DIR"
TARFILE="${ART_NAME}-${DIST_VERSION}-linux-${ARCH}.tar"
rm -f "$TAR_FILE" "$TARFILE" "$RUN_FILE" "$SHA_FILE"
# 属主统一成 root:root（0:0）。在 Windows 的 Git Bash 里 chmod 对 Linux 二进制不生效，直接 tar 得到 -rw-r--r--，
# 解压后不能执行；所以可执行文件单独追加并固定 0755，其余文件 0644 / 目录 0755。
EXEC_FILES=("${PKG_NAME}/dbtool" "${PKG_NAME}/dbt-admin" "${PKG_NAME}/install.sh")
if [ -f "$OUT/plugins/soar" ]; then EXEC_FILES+=("${PKG_NAME}/plugins/soar"); fi
excludes=()
for f in "${EXEC_FILES[@]}"; do excludes+=(--exclude="$f"); done
tar -cf "$TARFILE" --owner=0 --group=0 --numeric-owner --mode='a+rX,u+w,go-w' --anchored "${excludes[@]}" "$PKG_NAME"
for f in "${EXEC_FILES[@]}"; do
  tar -rf "$TARFILE" --owner=0 --group=0 --numeric-owner --mode=0755 "$f"
done
gzip -9 -n -f "$TARFILE"
ok "${TAR_FILE} ($(du -h "$TAR_FILE" | cut -f1))"
echo ""

# ========== 步骤 5：校验和（MAKE_RUN=1 时先生成自解压 .run） ==========
if [ "$MAKE_RUN" = "1" ]; then
  echo "==> 步骤 5：生成 ${RUN_FILE} 与 sha256"
  ARCHIVE_SHA="$(sha256sum "$TAR_FILE" | cut -d' ' -f1)"
  sed -e "s|@VERSION@|${DIST_VERSION}|g" -e "s|@ARCH@|${ARCH}|g" -e "s|@BUILD_TIME@|${BUILD_TIME}|g" \
      -e "s|@COMMIT@|${COMMIT}|g" -e "s|@ARCHIVE_SHA256@|${ARCHIVE_SHA}|" "$PROJECT_DIR/deploy/run-header.sh" > "$RUN_FILE"
  cat "$TAR_FILE" >> "$RUN_FILE"
  chmod +x "$RUN_FILE"
  sha256sum "$RUN_FILE" "$TAR_FILE" > "$SHA_FILE"
else
  echo "==> 步骤 5：生成 sha256（不生成 .run；需要自解压单文件时加 MAKE_RUN=1）"
  sha256sum "$TAR_FILE" > "$SHA_FILE"
fi
ok "$(cat "$SHA_FILE" | sed 's/^/  /' | tr '\n' ';')"
echo ""

# ========== 步骤 6：自检（打包后，按用户收到的样子检查） ==========
echo "==> 步骤 6：发布包验收（解压后重新校验）"
CHECK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/dbtool-pkgcheck.XXXXXX")"
trap 'rm -rf "$CHECK_DIR"' EXIT
tar -xzf "$TAR_FILE" -C "$CHECK_DIR"
(cd "$CHECK_DIR/${PKG_NAME}" && sha256sum -c --quiet SHA256SUMS) || die "解压后 SHA256SUMS 校验失败"
ok "tar.gz 解压后 SHA256SUMS 校验通过"
listing="$(tar -tvzf "$TAR_FILE")"
for f in dbtool dbt-admin install.sh ${HAS_SOAR:+plugins/soar}; do
  line="$(printf '%s\n' "$listing" | grep -E " ${PKG_NAME}/${f}\$" || true)"
  case "$line" in -rwxr-xr-x*" 0/0 "*) ;; *) die "包内 $f 的权限 / 属主不对（应为 -rwxr-xr-x 0/0）：$line" ;; esac
done
ok "可执行文件模式 0755、属主 0/0"
for b in dbtool dbt-admin ${HAS_SOAR:+plugins/soar}; do
  is_elf "$CHECK_DIR/${PKG_NAME}/$b" || die "包内 $b 不是 ELF"
done
if command -v file >/dev/null 2>&1; then
  desc="$(file -b "$CHECK_DIR/${PKG_NAME}/dbtool")"
  printf '%s' "$desc" | grep -Eq "ELF 64-bit.*(${ELF_MACHINE})" || die "dbtool 架构与 ARCH=${ARCH} 不一致：$desc"
  ok "dbtool：$desc"
fi
if [ "$MAKE_RUN" = "1" ]; then
  bash "./${RUN_FILE}" --check >/dev/null || die ".run --check 失败"
  ok ".run --check 通过（载荷 sha256 与包内 SHA256SUMS）"
fi
(cd "$DIST_DIR" && sha256sum -c --quiet "$SHA_FILE") || die "sha256 文件与产物不一致"
ok "sha256sum -c ${SHA_FILE} 通过"
echo ""

# ========== 完成 ==========
echo "✓ 打包完成"
echo ""
echo "输出文件（${DIST_DIR}）："
echo "  ${TAR_FILE}   ($(du -h "$TAR_FILE" | cut -f1))   解压后运行 dbtool/install.sh"
echo "  ${SHA_FILE}"
if [ "$MAKE_RUN" = "1" ]; then echo "  ${RUN_FILE}   ($(du -h "$RUN_FILE" | cut -f1))   自解压单文件（MAKE_RUN=1）"; fi
echo ""
echo "拷到 CentOS 服务器后："
echo "  sha256sum -c ${SHA_FILE}                         校验（两个文件放在同一目录）"
echo "  tar xzf ${TAR_FILE} && cd dbtool"
echo "  sudo ./install.sh doctor                         只读体检（不改任何东西）"
echo "  sudo ./install.sh install --gen-secrets --bootstrap --admin-user admin \\"
echo "       --database-url 'mysql://用户:密码@主机:3306/db_tool' --public-url https://dba.example.com --open-firewall"
echo "  sudo ./install.sh upgrade                        升级（保留配置与数据，失败自动回滚）"
if [ -n "$HAS_SOAR" ]; then echo "  （包里带有 SOAR：install / upgrade 会自动部署到 <安装目录>/plugins 并配置系统设置，可重复执行；测试库 DSN 需要管理员在系统配置页填写）"; fi
echo ""
if [ "$AUTO_BUMP" = "1" ]; then
  # 打包成功才加：1.15 → 1.16；1.99 → 2.00（按“百分之一”的整数计算，不用浮点）
  _n=$(( 10#${DIST_VERSION%%.*} * 100 + 10#${DIST_VERSION##*.} + 1 ))
  NEXT_VERSION="$(printf '%d.%02d' $(( _n / 100 )) $(( _n % 100 )))"
  printf '%s\n' "$NEXT_VERSION" > "$VERSION_FILE"
  echo "版本号：本次 ${DIST_VERSION}，下次打包将是 ${NEXT_VERSION}（deploy/RELEASE_VERSION 已更新）"
else
  echo "版本号：本次使用显式指定的 ${DIST_VERSION}，deploy/RELEASE_VERSION 未改动"
fi
