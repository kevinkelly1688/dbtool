#!/usr/bin/env bash
# @author: kevinkelly
# @license: Apache Licence
# @file: run.sh
# @time: 2026/10/28

# deploy/install.sh（一键部署脚本）的行为测试：纯 bash，不需要 root、不碰真实系统、不连真实库。
#
#   bash scripts/install-test/run.sh                 # 全部用例（顺序执行）
#   bash scripts/install-test/run.sh -j 6            # 6 个用例并行（每个用例自己的临时目录，互不影响）
#   bash scripts/install-test/run.sh gen_secrets     # 只跑名字里含 gen_secrets 的用例
#   bash scripts/install-test/run.sh -j 4 inv3       # 并行跑名字里含 inv3 的用例
#
# 做法：每个用例在临时目录里布置“安装包”（真实的 deploy/install.sh + 假的 dbtool / dbt-admin / sql / bootstrap + 真实生成的 SHA256SUMS），
# 并在 PATH 最前面放假的 id / systemctl / install / chown / chmod / useradd / userdel / curl / firewall-cmd / mysql / ss / df / sleep 等，
# 它们只把调用（argv）写进日志，行为由 FAKE_* 环境变量控制。配置目录、systemd 单元目录、锁文件、安装日志用 install.sh 的测试用环境变量
# （DBT_INSTALL_CONF_DIR / DBT_INSTALL_UNIT_DIR / DBT_INSTALL_LOCK_FILE / DBT_INSTALL_LOG_FILE / DBT_OS_RELEASE_FILE / DBT_INSTALL_TEST_TTY）
# 指到临时目录，安装目录用 --prefix。因此：不改系统、不需要 root；但也就没有验证真实的权限位 / 属主 / systemd / firewalld / SELinux /
# 真实 MySQL（见 docs/one-click-deploy.md“未验证项”）。
#
# 不变量与用例的对应（docs/one-click-deploy.md 第 4 节）：inv1 重复运行安全、inv2 库保护、inv3 升级可回滚、inv4 状态文件、inv5 卸载、
# inv6 不丢用户修改、inv7 幂等与并发 / dry-run / 失败报告、inv8 机密卫生、inv9 首次部署一键完成、inv10 日志。
set -u

# Windows Git Bash / MSYS 下 ln -s 默认是“复制”而不是符号链接；current -> releases/<版本> 需要真符号链接
# （需要 Windows 开发者模式或有创建符号链接的权限）。在 Linux 上这个变量无影响。
export MSYS=winsymlinks:nativestrict

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO=$(cd "$HERE/../.." && pwd)
INSTALL_SH=$REPO/deploy/install.sh
RUN_HEADER=$REPO/deploy/run-header.sh
ORIG_PATH=$PATH
REAL_SLEEP=$(command -v sleep)

# 元数据库“用户表”判空依据：dbt-admin status --porcelain 输出里的键。全测试只在这里写一次（表改名时只改这里）。
USERS_KEY=users_table

JOBS=1 FILTER="" EXACT=""
while [ $# -gt 0 ]; do
  case "$1" in
    -j) JOBS=${2:?}; shift 2 ;;
    --exact) EXACT=${2:?}; shift 2 ;;
    *) FILTER=$1; shift ;;
  esac
done

PASS=0 FAIL=0 FAILED_NAMES=()
T="" # 当前用例的临时目录

# ---------------- 断言 ----------------
fail() { echo "    ✗ $*"; CASE_FAILED=1; }
assert_eq() { [ "$1" = "$2" ] || fail "${3:-值不相等}：期望 [$2]，得到 [$1]"; }
assert_ne() { [ "$1" != "$2" ] || fail "${3:-值不应相等}：[$1]"; }
assert_rc() { [ "$RC" = "$1" ] || fail "${2:-退出码}：期望 $1，得到 $RC"; }
assert_file() { [ -e "$1" ] || [ -L "$1" ] || fail "应存在：$1"; }
assert_no_file() { { [ ! -e "$1" ] && [ ! -L "$1" ]; } || fail "不应存在：$1"; }
assert_has() { grep -qE -- "$2" "$1" 2>/dev/null || fail "$1 里应匹配 /$2/"; }
assert_lacks() { ! grep -qE -- "$2" "$1" 2>/dev/null || fail "$1 里不应匹配 /$2/"; }
assert_str_has() { case "$1" in *"$2"*) ;; *) fail "输出里应包含 [$2]" ;; esac; }
assert_str_lacks() { case "$1" in *"$2"*) fail "输出里不应包含 [$2]" ;; esac; }
assert_same_file() { cmp -s "$1" "$2" || fail "$1 与 $2 内容应相同"; }
assert_count() { local n; n=$(grep -cE -- "$2" "$1" 2>/dev/null || true); [ "$n" = "$3" ] || fail "$1 里 /$2/ 应出现 $3 次，实际 $n"; }
# 日志里两条记录的先后（行号）
line_of() { grep -nE -- "$2" "$1" | head -n1 | cut -d: -f1; }
assert_before() {
  local a b
  a=$(line_of "$1" "$2"); b=$(line_of "$1" "$3")
  [ -n "$a" ] && [ -n "$b" ] || { fail "$1 里应同时有 /$2/ 与 /$3/（得到 $a / $b）"; return; }
  [ "$a" -lt "$b" ] || fail "/$2/ 应在 /$3/ 之前（行 $a / $b）"
}
# 最后一条匹配与第一条匹配比较：assert_last_before 日志 前 后（“前”的最后一次出现应在“后”的第一次出现之前）
st_get() { sed -n "s/^$1=//p" "$STATE_FILE" | head -n1; }
link_id() { local t; t=$(readlink "$PREFIX/current" 2>/dev/null); echo "${t##*/}"; }
sha_file() { sha256sum "$1" | cut -d' ' -f1; }
list_releases() { ls -1 "$PREFIX/releases" 2>/dev/null | grep -v '^\.' | sort | tr '\n' ' '; }

# 会改动系统 / 库的调用（dry-run、doctor 的日志里不允许出现）
MUTATING_RE='^(useradd|userdel|chown|chmod|restorecon|install |mysql |firewall-cmd .*(--add-port|--remove-port|--reload)|systemctl (restart|enable|disable|start|stop|daemon-reload)|dbt-admin (bootstrap .*--apply|admin ))'

# ---------------- 环境布置 ----------------
write_fake_dbt_admin() {
  cat > "$1" <<'EOF'
#!/bin/bash
# 假 dbt-admin：只记录调用（不记录任何密码 / 密钥值）；库状态在 $FAKE_DB 下的 users / supers 两个文件里，行为由 FAKE_* 控制
user=${DATABASE_URL#mysql://}; user=${user%%:*}
echo "dbt-admin $* | url_user=${user:-none} pw_env=${DBT_ADMIN_PASSWORD:+set} tz=${TZ:-} keys=${SECRET_KEY:+S}${MIRAGE_SECRET_KEY:+M}" >> "$FAKE_LOG"
mkdir -p "$FAKE_DB"
[ -f "$FAKE_DB/users" ] || echo "${FAKE_SQL_USERS:-empty}" > "$FAKE_DB/users"
[ -f "$FAKE_DB/supers" ] || echo "${FAKE_SUPERUSERS:-0}" > "$FAKE_DB/supers"
only_sql=0; apply=0
for a in "$@"; do [ "$a" = --only-sql ] && only_sql=1; [ "$a" = --apply ] && apply=1; done
case "$1" in
  gen-secrets)
    [ -n "$FAKE_GEN_BAD" ] && exit 1
    echo "SECRET_KEY=FAKEGENSECRETKEY0123456789FAKEGENSECRETKEY0123456789FAKEGENSECRETKEY01"
    echo "MIRAGE_SECRET_KEY=FAKEGENMIRAGEKEY0123456789FAKEGENMIRAGEKEY0123456789FAKEGENMIRAGE01"
    echo "MIRAGE_CIPHER_MODE=ECB"
    echo "MIRAGE_CIPHER_IV=FAKEGENIV0123456"
    exit 0 ;;
  encrypt-url)
    # 假加密：把密码换成 ENC(FAKECIPHER)；FAKE_ENC_BAD 非空时模拟失败（如公开缺省密钥）。不记录密钥与密码
    [ -n "$FAKE_ENC_BAD" ] && { echo "错误：当前生效的密钥是公开的缺省值（假）" >&2; exit 2; }
    case "$DATABASE_URL" in *:ENC\(*) echo "$DATABASE_URL" ;; *) echo "mysql://${user}:ENC(FAKECIPHER)@${DATABASE_URL#*@}" ;; esac
    exit 0 ;;
  status)
    if [ -n "$FAKE_STATUS_RC" ] && [ "$FAKE_STATUS_RC" != 0 ]; then echo "错误：连接失败（假）" >&2; exit "$FAKE_STATUS_RC"; fi
    echo "db=db_tool"; echo "host=h:3306"
    echo "${FAKE_USERS_KEY}=$(cat "$FAKE_DB/users")"; echo "users=0"; echo "superusers=$(cat "$FAKE_DB/supers")"; echo "bootstrap_applied=0"
    exit 0 ;;
  bootstrap)
    if [ "$only_sql" = 1 ] && [ -n "$FAKE_ONLYSQL_RC" ]; then exit "$FAKE_ONLYSQL_RC"; fi
    if [ "$only_sql" = 0 ] && [ -n "$FAKE_BOOTSTRAP_RC" ] && [ "$FAKE_BOOTSTRAP_RC" != 0 ]; then exit "$FAKE_BOOTSTRAP_RC"; fi
    if [ "$only_sql" = 0 ] && [ "$apply" = 1 ]; then echo nonempty > "$FAKE_DB/users"; fi
    exit 0 ;;
  admin)
    if [ -n "$FAKE_ADMIN_RC" ] && [ "$FAKE_ADMIN_RC" != 0 ]; then exit "$FAKE_ADMIN_RC"; fi
    if [ "$2" = create ]; then echo $(( $(cat "$FAKE_DB/supers") + 1 )) > "$FAKE_DB/supers"; fi
    exit 0 ;;
  perm)
    if [ -n "$FAKE_PERM_RC" ] && [ "$FAKE_PERM_RC" != 0 ]; then echo "错误：权限迁移失败（假）" >&2; exit "$FAKE_PERM_RC"; fi
    permprev=""; for a in "$@"; do [ "$permprev" = --rollback-file ] && echo "-- fake rollback" > "$a"; permprev=$a; done
    echo "已迁移：权限组新增按钮 1 条，单用户新增允许 0 条，标记为已迁移：true。"
    exit 0 ;;
  settings)
    if [ -n "$FAKE_SETTINGS_RC" ] && [ "$FAKE_SETTINGS_RC" != 0 ]; then echo "错误：库里没有 dbt_setting 表（假）" >&2; exit "$FAKE_SETTINGS_RC"; fi
    skey=""; sval=""; sprev=""
    for a in "$@"; do case "$sprev" in --key) skey=$a ;; --value) sval=$a ;; esac; sprev=$a; done
    sf="$FAKE_DB/setting.$skey"
    case "$2" in
      get)
        echo "键：$skey"
        if [ "$skey" = optimizer.soar_test_dsn ]; then
          if [ -s "$sf" ]; then echo "当前值：（已设置）"; else echo "当前值：（未设置）"; fi
        else echo "当前值：$(cat "$sf" 2>/dev/null || true)" | sed 's/：$/：-/'; fi   # 真 dbt-admin 没有值时输出半角 “-”（orDash）
        exit 0 ;;
      set) printf '%s' "$sval" > "$sf"; echo "已修改。"; exit 0 ;;
    esac
    exit 0 ;;
esac
exit 0
EOF
  chmod +x "$1"
}

# mkpkg 目录 版本 [build_time] [标记]：布置一个完整的“安装包”（含真实 SHA256SUMS）
mkpkg() {
  local d=$1 ver=$2 bt=${3:-2026-01-01T00:00:00Z} mark=${4:-$2}
  mkdir -p "$d/sql" "$d/bootstrap" "$d/docs"
  cp "$INSTALL_SH" "$d/install.sh"
  printf '#!/bin/sh\necho fake dbtool %s\n' "$mark" > "$d/dbtool"
  chmod +x "$d/dbtool"
  write_fake_dbt_admin "$d/dbt-admin"
  echo "CREATE TABLE IF NOT EXISTS dbt_a (id INT);" > "$d/sql/001_a.sql"
  echo "CREATE TABLE IF NOT EXISTS dbt_b (id INT);" > "$d/sql/002_b.sql"
  echo "CREATE TABLE IF NOT EXISTS base_t (id INT);" > "$d/bootstrap/001_base_schema.sql"
  echo "INSERT IGNORE INTO dbt_auth_group (id) VALUES (1);" > "$d/bootstrap/002_base_data.sql"
  echo "# docs" > "$d/docs/ops-manual.md"
  echo "# env" > "$d/env.example"
  printf '[inc]
backup_host = "127.0.0.1"
' > "$d/goinception.config.toml"
  printf 'version=%s\nbuild_time=%s\ncommit=abc\narch=amd64\n' "$ver" "$bt" > "$d/VERSION"
  (cd "$d" && find . -type f ! -name SHA256SUMS | sed 's|^\./||' | LC_ALL=C sort | xargs sha256sum > SHA256SUMS)
}

setup_env() {
  hash -r
  T=$(mktemp -d)
  mkdir -p "$T/fakebin" "$T/etc" "$T/unit" "$T/opt"
  : > "$T/calls.log"
  mkpkg "$T/pkg" 1.0.0
  write_fakes
  export PATH="$T/fakebin:$ORIG_PATH"
  export FAKE_LOG="$T/calls.log"
  export FAKE_DB="$T/fakedb"
  export FAKE_USERS_KEY=$USERS_KEY
  export DBT_INSTALL_CONF_DIR="$T/etc/dbtool"
  export DBT_INSTALL_UNIT_DIR="$T/unit"
  export DBT_INSTALL_LOCK_FILE="$T/lock"
  export DBT_INSTALL_LOG_FILE="$T/install.log"
  export DBT_OS_RELEASE_FILE="$T/os-release"
  printf 'ID="centos"\nID_LIKE="rhel fedora"\nPRETTY_NAME="CentOS Linux 7 (Core)"\n' > "$DBT_OS_RELEASE_FILE"
  PREFIX=$T/opt/dbtool
  export FAKE_PREFIX=$PREFIX
  ENV_FILE=$DBT_INSTALL_CONF_DIR/dbtool.env
  TOML_FILE=$DBT_INSTALL_CONF_DIR/dbtool.toml
  STATE_FILE=$DBT_INSTALL_CONF_DIR/.install-state
  UNIT_FILE=$T/unit/dbtool.service
  INSTALL_LOG=$DBT_INSTALL_LOG_FILE
  unset DBT_ADMIN_PASSWORD DDL_DATABASE_URL DBT_DATABASE_URL DBT_SECRET_KEY DBT_HEALTH_TIMEOUT DBT_INSTALL_TEST_TTY DBT_DOCTOR_MIN_FREE_KB
  local v
  for v in $(compgen -v FAKE_); do case "$v" in FAKE_LOG|FAKE_DB|FAKE_USERS_KEY|FAKE_PREFIX) ;; *) unset "$v" ;; esac; done
  export FAKE_ENC_BAD=1   # 默认 encrypt-url 不可用（连接串保持明文）；加密相关用例 unset 它
}

teardown_env() { rm -rf "$T"; hash -r; }

write_fakes() {
  local f=$T/fakebin n
  cat > "$f/id" <<'EOF'
#!/bin/sh
if [ "$1" = "-u" ]; then echo 0; exit 0; fi
if [ "$1" = "-un" ]; then echo root; exit 0; fi
if [ -n "$FAKE_ID_MISSING" ] && [ ! -f "$FAKE_LOG.user" ]; then exit 1; fi
exit 0
EOF
  for n in useradd userdel chown chmod restorecon journalctl; do
    cat > "$f/$n" <<EOF
#!/bin/sh
echo "$n \$*" >> "\$FAKE_LOG"
case "$n" in
  useradd) : > "\$FAKE_LOG.user" ;;
  userdel) rm -f "\$FAKE_LOG.user" ;;
esac
exit 0
EOF
  done
  cat > "$f/systemctl" <<'EOF'
#!/bin/bash
echo "systemctl $*" >> "$FAKE_LOG"
case "$1" in
  is-active)
    if [ "$3" = firewalld ] || [ "$2" = firewalld ]; then [ -n "$FAKE_FIREWALLD_OFF" ] && exit 3; exit 0; fi
    [ -n "$FAKE_SVC_INACTIVE" ] && exit 3
    exit 0 ;;
esac
exit 0
EOF
  cat > "$f/firewall-cmd" <<'EOF'
#!/bin/bash
echo "firewall-cmd $*" >> "$FAKE_LOG"
ports=$FAKE_LOG.fwports
touch "$ports"
for a in "$@"; do
  case "$a" in
    --query-port=*) p=${a#--query-port=}; grep -qx "$p" "$ports" && exit 0; exit 1 ;;
    --add-port=*) echo "${a#--add-port=}" >> "$ports" ;;
    --remove-port=*) p=${a#--remove-port=}; grep -vx "$p" "$ports" > "$ports.new"; mv "$ports.new" "$ports" ;;
  esac
done
exit 0
EOF
  cat > "$f/curl" <<'EOF'
#!/bin/bash
echo "curl $*" >> "$FAKE_LOG"
cur=$(readlink "$FAKE_PREFIX/current" 2>/dev/null); cur=${cur##*/}
[ -n "$FAKE_CURL_FAIL_ALL" ] && exit 22
case ",$FAKE_CURL_FAIL_FOR," in *",$cur,"*) exit 22 ;; esac
# 配置被改成“坏值”时服务起不来（reconfigure 自动恢复用例）
if grep -q 'bad.example.com' "$DBT_INSTALL_CONF_DIR/dbtool.env" 2>/dev/null; then exit 22; fi
case "$*" in
  *readyz*) [ -n "$FAKE_READYZ_FAIL" ] && exit 22; echo "{\"status\":\"${FAKE_READYZ_STATUS:-ok}\"}" ;;
  *) echo '{"status":"ok"}' ;;
esac
exit 0
EOF
  cat > "$f/file" <<'EOF'
#!/bin/sh
echo "$1: ELF 64-bit LSB executable"
EOF
  cat > "$f/mysql" <<'EOF'
#!/bin/sh
echo "mysql $* PWD_SET=${MYSQL_PWD:+yes}" >> "$FAKE_LOG"
cat >> "$FAKE_LOG.mysql-stdin"
exit 0
EOF
  cat > "$f/sleep" <<'EOF'
#!/bin/sh
exit 0
EOF
  cat > "$f/ss" <<'EOF'
#!/bin/sh
echo "State Recv-Q Send-Q Local Address:Port Peer Address:Port"
[ -n "$FAKE_PORT_BUSY" ] && echo "LISTEN 0 128 0.0.0.0:$FAKE_PORT_BUSY 0.0.0.0:*"
exit 0
EOF
  cat > "$f/df" <<'EOF'
#!/bin/sh
if [ -n "$FAKE_DF_AVAIL_KB" ]; then
  echo "Filesystem 1024-blocks Used Available Capacity Mounted on"
  echo "fake 1000000 1 $FAKE_DF_AVAIL_KB 1% /"
  exit 0
fi
exec /usr/bin/df "$@"
EOF
  cat > "$f/install" <<'EOF'
#!/bin/bash
echo "install $*" >> "$FAKE_LOG"
dmode=0; args=()
while [ $# -gt 0 ]; do
  case "$1" in -d) dmode=1 ;; -o|-g|-m) shift ;; *) args+=("$1") ;; esac
  shift
done
if [ "$dmode" -eq 1 ]; then mkdir -p "${args[@]}"; else cp "${args[0]}" "${args[1]}"; fi
EOF
  chmod +x "$f"/*
}

# 在 PATH 里放“记录 argv 再转交真实命令”的包装（只给机密卫生用例用，较慢）
install_spies() {
  local d=$T/spybin c real
  mkdir -p "$d"
  export SPY_LOG=$T/spy.log
  : > "$SPY_LOG"
  for c in sed grep awk cat tr head tail cut cp mv ln rm tee mktemp sort; do
    real=$(PATH=$ORIG_PATH command -v "$c" 2>/dev/null) || continue
    case "$real" in /*) ;; *) continue ;; esac
    printf '#!/bin/bash\necho "%s $*" >> "$SPY_LOG"\nexec "%s" "$@"\n' "$c" "$real" > "$d/$c"
    chmod +x "$d/$c"
  done
  export PATH="$T/fakebin:$d:$ORIG_PATH"
  hash -r
}

# fake_db users nonempty supers 1：直接设置假库的状态
fake_db() { mkdir -p "$FAKE_DB"; while [ $# -ge 2 ]; do echo "$2" > "$FAKE_DB/$1"; shift 2; done; }

# run_in 包目录 参数...：在包目录里运行 install.sh，输出到 $T/out.txt，退出码放 RC
run_in() {
  local d=$1; shift
  ( cd "$d" && bash ./install.sh "$@" ) > "$T/out.txt" 2>&1 < /dev/null
  RC=$?
  OUT=$(cat "$T/out.txt")
}
run_install() { run_in "$T/pkg" "$@"; }
# run_tty 包目录 "输入" 参数...：用管道给标准输入并让脚本把它当终端（DBT_INSTALL_TEST_TTY=1）
run_tty() {
  local d=$1 input=$2; shift 2
  ( cd "$d" && printf '%b' "$input" | DBT_INSTALL_TEST_TTY=1 bash ./install.sh "$@" ) > "$T/out.txt" 2>&1
  RC=$?
  OUT=$(cat "$T/out.txt")
}

BASE_ARGS=(--prefix "__PREFIX__" -y --database-url "mysql://dbtool:Rt%40Pass@10.0.0.5:3306/db_tool" --secret-key "SK-from-user-0123456789-0123456789-0123456789-abcdef")
base_args() { local a; for a in "${BASE_ARGS[@]}"; do if [ "$a" = "__PREFIX__" ]; then echo "$PREFIX"; else echo "$a"; fi; done; }
# run_base 额外参数...
run_base() { local args=(); while IFS= read -r l; do args+=("$l"); done < <(base_args); run_install "${args[@]}" "$@"; }
# 先装好一个 1.0.0（失败即让用例失败）
do_install() {
  run_base "$@"
  [ "$RC" -eq 0 ] || fail "前置安装失败（退出码 $RC）：$(tail -n 5 "$T/out.txt")"
}
# 准备并返回 2.0.0 包目录：$T/pkg2
mk_pkg2() { mkpkg "$T/pkg2" "${1:-2.0.0}" "${2:-2026-02-01T00:00:00Z}" "${3:-}"; }

# 目录快照（路径 + 文件内容哈希 + 符号链接目标），用于“没写任何东西”的断言
snapshot() {
  ( cd "$T" && {
      find etc opt unit -print | LC_ALL=C sort
      find etc opt unit -type f -exec sha256sum {} + | LC_ALL=C sort -k2
      find etc opt unit -type l -exec readlink {} +
    } ) 2>/dev/null
}

# 持有安装锁（模拟另一个实例在运行）；hold_lock 之后 release_lock
hold_lock() {
  if command -v flock >/dev/null 2>&1; then
    ( flock "$DBT_INSTALL_LOCK_FILE" "$REAL_SLEEP" 120 ) &
    LOCK_PID=$!
    local i
    for i in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20; do
      flock -n "$DBT_INSTALL_LOCK_FILE" true 2>/dev/null || break
      "$REAL_SLEEP" 0.2
    done
  else
    mkdir "$DBT_INSTALL_LOCK_FILE.d"
    "$REAL_SLEEP" 120 &
    LOCK_PID=$!
    echo "$LOCK_PID" > "$DBT_INSTALL_LOCK_FILE.d/pid"
  fi
}
release_lock() { kill "$LOCK_PID" 2>/dev/null; wait "$LOCK_PID" 2>/dev/null; rm -rf "$DBT_INSTALL_LOCK_FILE.d"; }

# ================= 用例：语法 / 帮助 / 基本 =================
test_syntax_and_help() {
  bash -n "$INSTALL_SH" || fail "bash -n install.sh 失败"
  bash -n "$RUN_HEADER" || fail "bash -n run-header.sh 失败"
  local h h2 opt
  h=$(bash "$INSTALL_SH" --help 2>&1)
  h2=$(bash "$INSTALL_SH" help 2>&1)
  assert_eq "$h2" "$h" "help 子命令与 --help 输出应一致"
  for opt in --bootstrap --gen-secrets --admin-user --ddl-database-url --apply-sql --database-url DBT_ADMIN_PASSWORD \
             upgrade rollback uninstall doctor init-db reconfigure --dry-run --purge --keep --allow-downgrade --to --health-timeout \
             --public-url --port --listen --timezone --require-goinception --open-firewall --restart; do
    assert_str_has "$h" "$opt"
  done
  ! grep -q $'\r' "$INSTALL_SH" || fail "install.sh 不应含 CR"
  ! grep -q $'\r' "$RUN_HEADER" || fail "run-header.sh 不应含 CR"
  # 元数据库用户表名只允许在脚本里出现一次（单一常量）
  assert_eq "$(grep -c "$USERS_KEY" "$INSTALL_SH")" 1 "install.sh 里用户表名的出现次数（单一常量）"
  assert_eq "$(grep -c "$USERS_KEY" "${BASH_SOURCE[0]}")" 1 "run.sh 里用户表名的出现次数（单一常量）"
  # 未知选项 / 缺值：退出码 2
  run_install --no-such-option
  assert_rc 2 "未知选项"
  run_install --port
  assert_rc 2 "缺值"
}

test_version_and_status() {
  run_install version
  assert_rc 0
  assert_str_has "$OUT" "1.0.0"
  assert_str_has "$OUT" "未安装"
  do_install
  run_install version
  assert_str_has "$OUT" "已安装版本: 1.0.0"
  run_install status
  assert_rc 0
  assert_str_has "$OUT" "当前版本   1.0.0"
  assert_str_has "$OUT" "监听端口   8000"
  assert_has "$FAKE_LOG" '^curl .*healthz'
}

test_fresh_install_basics() {
  run_base
  assert_rc 0
  assert_has "$ENV_FILE" '^DATABASE_URL="mysql://dbtool:Rt%40Pass@10.0.0.5:3306/db_tool"$'
  assert_has "$ENV_FILE" "^EXPORT_BASE_DIR=\"$PREFIX/data/export\"$"
  assert_has "$ENV_FILE" "^ARCHIVE_SPOOL_DIR=\"$PREFIX/data/archive-spool\"$"
  assert_file "$PREFIX/data/export"
  assert_file "$PREFIX/data/archive-spool"
  assert_has "$FAKE_LOG" "install -d -o dbtool -g dbtool -m 0750 $PREFIX/data $PREFIX/data/export $PREFIX/data/archive-spool"
  # releases / current 布局；systemd 单元指向 current
  assert_file "$PREFIX/releases/1.0.0/dbtool"
  assert_file "$PREFIX/releases/1.0.0/dbt-admin"
  assert_file "$PREFIX/releases/1.0.0/sql/001_a.sql"
  assert_file "$PREFIX/releases/1.0.0/bootstrap/001_base_schema.sql"
  assert_file "$PREFIX/releases/1.0.0/docs/ops-manual.md"
  assert_file "$PREFIX/releases/1.0.0/VERSION"
  assert_file "$PREFIX/releases/1.0.0/SHA256SUMS"
  assert_eq "$(readlink "$PREFIX/current")" "releases/1.0.0" "current 的指向"
  assert_has "$UNIT_FILE" "^WorkingDirectory=$PREFIX$"
  assert_has "$UNIT_FILE" "^ExecStart=$PREFIX/current/dbtool -config $DBT_INSTALL_CONF_DIR/dbtool.toml$"
  assert_has "$UNIT_FILE" "^EnvironmentFile=$ENV_FILE$"
  assert_has "$UNIT_FILE" '^TimeoutStopSec=660$'
  assert_has "$TOML_FILE" '^port = 8000$'
  # 没加 --bootstrap：不碰库
  assert_lacks "$FAKE_LOG" '^dbt-admin (bootstrap|admin)'
  assert_has "$FAKE_LOG" '^systemctl restart dbtool'
  # data 目录不应被递归 chown 成 root；版本目录才 chown
  assert_lacks "$FAKE_LOG" "chown -R root:root $PREFIX( |\$)"
  assert_has "$FAKE_LOG" "chown -R root:root $PREFIX/releases/\\.stage"
  assert_str_lacks "$OUT" "Rt%40Pass"
  assert_str_has "$OUT" "1.0.0"
}

test_gen_secrets_fresh() {
  # 没有 --secret-key：随机生成，写入 env，不回显
  local args=(--prefix "$PREFIX" -y --database-url "mysql://dbtool:Rt%40Pass@10.0.0.5:3306/db_tool" --gen-secrets)
  run_install "${args[@]}"
  assert_rc 0
  assert_has "$ENV_FILE" '^SECRET_KEY="FAKEGENSECRETKEY'
  assert_has "$ENV_FILE" '^MIRAGE_SECRET_KEY="FAKEGENMIRAGEKEY'
  assert_has "$ENV_FILE" '^MIRAGE_CIPHER_MODE="ECB"$'
  assert_has "$ENV_FILE" '^MIRAGE_CIPHER_IV="FAKEGENIV0123456"$'
  assert_has "$ENV_FILE" '^# SECRET_KEY / MIRAGE_\* 由 install.sh --gen-secrets 随机生成'
  assert_has "$FAKE_LOG" "chmod 0640 $ENV_FILE\\.new"
  assert_has "$FAKE_LOG" "chown root:dbtool $ENV_FILE\\.new"
  assert_str_lacks "$OUT" "FAKEGENSECRETKEY"
  assert_str_lacks "$OUT" "FAKEGENMIRAGEKEY"
  assert_str_lacks "$OUT" "FAKEGENIV"
  assert_str_has "$OUT" "SECRET_KEY"
  assert_str_has "$OUT" "备份"
  assert_str_has "$OUT" "$ENV_FILE"
}

test_gen_secrets_fallback_without_dbt_admin() {
  rm -f "$T/pkg/dbt-admin"
  (cd "$T/pkg" && find . -type f ! -name SHA256SUMS | sed 's|^\./||' | LC_ALL=C sort | xargs sha256sum > SHA256SUMS)
  run_install --prefix "$PREFIX" -y --database-url "mysql://dbtool:p@10.0.0.5:3306/db_tool" --gen-secrets
  assert_rc 0
  local sk mk iv
  sk=$(sed -nE 's/^SECRET_KEY="(.*)"$/\1/p' "$ENV_FILE")
  mk=$(sed -nE 's/^MIRAGE_SECRET_KEY="(.*)"$/\1/p' "$ENV_FILE")
  iv=$(sed -nE 's/^MIRAGE_CIPHER_IV="(.*)"$/\1/p' "$ENV_FILE")
  [ "${#sk}" -ge 50 ] || fail "SECRET_KEY 至少 50 字符，得到 ${#sk}"
  [ "${#mk}" -ge 32 ] || fail "MIRAGE_SECRET_KEY 至少 32 字符，得到 ${#mk}"
  [ "${#iv}" -eq 16 ] || fail "MIRAGE_CIPHER_IV 必须 16 字节，得到 ${#iv}"
  [ "$sk" != "$mk" ] || fail "两个密钥不应相同"
  assert_has "$ENV_FILE" '^MIRAGE_CIPHER_MODE="ECB"$'
  assert_str_lacks "$OUT" "$sk"
  assert_str_lacks "$OUT" "$mk"
}

test_gen_secrets_bad_tool_output_falls_back() {
  export FAKE_GEN_BAD=1
  run_install --prefix "$PREFIX" -y --database-url "mysql://dbtool:p@10.0.0.5:3306/db_tool" --gen-secrets
  assert_rc 0
  assert_lacks "$ENV_FILE" 'FAKEGEN'
  local sk
  sk=$(sed -nE 's/^SECRET_KEY="(.*)"$/\1/p' "$ENV_FILE")
  [ "${#sk}" -ge 50 ] || fail "回退生成的 SECRET_KEY 至少 50 字符"
}

test_gen_secrets_keeps_given_secret_key() {
  # 给了 --secret-key：沿用它，不生成 MIRAGE_*（未设置的 MIRAGE_* 沿用 SECRET_KEY）
  run_base --gen-secrets
  assert_rc 0
  assert_has "$ENV_FILE" '^SECRET_KEY="SK-from-user-'
  assert_lacks "$ENV_FILE" '^MIRAGE_SECRET_KEY='
  assert_lacks "$ENV_FILE" 'FAKEGEN'
  assert_lacks "$FAKE_LOG" 'gen-secrets'
}

test_no_secret_key_without_gen_secrets_fails() {
  run_install --prefix "$PREFIX" -y --database-url "mysql://dbtool:p@10.0.0.5:3306/db_tool"
  assert_rc 2
  assert_str_has "$OUT" "--gen-secrets"
  assert_no_file "$ENV_FILE"
  assert_no_file "$PREFIX/releases"
}

test_secrets_from_environment_variables() {
  # 连接串 / 密钥也可以用环境变量给（不出现在 install.sh 的命令行上）
  DBT_DATABASE_URL="mysql://dbtool:EnvPass@10.0.0.5:3306/db_tool" DBT_SECRET_KEY="SK-from-env-0123456789-0123456789-0123456789-abcdefgh" \
    run_install --prefix "$PREFIX" -y
  assert_rc 0
  assert_has "$ENV_FILE" '^DATABASE_URL="mysql://dbtool:EnvPass@'
  assert_has "$ENV_FILE" '^SECRET_KEY="SK-from-env-'
}

# ================= 用例：--bootstrap / 库 =================
test_bootstrap_fresh_order_and_no_leak() {
  export DBT_ADMIN_PASSWORD='Sup3r-Secret-Pw!'
  run_base --bootstrap --admin-user ops
  assert_rc 0
  assert_has "$FAKE_LOG" "^dbt-admin status --porcelain \| url_user=dbtool "
  assert_has "$FAKE_LOG" "^dbt-admin bootstrap --sql-root $PREFIX/releases/1.0.0 --apply --yes \| url_user=dbtool "
  assert_has "$FAKE_LOG" "^dbt-admin admin create --username ops --yes \| url_user=dbtool pw_env=set "
  # 顺序：状态 → bootstrap → 创建管理员 → 启动服务
  assert_before "$FAKE_LOG" '^dbt-admin status' '^dbt-admin bootstrap'
  assert_before "$FAKE_LOG" '^dbt-admin bootstrap' '^dbt-admin admin create'
  assert_before "$FAKE_LOG" '^dbt-admin admin create' '^systemctl restart'
  # 权限迁移在健康检查通过之后（撤销脚本放在配置备份目录里）
  assert_before "$FAKE_LOG" '^curl .*readyz' '^dbt-admin perm migrate --apply --yes --rollback-file '
  # 密码与连接串不上命令行、不进日志、不回显
  assert_lacks "$FAKE_LOG" 'Sup3r-Secret-Pw'
  assert_lacks "$FAKE_LOG" 'Rt%40Pass'
  assert_str_lacks "$OUT" 'Sup3r-Secret-Pw'
  assert_str_lacks "$OUT" 'Rt%40Pass'
  assert_lacks "$ENV_FILE" 'Sup3r-Secret-Pw'
  # 时区传给 dbt-admin（库里的时间是本地时间）
  assert_has "$FAKE_LOG" 'tz=Asia/Shanghai'
}

test_bootstrap_uses_ddl_account() {
  export DBT_ADMIN_PASSWORD='Sup3r-Secret-Pw!'
  run_base --bootstrap --admin-user ops --ddl-database-url "mysql://ddluser:x@10.0.0.5:3306/db_tool"
  assert_rc 0
  assert_has "$FAKE_LOG" "^dbt-admin bootstrap .*url_user=ddluser "
  assert_has "$FAKE_LOG" "^dbt-admin status .*url_user=ddluser "
  assert_has "$ENV_FILE" 'mysql://dbtool:'     # 运行账号写进 env 文件，建表账号不写
  assert_lacks "$ENV_FILE" 'ddluser'
}

test_bootstrap_noninteractive_requires_admin_credentials() {
  run_base --bootstrap                     # 缺 --admin-user 与密码
  assert_rc 2
  assert_str_has "$OUT" "DBT_ADMIN_PASSWORD"
  # 在改动系统之前失败：没有安装文件、没有重启服务、没有执行 bootstrap
  assert_no_file "$PREFIX/releases"
  assert_no_file "$ENV_FILE"
  assert_lacks "$FAKE_LOG" '^systemctl (restart|enable)'
  assert_lacks "$FAKE_LOG" '^dbt-admin bootstrap'

  teardown_env; setup_env
  export DBT_ADMIN_PASSWORD='Sup3r-Secret-Pw!'
  run_base --bootstrap                     # 有密码缺 --admin-user
  assert_rc 2
  assert_lacks "$FAKE_LOG" '^dbt-admin bootstrap'
}

test_bootstrap_nonempty_runs_only_sql() {
  # inv2：库里已有用户 → 不重复初始化基础表与数据，只执行幂等的 dbt_* 脚本；已有超级用户则不创建管理员
  fake_db users nonempty supers 1
  run_base --bootstrap --admin-user ops
  assert_rc 0
  assert_lacks "$FAKE_LOG" '^dbt-admin bootstrap --sql-root'
  assert_has "$FAKE_LOG" "^dbt-admin bootstrap --only-sql --sql-root $PREFIX/releases/1.0.0 --apply --yes"
  assert_lacks "$FAKE_LOG" '^dbt-admin admin create'
  assert_str_has "$OUT" "只执行幂等"
  assert_has "$FAKE_LOG" '^systemctl restart'     # 安装本身照常完成
}

test_bootstrap_nonempty_without_superuser_creates_admin_only() {
  fake_db users nonempty supers 0
  export DBT_ADMIN_PASSWORD='Sup3r-Secret-Pw!'
  run_base --bootstrap --admin-user ops
  assert_rc 0
  assert_lacks "$FAKE_LOG" '^dbt-admin bootstrap --sql-root'
  assert_has "$FAKE_LOG" '^dbt-admin bootstrap --only-sql'
  assert_has "$FAKE_LOG" '^dbt-admin admin create --username ops'
}

test_bootstrap_failure_stops_before_service_start() {
  export FAKE_BOOTSTRAP_RC=1 DBT_ADMIN_PASSWORD='Sup3r-Secret-Pw!'
  run_base --bootstrap --admin-user ops
  assert_ne "$RC" 0 "bootstrap 失败应使安装失败"
  assert_lacks "$FAKE_LOG" '^dbt-admin admin create'
  assert_lacks "$FAKE_LOG" '^systemctl restart'
  assert_str_has "$OUT" "bootstrap 失败"
  assert_no_file "$PREFIX/current"
}

test_bootstrap_db_unreachable_fails_early() {
  export FAKE_STATUS_RC=1 DBT_ADMIN_PASSWORD='Sup3r-Secret-Pw!'
  run_base --bootstrap --admin-user ops
  assert_ne "$RC" 0 "连不上库应失败"
  assert_no_file "$PREFIX/releases"
  assert_lacks "$FAKE_LOG" '^systemctl (restart|enable)'
}

test_bootstrap_requires_dbt_admin() {
  rm -f "$T/pkg/dbt-admin"
  (cd "$T/pkg" && find . -type f ! -name SHA256SUMS | sed 's|^\./||' | LC_ALL=C sort | xargs sha256sum > SHA256SUMS)
  export DBT_ADMIN_PASSWORD='Sup3r-Secret-Pw!'
  run_base --bootstrap --admin-user ops
  assert_ne "$RC" 0 "没有 dbt-admin 应失败"
  assert_str_has "$OUT" "dbt-admin"
}

test_apply_sql_prefers_dbt_admin_with_ddl_account() {
  run_base --apply-sql --ddl-database-url "mysql://ddluser:x@10.0.0.5:3306/db_tool"
  assert_rc 0
  assert_has "$FAKE_LOG" "^dbt-admin bootstrap --only-sql --sql-root $PREFIX/releases/1.0.0 --apply --yes \| url_user=ddluser "
  assert_lacks "$FAKE_LOG" '^mysql '
  assert_before "$FAKE_LOG" '^dbt-admin bootstrap --only-sql' '^systemctl restart'
}

test_apply_sql_env_var_for_ddl_account() {
  fake_db users nonempty supers 1
  export DDL_DATABASE_URL="mysql://ddlenv:x@10.0.0.5:3306/db_tool"
  run_base --apply-sql
  assert_rc 0
  assert_has "$FAKE_LOG" "^dbt-admin bootstrap --only-sql .*url_user=ddlenv "
}

test_apply_sql_without_ddl_account_uses_runtime_account() {
  fake_db users nonempty supers 1
  run_base --apply-sql
  assert_rc 0
  assert_has "$FAKE_LOG" '^dbt-admin bootstrap --only-sql .*url_user=dbtool '
  assert_lacks "$FAKE_LOG" '^mysql '
}

test_apply_sql_mysql_fallback_when_no_dbt_admin() {
  rm -f "$T/pkg/dbt-admin"
  (cd "$T/pkg" && find . -type f ! -name SHA256SUMS | sed 's|^\./||' | LC_ALL=C sort | xargs sha256sum > SHA256SUMS)
  run_base --apply-sql
  assert_rc 0
  # 没有 dbt-admin（旧包）：沿用原做法 mysql 客户端 + 运行账号，并提示权限问题
  assert_has "$FAKE_LOG" '^mysql -h 10.0.0.5 -P 3306 -u dbtool db_tool PWD_SET=yes'
  assert_str_has "$OUT" "CREATE 权限"
  assert_str_has "$OUT" "--ddl-database-url"
  assert_str_lacks "$OUT" "Rt%40Pass"
}

test_apply_sql_not_repeated_after_bootstrap() {
  export DBT_ADMIN_PASSWORD='Sup3r-Secret-Pw!'
  run_base --bootstrap --admin-user ops --apply-sql
  assert_rc 0
  assert_count "$FAKE_LOG" '^dbt-admin bootstrap' 1
  assert_lacks "$FAKE_LOG" '^mysql '
}

test_default_no_flags_hint() {
  run_base
  assert_rc 0
  assert_str_has "$OUT" "--bootstrap"
  assert_lacks "$FAKE_LOG" '^mysql '
  assert_lacks "$FAKE_LOG" '^dbt-admin (bootstrap|admin)'
}

test_sql_failure_with_runtime_account_is_warning_on_upgrade() {
  do_install
  fake_db users nonempty supers 1
  mk_pkg2
  export FAKE_ONLYSQL_RC=1
  run_in "$T/pkg2" upgrade
  assert_rc 0 "运行账号建表失败只警告，升级继续"
  assert_str_has "$OUT" "建表脚本失败"
  assert_eq "$(link_id)" 2.0.0 "仍应切换到新版本"
  # 给了建表账号而失败是致命的：不切换
  mk_pkg2 3.0.0 2026-03-01T00:00:00Z
  rm -rf "$T/pkg3"; mv "$T/pkg2" "$T/pkg3"
  run_in "$T/pkg3" upgrade --ddl-database-url "mysql://ddluser:x@10.0.0.5:3306/db_tool"
  assert_ne "$RC" 0 "建表账号失败应使升级失败"
  assert_eq "$(link_id)" 2.0.0 "失败时不切换"
}

# ================= 权限迁移（原权限码 → 按钮权限）=================
test_perm_migrate_runs_after_health_check_on_upgrade() {
  do_install
  fake_db users nonempty supers 1
  mk_pkg2
  : > "$FAKE_LOG"
  run_in "$T/pkg2" upgrade
  assert_rc 0
  assert_has "$FAKE_LOG" "^dbt-admin perm migrate --apply --yes --rollback-file $DBT_INSTALL_CONF_DIR/backup/.*/perm-migrate-rollback-.*\.sql \| url_user=dbtool "
  assert_before "$FAKE_LOG" '^dbt-admin bootstrap --only-sql' '^systemctl restart'
  assert_before "$FAKE_LOG" '^systemctl restart' '^curl .*healthz'
  assert_before "$FAKE_LOG" '^curl .*readyz' '^dbt-admin perm migrate'
  assert_str_has "$OUT" "权限迁移"
  assert_str_has "$OUT" "撤销脚本"
  local rb; rb=$(ls -1 "$DBT_INSTALL_CONF_DIR"/backup/*/perm-migrate-rollback-*.sql 2>/dev/null | head -n1)
  assert_file "$rb"
}

test_perm_migrate_not_run_when_new_version_is_rolled_back() {
  do_install
  fake_db users nonempty supers 1
  mk_pkg2
  export FAKE_CURL_FAIL_FOR=2.0.0
  : > "$FAKE_LOG"
  run_in "$T/pkg2" upgrade --health-timeout 3
  assert_rc 5 "新版本不健康：自动回滚"
  assert_lacks "$FAKE_LOG" '^dbt-admin perm' "没起来的版本不应动权限"
}

test_perm_migrate_failure_is_only_a_warning() {
  do_install
  fake_db users nonempty supers 1
  mk_pkg2
  export FAKE_PERM_RC=3
  run_in "$T/pkg2" upgrade
  assert_rc 0 "迁移失败只警告，升级照常完成"
  assert_eq "$(link_id)" 2.0.0
  assert_str_has "$OUT" "权限迁移没有完成"
  assert_str_has "$OUT" "perm migrate"
}

test_perm_migrate_not_run_without_db_step() {
  do_install                       # 没有 --bootstrap、也没有已有配置：不碰元数据库
  assert_lacks "$FAKE_LOG" '^dbt-admin perm'
}

test_perm_migrate_skip_flag() {
  do_install
  fake_db users nonempty supers 1
  mk_pkg2
  : > "$FAKE_LOG"
  run_in "$T/pkg2" upgrade --skip-perm-migrate
  assert_rc 0
  assert_eq "$(link_id)" 2.0.0
  assert_lacks "$FAKE_LOG" '^dbt-admin perm'
  assert_str_has "$OUT" "已指定 --skip-perm-migrate"
}

test_perm_migrate_dry_run_plans_it_without_running() {
  do_install
  fake_db users nonempty supers 1
  mk_pkg2
  : > "$FAKE_LOG"
  run_in "$T/pkg2" upgrade --dry-run
  assert_rc 0
  assert_str_has "$OUT" "权限迁移（健康检查通过后）"
  assert_lacks "$FAKE_LOG" '^dbt-admin perm'
}

# ================= inv1：重复运行安全 =================
test_inv1_reinstall_is_upgrade_and_keeps_config() {
  export DBT_ADMIN_PASSWORD='Sup3r-Secret-Pw!'
  run_install --prefix "$PREFIX" -y --database-url "mysql://dbtool:Rt%40Pass@10.0.0.5:3306/db_tool" --gen-secrets --bootstrap --admin-user ops
  assert_rc 0 "首次安装"
  echo keepme > "$PREFIX/data/export/precious.csv"
  echo spool > "$PREFIX/data/archive-spool/backup.tmp"
  # 手工修改 env 与 toml
  printf 'PUBLIC_URL="https://mine.example.com"\nMY_CUSTOM="keep me"\n' >> "$ENV_FILE"
  sed -i 's/^port = 8000$/port = 9100/' "$TOML_FILE"
  cp "$ENV_FILE" "$T/env.before"; cp "$TOML_FILE" "$T/toml.before"
  : > "$FAKE_LOG"
  # 新版本包 + 一堆会“想改配置”的选项：全部被忽略并提示
  mk_pkg2 1.1.0
  run_in "$T/pkg2" install --database-url "mysql://other:zz@10.9.9.9:3306/other" --secret-key "NEW-KEY-0123456789-0123456789-0123456789-0123456789" \
    --public-url https://x.example.com --port 7000 --gen-secrets --timezone UTC --require-goinception --bootstrap --admin-user newadmin -y
  assert_rc 0 "再次 install（升级）"
  assert_same_file "$ENV_FILE" "$T/env.before"
  assert_same_file "$TOML_FILE" "$T/toml.before"
  assert_str_has "$OUT" "reconfigure"
  assert_str_has "$OUT" "被忽略"
  # 不重新生成密钥、不重建管理员、不重复基础数据初始化
  assert_lacks "$FAKE_LOG" 'gen-secrets'
  assert_lacks "$FAKE_LOG" '^dbt-admin admin create'
  assert_lacks "$FAKE_LOG" '^dbt-admin bootstrap --sql-root'
  assert_has "$FAKE_LOG" '^dbt-admin bootstrap --only-sql'
  # 数据目录原样保留
  assert_eq "$(cat "$PREFIX/data/export/precious.csv")" keepme "导出文件"
  assert_file "$PREFIX/data/archive-spool/backup.tmp"
  assert_lacks "$FAKE_LOG" "install -d .*$PREFIX/data"
  assert_eq "$(link_id)" 1.1.0 "升级后的版本"
}

test_inv1_same_package_twice_is_noop() {
  do_install
  : > "$FAKE_LOG"
  local nb; nb=$(ls "$DBT_INSTALL_CONF_DIR/backup" 2>/dev/null | wc -l)
  cp "$ENV_FILE" "$T/env.before"; cp "$STATE_FILE" "$T/state.before"
  run_base
  assert_rc 0
  assert_same_file "$ENV_FILE" "$T/env.before"
  assert_lacks "$FAKE_LOG" '^systemctl (restart|daemon-reload)'
  assert_lacks "$FAKE_LOG" '^useradd'
  assert_eq "$(ls "$DBT_INSTALL_CONF_DIR/backup" 2>/dev/null | wc -l)" "$nb" "没有变化时不应产生新的备份"
  assert_str_has "$OUT" "没有变化"
  # 连续运行三次，env 一字不变
  run_base; assert_rc 0
  run_base; assert_rc 0
  assert_same_file "$ENV_FILE" "$T/env.before"
  assert_eq "$(list_releases)" "1.0.0 " "版本目录"
}

# goInception 初始配置：首次安装部署到 <prefix>/goinception/config.toml（属主运行用户、0640）并写入 GOINCEPTION_CONFIG_FILE；
# 用户改过的文件，升级不覆盖。
test_goinception_config_deployed_and_kept_on_upgrade() {
  do_install
  local gi=$PREFIX/goinception/config.toml
  assert_has "$ENV_FILE" "^GOINCEPTION_CONFIG_FILE=\"$gi\"$"
  assert_file "$gi"
  assert_has "$FAKE_LOG" "install -d -o dbtool -g dbtool -m 0750 $PREFIX/goinception"
  assert_has "$FAKE_LOG" "install -o dbtool -g dbtool -m 0640 .*goinception.config.toml $gi"
  echo "# user edit" >> "$gi"
  cp "$gi" "$T/gi.before"
  mk_pkg2
  run_in "$T/pkg2" upgrade
  assert_rc 0
  assert_same_file "$gi" "$T/gi.before"
  assert_count "$ENV_FILE" '^GOINCEPTION_CONFIG_FILE=' 1
}

# env 里 GOINCEPTION_CONFIG_FILE 指向别处：不部署模板、不改这个值。
test_goinception_config_not_deployed_when_env_points_elsewhere() {
  mkdir -p "$DBT_INSTALL_CONF_DIR"
  cat > "$ENV_FILE" <<'ENVEOF'
DATABASE_URL="mysql://dbtool:old@10.0.0.9:3306/db_tool"
SECRET_KEY="keep-this-secret-key-keep-this-secret-key-0123456789"
GOINCEPTION_CONFIG_FILE="/srv/gi/config.toml"
TZ="Asia/Shanghai"
ENVEOF
  run_install --prefix "$PREFIX" -y
  assert_rc 0
  assert_no_file "$PREFIX/goinception/config.toml"
  assert_has "$ENV_FILE" '^GOINCEPTION_CONFIG_FILE="/srv/gi/config.toml"$'
  assert_count "$ENV_FILE" '^GOINCEPTION_CONFIG_FILE=' 1
}

# ---- SOAR（plugins/soar）：随包部署到 <prefix>/plugins，系统设置自动指向它，升级可重复部署 ----
# add_plugins 包目录 [标记]：给安装包加上 plugins/soar（假程序）与 soar.yaml（CRLF，log-output 指向 /opt/dbtool/soar.log），并重新生成 SHA256SUMS
add_plugins() {
  local d=$1 mark=${2:-v1}
  mkdir -p "$d/plugins"
  # 文本里带上 dbtool 传给 SOAR 的参数名：install.sh 靠它们认出“这是 SQL 优化工具 SOAR”
  printf '#!/bin/sh\n# flags: -online-dsn -test-dsn -allow-online-as-test\necho fake soar %s\n' "$mark" > "$d/plugins/soar"
  PATH=$ORIG_PATH chmod +x "$d/plugins/soar"   # 真 chmod：PATH 里的假 chmod 会把调用写进日志，干扰“没有改动任何东西”的断言
  printf 'report-type: markdown\r\nlog-output: /opt/dbtool/soar.log\r\nlog-level: 7\r\n' > "$d/plugins/soar.yaml"
  (cd "$d" && find . -type f ! -name SHA256SUMS | sed 's|^\./||' | LC_ALL=C sort | xargs sha256sum > SHA256SUMS)
}

test_soar_fresh_install_deploys_files_and_settings() {
  add_plugins "$T/pkg" v1
  do_install --apply-sql
  local d=$PREFIX/plugins
  assert_file "$d/soar"
  assert_same_file "$d/soar" "$T/pkg/plugins/soar"
  assert_file "$d/soar.yaml"
  assert_file "$d/soar.yaml.dist"
  # log-output 改到运行用户可写的 <prefix>/data/soar.log；CRLF 被去掉
  assert_has "$d/soar.yaml" "^log-output: $PREFIX/data/soar.log\$"
  assert_lacks "$d/soar.yaml" $'\r'
  assert_has "$d/soar.yaml" '^report-type: markdown$'
  # 先写同目录临时文件再 rename；属主 root
  assert_has "$FAKE_LOG" "^install -m 0755 .*plugins/soar $d/\.soar\."
  assert_has "$FAKE_LOG" "^chown root:root $d/\.soar\."
  # 系统设置指向它们；测试库 DSN 要管理员自己填
  assert_has "$FAKE_LOG" "^dbt-admin settings set --key optimizer.soar_path --value $d/soar --apply --yes"
  assert_has "$FAKE_LOG" "^dbt-admin settings set --key optimizer.soar_config --value $d/soar.yaml --apply --yes"
  assert_eq "$(cat "$FAKE_DB/setting.optimizer.soar_path")" "$d/soar"
  assert_str_has "$OUT" "SOAR 还需要一个测试库"
  # 设置在建表脚本之后写（dbt_setting 表在 sql/ 里建）
  assert_before "$FAKE_LOG" '^dbt-admin bootstrap' '^dbt-admin settings set --key optimizer.soar_path'
}

test_soar_upgrade_redeploys_repeatably() {
  add_plugins "$T/pkg" v1
  do_install
  local d=$PREFIX/plugins
  # 同一个包再升级一次：什么都不重复做
  : > "$FAKE_LOG"
  run_install upgrade
  assert_rc 0
  assert_str_has "$OUT" "SOAR 已是最新"
  assert_str_has "$OUT" "系统设置 optimizer.soar_path 已是 $d/soar"
  assert_lacks "$FAKE_LOG" '^dbt-admin settings set'
  assert_lacks "$FAKE_LOG" "^install -m 0755 .*plugins/soar"
  # 新包里的 soar 变了：原子替换；管理员改过的 soar.yaml 不动，包内新版另存 .dist；设置已有值，不再写
  mk_pkg2
  add_plugins "$T/pkg2" v2
  printf 'log-output: /opt/dbtool/soar.log\r\nreport-type: json\r\n' > "$T/pkg2/plugins/soar.yaml"
  (cd "$T/pkg2" && find . -type f ! -name SHA256SUMS | sed 's|^\./||' | LC_ALL=C sort | xargs sha256sum > SHA256SUMS)
  echo "# admin edit" >> "$d/soar.yaml"
  cp "$d/soar.yaml" "$T/yaml.before"
  : > "$FAKE_LOG"
  run_in "$T/pkg2" upgrade
  assert_rc 0
  assert_same_file "$d/soar" "$T/pkg2/plugins/soar"
  assert_str_has "$OUT" "已更新 SOAR"
  assert_same_file "$d/soar.yaml" "$T/yaml.before"
  assert_str_has "$OUT" "保持不动"
  assert_has "$d/soar.yaml.dist" '^report-type: json$'
  assert_lacks "$FAKE_LOG" '^dbt-admin settings set'
  [ -z "$(ls -A "$d" | grep '^\.soar\.' || true)" ] || fail "不应残留 .soar.* 临时文件"
  # 再升级一次 pkg2：又回到“已是最新”
  run_in "$T/pkg2" upgrade
  assert_rc 0
  assert_str_has "$OUT" "SOAR 已是最新"
}

test_soar_keeps_settings_the_admin_configured() {
  add_plugins "$T/pkg" v1
  fake_db setting.optimizer.soar_path /usr/local/bin/soar setting.optimizer.soar_test_dsn x
  do_install
  assert_lacks "$FAKE_LOG" '^dbt-admin settings set --key optimizer.soar_path'
  assert_str_has "$OUT" "系统设置 optimizer.soar_path 已设置为 /usr/local/bin/soar，保持不变"
  assert_eq "$(cat "$FAKE_DB/setting.optimizer.soar_path")" /usr/local/bin/soar
  # 没设置过的配置文件项仍然会设置；测试库已配置就不再提醒
  assert_has "$FAKE_LOG" '^dbt-admin settings set --key optimizer.soar_config'
  assert_str_lacks "$OUT" "SOAR 还需要一个测试库"
}

test_soar_settings_failure_does_not_block_install() {
  add_plugins "$T/pkg" v1
  export FAKE_SETTINGS_RC=4
  run_base
  assert_rc 0
  assert_file "$PREFIX/plugins/soar"
  assert_str_has "$OUT" "没有自动配置 SOAR"
  assert_eq "$(link_id)" 1.0.0
}

# 名字也叫 soar 的其他程序（如同名的软件包管理器）：不部署、不改系统设置，但安装照常成功
test_soar_wrong_program_is_not_deployed() {
  add_plugins "$T/pkg" v1
  printf '#!/bin/sh\necho I am the soar package manager\n' > "$T/pkg/plugins/soar"
  (cd "$T/pkg" && find . -type f ! -name SHA256SUMS | sed 's|^\./||' | LC_ALL=C sort | xargs sha256sum > SHA256SUMS)
  run_base
  assert_rc 0
  assert_str_has "$OUT" "不像 SQL 优化工具 SOAR"
  assert_no_file "$PREFIX/plugins"
  assert_lacks "$FAKE_LOG" '^dbt-admin settings'
  assert_eq "$(link_id)" 1.0.0
}

test_soar_package_without_plugins_is_unaffected() {
  do_install
  assert_no_file "$PREFIX/plugins"
  assert_lacks "$FAKE_LOG" '^dbt-admin settings'
  assert_str_has "$OUT" "安装包里没有 plugins/soar"
}

test_soar_dry_run_writes_nothing() {
  add_plugins "$T/pkg" v1
  snapshot > "$T/snap.before"
  run_base --dry-run
  assert_rc 0
  assert_str_has "$OUT" "部署 SOAR 程序到 $PREFIX/plugins/soar"
  assert_str_has "$OUT" "optimizer.soar_path / optimizer.soar_config"
  snapshot > "$T/snap.after"
  assert_same_file "$T/snap.before" "$T/snap.after"
  assert_lacks "$FAKE_LOG" "$MUTATING_RE"
  assert_lacks "$FAKE_LOG" '^dbt-admin settings'
}

test_soar_uninstall_removes_plugins_but_keeps_data() {
  add_plugins "$T/pkg" v1
  do_install
  assert_file "$PREFIX/plugins/soar"
  run_install uninstall
  assert_rc 0
  assert_no_file "$PREFIX/plugins"
  assert_file "$PREFIX/data"
}

test_soar_doctor_reports_deployment() {
  add_plugins "$T/pkg" v1
  run_install doctor --prefix "$PREFIX"
  assert_str_has "$OUT" "SOAR 尚未部署"
  do_install
  run_install doctor
  assert_str_has "$OUT" "SOAR 已部署"
  assert_str_has "$OUT" "SOAR 与安装包里的一致"
  mk_pkg2
  add_plugins "$T/pkg2" v2
  run_in "$T/pkg2" doctor
  assert_str_has "$OUT" "SOAR 与安装包里的不同"
}

test_inv1_upgrade_only_backfills_missing_defaults() {
  do_install
  # 模拟“新版本新增了配置项”：env 里缺 ARCHIVE_SPOOL_DIR，EXPORT_BASE_DIR 是用户改过的值
  sed -i -e '/^ARCHIVE_SPOOL_DIR=/d' -e 's#^EXPORT_BASE_DIR=.*#EXPORT_BASE_DIR="/srv/exports"#' "$ENV_FILE"
  cp "$ENV_FILE" "$T/env.before"
  mk_pkg2
  run_in "$T/pkg2" upgrade
  assert_rc 0
  assert_has "$ENV_FILE" '^EXPORT_BASE_DIR="/srv/exports"$'
  assert_has "$ENV_FILE" "^ARCHIVE_SPOOL_DIR=\"$PREFIX/data/archive-spool\"$"
  assert_count "$ENV_FILE" '^EXPORT_BASE_DIR=' 1
  # 除新增的一行外，其余与升级前逐行相同
  diff <(cat "$T/env.before") <(grep -v '^ARCHIVE_SPOOL_DIR=' "$ENV_FILE") >/dev/null || fail "env 里除补缺省外不应有其它变化"
}

test_upgrade_keeps_existing_env_values_without_state() {
  # 旧版本安装（没有状态文件）的 env：沿用，只补缺省
  mkdir -p "$DBT_INSTALL_CONF_DIR"
  cat > "$ENV_FILE" <<'EOF'
DATABASE_URL="mysql://dbtool:old@10.0.0.9:3306/db_tool"
SECRET_KEY="keep-this-secret-key-keep-this-secret-key-0123456789"
EXPORT_BASE_DIR="/srv/exports"
TZ="Asia/Shanghai"
EOF
  mkdir -p "$PREFIX/data/export"
  echo keepme > "$PREFIX/data/export/precious.csv"
  run_install --prefix "$PREFIX" -y
  assert_rc 0 "升级"
  assert_has "$ENV_FILE" '^EXPORT_BASE_DIR="/srv/exports"$'
  assert_lacks "$ENV_FILE" "EXPORT_BASE_DIR=\"$PREFIX/data/export\""
  assert_has "$ENV_FILE" "^ARCHIVE_SPOOL_DIR=\"$PREFIX/data/archive-spool\"$"
  assert_has "$ENV_FILE" '^SECRET_KEY="keep-this-secret-key'
  assert_count "$ENV_FILE" '^EXPORT_BASE_DIR=' 1
  assert_file "$PREFIX/data/export/precious.csv"
  assert_str_has "$OUT" "已有"
  # 再运行一次：env 文件不再变化（幂等）
  cp "$ENV_FILE" "$T/env.before"
  run_install --prefix "$PREFIX" -y
  assert_rc 0 "再次运行"
  assert_same_file "$ENV_FILE" "$T/env.before"
}

test_env_without_trailing_newline() {
  mkdir -p "$DBT_INSTALL_CONF_DIR"
  printf 'DATABASE_URL="mysql://dbtool:old@10.0.0.9:3306/db_tool"\nSECRET_KEY="k-0123456789-0123456789-0123456789-0123456789-0123456"' > "$ENV_FILE"
  run_install --prefix "$PREFIX" -y
  assert_rc 0
  assert_has "$ENV_FILE" '^SECRET_KEY="k-0123456789'
  assert_has "$ENV_FILE" "^EXPORT_BASE_DIR="
  # 追加不能和上一行粘在一起
  assert_lacks "$ENV_FILE" 'k-0123456789-0123456789-0123456789-0123456789-0123456"EXPORT_BASE_DIR'
}

test_upgrade_gen_secrets_does_not_touch_keys() {
  run_base
  assert_rc 0 "第一次"
  cp "$ENV_FILE" "$T/env.before"
  run_install --prefix "$PREFIX" -y --gen-secrets
  assert_rc 0 "再次运行"
  assert_str_has "$OUT" "--gen-secrets"
  assert_same_file "$ENV_FILE" "$T/env.before"
}

test_legacy_listen_kept_and_toml_generated() {
  # 旧版本把端口写在 env 的 LISTEN=：补生成等价的 toml，env 里的 LISTEN 不动（行为不变）
  mkdir -p "$DBT_INSTALL_CONF_DIR"
  cat > "$ENV_FILE" <<'EOF'
DATABASE_URL="mysql://dbtool:old@10.0.0.9:3306/db_tool"
SECRET_KEY="keep-this-secret-key-keep-this-secret-key-0123456789"
LISTEN="127.0.0.1:9000"
EOF
  cp "$ENV_FILE" "$T/env.before"
  run_install --prefix "$PREFIX" -y
  assert_rc 0
  assert_has "$TOML_FILE" '^port = 9000$'
  assert_has "$TOML_FILE" '^host = "127.0.0.1"$'
  assert_has "$ENV_FILE" '^LISTEN="127.0.0.1:9000"$'
  assert_str_has "$OUT" "LISTEN"
  # 健康检查用 127.0.0.1:9000
  assert_has "$FAKE_LOG" '^curl .*127.0.0.1:9000/healthz'
}

test_prefix_and_user_conflict_with_installed() {
  do_install
  run_install --prefix "$T/opt/other" -y
  assert_rc 2
  assert_str_has "$OUT" "不支持用 --prefix"
  run_install --user otheruser -y
  assert_rc 2
  assert_str_has "$OUT" "不支持用 --user"
  assert_eq "$(link_id)" 1.0.0
}

# ================= inv2：库保护 =================
test_inv2_init_db_only_when_empty() {
  do_install
  export DBT_ADMIN_PASSWORD='Sup3r-Secret-Pw!'
  fake_db users empty supers 0
  : > "$FAKE_LOG"
  run_install init-db --admin-user ops
  assert_rc 0
  assert_has "$FAKE_LOG" "^dbt-admin bootstrap --sql-root $PREFIX/current --apply --yes"
  assert_has "$FAKE_LOG" '^dbt-admin admin create --username ops'
  # 现在库里有用户了：再运行 init-db 只执行 --only-sql，不再创建管理员
  : > "$FAKE_LOG"
  run_install init-db --admin-user ops
  assert_rc 0
  assert_lacks "$FAKE_LOG" '^dbt-admin bootstrap --sql-root'
  assert_has "$FAKE_LOG" "^dbt-admin bootstrap --only-sql --sql-root $PREFIX/current --apply --yes"
  assert_lacks "$FAKE_LOG" '^dbt-admin admin create'
  assert_str_lacks "$OUT" 'Sup3r-Secret-Pw'
}

test_inv2_no_drop_or_truncate_and_uninstall_never_touches_db() {
  export DBT_ADMIN_PASSWORD='Sup3r-Secret-Pw!'
  run_base --bootstrap --admin-user ops --open-firewall
  assert_rc 0
  mk_pkg2
  run_in "$T/pkg2" upgrade; assert_rc 0 "升级"
  run_install init-db; assert_rc 0 "init-db"
  run_install reconfigure --public-url https://a.example.com -y --no-restart; assert_rc 0 "reconfigure"
  run_install rollback -y; assert_rc 0 "rollback"
  assert_lacks "$FAKE_LOG" 'DROP +(DATABASE|SCHEMA|TABLE)|drop +(database|schema|table)|TRUNCATE|truncate'
  [ ! -f "$FAKE_LOG.mysql-stdin" ] || assert_lacks "$FAKE_LOG.mysql-stdin" 'DROP|drop|TRUNCATE|truncate'
  # 卸载（含 --purge）：一次库调用都没有
  : > "$FAKE_LOG"
  run_install uninstall --purge --yes
  assert_rc 0
  assert_lacks "$FAKE_LOG" '^(dbt-admin|mysql) '
  assert_str_has "$OUT" "没有动"
}

# ================= inv3：升级可回滚 =================
test_upgrade_skips_sql_when_db_is_empty() {
  do_install
  mk_pkg2
  : > "$FAKE_LOG"
  run_in "$T/pkg2" upgrade
  assert_rc 0
  assert_str_has "$OUT" "元数据库还是空的"
  assert_lacks "$FAKE_LOG" '^dbt-admin bootstrap'
  assert_eq "$(link_id)" 2.0.0
}

test_inv3_upgrade_flow_layout_and_order() {
  do_install
  fake_db users nonempty supers 1
  mk_pkg2
  : > "$FAKE_LOG"
  run_in "$T/pkg2" upgrade
  assert_rc 0
  assert_eq "$(list_releases)" "1.0.0 2.0.0 " "版本目录"
  assert_eq "$(readlink "$PREFIX/current")" "releases/2.0.0"
  assert_eq "$(st_get VERSION)" 2.0.0
  assert_eq "$(st_get PREVIOUS_VERSION)" 1.0.0
  # 配置备份（env 0600，其余 0640）
  local bk; bk=$(ls -1 "$DBT_INSTALL_CONF_DIR/backup" | head -n1)
  assert_file "$DBT_INSTALL_CONF_DIR/backup/$bk/dbtool.env"
  assert_file "$DBT_INSTALL_CONF_DIR/backup/$bk/dbtool.toml"
  assert_file "$DBT_INSTALL_CONF_DIR/backup/$bk/dbtool.service"
  assert_has "$FAKE_LOG" "install -m 0600 $ENV_FILE $DBT_INSTALL_CONF_DIR/backup/$bk/dbtool.env"
  assert_has "$FAKE_LOG" "install -m 0640 $TOML_FILE $DBT_INSTALL_CONF_DIR/backup/$bk/dbtool.toml"
  assert_has "$FAKE_LOG" "install -d -m 0700 $DBT_INSTALL_CONF_DIR/backup"
  # 顺序：预检（dbt-admin status）→ 建表脚本（--only-sql）→ 重启 → 健康检查
  assert_before "$FAKE_LOG" '^dbt-admin status' '^dbt-admin bootstrap --only-sql'
  assert_before "$FAKE_LOG" '^dbt-admin bootstrap --only-sql' '^systemctl restart'
  assert_before "$FAKE_LOG" '^systemctl restart' '^curl .*healthz'
  assert_before "$FAKE_LOG" '^curl .*healthz' '^curl .*readyz'
  assert_has "$FAKE_LOG" "^dbt-admin bootstrap --only-sql --sql-root $PREFIX/releases/2.0.0 --apply --yes"
  # 新版本目录的校验清单与文件
  assert_file "$PREFIX/releases/2.0.0/SHA256SUMS"
  assert_str_has "$OUT" "sha256 校验通过"
}

test_inv3_keep_prunes_old_releases() {
  do_install
  local v
  for v in 2.0.0 3.0.0 4.0.0 5.0.0; do
    mkpkg "$T/pk$v" "$v" "2026-0${v%%.*}-01T00:00:00Z"
    run_in "$T/pk$v" upgrade --keep 3
    assert_rc 0 "升级到 $v"
    "$REAL_SLEEP" 1.1   # 让目录 mtime 有区分
  done
  assert_eq "$(link_id)" 5.0.0
  local n; n=$(ls -1 "$PREFIX/releases" | grep -vc '^\.')
  assert_eq "$n" 3 "保留的版本数（--keep 3）"
  assert_file "$PREFIX/releases/5.0.0"
  assert_file "$PREFIX/releases/4.0.0"     # 上一版本总是保留
  assert_no_file "$PREFIX/releases/1.0.0"
}

test_inv3_auto_rollback_on_unhealthy() {
  do_install
  mk_pkg2
  export FAKE_CURL_FAIL_FOR=2.0.0
  : > "$FAKE_LOG"
  run_in "$T/pkg2" upgrade --health-timeout 3
  assert_rc 5 "失败自动回滚后的退出码"
  assert_eq "$(link_id)" 1.0.0 "应回到旧版本"
  assert_eq "$(st_get VERSION)" 1.0.0
  assert_count "$FAKE_LOG" '^systemctl restart' 2      # 升级重启 + 回滚重启
  assert_str_has "$OUT" "已回滚"
  assert_str_has "$OUT" "healthz"                      # 明确原因
  assert_str_has "$OUT" "已完成"
  assert_file "$PREFIX/releases/2.0.0"                 # 失败版本保留供排查
}

test_inv3_rollback_when_service_dies() {
  do_install
  mk_pkg2
  # 新版本起不来：is-active 对新版本为非 active（用 curl 失败 + 服务进程退出模拟）
  export FAKE_CURL_FAIL_ALL=1 FAKE_SVC_INACTIVE=1
  run_in "$T/pkg2" upgrade --health-timeout 3
  # 回滚后服务仍不健康（假环境里它对两个版本一样）：应报“无法恢复”
  assert_rc 6
  assert_str_has "$OUT" "需要人工处理"
}

test_inv3_manual_rollback_and_toggle() {
  do_install
  mk_pkg2
  run_in "$T/pkg2" upgrade; assert_rc 0
  : > "$FAKE_LOG"
  run_install rollback -y
  assert_rc 0
  assert_eq "$(link_id)" 1.0.0
  assert_eq "$(st_get VERSION)" 1.0.0
  assert_eq "$(st_get PREVIOUS_VERSION)" 2.0.0
  assert_has "$FAKE_LOG" '^systemctl restart'
  assert_lacks "$FAKE_LOG" '^dbt-admin'                # 回滚不碰库
  # 再回滚一次回到 2.0.0
  run_install rollback -y
  assert_rc 0
  assert_eq "$(link_id)" 2.0.0
  # --to 指定版本；不存在的版本报错
  run_install rollback --to 1.0.0 -y
  assert_rc 0
  assert_eq "$(link_id)" 1.0.0
  run_install rollback --to 9.9.9 -y
  assert_rc 2
  assert_str_has "$OUT" "不存在"
  run_install rollback --to ../../etc -y
  assert_rc 2
}

test_inv3_rollback_without_previous_fails() {
  do_install
  run_install rollback -y
  assert_rc 2
  assert_str_has "$OUT" "没有记录上一版本"
}

test_inv3_downgrade_refused_and_allowed() {
  do_install
  mk_pkg2 2.0.0
  run_in "$T/pkg2" upgrade; assert_rc 0
  mkpkg "$T/pkgold" 1.5.0 2025-12-01T00:00:00Z
  : > "$FAKE_LOG"
  run_in "$T/pkgold" upgrade
  assert_rc 7 "降级应拒绝"
  assert_str_has "$OUT" "--allow-downgrade"
  assert_eq "$(link_id)" 2.0.0
  assert_lacks "$FAKE_LOG" '^systemctl restart'
  run_in "$T/pkgold" upgrade --allow-downgrade
  assert_rc 0
  assert_eq "$(link_id)" 1.5.0
  assert_str_has "$OUT" "降级"
}

test_inv3_downgrade_by_build_time_for_non_semver() {
  mkpkg "$T/pkg" abc1234 2026-03-01T00:00:00Z
  run_install --prefix "$PREFIX" -y --database-url "mysql://dbtool:p@10.0.0.5:3306/db_tool" --secret-key "SK-0123456789-0123456789-0123456789-0123456789-abcdef"
  assert_rc 0
  mkpkg "$T/pkg2" def5678 2026-01-01T00:00:00Z
  run_in "$T/pkg2" upgrade
  assert_rc 7 "非 semver 版本按 build_time 判定降级"
}

test_inv3_checksum_mismatch_aborts_before_switch() {
  do_install
  mk_pkg2
  echo "tampered" >> "$T/pkg2/dbtool"
  : > "$FAKE_LOG"
  run_in "$T/pkg2" upgrade
  assert_ne "$RC" 0
  assert_rc 3
  assert_str_has "$OUT" "sha256"
  assert_eq "$(link_id)" 1.0.0
  assert_lacks "$FAKE_LOG" '^systemctl restart'
  assert_no_file "$PREFIX/releases/2.0.0"
  [ -z "$(ls -A "$PREFIX/releases" | grep -F '.stage')" ] || fail "不应留下 .stage 临时目录"
}

test_inv3_preflight_failure_changes_nothing() {
  do_install
  mk_pkg2
  snapshot > "$T/snap.before"
  export FAKE_DF_AVAIL_KB=1000          # 磁盘不足
  : > "$FAKE_LOG"
  run_in "$T/pkg2" upgrade
  assert_rc 3
  assert_str_has "$OUT" "磁盘空间不足"
  snapshot > "$T/snap.after"
  assert_same_file "$T/snap.before" "$T/snap.after"
  assert_lacks "$FAKE_LOG" '^systemctl restart'
  # --skip-preflight 可强制继续
  run_in "$T/pkg2" upgrade --skip-preflight
  assert_rc 0
}

test_inv3_same_version_different_binary_gets_distinct_release() {
  do_install
  mkpkg "$T/pkg2" 1.0.0 2026-01-01T00:00:00Z rebuilt
  run_in "$T/pkg2" upgrade
  assert_rc 0
  local id; id=$(link_id)
  assert_ne "$id" 1.0.0 "二进制不同的同版本号应使用带 sha 后缀的目录"
  case "$id" in 1.0.0-*) ;; *) fail "目录名应为 1.0.0-<sha8>，得到 $id" ;; esac
  assert_file "$PREFIX/releases/1.0.0/dbtool"
  assert_eq "$(st_get PREVIOUS_VERSION)" 1.0.0
}

# ================= inv4：状态文件 =================
test_inv4_state_file_fields() {
  export FAKE_ID_MISSING=1
  run_base --open-firewall
  assert_rc 0
  assert_file "$STATE_FILE"
  assert_eq "$(st_get INSTALLED)" 1
  assert_eq "$(st_get VERSION)" 1.0.0
  assert_eq "$(st_get PREFIX)" "$PREFIX"
  assert_eq "$(st_get RUN_USER)" dbtool
  assert_eq "$(st_get USER_CREATED)" 1
  assert_eq "$(st_get PORT)" 8000
  assert_eq "$(st_get FIREWALL_OPENED)" 1
  assert_eq "$(st_get FIREWALL_PORT)" 8000
  assert_eq "$(st_get UNIT_SHA256)" "$(sha_file "$UNIT_FILE")" "单元文件 sha256"
  [ -n "$(st_get INSTALLED_AT)" ] || fail "INSTALLED_AT 不应为空"
  assert_has "$FAKE_LOG" "chmod 0640 $STATE_FILE"
  # 不含机密
  assert_lacks "$STATE_FILE" 'Rt%40Pass|SK-from-user'
}

test_inv4_firewall_rule_not_ours_is_not_recorded() {
  echo "8000/tcp" > "$FAKE_LOG.fwports"
  run_base --open-firewall
  assert_rc 0
  assert_lacks "$FAKE_LOG" 'firewall-cmd .*--add-port'
  assert_ne "$(st_get FIREWALL_OPENED)" 1 "不是本脚本添加的规则不应记录"
  run_install uninstall --yes
  assert_rc 0
  assert_lacks "$FAKE_LOG" 'firewall-cmd .*--remove-port'
  assert_has "$FAKE_LOG.fwports" '^8000/tcp$'
}

# ================= inv5：卸载 =================
test_inv5_uninstall_keeps_data_and_config_unless_purge() {
  export FAKE_ID_MISSING=1
  run_base --open-firewall
  assert_rc 0
  echo spool > "$PREFIX/data/archive-spool/backup.tmp"
  : > "$FAKE_LOG"
  run_install uninstall
  assert_rc 0
  assert_file "$PREFIX/data/archive-spool/backup.tmp"
  assert_no_file "$PREFIX/releases"
  assert_no_file "$PREFIX/current"
  assert_no_file "$UNIT_FILE"
  assert_file "$ENV_FILE"
  assert_file "$TOML_FILE"
  assert_has "$FAKE_LOG" '^systemctl disable --now dbtool'
  # 只回收本脚本添加的防火墙规则；由本脚本创建的用户随卸载一起删除（保留的数据目录与配置先改成 root 属主）
  assert_has "$FAKE_LOG" 'firewall-cmd .*--remove-port=8000/tcp'
  assert_has "$FAKE_LOG" "^chown -R root:root $PREFIX/data"
  assert_has "$FAKE_LOG" '^userdel dbtool'
  assert_before "$FAKE_LOG" "^chown -R root:root $PREFIX/data" '^userdel dbtool'
  assert_lacks "$FAKE_LOG" '^(dbt-admin|mysql) '
  assert_eq "$(st_get INSTALLED)" 0
  assert_eq "$(st_get USER_CREATED)" 0 "用户已删除"
  assert_eq "$(st_get DATA_RESET)" 1 "状态里记下数据目录属主被重置，重装时还原"
  assert_str_has "$OUT" "保留"
  # --purge 才删除 etc 与 data（用户此时已经不在，不再重复删除）
  run_install uninstall --purge --yes
  assert_rc 0
  assert_no_file "$PREFIX"
  assert_no_file "$DBT_INSTALL_CONF_DIR"
}

test_inv5_uninstall_removes_script_created_user_but_not_foreign_user() {
  # 用户不是本脚本创建的（预先存在）：卸载不删
  export FAKE_ID_MISSING=
  do_install
  assert_eq "$(st_get USER_CREATED)" "" "用户本来就存在，状态里不记 USER_CREATED"
  : > "$FAKE_LOG"
  run_install uninstall
  assert_rc 0
  assert_lacks "$FAKE_LOG" '^userdel'
  assert_lacks "$FAKE_LOG" "^chown -R root:root $PREFIX/data"
}

test_inv5_purge_requires_confirmation_and_lists_paths() {
  do_install
  echo spool > "$PREFIX/data/archive-spool/backup.tmp"
  # 非交互且没有 --yes：拒绝，什么都不删
  run_install uninstall --purge
  assert_rc 2
  assert_str_has "$OUT" "--yes"
  assert_file "$PREFIX/data/archive-spool/backup.tmp"
  assert_file "$ENV_FILE"
  assert_file "$PREFIX/current"
  # 交互：输入不是 PURGE → 取消
  run_tty "$T/pkg" 'no\n' uninstall --purge
  assert_rc 0
  assert_str_has "$OUT" "已取消"
  assert_file "$ENV_FILE"
  assert_file "$PREFIX/data/archive-spool/backup.tmp"
  # 交互：输入 PURGE（先打印将删除的绝对路径清单）
  run_tty "$T/pkg" 'PURGE\n' uninstall --purge
  assert_rc 0
  assert_str_has "$OUT" "$PREFIX"
  assert_str_has "$OUT" "$DBT_INSTALL_CONF_DIR"
  assert_no_file "$PREFIX"
  assert_no_file "$DBT_INSTALL_CONF_DIR"
}

test_inv5_purge_yes_prints_list_first() {
  do_install
  run_install uninstall --purge --yes
  assert_rc 0
  assert_str_has "$OUT" "将要删除"
  assert_str_has "$OUT" "$PREFIX"
  assert_str_has "$OUT" "$DBT_INSTALL_CONF_DIR"
  assert_str_has "$OUT" "不会删除：数据库"
  assert_no_file "$PREFIX"
}

test_inv5_reinstall_after_uninstall_reuses_config() {
  run_install --prefix "$PREFIX" -y --database-url "mysql://dbtool:Rt%40Pass@10.0.0.5:3306/db_tool" --gen-secrets
  assert_rc 0
  cp "$ENV_FILE" "$T/env.before"
  mkdir -p "$DBT_INSTALL_CONF_DIR/dbtool.service.d"
  run_install uninstall
  assert_rc 0
  : > "$FAKE_LOG"
  run_install --prefix "$PREFIX" -y --gen-secrets
  assert_rc 0
  assert_str_has "$OUT" "旧配置"
  assert_same_file "$ENV_FILE" "$T/env.before"
  assert_lacks "$FAKE_LOG" 'gen-secrets'
  assert_eq "$(link_id)" 1.0.0
  assert_eq "$(st_get INSTALLED)" 1
  assert_has "$FAKE_LOG" '^systemctl restart'
}

test_inv5_reinstall_after_uninstall_restores_data_ownership() {
  export FAKE_ID_MISSING=1
  run_base
  assert_rc 0
  echo spool > "$PREFIX/data/archive-spool/backup.tmp"
  run_install uninstall
  assert_rc 0
  assert_has "$FAKE_LOG" '^userdel dbtool'
  : > "$FAKE_LOG"
  run_base
  assert_rc 0
  # 用户重新创建，保留下来的数据目录还给它，配置文件组还原
  assert_has "$FAKE_LOG" '^useradd .*dbtool'
  assert_has "$FAKE_LOG" "^chown -R dbtool $PREFIX/data"
  assert_eq "$(st_get DATA_RESET)" 0
  assert_eq "$(st_get USER_CREATED)" 1
  assert_file "$PREFIX/data/archive-spool/backup.tmp"
}

test_inv5_uninstall_on_clean_host_is_harmless() {
  run_install uninstall
  assert_rc 0
  assert_no_file "$PREFIX"
}

# ================= inv6：不丢用户修改 =================
test_inv6_unit_modification_backed_up_and_dropin_untouched() {
  do_install
  mkdir -p "$UNIT_FILE.d"
  printf '[Service]\nReadWritePaths=/etc/goinception\n' > "$UNIT_FILE.d/override.conf"
  local dropin_sha; dropin_sha=$(sha_file "$UNIT_FILE.d/override.conf")
  # 手工改主单元
  echo "Environment=MY_EXTRA=1" >> "$UNIT_FILE"
  mk_pkg2
  run_in "$T/pkg2" upgrade
  assert_rc 0
  assert_file "$UNIT_FILE.bak"
  assert_has "$UNIT_FILE.bak" 'MY_EXTRA=1'
  assert_lacks "$UNIT_FILE" 'MY_EXTRA=1'
  assert_str_has "$OUT" "drop-in"
  assert_eq "$(sha_file "$UNIT_FILE.d/override.conf")" "$dropin_sha" "drop-in 不应被改动"
  local bk; bk=$(ls -1t "$DBT_INSTALL_CONF_DIR/backup" | head -n1)
  assert_has "$DBT_INSTALL_CONF_DIR/backup/$bk/dbtool.service" 'MY_EXTRA=1'
  # 卸载：主单元被改过 → 备份为 .bak；drop-in 仍在
  echo "Environment=MY_EXTRA=2" >> "$UNIT_FILE"
  run_install uninstall
  assert_rc 0
  assert_has "$UNIT_FILE.bak" 'MY_EXTRA=2'
  assert_file "$UNIT_FILE.d/override.conf"
  assert_no_file "$UNIT_FILE"
}

test_inv6_nothing_but_reconfigure_changes_config() {
  do_install
  cp "$ENV_FILE" "$T/env.before"; cp "$TOML_FILE" "$T/toml.before"
  mk_pkg2; run_in "$T/pkg2" upgrade; assert_rc 0
  run_install rollback -y; assert_rc 0
  run_install init-db; assert_rc 0
  run_install doctor; assert_rc 0
  assert_same_file "$ENV_FILE" "$T/env.before"
  assert_same_file "$TOML_FILE" "$T/toml.before"
}

# ================= inv7：幂等 / 并发 / dry-run / 失败报告 =================
test_inv7_dry_run_writes_nothing() {
  export DBT_ADMIN_PASSWORD='Sup3r-Secret-Pw!'
  # 全新主机的 install --dry-run
  snapshot > "$T/snap.before"
  run_base --bootstrap --admin-user ops --open-firewall --dry-run
  assert_rc 0 "全新主机 dry-run"
  assert_str_has "$OUT" "dry-run"
  snapshot > "$T/snap.after"
  assert_same_file "$T/snap.before" "$T/snap.after"
  assert_lacks "$FAKE_LOG" "$MUTATING_RE"
  assert_no_file "$INSTALL_LOG"
  assert_no_file "$STATE_FILE"
  # 已安装主机上的各子命令
  teardown_env; setup_env
  export DBT_ADMIN_PASSWORD='Sup3r-Secret-Pw!'
  do_install --open-firewall
  mk_pkg2
  run_in "$T/pkg2" upgrade; assert_rc 0 "前置升级"
  mkpkg "$T/pkg3" 3.0.0 2026-03-01T00:00:00Z
  local c
  for c in "upgrade|$T/pkg3" "install|$T/pkg3" "uninstall|$T/pkg" "uninstall --purge|$T/pkg" "reconfigure --public-url https://n.example.com --port 9300|$T/pkg" \
           "init-db --admin-user ops|$T/pkg" "rollback|$T/pkg" "doctor|$T/pkg"; do
    snapshot > "$T/snap.before"
    : > "$FAKE_LOG"
    # shellcheck disable=SC2086
    run_in "${c#*|}" ${c%%|*} --dry-run
    assert_rc 0 "dry-run: ${c%%|*}"
    snapshot > "$T/snap.after"
    assert_same_file "$T/snap.before" "$T/snap.after"
    assert_lacks "$FAKE_LOG" "$MUTATING_RE"
  done
  assert_eq "$(link_id)" 2.0.0
}

test_inv7_dry_run_prints_plan() {
  do_install
  mk_pkg2
  run_in "$T/pkg2" upgrade --dry-run
  assert_rc 0
  assert_str_has "$OUT" "[dry-run]"
  assert_str_has "$OUT" "releases/2.0.0"
  assert_str_has "$OUT" "1.0.0 → 2.0.0"
}

test_inv7_concurrent_run_is_refused() {
  hold_lock
  run_base
  local rc=$RC
  release_lock
  assert_eq "$rc" 4 "拿不到锁的退出码"
  assert_str_has "$OUT" "正在运行"
  assert_no_file "$PREFIX/releases"
  assert_no_file "$ENV_FILE"
  assert_lacks "$FAKE_LOG" '^systemctl'
  # 锁释放后可以正常运行
  run_base
  assert_rc 0
}

test_inv7_failure_reports_done_and_pending_steps_and_rerun_is_safe() {
  export FAKE_BOOTSTRAP_RC=1 DBT_ADMIN_PASSWORD='Sup3r-Secret-Pw!'
  run_install --prefix "$PREFIX" -y --database-url "mysql://dbtool:Rt%40Pass@10.0.0.5:3306/db_tool" --gen-secrets --bootstrap --admin-user ops
  assert_ne "$RC" 0
  assert_str_has "$OUT" "已完成"
  assert_str_has "$OUT" "失败于: 元数据库初始化"
  assert_str_has "$OUT" "未执行"
  assert_str_has "$OUT" "切换版本并重启服务"
  assert_str_lacks "$OUT" "Sup3r-Secret-Pw"
  # 配置已写入（含生成的密钥）；修复后直接重跑同一条命令：不重新生成密钥，补完剩余步骤
  cp "$ENV_FILE" "$T/env.before"
  unset FAKE_BOOTSTRAP_RC
  run_install --prefix "$PREFIX" -y --database-url "mysql://dbtool:Rt%40Pass@10.0.0.5:3306/db_tool" --gen-secrets --bootstrap --admin-user ops
  assert_rc 0 "重跑"
  assert_same_file "$ENV_FILE" "$T/env.before"
  assert_has "$FAKE_LOG" '^dbt-admin admin create --username ops'
  assert_eq "$(link_id)" 1.0.0
  assert_eq "$(st_get INSTALLED)" 1
}

test_inv7_interrupted_stage_dir_is_cleaned() {
  # 上次中断留下的残缺版本目录（没有完成标记）：下次运行会清掉重来
  mkdir -p "$PREFIX/releases/1.0.0"
  echo junk > "$PREFIX/releases/1.0.0/dbtool"
  mkdir -p "$PREFIX/releases/.stage.99999"
  run_base
  assert_rc 0
  assert_file "$PREFIX/releases/1.0.0/.installed_at"
  assert_no_file "$PREFIX/releases/.stage.99999"
  assert_lacks "$PREFIX/releases/1.0.0/dbtool" '^junk$'
}

# ================= inv8：机密卫生 =================
test_inv8_secrets_never_in_argv_logs_or_output() {
  install_spies
  export DBT_ADMIN_PASSWORD='Sup3r-Secret-Pw!'
  run_install --prefix "$PREFIX" -y --database-url "mysql://dbtool:Rt%40Pass@10.0.0.5:3306/db_tool" \
    --ddl-database-url "mysql://ddluser:DdlPassw0rd@10.0.0.5:3306/db_tool" --gen-secrets --bootstrap --admin-user ops --public-url https://dba.example.com
  assert_rc 0 "首次安装"
  local out1=$OUT
  # 升级与 reconfigure 同样不泄漏
  mk_pkg2
  run_in "$T/pkg2" upgrade
  assert_rc 0 "升级"
  local out2=$OUT
  run_install reconfigure --secret-key 'NewSecretKey-0123456789-0123456789-0123456789-0123456789' --database-url 'mysql://dbtool:NewDbPass1@10.0.0.5:3306/db_tool' --yes --no-restart
  assert_rc 0 "reconfigure"
  local out3=$OUT
  # 去掉包装再断言（断言自己的 grep 会把要查找的字符串写进包装日志）
  cp "$SPY_LOG" "$T/spy.final"
  export PATH="$T/fakebin:$ORIG_PATH"; hash -r
  local s
  for s in 'Rt%40Pass' 'Rt@Pass' 'DdlPassw0rd' 'Sup3r-Secret-Pw' 'FAKEGENSECRETKEY' 'FAKEGENMIRAGEKEY' 'FAKEGENIV' 'NewSecretKey' 'NewDbPass1'; do
    assert_str_lacks "$out1" "$s"
    assert_str_lacks "$out2" "$s"
    assert_str_lacks "$out3" "$s"
    assert_lacks "$FAKE_LOG" "$s"
    assert_lacks "$INSTALL_LOG" "$s"
    assert_lacks "$T/spy.final" "$s"
    assert_lacks "$STATE_FILE" "$s"
  done
  [ -s "$T/spy.final" ] || fail "包装日志为空：没有真正记录到命令行"
  # 运行账号 / 建表账号的连接串只经环境变量传给 dbt-admin（假 dbt-admin 从 DATABASE_URL 取到了用户名）
  assert_has "$FAKE_LOG" 'url_user=ddluser'
  assert_has "$FAKE_LOG" 'url_user=dbtool'
  # 密钥确实写进了 env（0640 root:运行组，先写临时文件再原子 mv）
  assert_has "$ENV_FILE" '^SECRET_KEY="NewSecretKey'
  assert_has "$FAKE_LOG" "chown root:dbtool $ENV_FILE\\.new"
  assert_has "$FAKE_LOG" "chmod 0640 $ENV_FILE\\.new"
}

# ================= inv9：首次部署一键完成 =================
test_inv9_one_command_full_deploy() {
  export FAKE_ID_MISSING=1 DBT_ADMIN_PASSWORD='Sup3r-Secret-Pw!'
  run_install --prefix "$PREFIX" --gen-secrets --bootstrap --admin-user admin \
    --database-url "mysql://dbtool:Rt%40Pass@10.0.0.5:3306/db_tool" --ddl-database-url "mysql://ddl:x@10.0.0.5:3306/db_tool" \
    --public-url https://dba.example.com --open-firewall -y
  assert_rc 0
  # 用户、目录、密钥、配置、基础表与数据、管理员、单元、防火墙、启动、健康检查，一个不少
  assert_has "$FAKE_LOG" '^useradd --system --no-create-home --shell /sbin/nologin dbtool'
  assert_file "$PREFIX/data/export"
  assert_file "$PREFIX/data/archive-spool"
  assert_has "$ENV_FILE" '^SECRET_KEY="'
  assert_has "$ENV_FILE" '^PUBLIC_URL="https://dba.example.com"$'
  assert_file "$TOML_FILE"
  assert_has "$FAKE_LOG" '^dbt-admin bootstrap --sql-root .* --apply --yes'
  assert_has "$FAKE_LOG" '^dbt-admin admin create --username admin'
  assert_file "$UNIT_FILE"
  assert_has "$FAKE_LOG" 'firewall-cmd --permanent --add-port=8000/tcp'
  assert_has "$FAKE_LOG" '^systemctl restart dbtool'
  assert_has "$FAKE_LOG" '^curl .*healthz'
  assert_before "$FAKE_LOG" '^useradd' "^install -d -o dbtool"
  assert_before "$FAKE_LOG" '^dbt-admin bootstrap' '^dbt-admin admin create'
  assert_before "$FAKE_LOG" '^dbt-admin admin create' '^systemctl daemon-reload'
  assert_before "$FAKE_LOG" '^systemctl restart' 'firewall-cmd --permanent --add-port'
  assert_before "$FAKE_LOG" 'firewall-cmd --permanent --add-port' '^curl .*healthz'
  # 访问地址与下一步清单
  assert_str_has "$OUT" "https://dba.example.com"
  assert_str_has "$OUT" "goInception"
  assert_str_has "$OUT" "nginx"
  assert_str_has "$OUT" "必配项"
  assert_str_has "$OUT" "SECRET_KEY"
  # 阶段进度与耗时
  assert_str_has "$OUT" "[1/11]"
  assert_str_has "$OUT" "[11/11]"
  assert_str_has "$OUT" "完成（"
}

test_inv9_noninteractive_missing_fields_errors() {
  run_install --prefix "$PREFIX" -y
  assert_rc 2
  assert_str_has "$OUT" "--database-url"
  assert_no_file "$PREFIX/releases"
  assert_no_file "$ENV_FILE"
}

test_inv9_interactive_prompts_for_missing() {
  # 终端交互：逐项询问 DATABASE_URL、SECRET_KEY、端口、PUBLIC_URL
  run_tty "$T/pkg" 'mysql://dbtool:Pw1@10.0.0.5:3306/db_tool\nSK-interactive-0123456789-0123456789-0123456789-abcdef\n9000\nhttps://dba.example.com\n' --prefix "$PREFIX"
  assert_rc 0
  assert_has "$ENV_FILE" '^DATABASE_URL="mysql://dbtool:Pw1@'
  assert_has "$ENV_FILE" '^SECRET_KEY="SK-interactive-'
  assert_has "$ENV_FILE" '^PUBLIC_URL="https://dba.example.com"$'
  assert_has "$TOML_FILE" '^port = 9000$'
  assert_str_lacks "$OUT" "SK-interactive"
}

# ================= inv10：日志 =================
test_inv10_install_log() {
  export DBT_ADMIN_PASSWORD='Sup3r-Secret-Pw!'
  run_install --prefix "$PREFIX" -y --database-url "mysql://dbtool:Rt%40Pass@10.0.0.5:3306/db_tool" --gen-secrets --bootstrap --admin-user ops
  assert_rc 0
  assert_file "$INSTALL_LOG"
  assert_has "$FAKE_LOG" "chmod 0640 $INSTALL_LOG"
  assert_has "$INSTALL_LOG" '^[0-9]{4}-[0-9]{2}-[0-9]{2} [0-9:]{8} \[[0-9]+\] STEP \[1/11\]'
  assert_has "$INSTALL_LOG" 'STEP 完成'
  assert_lacks "$INSTALL_LOG" 'Sup3r-Secret-Pw|Rt%40Pass|FAKEGEN'
  # 失败时也写日志并带退出码
  export FAKE_STATUS_RC=1
  run_install init-db
  assert_ne "$RC" 0
  assert_has "$INSTALL_LOG" 'ERROR'
}

# ================= doctor =================
test_doctor_healthy_is_read_only() {
  do_install
  snapshot > "$T/snap.before"
  : > "$FAKE_LOG"
  run_install doctor
  assert_rc 0
  assert_str_has "$OUT" "FAIL 0"
  assert_str_has "$OUT" "元数据库可连接"
  assert_str_has "$OUT" "/healthz 正常"
  assert_str_has "$OUT" "/readyz 就绪"
  assert_has "$FAKE_LOG" '^dbt-admin status --porcelain'
  assert_lacks "$FAKE_LOG" "$MUTATING_RE"
  snapshot > "$T/snap.after"
  assert_same_file "$T/snap.before" "$T/snap.after"
}

test_doctor_reports_failures() {
  # 端口被占用 + 服务未运行 + 库连不上 + 缺必填配置
  mkdir -p "$DBT_INSTALL_CONF_DIR"
  printf 'DATABASE_URL="mysql://dbtool:p@10.0.0.9:3306/db_tool"\n' > "$ENV_FILE"
  export FAKE_PORT_BUSY=8000 FAKE_SVC_INACTIVE=1 FAKE_STATUS_RC=1
  run_install doctor
  assert_rc 3
  assert_str_has "$OUT" "[FAIL]"
  assert_str_has "$OUT" "端口 8000 已被其它进程占用"
  assert_str_has "$OUT" "SECRET_KEY"
  assert_str_has "$OUT" "连不上元数据库"
}

test_doctor_degraded_readyz_and_missing_commands_warn() {
  do_install
  export FAKE_READYZ_STATUS=degraded
  run_install doctor
  assert_rc 0
  assert_str_has "$OUT" "degraded"
  assert_str_has "$OUT" "[WARN]"
}

test_doctor_on_clean_host_with_database_url() {
  # 全新主机的预检：没有配置，用 --database-url 检查库
  run_install doctor --database-url "mysql://dbtool:Rt%40Pass@10.0.0.5:3306/db_tool"
  assert_rc 0
  assert_str_has "$OUT" "元数据库可连接"
  assert_str_has "$OUT" "库还是空的"
  assert_str_lacks "$OUT" "Rt%40Pass"
  assert_no_file "$PREFIX"
}

test_doctor_detects_corrupted_release() {
  do_install
  echo corrupted >> "$PREFIX/releases/1.0.0/dbtool"
  run_install doctor
  assert_rc 3
  assert_str_has "$OUT" "SHA256SUMS 不一致"
}

# ================= reconfigure =================
test_reconfigure_changes_values_atomically_with_backup() {
  do_install
  cp "$ENV_FILE" "$T/env.before"
  : > "$FAKE_LOG"
  run_install reconfigure --public-url https://new.example.com --timezone UTC --require-goinception
  assert_rc 0
  assert_has "$ENV_FILE" '^PUBLIC_URL="https://new.example.com"$'
  assert_has "$ENV_FILE" '^TZ="UTC"$'
  assert_has "$ENV_FILE" '^REQUIRE_GOINCEPTION="true"$'
  # 其它值不变
  assert_has "$ENV_FILE" '^DATABASE_URL="mysql://dbtool:Rt%40Pass@10.0.0.5:3306/db_tool"$'
  assert_has "$ENV_FILE" '^SECRET_KEY="SK-from-user-'
  local bk; bk=$(ls -1t "$DBT_INSTALL_CONF_DIR/backup" | head -n1)
  assert_same_file "$DBT_INSTALL_CONF_DIR/backup/$bk/dbtool.env" "$T/env.before"
  # 原子替换：先写临时文件再 mv；没有 --restart 且非交互：不重启
  assert_has "$FAKE_LOG" "chown root:dbtool $ENV_FILE\\.new"
  assert_lacks "$FAKE_LOG" '^systemctl restart'
  assert_str_has "$OUT" "尚未生效"
  # 再次给同样的值：无变化
  run_install reconfigure --public-url https://new.example.com
  assert_rc 0
  assert_str_has "$OUT" "没有变化"
}

test_reconfigure_port_updates_toml_and_retires_listen() {
  do_install
  echo 'LISTEN=":9000"' >> "$ENV_FILE"
  : > "$FAKE_LOG"
  run_install reconfigure --port 9100 --restart --health-timeout 3
  assert_rc 0
  assert_has "$TOML_FILE" '^port = 9100$'
  assert_lacks "$ENV_FILE" '^LISTEN='
  assert_has "$ENV_FILE" '^# 已迁移到 dbtool.toml'
  assert_has "$FAKE_LOG" '^systemctl restart'
  assert_has "$FAKE_LOG" '^curl .*:9100/healthz'
  assert_eq "$(st_get PORT)" 9100
}

test_reconfigure_firewall_follows_port_only_for_script_rules() {
  do_install --open-firewall
  assert_has "$FAKE_LOG.fwports" '^8000/tcp$'
  run_install reconfigure --port 9100 --open-firewall --no-restart
  assert_rc 0
  assert_has "$FAKE_LOG.fwports" '^9100/tcp$'
  assert_lacks "$FAKE_LOG.fwports" '^8000/tcp$'
  assert_eq "$(st_get FIREWALL_PORT)" 9100
}

test_reconfigure_restart_failure_restores_backup() {
  do_install
  cp "$ENV_FILE" "$T/env.before"
  run_install reconfigure --public-url https://bad.example.com --restart --health-timeout 3
  assert_rc 5
  assert_same_file "$ENV_FILE" "$T/env.before"
  assert_str_has "$OUT" "恢复"
}

test_reconfigure_database_url_is_verified() {
  do_install
  cp "$ENV_FILE" "$T/env.before"
  export FAKE_STATUS_RC=1
  run_install reconfigure --database-url "mysql://dbtool:NewPass@10.0.0.7:3306/db_tool" --no-restart
  assert_rc 3
  assert_same_file "$ENV_FILE" "$T/env.before"
  assert_str_lacks "$OUT" "NewPass"
  run_install reconfigure --database-url "mysql://dbtool:NewPass@10.0.0.7:3306/db_tool" --no-restart --no-verify
  assert_rc 0
  assert_has "$ENV_FILE" '^DATABASE_URL="mysql://dbtool:NewPass@10.0.0.7'
}

# ---- DATABASE_URL 的密码加密（ENC(密文)）----
test_db_url_password_is_encrypted_on_fresh_install() {
  unset FAKE_ENC_BAD
  run_base
  assert_rc 0
  assert_has "$ENV_FILE" '^DATABASE_URL="mysql://dbtool:ENC\(FAKECIPHER\)@10.0.0.5:3306/db_tool"$'
  assert_lacks "$ENV_FILE" 'Rt%40Pass'
  # 加密发生在写 env 之前；dbt-admin 做库初始化等操作仍用明文连接串（只经环境变量传，不写盘）
  assert_has "$FAKE_LOG" '^dbt-admin encrypt-url '
  assert_str_lacks "$OUT" 'Rt%40Pass'
  assert_lacks "$FAKE_LOG" 'Rt%40Pass'
}

test_db_url_stays_plaintext_when_encrypt_fails() {
  export FAKE_ENC_BAD=1
  run_base
  assert_rc 0
  assert_has "$ENV_FILE" '^DATABASE_URL="mysql://dbtool:Rt%40Pass@10.0.0.5:3306/db_tool"$'
  assert_str_has "$OUT" "没有加密"
}

test_encrypt_db_url_command_encrypts_existing_env() {
  export FAKE_ENC_BAD=1
  do_install
  assert_has "$ENV_FILE" '^DATABASE_URL="mysql://dbtool:Rt%40Pass@10.0.0.5'
  unset FAKE_ENC_BAD
  run_install encrypt-db-url
  assert_rc 0
  assert_has "$ENV_FILE" '^DATABASE_URL="mysql://dbtool:ENC\(FAKECIPHER\)@10.0.0.5:3306/db_tool"$'
  assert_lacks "$ENV_FILE" 'Rt%40Pass'
  assert_str_lacks "$OUT" 'Rt%40Pass'
  # 其它配置一个字都不变
  assert_has "$ENV_FILE" '^SECRET_KEY='
  # 再运行一次：已经加密，什么也不改
  cp "$ENV_FILE" "$T/env.enc"
  run_install encrypt-db-url
  assert_rc 0
  assert_str_has "$OUT" "已经是加密的"
  assert_same_file "$ENV_FILE" "$T/env.enc"
  # doctor 认得出
  run_install doctor
  assert_str_has "$OUT" "密码是加密的"
}

test_encrypt_db_url_command_fails_without_usable_key() {
  export FAKE_ENC_BAD=1
  do_install
  cp "$ENV_FILE" "$T/env.before"
  run_install encrypt-db-url
  assert_rc 2
  assert_same_file "$ENV_FILE" "$T/env.before"
}

test_encrypted_db_url_passes_keys_to_dbt_admin() {
  unset FAKE_ENC_BAD
  run_base
  assert_rc 0
  : > "$FAKE_LOG"
  run_install doctor
  # env 里是密文连接串：dbt-admin 要带上密钥才解得开
  assert_has "$FAKE_LOG" '^dbt-admin status --porcelain .*keys=S'
}

test_reconfigure_database_url_stays_encrypted() {
  unset FAKE_ENC_BAD
  run_base
  assert_rc 0
  run_install reconfigure --database-url "mysql://dbtool:NewPass@10.0.0.7:3306/db_tool" --no-restart --no-verify
  assert_rc 0
  assert_has "$ENV_FILE" '^DATABASE_URL="mysql://dbtool:ENC\(FAKECIPHER\)@10.0.0.7:3306/db_tool"$'
  assert_lacks "$ENV_FILE" 'NewPass'
}

test_reconfigure_secret_key_refused_when_db_url_encrypted() {
  unset FAKE_ENC_BAD
  run_base
  assert_rc 0
  cp "$ENV_FILE" "$T/env.before"
  run_install reconfigure --secret-key "BrandNewSecretKey-0123456789-0123456789-0123456789" --no-restart -y
  assert_rc 2
  assert_str_has "$OUT" "解不开"
  assert_same_file "$ENV_FILE" "$T/env.before"
}

test_reconfigure_secret_key_needs_confirmation() {
  do_install
  cp "$ENV_FILE" "$T/env.before"
  run_install reconfigure --secret-key 'BrandNewSecret-0123456789-0123456789-0123456789-abcd' --no-restart
  assert_rc 2
  assert_same_file "$ENV_FILE" "$T/env.before"
  run_install reconfigure --secret-key 'BrandNewSecret-0123456789-0123456789-0123456789-abcd' --no-restart --yes
  assert_rc 0
  assert_has "$ENV_FILE" '^SECRET_KEY="BrandNewSecret'
  assert_str_lacks "$OUT" "BrandNewSecret"
}

test_reconfigure_requires_existing_config_and_args() {
  run_install reconfigure --public-url https://a.example.com
  assert_rc 2
  do_install
  run_install reconfigure
  assert_rc 2
  assert_str_has "$OUT" "没有要修改的项"
}

# ================= run-header（.run 自解压头）=================
test_run_header_info_check_extract_and_passthrough() {
  # 用真实的 deploy/run-header.sh + 假包组装一个 .run（做法同 Makefile dist）
  local pkgdir=$T/payload/dbtool tgz=$T/payload.tar.gz run=$T/test.run sha
  mkpkg "$pkgdir" 1.2.3 2026-05-05T00:00:00Z
  tar -C "$T/payload" -czf "$tgz" dbtool
  sha=$(sha_file "$tgz")
  sed -e 's|@VERSION@|1.2.3|g' -e 's|@ARCH@|amd64|g' -e 's|@BUILD_TIME@|2026-05-05T00:00:00Z|g' -e 's|@COMMIT@|abc|g' -e "s|@ARCHIVE_SHA256@|$sha|" "$RUN_HEADER" > "$run"
  cat "$tgz" >> "$run"
  chmod +x "$run"
  local out
  out=$(bash "$run" --info 2>&1)
  assert_str_has "$out" "1.2.3"
  assert_str_has "$out" "$sha"
  assert_str_has "$out" "dbtool/install.sh"
  out=$(bash "$run" --check 2>&1); local rc=$?
  assert_eq "$rc" 0 "--check 退出码"
  assert_str_has "$out" "校验通过"
  out=$(bash "$run" --extract "$T/ex" 2>&1)
  assert_file "$T/ex/dbtool/install.sh"
  assert_file "$T/ex/dbtool/dbtool"
  assert_file "$T/ex/dbtool/SHA256SUMS"
  # 参数透传给包内 install.sh（version 子命令）
  out=$(bash "$run" version 2>&1)
  assert_str_has "$out" "1.2.3"
  # 篡改载荷：--check 与运行都应失败
  cp "$run" "$T/bad.run"
  printf 'garbage' >> "$T/bad.run"
  bash "$T/bad.run" --check > "$T/o2.txt" 2>&1; rc=$?
  assert_ne "$rc" 0 "载荷被改动时 --check 应失败"
  bash "$T/bad.run" version > "$T/o2.txt" 2>&1; rc=$?
  assert_ne "$rc" 0 "载荷被改动时运行应失败"
}

# ================= 运行 =================
run_case() {
  local fn=$1
  CASE_FAILED=0
  setup_env
  echo "● $fn"
  "$fn"
  if [ "$CASE_FAILED" -eq 0 ]; then echo "  ✓ 通过"; else
    echo "  --- 安装输出 ---"; sed 's/^/  | /' "$T/out.txt" 2>/dev/null | tail -n 40
    echo "  --- 调用日志 ---"; sed 's/^/  | /' "$T/calls.log" 2>/dev/null | tail -n 40
  fi
  teardown_env
  return "$CASE_FAILED"
}

if [ -n "$EXACT" ]; then
  run_case "$EXACT"
  exit $?
fi

# 前置检查：能创建符号链接
_lt=$(mktemp -d)
if ! ( cd "$_lt" && mkdir d && ln -s d l && [ -L l ] && [ "$(readlink l)" = d ] ) 2>/dev/null; then
  rm -rf "$_lt"
  echo "此环境不能创建符号链接，无法运行这些测试（Windows Git Bash 需要开启开发者模式或用管理员权限；脚本已设置 MSYS=winsymlinks:nativestrict）" >&2
  exit 2
fi
rm -rf "$_lt"

CASES=()
for fn in $(declare -F | awk '{print $3}' | grep '^test_' | sort); do
  if [ -n "$FILTER" ]; then
    # FILTER 可以是逗号分隔的多个子串，任一匹配即运行
    matched=0
    IFS=',' read -r -a _pats <<< "$FILTER"
    for _p in "${_pats[@]}"; do [[ "$fn" == *"$_p"* ]] && matched=1; done
    [ "$matched" -eq 1 ] || continue
  fi
  CASES+=("$fn")
done

START=$SECONDS
if [ "$JOBS" -le 1 ]; then
  for fn in "${CASES[@]}"; do
    if run_case "$fn"; then PASS=$((PASS + 1)); else FAIL=$((FAIL + 1)); FAILED_NAMES+=("$fn"); fi
  done
else
  OUTDIR=$(mktemp -d)
  running=0
  for fn in "${CASES[@]}"; do
    ( bash "${BASH_SOURCE[0]}" --exact "$fn" > "$OUTDIR/$fn.out" 2>&1; echo $? > "$OUTDIR/$fn.rc" ) &
    running=$((running + 1))
    if [ "$running" -ge "$JOBS" ]; then wait -n; running=$((running - 1)); fi
  done
  wait
  for fn in "${CASES[@]}"; do
    cat "$OUTDIR/$fn.out"
    if [ "$(cat "$OUTDIR/$fn.rc" 2>/dev/null)" = 0 ]; then PASS=$((PASS + 1)); else FAIL=$((FAIL + 1)); FAILED_NAMES+=("$fn"); fi
  done
  rm -rf "$OUTDIR"
fi

echo
echo "通过 $PASS，失败 $FAIL（用时 $((SECONDS - START)) 秒）"
if [ "$FAIL" -gt 0 ]; then printf '  失败：%s\n' "${FAILED_NAMES[@]}"; exit 1; fi
exit 0
