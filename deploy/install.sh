#!/usr/bin/env bash
# dbtool 一键部署脚本（CentOS 7 / 8 / 9，也适用于 Rocky、AlmaLinux、RHEL 同系发行版）：部署、升级、回滚、卸载、体检、初始化库、改配置。
# 反复运行不会影响已存在的系统：已安装时再次 install 等价于 upgrade，不重新生成密钥、不覆盖已有配置、不重建管理员、不动数据目录与数据库里的数据。
#
#   sudo ./install.sh [install] [选项]     首次部署（已存在安装时自动按升级处理）
#   sudo ./install.sh upgrade [选项]       升级到本安装包的版本（失败自动回滚到上一版本）
#   sudo ./install.sh rollback [--to 版本] 回滚到上一版本（或指定版本）；不改数据库
#   sudo ./install.sh uninstall [--purge]  卸载：默认保留 /etc/dbtool 与 <prefix>/data；--purge 才删除（要求键入 PURGE）；绝不触碰数据库
#   ./install.sh status                    状态：版本、服务、监听端口、/healthz /readyz
#   ./install.sh doctor                    只读预检与体检：系统 / systemd / 端口 / 磁盘 / 依赖命令 / 配置完整性 / 连库状态 / 服务健康
#   sudo ./install.sh init-db [选项]       仅初始化元数据库（等价调用 dbt-admin bootstrap）：空库才建基础表与数据，否则只执行幂等的 sql/*.sql 扩展表脚本
#   sudo ./install.sh reconfigure [选项]   显式修改配置（修改前备份、原子替换；--dry-run 只看计划；--restart 改完重启并校验）
#   sudo ./install.sh encrypt-db-url       把 env 里 DATABASE_URL 的明文密码加密成 ENC(密文)（用 MIRAGE_SECRET_KEY，缺省 SECRET_KEY；备份、原子替换）
#   ./install.sh version                   脚本 / 安装包 / 已安装版本
#
# 部署方式：把 dbtool-<版本>-linux-<arch>.tar.gz 传到服务器，解压后在包目录里运行本脚本：
#   tar xzf dbtool-<版本>-linux-amd64.tar.gz && cd dbtool
#   sudo ./install.sh install --gen-secrets --bootstrap --admin-user admin --database-url ... --public-url ... --open-firewall
#
# 全新安装：先建好空库（CREATE DATABASE db_tool DEFAULT CHARACTER SET utf8mb4;），然后
#   read -rs DBT_ADMIN_PASSWORD; export DBT_ADMIN_PASSWORD     # 管理员密码用环境变量 DBT_ADMIN_PASSWORD（或交互输入），不要写在命令行上
#   sudo --preserve-env=DBT_ADMIN_PASSWORD ./install.sh --database-url 'mysql://用户:密码@主机:3306/db_tool' --gen-secrets --bootstrap --admin-user admin
#
# 安装 / 升级 / 首次配置选项（已有配置时下面带 * 的选项被忽略并提示，要改请用 reconfigure）：
#   --database-url URL *    元数据库，mysql://用户:密码@主机:3306/库名（密码含特殊字符需 URL 编码）；也可用环境变量 DBT_DATABASE_URL
#   --secret-key KEY *      SECRET_KEY（会话签名与密文密钥；丢失后库里的实例账号密码等密文无法解密）；也可用环境变量 DBT_SECRET_KEY
#   --gen-secrets *         没有提供 --secret-key / MIRAGE_* 时随机生成 SECRET_KEY 与 MIRAGE_* 并写入 env 文件（0640）；
#                           只在终端提示备份，不显示密钥值
#   --port PORT *           监听端口，默认 8000；写入 /etc/dbtool/dbtool.toml 的 [server] port
#   --listen ADDR *         监听地址 主机:端口（如 127.0.0.1:9000），默认 0.0.0.0:8000；等价于同时给主机与 --port
#   --public-url URL *      对外访问地址，用于通知里的工单链接，如 https://dba.example.com
#   --require-goinception * 生产环境建议：检查/执行/权限校验必须使用 goInception，不降级
#   --timezone TZ *         默认 Asia/Shanghai（库里的时间是无时区的本地时间，一经使用不要改）
#   --bootstrap             全新空库初始化：启动服务前执行 dbt-admin bootstrap（基础表 + 基础数据 + sql/ 下的扩展表脚本），再创建初始管理员。
#                           只在用户表不存在 / 为空时执行基础结构与数据；库里已有用户时只执行幂等的扩展表脚本（--only-sql），
#                           也不再创建管理员（已有启用的超级用户时）
#   --admin-user NAME       --bootstrap / init-db 创建的超级用户名；非交互时必须给，且用环境变量 DBT_ADMIN_PASSWORD 提供密码
#   --ddl-database-url URL  建表账号的连接串（有 CREATE / ALTER 权限；也可用环境变量 DDL_DATABASE_URL）。给了就用它执行
#                           --bootstrap 与建表脚本，运行账号只需要 DML 权限；没给则沿用运行账号（需要建表权限）
#   --apply-sql             首次安装时也执行 sql/ 下的扩展表脚本（升级时默认就会执行；均可重复执行）
#   --skip-sql              升级时不执行建表脚本（之后用 init-db 补）
#   --skip-perm-migrate     不执行升级末尾的权限迁移（dbt-admin perm migrate）；之后可手工执行（迁移完成前普通用户没有任何按钮权限，超级用户不受影响）
#   --open-firewall         在 firewalld 里放行监听端口（只有本脚本添加的规则，卸载时才会被移除）
#   --build                 用本机 Go（需 1.25+）从源码编译，而不是使用现成的二进制（要在源码仓库的 deploy/ 目录里运行）
#   --prefix DIR            安装目录，默认 /opt/dbtool（已安装后不能改）
#   --user NAME             运行服务的系统用户，默认 dbtool（已安装后不能改）
#
# SOAR（SQL 优化 / 改写工具）：安装包里带有 plugins/soar（可选 plugins/soar.yaml）时，install / upgrade 每次都会自动部署：
#   程序放到 <prefix>/plugins/soar（root:root 0755，内容相同就不动，不同则原子替换），配置放到 <prefix>/plugins/soar.yaml
#   （已存在不覆盖，包里的新版另存为 soar.yaml.dist；日志指向 <prefix>/data/soar.log），再把系统设置 optimizer.soar_path /
#   optimizer.soar_config 指向它们（只在还没设置时写，管理员改过的不动）。测试库 DSN（optimizer.soar_test_dsn）要管理员自己填，必须是非生产库。
#   这一步出错只警告，不中断安装 / 升级；重复运行是安全的。
#
# 通用选项：
#   --dry-run               对所有会写东西的子命令生效：只打印计划，不改任何文件 / 服务 / 库
#   -y, --yes               不询问，使用默认值/已给的选项（卸载 --purge 非交互时必须给）
#   --keep N                保留最近 N 个版本（默认 3；当前版本与上一版本总是保留）
#   --health-timeout SEC    升级 / 回滚后等待 /healthz 与 /readyz 的秒数，默认 60（环境变量 DBT_HEALTH_TIMEOUT）
#   --allow-downgrade       允许安装比当前版本低的包（数据库脚本只增不改，通常兼容但不保证）
#   --to VERSION            rollback：回滚到指定版本（默认上一版本）
#   --restart / --no-restart  reconfigure：改完立即重启并做健康检查 / 不重启（默认：终端里询问，非交互不重启）
#   --no-verify             reconfigure：不用 dbt-admin 验证新的 DATABASE_URL 能否连通
#   --skip-preflight        跳过升级前的 doctor 预检（不推荐）
#   --purge                 仅 uninstall：连 /etc/dbtool 与 <prefix>/data 一起删
#   -h, --help              打印本说明
#
# 退出码：0 成功；1 一般错误；2 用法 / 参数错误；3 预检失败（doctor 有 FAIL）；4 已有另一个 install.sh 在运行（锁）；
#   5 升级 / 回滚失败，已自动回到原版本且服务健康；6 失败且无法恢复（需人工处理）；7 拒绝降级（加 --allow-downgrade）。
#
# 目录布局：<prefix>/releases/<版本>/{dbtool,dbt-admin,sql,bootstrap,docs,VERSION,...}、<prefix>/current -> releases/<版本>、
# <prefix>/plugins/{soar,soar.yaml}（SOAR，包里带才有）、<prefix>/data/{export,archive-spool}。状态文件 /etc/dbtool/.install-state；升级前配置备份 /etc/dbtool/backup/<时间戳>/；日志 /var/log/dbtool-install.log。
# 详见 docs/one-click-deploy.md 与 docs/ops-manual.md 第 4 章。

# @author: kevinkelly
# @license: Apache Licence
# @file: install.sh
# @time: 2026/10/28

set -euo pipefail
umask 022
# 本脚本不使用调用者环境里的 DATABASE_URL（连接串只经 run_admin 的变量前缀传给 dbt-admin）
unset DATABASE_URL 2>/dev/null || true

SERVICE=dbtool
DEFAULT_PREFIX=/opt/dbtool
DEFAULT_USER=dbtool
# 下面这些路径可用环境变量覆盖，仅供 scripts/install-test 在临时目录里测试脚本用；正常安装不要设置。
CONF_DIR=${DBT_INSTALL_CONF_DIR:-/etc/dbtool}
ENV_FILE=$CONF_DIR/dbtool.env
CONF_FILE=$CONF_DIR/dbtool.toml
UNIT_DIR=${DBT_INSTALL_UNIT_DIR:-/etc/systemd/system}
UNIT=$UNIT_DIR/$SERVICE.service
STATE_FILE=$CONF_DIR/.install-state
BACKUP_ROOT=$CONF_DIR/backup
LOCK_FILE=${DBT_INSTALL_LOCK_FILE:-/var/lock/dbtool-install.lock}
LOG_FILE=${DBT_INSTALL_LOG_FILE:-/var/log/dbtool-install.log}
OS_RELEASE_FILE=${DBT_OS_RELEASE_FILE:-/etc/os-release}

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SCRIPT_PATH="$SCRIPT_DIR/$(basename "${BASH_SOURCE[0]}")"

if [ -t 1 ]; then
  c_red=$'\033[31m'; c_grn=$'\033[32m'; c_ylw=$'\033[33m'; c_blu=$'\033[36m'; c_off=$'\033[0m'
else
  c_red="" c_grn="" c_ylw="" c_blu="" c_off=""
fi

# ---------------- 日志 / 输出 ----------------
DRY_RUN=0
LOG_READY=0
DIE_CODE=1
# log 级别 消息：只写文件，不含机密（调用方保证：连接串 / 密钥 / 密码从不传给 log、info、warn、die）
log() {
  if [ "$LOG_READY" -ne 1 ]; then return 0; fi
  printf '%(%F %T)T [%s] %s %s\n' -1 "$$" "$1" "${*:2}" >> "$LOG_FILE" 2>/dev/null || true
}
log_init() {
  if [ "$DRY_RUN" -eq 1 ]; then return 0; fi
  if [ ! -e "$LOG_FILE" ]; then
    if { : > "$LOG_FILE"; } 2>/dev/null; then chmod 0640 "$LOG_FILE" 2>/dev/null || true; else return 0; fi
  fi
  LOG_READY=1
  log INFO "---- install.sh $CMD 开始（pid $$，用户 $(id -un 2>/dev/null || echo ?)）----"
}
info() { echo "${c_grn}==>${c_off} $*"; log INFO "$*"; }
note() { echo "    $*"; log INFO "$*"; }
warn() { echo "${c_ylw}警告:${c_off} $*" >&2; log WARN "$*"; }
die()  { echo "${c_red}错误:${c_off} $*" >&2; log ERROR "$*"; exit "$DIE_CODE"; }
die_code() { DIE_CODE=$1; shift; die "$@"; }
plan() { echo "    [dry-run] $*"; }

# 打印文件开头那段连续的 # 注释（用法说明）
usage() { awk 'NR > 1 { if ($0 ~ /^#/) { sub(/^# ?/, ""); print } else exit }' "$SCRIPT_PATH"; exit 0; }

# ---------------- 阶段进度 / 失败时的“已完成 / 未完成”报告 ----------------
PLAN=() STEP_N=0 STEP_TOTAL=0 STEP_CUR="" STEP_T0=0 STEPS_DONE=()
STAGE_TMP="" LOCK_DIR_HELD="" ROLLBACK_NOTE=""
step() {
  step_end
  STEP_CUR=${PLAN[$STEP_N]}
  STEP_N=$((STEP_N + 1))
  STEP_T0=$SECONDS
  echo "${c_blu}[$STEP_N/$STEP_TOTAL]${c_off} $STEP_CUR"
  log STEP "[$STEP_N/$STEP_TOTAL] $STEP_CUR"
}
step_end() {
  if [ -n "$STEP_CUR" ]; then
    STEPS_DONE+=("$STEP_CUR")
    echo "      完成（$((SECONDS - STEP_T0)) 秒）"
    log STEP "完成 $STEP_CUR（$((SECONDS - STEP_T0)) 秒）"
    STEP_CUR=""
  fi
}
join_by() { local sep=$1 out="" x; shift; for x in "$@"; do out+="${out:+$sep}$x"; done; printf '%s' "$out"; }
on_exit() {
  local rc=$?
  trap - EXIT
  if [ -n "$STAGE_TMP" ] && [ -d "$STAGE_TMP" ]; then rm -rf "$STAGE_TMP"; fi
  if [ -n "$LOCK_DIR_HELD" ]; then rm -rf "$LOCK_DIR_HELD"; fi
  if [ "$rc" -ne 0 ] && [ "$STEP_N" -gt 0 ]; then
    local i rest=()
    for ((i = STEP_N; i < ${#PLAN[@]}; i++)); do rest+=("${PLAN[$i]}"); done
    {
      echo
      echo "---- 执行情况（退出码 $rc）----"
      echo "已完成: ${STEPS_DONE[*]:+$(join_by '、' "${STEPS_DONE[@]}")}"
      [ -n "$STEP_CUR" ] && echo "失败于: $STEP_CUR"
      [ "${#rest[@]}" -gt 0 ] && echo "未执行: $(join_by '、' "${rest[@]}")"
      [ -n "$ROLLBACK_NOTE" ] && echo "$ROLLBACK_NOTE"
      echo "各步骤幂等：原因修复后直接重新运行同一条命令即可；日志 $LOG_FILE"
    } >&2
    log ERROR "退出码 $rc；已完成: ${STEPS_DONE[*]:-}；失败于: ${STEP_CUR:-}"
  fi
  exit "$rc"
}
trap on_exit EXIT
trap 'log ERROR "命令失败（第 $LINENO 行）: $BASH_COMMAND"' ERR

# ---------------- 参数 ----------------
CMD=install
if [ $# -gt 0 ]; then
  case "$1" in
    install|upgrade|rollback|uninstall|status|doctor|init-db|reconfigure|encrypt-db-url|version) CMD=$1; shift ;;
    help) usage ;;
    -V|--version) CMD=version; shift ;;
  esac
fi

LISTEN="" PORT_ARG="" OPT_DB_URL=${DBT_DATABASE_URL:-} SECRET_KEY=${DBT_SECRET_KEY:-} PUBLIC_URL="" TZ_NAME=""
REQUIRE_GI="" APPLY_SQL=0 SKIP_SQL=0 SKIP_PERM=0 OPEN_FW=0 BUILD=0 ASSUME_YES=0 PURGE=0
MIRAGE_SECRET_KEY="" MIRAGE_CIPHER_MODE="" MIRAGE_CIPHER_IV=""
BOOTSTRAP=0 GEN_SECRETS=0 ADMIN_USER="" DDL_URL="${DDL_DATABASE_URL:-}" SECRETS_GENERATED=0
PREFIX_ARG="" USER_ARG="" KEEP=3 BACKUP_KEEP=3 HEALTH_TIMEOUT=${DBT_HEALTH_TIMEOUT:-60} ALLOW_DOWNGRADE=0 TO_VERSION=""
RESTART_OPT="" NO_VERIFY=0 SKIP_PREFLIGHT=0
GIVEN_CFG=()      # 本次命令行里给了哪些“配置类”选项（已有配置时它们被忽略并提示）
GIVEN_PUBLIC=0 GIVEN_TZ=0 GIVEN_DB=0 GIVEN_SECRET=0 GIVEN_LISTEN=0 GIVEN_REQGI=0

usage_err() { DIE_CODE=2; die "$1（用 --help 查看用法）"; }
need_val() { if [ "$#" -lt 2 ]; then usage_err "选项 $1 需要一个值"; fi; }
while [ $# -gt 0 ]; do
  case "$1" in
    --database-url=*|--secret-key=*|--listen=*|--port=*|--public-url=*|--timezone=*|--admin-user=*|\
    --ddl-database-url=*|--prefix=*|--user=*|--keep=*|--health-timeout=*|--to=*)
      set -- "${1%%=*}" "${1#*=}" "${@:2}" ;;
  esac
  case "$1" in
    --database-url)      need_val "$@"; OPT_DB_URL=$2; GIVEN_DB=1; GIVEN_CFG+=(--database-url); shift 2 ;;
    --secret-key)        need_val "$@"; SECRET_KEY=$2; GIVEN_SECRET=1; GIVEN_CFG+=(--secret-key); shift 2 ;;
    --listen)            need_val "$@"; LISTEN=$2; GIVEN_LISTEN=1; GIVEN_CFG+=(--listen); shift 2 ;;
    --port)              need_val "$@"; PORT_ARG=$2; GIVEN_LISTEN=1; GIVEN_CFG+=(--port); shift 2 ;;
    --public-url)        need_val "$@"; PUBLIC_URL=$2; GIVEN_PUBLIC=1; GIVEN_CFG+=(--public-url); shift 2 ;;
    --timezone)          need_val "$@"; TZ_NAME=$2; GIVEN_TZ=1; GIVEN_CFG+=(--timezone); shift 2 ;;
    --require-goinception|--require-goinception=true) REQUIRE_GI=true; GIVEN_REQGI=1; GIVEN_CFG+=(--require-goinception); shift ;;
    --no-require-goinception|--require-goinception=false) REQUIRE_GI=false; GIVEN_REQGI=1; shift ;;
    --apply-sql)         APPLY_SQL=1; shift ;;
    --skip-sql)          SKIP_SQL=1; shift ;;
    --skip-perm-migrate) SKIP_PERM=1; shift ;;
    --bootstrap)         BOOTSTRAP=1; shift ;;
    --gen-secrets)       GEN_SECRETS=1; GIVEN_CFG+=(--gen-secrets); shift ;;
    --admin-user)        need_val "$@"; ADMIN_USER=$2; shift 2 ;;
    --ddl-database-url)  need_val "$@"; DDL_URL=$2; shift 2 ;;
    --open-firewall)     OPEN_FW=1; shift ;;
    --build)             BUILD=1; shift ;;
    --prefix)            need_val "$@"; PREFIX_ARG=$2; shift 2 ;;
    --user)              need_val "$@"; USER_ARG=$2; shift 2 ;;
    --purge)             PURGE=1; shift ;;
    --dry-run)           DRY_RUN=1; shift ;;
    --keep)              need_val "$@"; KEEP=$2; shift 2 ;;
    --health-timeout)    need_val "$@"; HEALTH_TIMEOUT=$2; shift 2 ;;
    --allow-downgrade)   ALLOW_DOWNGRADE=1; shift ;;
    --to)                need_val "$@"; TO_VERSION=$2; shift 2 ;;
    --restart)           RESTART_OPT=yes; shift ;;
    --no-restart)        RESTART_OPT=no; shift ;;
    --no-verify)         NO_VERIFY=1; shift ;;
    --skip-preflight)    SKIP_PREFLIGHT=1; shift ;;
    -y|--yes)            ASSUME_YES=1; shift ;;
    -h|--help)           usage ;;
    *) usage_err "未知选项: $1" ;;
  esac
done
case "$KEEP" in ''|*[!0-9]*) usage_err "--keep 必须是正整数" ;; esac
[ "$KEEP" -ge 1 ] || usage_err "--keep 必须 >= 1"
case "$HEALTH_TIMEOUT" in ''|*[!0-9]*) usage_err "--health-timeout 必须是正整数（秒）" ;; esac

# ---------------- 通用工具函数 ----------------
require_root() {
  if [ "$DRY_RUN" -eq 1 ]; then return 0; fi
  if [ "$(id -u)" -ne 0 ]; then DIE_CODE=2; die "请用 root 运行（sudo ./install.sh $CMD ...）"; fi
}
require_systemd() {
  command -v systemctl >/dev/null 2>&1 || die_code 3 "需要 systemd（CentOS 7+）：没有找到 systemctl"
}

# 标准输入是否是终端（DBT_INSTALL_TEST_TTY=1 仅供 scripts/install-test 用管道模拟交互输入）
tty_in() { [ -t 0 ] || [ "${DBT_INSTALL_TEST_TTY:-}" = 1 ]; }
is_tty() { tty_in && [ "$ASSUME_YES" -eq 0 ]; }

# ask VAR "提示" [默认值] [secret]
ask() {
  local var=$1 prompt=$2 def=${3:-} secret=${4:-} val=""
  if [ -n "${!var}" ]; then return 0; fi
  if is_tty; then
    if [ -n "$secret" ]; then read -r -s -p "$prompt: " val; echo
    else read -r -p "$prompt${def:+ [$def]}: " val; fi
  fi
  val=${val:-$def}
  printf -v "$var" '%s' "$val"
}

# confirm "提示"：0=确认。非交互环境必须带 --yes
confirm() {
  if [ "$ASSUME_YES" -eq 1 ]; then return 0; fi
  if ! tty_in; then DIE_CODE=2; die "需要确认：$1（非交互环境请加 --yes）"; fi
  local a=""
  read -r -p "$1 [y/N]: " a
  case "$a" in y|Y|yes|YES) return 0 ;; *) return 1 ;; esac
}

# systemd EnvironmentFile 的取值写法：双引号包起来，转义 \ 与 "
q() { local v=$1; v=${v//\\/\\\\}; v=${v//\"/\\\"}; printf '"%s"' "$v"; }

# ---- 已有 env 文件 / toml 的内容（纯 bash 解析，值不经过任何外部命令的命令行）----
declare -A ENVV=() TOMLV=()
CUR_PORT=8000 CUR_HOST=""
env_load() {
  ENVV=()
  [ -r "$ENV_FILE" ] || return 0
  local line k v re='^[A-Za-z_][A-Za-z0-9_]*$'
  while IFS= read -r line || [ -n "$line" ]; do
    line=${line%$'\r'}
    line=${line#"${line%%[![:space:]]*}"}
    case "$line" in ''|\#*) continue ;; esac
    case "$line" in export[[:space:]]*) line=${line#export}; line=${line#"${line%%[![:space:]]*}"} ;; esac
    case "$line" in *=*) ;; *) continue ;; esac
    k=${line%%=*}; v=${line#*=}
    [[ $k =~ $re ]] || continue
    case "$v" in
      \"*\") v=${v#\"}; v=${v%\"}; v=${v//\\\"/\"}; v=${v//\\\\/\\} ;;
      \'*\') v=${v#\'}; v=${v%\'} ;;
    esac
    ENVV[$k]=$v
  done < "$ENV_FILE"
}
env_has() { [ -n "${ENVV[$1]+x}" ]; }
toml_load() {
  TOMLV=()
  [ -r "$CONF_FILE" ] || return 0
  local line sect="" re_sect re_kv
  re_sect='^[[:space:]]*\[([A-Za-z0-9_.]+)\][[:space:]]*(#.*)?$'
  re_kv='^[[:space:]]*([A-Za-z_][A-Za-z0-9_]*)[[:space:]]*=[[:space:]]*"?([^"#]*[^"#[:space:]])"?[[:space:]]*(#.*)?$'
  while IFS= read -r line || [ -n "$line" ]; do
    line=${line%$'\r'}
    if [[ $line =~ $re_sect ]]; then sect=${BASH_REMATCH[1]}; continue; fi
    if [[ $line =~ $re_kv ]]; then TOMLV["$sect.${BASH_REMATCH[1]}"]=${BASH_REMATCH[2]}; fi
  done < "$CONF_FILE"
}
# 当前生效的监听地址：环境变量里的 LISTEN 优先（与程序一致），否则 toml 的 [server]
cfg_load() {
  env_load; toml_load
  local l=${ENVV[LISTEN]:-}
  if [ -n "$l" ]; then
    CUR_PORT=${l##*:}
    case "$l" in *:*) CUR_HOST=${l%:*} ;; *) CUR_HOST="" ;; esac
  else
    CUR_PORT=${TOMLV[server.port]:-8000}
    CUR_HOST=${TOMLV[server.host]:-}
  fi
}
health_host() { case "$CUR_HOST" in ''|0.0.0.0|'::'|'[::]') echo 127.0.0.1 ;; *) echo "$CUR_HOST" ;; esac; }

# env_append KEY VALUE：只在 env 文件里没有设置 KEY 时追加（已有值不覆盖）；追加了返回 0，已有返回 1
env_append() {
  env_has "$1" && return 1
  # 文件末尾没有换行时先补一个
  if [ -s "$ENV_FILE" ] && [ -n "$(tail -c1 "$ENV_FILE" 2>/dev/null)" ]; then echo >> "$ENV_FILE"; fi
  printf '%s=%s\n' "$1" "$(q "$2")" >> "$ENV_FILE"
  ENVV[$1]=$2
}

# gen_rand N：输出 N 个随机字母数字（/dev/urandom），不回显到终端
gen_rand() {
  local n=$1 v
  v=$( { LC_ALL=C tr -dc 'A-Za-z0-9' < /dev/urandom | head -c "$n"; } || true )
  [ "${#v}" -eq "$n" ] || die "生成随机数失败（读取 /dev/urandom）"
  printf '%s' "$v"
}

# 生成 SECRET_KEY 与 MIRAGE_*（只在当前 shell 变量里，不打印）。优先用安装包里的 dbt-admin gen-secrets，失败则用 /dev/urandom。
gen_secrets_into_vars() {
  local out="" k v sk="" mk="" miv="" mm=""
  if [ -n "${DBT_ADMIN_SRC:-}" ] && [ -x "$DBT_ADMIN_SRC" ]; then
    out=$("$DBT_ADMIN_SRC" gen-secrets 2>/dev/null) || out=""
    while IFS='=' read -r k v; do
      case "$k" in
        SECRET_KEY) sk=$v ;;
        MIRAGE_SECRET_KEY) mk=$v ;;
        MIRAGE_CIPHER_IV) miv=$v ;;
        MIRAGE_CIPHER_MODE) mm=$v ;;
      esac
    done <<< "$out"
    out=""
    # 校验：长度达标才采用，否则回退
    if [ "${#sk}" -lt 50 ] || [ "${#mk}" -lt 32 ] || [ "${#miv}" -ne 16 ] || [ "$mm" != ECB ]; then sk="" mk="" miv="" mm=""; fi
  fi
  if [ -z "$sk" ]; then sk=$(gen_rand 64); mk=$(gen_rand 64); miv=$(gen_rand 16); mm=ECB; fi
  GEN_SK=$sk GEN_MK=$mk GEN_MIV=$miv GEN_MM=$mm
}

# URL 解码（%XX）
urldecode() { local s=${1//+/ }; printf '%b' "${s//%/\\x}"; }

# 解析 mysql://user:pass@host:port/db 到 DB_USER DB_PASS DB_HOST DB_PORT DB_NAME（出错时不回显连接串）
parse_db_url() {
  local u=$1 rest hostport cred
  case "$u" in mysql://*) ;; *) DIE_CODE=2; die "DATABASE_URL 必须以 mysql:// 开头" ;; esac
  rest=${u#mysql://}
  case "$rest" in *@*) ;; *) DIE_CODE=2; die "DATABASE_URL 格式不对，应为 mysql://用户:密码@主机:3306/库名" ;; esac
  cred=${rest%%@*}; rest=${rest#*@}
  DB_USER=$(urldecode "${cred%%:*}")
  DB_PASS=$(urldecode "${cred#*:}")
  hostport=${rest%%/*}
  DB_NAME=${rest#*/}; DB_NAME=${DB_NAME%%\?*}
  DB_HOST=${hostport%%:*}
  DB_PORT=3306
  case "$hostport" in *:*) DB_PORT=${hostport##*:} ;; esac
  if [ -z "$DB_USER" ] || [ -z "$DB_HOST" ] || [ -z "$DB_NAME" ] || [ "$rest" = "$hostport" ]; then
    DIE_CODE=2; die "DATABASE_URL 格式不对，应为 mysql://用户:密码@主机:3306/库名"
  fi
}
# 把连接串里的密码去掉，用于日志 / 提示：mysql://用户:***@主机:端口/库
mask_url() {
  local u=$1 rest cred hp
  rest=${u#mysql://}
  case "$rest" in *@*) cred=${rest%%@*}; hp=${rest#*@}; printf 'mysql://%s:***@%s' "${cred%%:*}" "$hp" ;; *) printf '(格式不对)' ;; esac
}

valid_port() { case "$1" in ''|*[!0-9]*) return 1 ;; esac; [ "$1" -ge 1 ] && [ "$1" -le 65535 ]; }

sha_of() { local h=""; if [ -f "$1" ]; then read -r h _ < <(sha256sum "$1"); fi; printf '%s' "$h"; }

pkg_install() {
  if command -v dnf >/dev/null 2>&1; then dnf -y install "$@"; else yum -y install "$@"; fi
}

# 路径安全：prefix 会被 rm -rf（卸载 --purge），只接受简单的绝对路径，并拒绝系统目录本身
check_prefix() {
  local re='^/[A-Za-z0-9._+/-]+$'
  [[ $PREFIX =~ $re ]] || { DIE_CODE=2; die "--prefix 必须是绝对路径，且只含字母、数字和 . _ + - /：$PREFIX"; }
  case "$PREFIX" in
    //*|*//*|*/..|*/../*|*/.|*/./*) DIE_CODE=2; die "--prefix 不能含 .. 或空路径段：$PREFIX" ;;
    /bin|/boot|/dev|/etc|/home|/lib|/lib64|/opt|/proc|/root|/run|/sbin|/srv|/sys|/tmp|/usr|/var|/mnt|/media)
      DIE_CODE=2; die "--prefix 不能是系统目录 $PREFIX 本身（例如用 /opt/dbtool）" ;;
  esac
}
valid_release_id() { local re='^[A-Za-z0-9][A-Za-z0-9._+-]*$'; [[ $1 =~ $re ]]; }

# ---------------- 状态文件 /etc/dbtool/.install-state ----------------
declare -A ST=()
STATE_KEYS=(STATE_VERSION INSTALLED VERSION PREVIOUS_VERSION INSTALLED_AT UPDATED_AT PREFIX RUN_USER USER_CREATED DATA_RESET PORT FIREWALL_OPENED FIREWALL_PORT UNIT_SHA256)
state_load() {
  ST=()
  # 非 root 运行 status / doctor / version 时状态文件（0640）可能读不了：当作没有，不报错
  [ -f "$STATE_FILE" ] && [ -r "$STATE_FILE" ] || return 0
  local k v
  while IFS='=' read -r k v || [ -n "$k" ]; do
    case "$k" in ''|\#*) continue ;; esac
    ST[$k]=${v%$'\r'}
  done < "$STATE_FILE"
}
state_flush() {
  if [ "$DRY_RUN" -eq 1 ]; then return 0; fi
  local tmp="$STATE_FILE.tmp.$$" k
  if [ ! -d "$CONF_DIR" ]; then install -d -m 0755 "$CONF_DIR"; fi
  ST[STATE_VERSION]=1
  printf -v k '%(%FT%T%z)T' -1
  ST[UPDATED_AT]=$k
  {
    echo "# dbtool install.sh 状态文件（key=value）。uninstall / rollback 依据它精确回收，请勿手工修改。不含机密。"
    for k in "${STATE_KEYS[@]}"; do printf '%s=%s\n' "$k" "${ST[$k]:-}"; done
    for k in "${!ST[@]}"; do
      case " ${STATE_KEYS[*]} " in *" $k "*) ;; *) printf '%s=%s\n' "$k" "${ST[$k]}" ;; esac
    done
  } > "$tmp"
  chmod 0640 "$tmp"
  mv -f "$tmp" "$STATE_FILE"
}

# ---------------- 上下文：安装目录 / 运行用户（已安装则以状态文件为准）----------------
PREFIX="" RUN_USER=""
resolve_context() {
  state_load
  local st_prefix=${ST[PREFIX]:-} st_user=${ST[RUN_USER]:-} installed=${ST[INSTALLED]:-0}
  if [ -n "$PREFIX_ARG" ]; then
    PREFIX=${PREFIX_ARG%/}
    if [ "$installed" = 1 ] && [ -n "$st_prefix" ] && [ "$st_prefix" != "$PREFIX" ]; then
      DIE_CODE=2; die "已安装在 $st_prefix，不支持用 --prefix 改成 $PREFIX。要换目录：先 uninstall（配置与数据默认保留，可手工迁移），再用新目录 install"
    fi
  else
    PREFIX=${st_prefix:-$DEFAULT_PREFIX}
  fi
  if [ -n "$USER_ARG" ]; then
    RUN_USER=$USER_ARG
    if [ "$installed" = 1 ] && [ -n "$st_user" ] && [ "$st_user" != "$RUN_USER" ]; then
      DIE_CODE=2; die "已安装，运行用户是 $st_user，不支持用 --user 改成 $RUN_USER。要换用户：先 uninstall 再重装（配置保留，注意数据目录属主）"
    fi
  else
    RUN_USER=${st_user:-$DEFAULT_USER}
  fi
  local ure='^[a-z_][a-z0-9_-]{0,31}$'
  [[ $RUN_USER =~ $ure ]] || { DIE_CODE=2; die "--user 不是合法的系统用户名: $RUN_USER"; }
  check_prefix
}

# 当前 current 指向的版本目录名（没有则空）
current_id() {
  local t=""
  if [ -L "$PREFIX/current" ]; then t=$(readlink "$PREFIX/current" 2>/dev/null || true); fi
  t=${t%/}
  printf '%s' "${t##*/}"
}
release_ok() { [ -f "$PREFIX/releases/$1/.installed_at" ] && [ -f "$PREFIX/releases/$1/dbtool" ]; }
# 读版本目录里 VERSION 文件的某个键
release_field() {
  local f=$PREFIX/releases/$1/VERSION k v out=""
  if [ -f "$f" ]; then
    while IFS='=' read -r k v || [ -n "$k" ]; do
      if [ "$k" = "$2" ]; then out=${v%$'\r'}; fi
    done < "$f"
  fi
  printf '%s' "$out"
}

# ---------------- 锁：同一时刻只允许一个实例 ----------------
acquire_lock() {
  if [ "$DRY_RUN" -eq 1 ]; then return 0; fi
  local ldir pid
  if command -v flock >/dev/null 2>&1; then
    { exec 9> "$LOCK_FILE"; } 2>/dev/null || die "无法创建锁文件 $LOCK_FILE"
    if ! flock -n 9; then
      DIE_CODE=4; die "另一个 install.sh 正在运行（锁 $LOCK_FILE），请等它结束后再试"
    fi
  else
    # 没有 flock（util-linux）时的退路：mkdir 原子锁 + 进程存活检查
    ldir=$LOCK_FILE.d
    if ! mkdir "$ldir" 2>/dev/null; then
      pid=""
      if [ -r "$ldir/pid" ]; then read -r pid < "$ldir/pid" || true; fi
      if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
        DIE_CODE=4; die "另一个 install.sh 正在运行（pid $pid，锁 $ldir），请等它结束后再试"
      fi
      rm -rf "$ldir"
      mkdir "$ldir" 2>/dev/null || { DIE_CODE=4; die "无法获取锁 $ldir"; }
    fi
    echo "$$" > "$ldir/pid"
    LOCK_DIR_HELD=$ldir
  fi
  log_init
}

# ---------------- 安装包定位 ----------------
BIN_SRC="" DBT_ADMIN_SRC="" SQL_SRC="" BOOT_SRC="" DOCS_SRC="" SRC_ROOT="" PLUGINS_SRC=""
PKG_VER="" PKG_BUILD_TIME=""
first_existing() { # first_existing -f|-d 路径...
  local t=$1 p; shift
  for p in "$@"; do
    if [ "$t" = -f ] && [ -f "$p" ]; then printf '%s' "$p"; return 0; fi
    if [ "$t" = -d ] && [ -d "$p" ]; then printf '%s' "$p"; return 0; fi
  done
  return 0
}
read_version_file() { # read_version_file 文件 → PKG_VER / PKG_BUILD_TIME
  local f=$1 k v
  while IFS='=' read -r k v || [ -n "$k" ]; do
    v=${v%$'\r'}
    case "$k" in
      version) PKG_VER=$v ;;
      build_time) PKG_BUILD_TIME=$v ;;
      *) if [ -z "$PKG_VER" ] && [ -z "$v" ] && [ -n "$k" ]; then PKG_VER=${k%$'\r'}; fi ;;
    esac
  done < "$f"
}
compute_pkg_version() {
  PKG_VER="" PKG_BUILD_TIME=""
  if [ -f "$SCRIPT_DIR/VERSION" ] && [ "$BUILD" -eq 0 ]; then read_version_file "$SCRIPT_DIR/VERSION"; fi
  if [ -z "$PKG_VER" ] && command -v git >/dev/null 2>&1 && [ -d "$SCRIPT_DIR/../.git" ]; then
    PKG_VER=$(git -C "$SCRIPT_DIR/.." describe --tags --always --dirty 2>/dev/null || true)
  fi
  if [ -z "$PKG_VER" ]; then printf -v PKG_VER '0.0.0-%(%Y%m%d)T' -1; fi
  if [ -z "$PKG_BUILD_TIME" ]; then printf -v PKG_BUILD_TIME '%(%FT%TZ)T' -1; fi
  PKG_VER=${PKG_VER//[^A-Za-z0-9._+-]/-}
  case "$PKG_VER" in .*|-*) PKG_VER=v$PKG_VER ;; esac
}
# locate_package [soft]：soft 时找不到不退出（doctor / status 用），返回 1
locate_package() {
  local soft=${1:-} p
  BIN_SRC="" DBT_ADMIN_SRC="" SQL_SRC="" BOOT_SRC="" DOCS_SRC="" PLUGINS_SRC=""
  if [ "$BUILD" -eq 1 ]; then
    command -v go >/dev/null 2>&1 || { [ -n "$soft" ] && return 1; die "--build 需要 Go 1.25+（未找到 go）。可在别处执行 make dist 后把安装包拷过来"; }
    SRC_ROOT=$(cd "$SCRIPT_DIR/.." && pwd)
    [ -f "$SRC_ROOT/go.mod" ] || { [ -n "$soft" ] && return 1; die "--build 需要在源码仓库的 deploy/ 目录里运行"; }
    BIN_SRC=$SRC_ROOT/bin/dbtool DBT_ADMIN_SRC=$SRC_ROOT/bin/dbt-admin
  else
    BIN_SRC=$(first_existing -f "$SCRIPT_DIR/dbtool" "$SCRIPT_DIR/bin/dbtool" "$SCRIPT_DIR/../bin/dbtool")
    DBT_ADMIN_SRC=$(first_existing -f "$SCRIPT_DIR/dbt-admin" "$SCRIPT_DIR/bin/dbt-admin" "$SCRIPT_DIR/../bin/dbt-admin")
    if [ -z "$BIN_SRC" ]; then
      if [ -n "$soft" ]; then return 1; fi
      die "找不到 dbtool 二进制。请使用 make dist 生成的安装包（tar.gz 解压后的目录），或加 --build 从源码编译"
    fi
  fi
  SQL_SRC=$(first_existing -d "$SCRIPT_DIR/sql" "$SCRIPT_DIR/deploy/sql")
  BOOT_SRC=$(first_existing -d "$SCRIPT_DIR/bootstrap" "$SCRIPT_DIR/deploy/bootstrap")
  DOCS_SRC=$(first_existing -d "$SCRIPT_DIR/docs" "$SCRIPT_DIR/../docs")
  # 可选的 SOAR：目录里有 soar 才算（源码树里在仓库根目录的 plugins/）
  PLUGINS_SRC=$(first_existing -d "$SCRIPT_DIR/plugins" "$SCRIPT_DIR/../plugins")
  if [ -n "$PLUGINS_SRC" ] && [ ! -f "$PLUGINS_SRC/soar" ]; then PLUGINS_SRC=""; fi
  compute_pkg_version
  return 0
}
# 编译（--build，真实运行时）：同时编译 dbtool / dbt-admin，并注入版本号
build_from_source() {
  info "编译（go $(go version | awk '{print $3}')，版本 $PKG_VER）"
  local ld="-s -w -X main.version=$PKG_VER" t
  mkdir -p "$SRC_ROOT/bin"
  for t in "dbtool:server" "dbt-admin:dbt-admin"; do
    (cd "$SRC_ROOT" && CGO_ENABLED=0 go build -trimpath -ldflags="$ld" -o "$SRC_ROOT/bin/${t%%:*}" "./cmd/${t##*:}")
  done
}
check_elf() {
  if command -v file >/dev/null 2>&1 && ! file "$1" | grep -qi "ELF"; then
    warn "$1 看起来不是 Linux 可执行文件（在 Windows/macOS 上编译需要 GOOS=linux，见 make dist）"
  fi
}

# verify_sums 目录：按目录里的 SHA256SUMS 校验（只校验存在的文件，且 dbtool 必须在清单里）。0=通过 1=不通过 2=没有清单
verify_sums() {
  local dir=$1 line f filt=""
  [ -f "$dir/SHA256SUMS" ] || return 2
  while IFS= read -r line || [ -n "$line" ]; do
    [ -n "$line" ] || continue
    f=${line#* }; f=${f#[ *]}   # "hash  路径"（文本模式）或 "hash *路径"（二进制模式）
    if [ -e "$dir/$f" ]; then filt+="$line"$'\n'; fi
  done < "$dir/SHA256SUMS"
  case "$filt" in *"  dbtool"$'\n'*|*" *dbtool"$'\n'*) ;; *) return 1 ;; esac
  (cd "$dir" && printf '%s' "$filt" | sha256sum -c --quiet - >/dev/null 2>&1)
}

# ---------------- 版本比较 / 降级判定 ----------------
semver_like() { local re='^v?[0-9]+\.[0-9]+'; [[ $1 =~ $re ]]; }
# is_downgrade 当前版本 新版本 当前build_time 新build_time → 0=是降级
is_downgrade() {
  local cur=${1#v} new=${2#v} cbt=$3 nbt=$4 lo
  [ "$cur" != "$new" ] || return 1
  if semver_like "$cur" && semver_like "$new"; then
    lo=$(printf '%s\n%s\n' "$cur" "$new" | sort -V | head -n1)
    [ "$lo" = "$new" ]
    return
  fi
  if [ -n "$cbt" ] && [ -n "$nbt" ] && [[ $nbt < $cbt ]]; then return 0; fi
  return 1
}

# ---------------- 元数据库：连接 / 判定 ----------------
# 元数据库“是否为空”的判定依据：dbt-admin status --porcelain 输出里“用户表状态”那一行的键。
# 全脚本只在这里出现一次，db_probe 是唯一的解析点；dbt-admin 改这个输出键时只改这一个常量。
STATUS_USERS_KEY=users_table
CUR_TZ="" ADMIN_DB_URL="" RUN_DB_URL=""
DB_USERS="" DB_SUPERS="" DB_INFO=""
# run_admin dbt-admin路径 连接串 参数...：连接串只经环境变量传给 dbt-admin，不出现在命令行（ps 看不到）
run_admin() {
  local bin=$1 url=$2; shift 2
  # 用 shell 的“变量赋值前缀”而不是 `env VAR=...`：后者会让连接串短暂出现在 env 进程的命令行里
  case "$url" in
    *':ENC('*)
      # 密码是 ENC(密文)：dbt-admin 要用与服务相同的密钥才解得开（密钥只经环境变量传，不出现在命令行）
      DATABASE_URL="$url" TZ="${CUR_TZ:-Asia/Shanghai}" \
        SECRET_KEY="${ENVV[SECRET_KEY]:-${SECRET_KEY:-}}" MIRAGE_SECRET_KEY="${ENVV[MIRAGE_SECRET_KEY]:-${MIRAGE_SECRET_KEY:-}}" \
        MIRAGE_CIPHER_MODE="${ENVV[MIRAGE_CIPHER_MODE]:-${MIRAGE_CIPHER_MODE:-}}" MIRAGE_CIPHER_IV="${ENVV[MIRAGE_CIPHER_IV]:-${MIRAGE_CIPHER_IV:-}}" \
        "$bin" "$@"
      ;;
    *) DATABASE_URL="$url" TZ="${CUR_TZ:-Asia/Shanghai}" "$bin" "$@" ;;
  esac
}
# enc_db_url 连接串：把密码加密成 ENC(密文) 后输出（密钥取命令行 / env 里的 SECRET_KEY、MIRAGE_*）。
# 已经是 ENC(...)、找不到 dbt-admin、或加密失败（如密钥是公开缺省值）时原样输出并给出提示——不会因此中断安装。
enc_db_url() {
  local url=$1 out sk mk mm miv
  case "$url" in *':ENC('*) printf '%s' "$url"; return 0 ;; esac
  if [ -z "${ADMIN_BIN:-}" ]; then find_admin_bin; fi
  if [ -z "${ADMIN_BIN:-}" ]; then
    warn "找不到 dbt-admin，DATABASE_URL 里的密码保持明文；之后可运行 ./install.sh encrypt-db-url 加密" >&2
    printf '%s' "$url"; return 0
  fi
  sk=${SECRET_KEY:-${ENVV[SECRET_KEY]:-}}; mk=${MIRAGE_SECRET_KEY:-${ENVV[MIRAGE_SECRET_KEY]:-}}
  mm=${MIRAGE_CIPHER_MODE:-${ENVV[MIRAGE_CIPHER_MODE]:-}}; miv=${MIRAGE_CIPHER_IV:-${ENVV[MIRAGE_CIPHER_IV]:-}}
  if out=$(DATABASE_URL="$url" SECRET_KEY="$sk" MIRAGE_SECRET_KEY="$mk" MIRAGE_CIPHER_MODE="$mm" MIRAGE_CIPHER_IV="$miv" "$ADMIN_BIN" encrypt-url 2>/dev/null) && [ -n "$out" ]; then
    printf '%s' "$out"
  else
    warn "DATABASE_URL 的密码没有加密（dbt-admin encrypt-url 失败：通常是还没有设置自己的 SECRET_KEY / MIRAGE_SECRET_KEY）；设置密钥后运行 ./install.sh encrypt-db-url" >&2
    printf '%s' "$url"
  fi
}
# db_probe dbt-admin 连接串：成功时设置 DB_USERS（absent|empty|nonempty）/ DB_SUPERS / DB_INFO；连不上返回 1
db_probe() {
  local out k v
  DB_USERS="" DB_SUPERS="" DB_INFO=""
  if ! out=$(run_admin "$1" "$2" status --porcelain); then return 1; fi
  while IFS='=' read -r k v; do
    case "$k" in
      "$STATUS_USERS_KEY") DB_USERS=$v ;;
      superusers) DB_SUPERS=$v ;;
      '') ;;
      *) DB_INFO+="$k=$v " ;;
    esac
  done <<< "$out"
  case "$DB_USERS" in absent|empty|nonempty) ;; *) DB_USERS="?" ;; esac
  case "$DB_SUPERS" in ''|*[!0-9]*) DB_SUPERS="?" ;; esac
  return 0
}
# 找一个可用的 dbt-admin：已安装的 current 优先，其次安装包
find_admin_bin() {
  ADMIN_BIN=""
  if [ -x "$PREFIX/current/dbt-admin" ]; then ADMIN_BIN=$PREFIX/current/dbt-admin
  elif [ -n "${DBT_ADMIN_SRC:-}" ] && [ -x "$DBT_ADMIN_SRC" ]; then ADMIN_BIN=$DBT_ADMIN_SRC
  elif [ -x "$SCRIPT_DIR/dbt-admin" ]; then ADMIN_BIN=$SCRIPT_DIR/dbt-admin
  fi
}

# ---------------- doctor：只读预检与体检 ----------------
D_OK=0 D_WARN=0 D_FAIL=0
d_ok()   { D_OK=$((D_OK + 1));     printf '  [ OK ] %s\n' "$*"; log DOCTOR "OK   $*"; }
d_info() {                         printf '  [INFO] %s\n' "$*"; log DOCTOR "INFO $*"; }
d_warn() { D_WARN=$((D_WARN + 1)); printf '  [WARN] %s\n' "$*"; log DOCTOR "WARN $*"; }
d_fail() { D_FAIL=$((D_FAIL + 1)); printf '  [FAIL] %s\n' "$*"; log DOCTOR "FAIL $*"; }
# 体检（full）里是 FAIL；升级预检（preflight）里只是警告：本次安装正是要把它修好 / 它只是回滚目标
d_bad() { if [ "${DOCTOR_MODE:-full}" = full ]; then d_fail "$*"; else d_warn "$*（本次升级会处理 / 之后可重新安装修复）"; fi; }

# port_busy 端口：0=有进程在监听 1=空闲 2=无法判断
port_busy() {
  local out=""
  if command -v ss >/dev/null 2>&1; then out=$(ss -ltn 2>/dev/null || true)
  elif command -v netstat >/dev/null 2>&1; then out=$(netstat -ltn 2>/dev/null || true)
  else return 2; fi
  local re=":$1([[:space:]]|\$)"
  if [[ $out =~ $re ]]; then return 0; fi
  return 1
}
avail_kb() { # avail_kb 路径：取最近的已存在祖先目录所在分区的可用 KB
  local d=$1 a=""
  while [ ! -d "$d" ] && [ "$d" != / ] && [ -n "$d" ]; do d=${d%/*}; done
  [ -n "$d" ] || d=/
  a=$(df -Pk "$d" 2>/dev/null | awk 'NR==2 {print $4}') || a=""
  printf '%s' "$a"
}
perm_of() { stat -c '%a %U:%G' "$1" 2>/dev/null || true; }

# doctor_run preflight|full
doctor_run() {
  local mode=$1 c a need pk url hh body
  DOCTOR_MODE=$mode
  D_OK=0 D_WARN=0 D_FAIL=0
  echo "dbtool 环境检查（${mode}；只读，不改动任何东西）"

  # --- 系统 ---
  local OS_ID="" OS_LIKE="" OS_NAME="" k v
  if [ -r "$OS_RELEASE_FILE" ]; then
    while IFS='=' read -r k v; do
      v=${v%\"}; v=${v#\"}
      case "$k" in ID) OS_ID=$v ;; ID_LIKE) OS_LIKE=$v ;; PRETTY_NAME) OS_NAME=$v ;; esac
    done < "$OS_RELEASE_FILE"
    case " $OS_ID $OS_LIKE " in
      *" rhel "*|*" centos "*|*" rocky "*|*" almalinux "*|*" fedora "*|*" ol "*|*" anolis "*|*" openEuler "*|*" kylin "*)
        d_ok "操作系统: ${OS_NAME:-$OS_ID}" ;;
      *) d_warn "操作系统 ${OS_NAME:-$OS_ID} 不是 RHEL / CentOS 系：脚本按 CentOS 7+ 编写（systemd、firewalld、useradd），其它发行版请核对" ;;
    esac
  else
    d_warn "读不到 $OS_RELEASE_FILE，无法判断发行版"
  fi
  if [ "$(id -u 2>/dev/null || echo 1)" -eq 0 ]; then d_ok "以 root 运行"; else d_warn "当前不是 root：部分检查（读取 env 文件、连库）可能因权限不足而跳过；安装 / 升级需要 root"; fi
  if command -v systemctl >/dev/null 2>&1; then
    if [ -d /run/systemd/system ]; then d_ok "systemd 可用"; else d_warn "找到 systemctl，但 /run/systemd/system 不存在（容器 / chroot？）：服务无法由 systemd 管理"; fi
  else
    d_fail "没有 systemctl：需要 systemd（CentOS 7+）"
  fi

  # --- 依赖命令 ---
  for c in curl sha256sum install awk sed grep; do
    if command -v "$c" >/dev/null 2>&1; then :; else d_fail "缺少命令 $c"; fi
  done
  if ! id "$RUN_USER" >/dev/null 2>&1 && ! command -v useradd >/dev/null 2>&1; then d_fail "运行用户 $RUN_USER 不存在且没有 useradd"; fi
  if command -v flock >/dev/null 2>&1; then d_ok "flock 可用（并发加锁）"; else d_warn "没有 flock（util-linux）：改用 mkdir 锁，建议安装 util-linux"; fi
  for c in tar firewall-cmd journalctl; do
    command -v "$c" >/dev/null 2>&1 || d_info "可选命令 $c 不存在（$(case $c in tar) echo 解 .run 包需要 ;; firewall-cmd) echo --open-firewall 需要 ;; *) echo 失败时看日志需要 ;; esac)）"
  done
  if ! command -v mysql >/dev/null 2>&1; then d_info "没有 mysql 客户端：不影响（建表 / 初始化用安装包里的 dbt-admin）"; fi

  # --- 磁盘 ---
  need=307200   # KB，约 300 MB
  if [ -n "${DBT_DOCTOR_MIN_FREE_KB:-}" ]; then need=$DBT_DOCTOR_MIN_FREE_KB; fi
  pk=$(du -sk "$SCRIPT_DIR" 2>/dev/null | awk '{print $1}') || pk=""
  if [ -n "$pk" ] && [ "$pk" -gt 0 ] 2>/dev/null; then if [ $((pk * 2 + 51200)) -gt "$need" ] && [ -z "${DBT_DOCTOR_MIN_FREE_KB:-}" ]; then need=$((pk * 2 + 51200)); fi; fi
  a=$(avail_kb "$PREFIX")
  if [ -z "$a" ]; then d_warn "无法读取 $PREFIX 所在分区的可用空间"
  elif [ "$a" -lt "$need" ]; then d_fail "磁盘空间不足：$PREFIX 所在分区可用 $((a / 1024)) MB，至少需要 $((need / 1024)) MB（每个版本约一个二进制大小，默认保留 3 个版本）"
  else d_ok "磁盘空间：$PREFIX 所在分区可用 $((a / 1024)) MB"; fi

  # --- 配置完整性 ---
  cfg_load
  if [ -f "$ENV_FILE" ]; then
    if [ ! -r "$ENV_FILE" ]; then
      d_warn "无权读取 $ENV_FILE（请用 root 运行 doctor）"
    else
      local miss=""
      env_has DATABASE_URL && [ -n "${ENVV[DATABASE_URL]}" ] || miss+=" DATABASE_URL"
      env_has SECRET_KEY && [ -n "${ENVV[SECRET_KEY]}" ] || miss+=" SECRET_KEY"
      if [ -n "$miss" ]; then d_fail "$ENV_FILE 缺少必填项:$miss（用 reconfigure 补上）"; else d_ok "配置 $ENV_FILE：DATABASE_URL、SECRET_KEY 已设置"; fi
      case "${ENVV[DATABASE_URL]:-}" in mysql://*|'') ;; *) d_fail "DATABASE_URL 必须以 mysql:// 开头" ;; esac
      case "${ENVV[DATABASE_URL]:-}" in
        *':ENC('*) d_ok "DATABASE_URL 里的密码是加密的（ENC(...)）" ;;
        mysql://*:*@*) d_info "DATABASE_URL 里的密码是明文：可运行 ./install.sh encrypt-db-url 加密（用 MIRAGE_SECRET_KEY，缺省 SECRET_KEY）" ;;
      esac
      env_has TZ || d_warn "env 里没有 TZ：库里的时间是无时区的本地时间，须与 dbtool 服务一致（reconfigure --timezone）"
      if [ "${ENVV[REQUIRE_GOINCEPTION]:-}" != true ]; then d_info "REQUIRE_GOINCEPTION 未开启：生产环境建议开启（reconfigure --require-goinception）"; fi
      [ -n "${ENVV[PUBLIC_URL]:-}" ] || d_info "PUBLIC_URL 未设置：通知里的工单链接会回落为 http://127.0.0.1:端口（reconfigure --public-url）"
      pk=$(perm_of "$ENV_FILE")
      case "$pk" in
        "640 root:$RUN_USER"|"600 root:root"|"600 root:$RUN_USER") d_ok "$ENV_FILE 权限 $pk" ;;
        '') ;;
        *) d_warn "$ENV_FILE 权限 $pk（期望 640 root:$RUN_USER，含密钥与库密码，不应对其他用户可读）" ;;
      esac
      if [ -n "${ENVV[LISTEN]:-}" ]; then d_warn "env 里有 LISTEN=${ENVV[LISTEN]}，它优先于 $CONF_FILE（reconfigure --port 会把它注释掉）"; fi
      for k in EXPORT_BASE_DIR ARCHIVE_SPOOL_DIR; do
        if [ -n "${ENVV[$k]:-}" ] && [ ! -d "${ENVV[$k]}" ]; then d_warn "$k=${ENVV[$k]} 目录不存在"; fi
      done
    fi
    if [ -f "$CONF_FILE" ]; then
      if valid_port "$CUR_PORT"; then d_ok "监听端口 $CUR_PORT（${CUR_HOST:-所有网卡}）"; else d_fail "监听端口不合法: $CUR_PORT"; fi
    else
      d_warn "没有 $CONF_FILE：程序用默认端口 8000（install 会补生成）"
    fi
  else
    d_info "还没有配置 $ENV_FILE（全新主机；install 时会生成）"
    CUR_PORT=${PORT_N:-${PORT_ARG:-8000}}
  fi

  # --- 安装包完整性 / 已安装版本完整性 ---
  if [ -f "$SCRIPT_DIR/SHA256SUMS" ] && [ -n "${BIN_SRC:-}" ]; then
    if verify_sums "$SCRIPT_DIR"; then d_ok "安装包 SHA256SUMS 校验通过"; else d_fail "安装包 SHA256SUMS 校验失败：包已损坏或被改动，请重新获取"; fi
  fi
  doctor_soar
  c=$(current_id)
  if [ -n "$c" ]; then
    if release_ok "$c"; then
      if verify_sums "$PREFIX/releases/$c"; then d_ok "已安装版本 $c 的文件校验通过"
      else
        if [ -f "$PREFIX/releases/$c/SHA256SUMS" ]; then d_bad "已安装版本 $c 的文件与 SHA256SUMS 不一致（被改动或损坏）"; else d_info "已安装版本 $c 没有 SHA256SUMS，跳过校验"; fi
      fi
    else
      d_bad "current 指向的版本目录 $PREFIX/releases/$c 不完整"
    fi
  fi

  # --- 单元文件 ---
  if [ -f "$UNIT" ]; then
    local us=${ST[UNIT_SHA256]:-} cs
    cs=$(sha_of "$UNIT")
    if [ -z "$us" ]; then d_info "单元文件 $UNIT 存在（状态文件里没有记录其 sha256）"
    elif [ "$us" != "$cs" ]; then d_warn "单元文件 $UNIT 与脚本生成的版本不同（被手工改过）：升级时会先备份再重写，请把自定义项写成 drop-in（$SERVICE.service.d/*.conf）"
    else d_ok "单元文件未被手工修改"; fi
  fi

  # --- 端口 ---
  if [ "$mode" = full ] && valid_port "$CUR_PORT"; then
    local active=0
    if command -v systemctl >/dev/null 2>&1 && systemctl is-active --quiet "$SERVICE" 2>/dev/null; then active=1; fi
    if port_busy "$CUR_PORT"; then
      if [ "$active" -eq 1 ]; then d_ok "端口 $CUR_PORT 由运行中的 $SERVICE 占用"; else d_fail "端口 $CUR_PORT 已被其它进程占用（$SERVICE 未运行）：换端口或停掉占用者"; fi
    else
      case $? in 1) d_ok "端口 $CUR_PORT 空闲" ;; *) d_info "没有 ss / netstat，无法检查端口占用" ;; esac
    fi
  fi

  # --- 连库状态（dbt-admin status）---
  find_admin_bin
  url=${OPT_DB_URL:-${ENVV[DATABASE_URL]:-}}
  CUR_TZ=${ENVV[TZ]:-${TZ_NAME:-Asia/Shanghai}}
  if [ -z "$url" ]; then
    d_info "没有连接串（--database-url 或已有 env），跳过连库检查"
  elif [ -z "$ADMIN_BIN" ]; then
    d_warn "找不到 dbt-admin，无法检查元数据库状态"
  else
    if db_probe "$ADMIN_BIN" "$url"; then
      d_ok "元数据库可连接：$STATUS_USERS_KEY=$DB_USERS，启用的超级用户 $DB_SUPERS（${DB_INFO% }）"
      if [ "$DB_USERS" != nonempty ]; then d_info "库还是空的：用 install --bootstrap 或 init-db 初始化基础表与数据"; fi
    else
      d_fail "连不上元数据库，或库不存在（dbt-admin status 失败，原因见上；dbt-admin 不会自动建库）"
    fi
  fi

  # --- 服务健康 ---
  if [ "$mode" = full ]; then
    if command -v systemctl >/dev/null 2>&1; then
      if systemctl is-active --quiet "$SERVICE" 2>/dev/null; then d_ok "服务 $SERVICE 运行中"; else
        if [ -f "$UNIT" ]; then d_warn "服务 $SERVICE 未运行"; else d_info "服务 $SERVICE 尚未安装"; fi
      fi
      if systemctl is-enabled --quiet "$SERVICE" 2>/dev/null; then d_ok "服务已设置开机自启"; fi
    fi
    if [ -f "$UNIT" ] && command -v curl >/dev/null 2>&1 && valid_port "$CUR_PORT"; then
      hh=$(health_host)
      if curl -fsS -m 3 "http://$hh:$CUR_PORT/healthz" >/dev/null 2>&1; then d_ok "/healthz 正常"; else d_warn "/healthz 无响应（http://$hh:$CUR_PORT）"; fi
      if body=$(curl -fsS -m 3 "http://$hh:$CUR_PORT/readyz" 2>/dev/null); then
        case "$body" in *degraded*) d_warn "/readyz 为 degraded：缺少部分可选扩展表，执行 init-db 补建" ;; *) d_ok "/readyz 就绪" ;; esac
      else d_warn "/readyz 未就绪（元数据库不可达或核心表缺失）"; fi
    fi
  fi
  echo "结果：OK $D_OK，WARN $D_WARN，FAIL $D_FAIL"
  [ "$D_FAIL" -eq 0 ]
}

# ---------------- 健康检查 ----------------
HEALTH_REASON="" HEALTH_NOTE=""
# health_wait 超时秒数：先等 /healthz，再等 /readyz（元数据库可达 + 核心表存在）。0=健康；1=失败（HEALTH_REASON）
health_wait() {
  local t=$1 i=0 hh body=""
  HEALTH_REASON="" HEALTH_NOTE=""
  cfg_load
  hh=$(health_host)
  local base="http://$hh:$CUR_PORT" ok_h=0
  while [ "$i" -lt "$t" ]; do
    if curl -fsS -m 3 "$base/healthz" >/dev/null 2>&1; then ok_h=1; break; fi
    if ! systemctl is-active --quiet "$SERVICE" 2>/dev/null; then
      HEALTH_REASON="服务进程已退出（systemctl is-active 不是 active），常见原因：配置错误、库连不上、端口被占用"; return 1
    fi
    i=$((i + 1)); sleep 1
  done
  if [ "$ok_h" -ne 1 ]; then HEALTH_REASON="$t 秒内 $base/healthz 没有响应"; return 1; fi
  while [ "$i" -lt "$t" ] || [ "$i" -eq 0 ]; do
    if body=$(curl -fsS -m 3 "$base/readyz" 2>/dev/null); then
      case "$body" in *degraded*) HEALTH_NOTE="/readyz 为 degraded：缺少部分可选扩展表（对应功能降级），可执行 init-db 补建" ;; esac
      return 0
    fi
    i=$((i + 1)); [ "$i" -lt "$t" ] || break; sleep 1
  done
  HEALTH_REASON="/healthz 通过，但 $t 秒内 /readyz 未就绪：元数据库不可达或核心表缺失（journalctl -u $SERVICE 看详情）"
  return 1
}
journal_tail() { journalctl -u "$SERVICE" -n 30 --no-pager 2>/dev/null || true; }

# ---------------- 备份 / 切换 / 清理 ----------------
BACKUP_DIR=""
backup_config() {
  local ts bk
  printf -v ts '%(%Y%m%d-%H%M%S)T' -1
  bk=$BACKUP_ROOT/$ts
  if [ -e "$bk" ]; then bk=$bk-$$; fi
  if [ "$DRY_RUN" -eq 1 ]; then plan "备份 env / toml / unit / 状态文件到 $bk/（env 0600，其余 0640）"; return 0; fi
  install -d -m 0700 "$BACKUP_ROOT" "$bk"
  if [ -f "$ENV_FILE" ]; then install -m 0600 "$ENV_FILE" "$bk/dbtool.env"; fi
  if [ -f "$CONF_FILE" ]; then install -m 0640 "$CONF_FILE" "$bk/dbtool.toml"; fi
  if [ -f "$UNIT" ]; then install -m 0640 "$UNIT" "$bk/dbtool.service"; fi
  if [ -f "$STATE_FILE" ]; then install -m 0640 "$STATE_FILE" "$bk/.install-state"; fi
  BACKUP_DIR=$bk
  info "已备份当前配置到 $bk/"
  # 只保留最近 BACKUP_KEEP（3）份（目录名是时间戳）
  local old n=0 list
  list=$(ls -1 "$BACKUP_ROOT" 2>/dev/null | sort -r || true)
  while IFS= read -r old; do
    [ -n "$old" ] || continue
    n=$((n + 1))
    if [ "$n" -gt "$BACKUP_KEEP" ]; then rm -rf "${BACKUP_ROOT:?}/$old"; fi
  done <<< "$list"
}
# 原子切换 current：先建临时符号链接再 mv -T 覆盖（相对路径，整个 prefix 搬家也不会失效）
switch_current() {
  local id=$1 tmp="$PREFIX/.current.new.$$"
  rm -f "$tmp"
  ln -s "releases/$id" "$tmp"
  mv -T -f "$tmp" "$PREFIX/current"
}
prune_releases() {
  local keep=$KEEP cur prev r n=0 list
  cur=$(current_id); prev=${ST[PREVIOUS_VERSION]:-}
  list=$(ls -1At "$PREFIX/releases" 2>/dev/null || true)
  while IFS= read -r r; do
    [ -n "$r" ] || continue
    case "$r" in .stage.*) rm -rf "${PREFIX:?}/releases/$r"; continue ;; .*) continue ;; esac
    if [ "$r" = "$cur" ] || [ "$r" = "$prev" ]; then n=$((n + 1)); continue; fi
    if [ "$n" -lt "$keep" ]; then n=$((n + 1)); continue; fi
    info "清理旧版本 $r（保留最近 $keep 个）"
    rm -rf "${PREFIX:?}/releases/$r"
  done <<< "$list"
}

# ---------------- 版本目录：解包 + 校验 ----------------
NEW_ID="" REL_REUSE=0 CUR_ID=""
resolve_release_id() {
  local base=$PKG_VER d s8 h
  NEW_ID=$base REL_REUSE=0
  d=$PREFIX/releases/$base
  if [ -f "$d/.installed_at" ] && [ -f "$d/dbtool" ]; then
    if cmp -s "$d/dbtool" "$BIN_SRC" 2>/dev/null; then REL_REUSE=1; return 0; fi
    # 同一个版本号但二进制不同（例如 -dirty 构建）：用二进制 sha256 前 8 位区分，旧目录不动
    if [ -f "$BIN_SRC" ]; then h=$(sha_of "$BIN_SRC"); s8=${h:0:8}; else s8=new; fi
    NEW_ID=$base-$s8
    if [ -f "$PREFIX/releases/$NEW_ID/.installed_at" ] && cmp -s "$PREFIX/releases/$NEW_ID/dbtool" "$BIN_SRC" 2>/dev/null; then REL_REUSE=1; fi
  fi
}
stage_release() {
  local dest=$PREFIX/releases/$NEW_ID tmp
  if [ "$REL_REUSE" -eq 1 ]; then info "版本 $NEW_ID 已在 $dest，复用"; return 0; fi
  tmp=$PREFIX/releases/.stage.$$
  STAGE_TMP=$tmp
  rm -rf "$tmp"
  # 目标目录存在但没有完成标记（上次中断留下的残缺目录）：清掉重来
  if [ -e "$dest" ]; then rm -rf "${dest:?}"; fi
  install -d -m 0755 "$PREFIX/releases" "$tmp"
  install -m 0755 "$BIN_SRC" "$tmp/dbtool"
  if [ -n "$DBT_ADMIN_SRC" ] && [ -f "$DBT_ADMIN_SRC" ]; then install -m 0755 "$DBT_ADMIN_SRC" "$tmp/dbt-admin"; fi
  if [ -n "$SQL_SRC" ]; then cp -R "$SQL_SRC" "$tmp/sql"; fi
  if [ -n "$BOOT_SRC" ]; then cp -R "$BOOT_SRC" "$tmp/bootstrap"; fi
  if [ -n "$DOCS_SRC" ]; then cp -R "$DOCS_SRC" "$tmp/docs"; fi
  local f
  for f in install.sh env.example dbtool.example.toml goinception.config.toml; do
    if [ -f "$SCRIPT_DIR/$f" ]; then install -m 0644 "$SCRIPT_DIR/$f" "$tmp/$f"; fi
  done
  if [ ! -f "$tmp/env.example" ] && [ -f "$SCRIPT_DIR/../configs/env.example" ]; then install -m 0644 "$SCRIPT_DIR/../configs/env.example" "$tmp/env.example"; fi
  if [ ! -f "$tmp/dbtool.example.toml" ] && [ -f "$SCRIPT_DIR/../configs/dbtool.example.toml" ]; then install -m 0644 "$SCRIPT_DIR/../configs/dbtool.example.toml" "$tmp/dbtool.example.toml"; fi
  if [ ! -f "$tmp/goinception.config.toml" ] && [ -f "$SCRIPT_DIR/../configs/goinception.config.toml" ]; then install -m 0644 "$SCRIPT_DIR/../configs/goinception.config.toml" "$tmp/goinception.config.toml"; fi
  if [ -x "$tmp/install.sh" ] || [ -f "$tmp/install.sh" ]; then chmod 0755 "$tmp/install.sh"; fi
  # VERSION：优先沿用包里的；源码树 / --build 时现场生成
  if [ -f "$SCRIPT_DIR/VERSION" ] && [ "$BUILD" -eq 0 ]; then install -m 0644 "$SCRIPT_DIR/VERSION" "$tmp/VERSION"
  else printf 'version=%s\nbuild_time=%s\n' "$PKG_VER" "$PKG_BUILD_TIME" > "$tmp/VERSION"; fi
  # sha256：包里自带清单就用它校验；没有（源码树 / --build）则现场生成一份供 doctor 以后校验
  if [ -f "$SCRIPT_DIR/SHA256SUMS" ] && [ "$BUILD" -eq 0 ]; then
    install -m 0644 "$SCRIPT_DIR/SHA256SUMS" "$tmp/SHA256SUMS"
    if ! verify_sums "$tmp"; then
      DIE_CODE=3; die "安装包 sha256 校验失败（SHA256SUMS 与实际文件不一致，包已损坏或被改动）。已放弃，没有改动正在运行的版本；请重新获取安装包"
    fi
    info "sha256 校验通过（SHA256SUMS）"
  else
    warn "安装包里没有 SHA256SUMS（源码树 / --build），跳过校验，仅生成清单"
    (cd "$tmp" && find . -type f ! -name SHA256SUMS -exec sha256sum {} + | sed 's#  \./#  #' | LC_ALL=C sort -k2 > SHA256SUMS)
  fi
  printf -v f '%(%FT%T%z)T' -1
  printf '%s\n' "$f" > "$tmp/.installed_at"
  chown -R root:root "$tmp"
  command -v restorecon >/dev/null 2>&1 && restorecon -R "$tmp" 2>/dev/null || true
  mv -T "$tmp" "$dest"
  STAGE_TMP=""
  info "已解包到 $dest"
}
# ---------------- systemd 单元 ----------------
render_unit() {
  cat <<EOF
[Unit]
Description=dbtool 数据库运维管理平台
After=network-online.target
Wants=network-online.target

[Service]
# 由 install.sh 生成（每次升级会按此内容重写）。自定义项请写 drop-in：$SERVICE.service.d/*.conf，升级与卸载都不会改动它
Type=simple
User=$RUN_USER
Group=$RUN_USER
# 工作目录固定为安装目录：相对路径（未设置 EXPORT_BASE_DIR 时的导出目录等）不会落到 /。配置文件仍由 -config 显式指定
WorkingDirectory=$PREFIX
EnvironmentFile=$ENV_FILE
ExecStart=$PREFIX/current/dbtool -config $CONF_FILE
Restart=on-failure
RestartSec=5
# 停止时会等待正在执行的 SQL 工单（最长 10 分钟），所以给足时间
TimeoutStopSec=660
LimitNOFILE=65535
NoNewPrivileges=true
ProtectSystem=full
ProtectHome=true
PrivateTmp=true

[Install]
WantedBy=multi-user.target
EOF
}
UNIT_NEW_SHA="" UNIT_CUR_SHA="" UNIT_MODIFIED=0 UNIT_NEEDS_WRITE=0
unit_compute() {
  local h=""
  read -r h _ < <(render_unit | sha256sum)
  UNIT_NEW_SHA=$h
  UNIT_CUR_SHA=$(sha_of "$UNIT")
  UNIT_MODIFIED=0 UNIT_NEEDS_WRITE=0
  if [ ! -f "$UNIT" ] || [ "$UNIT_CUR_SHA" != "$UNIT_NEW_SHA" ]; then UNIT_NEEDS_WRITE=1; fi
  if [ -f "$UNIT" ] && [ -n "${ST[UNIT_SHA256]:-}" ] && [ "${ST[UNIT_SHA256]}" != "$UNIT_CUR_SHA" ]; then UNIT_MODIFIED=1; fi
}
write_unit() {
  if [ "$UNIT_NEEDS_WRITE" -ne 1 ]; then note "systemd 单元无变化"; return 1; fi
  if [ "$DRY_RUN" -eq 1 ]; then
    plan "重写 $UNIT（ExecStart=$PREFIX/current/dbtool）；drop-in 目录 $UNIT_DIR/$SERVICE.service.d 不改动"
    return 0
  fi
  install -d -m 0755 "$UNIT_DIR"
  if [ -f "$UNIT" ]; then
    if [ "$UNIT_MODIFIED" -eq 1 ]; then
      cp -f "$UNIT" "$UNIT.bak"
      warn "单元文件 $UNIT 被手工改过：已备份为 $UNIT.bak 并重写；请把其中的自定义项合并成 drop-in（$UNIT_DIR/$SERVICE.service.d/override.conf），drop-in 升级时不会被改动"
    elif [ -z "${ST[UNIT_SHA256]:-}" ]; then
      cp -f "$UNIT" "$UNIT.bak"
      note "已有单元文件来自旧版安装（状态文件里没有记录）：备份为 $UNIT.bak 后重写"
    fi
  fi
  render_unit > "$UNIT.new.$$"
  chmod 0644 "$UNIT.new.$$"
  mv -f "$UNIT.new.$$" "$UNIT"
  ST[UNIT_SHA256]=$UNIT_NEW_SHA
  state_flush
  systemctl daemon-reload
  info "已写入 $UNIT"
  return 0
}

# ---------------- 防火墙 ----------------
fw_available() { command -v firewall-cmd >/dev/null 2>&1 && systemctl is-active --quiet firewalld 2>/dev/null; }
fw_open() {
  local port=$1
  if [ "$DRY_RUN" -eq 1 ]; then plan "firewalld 放行 $port/tcp（已放行则不记录为本脚本添加）"; return 0; fi
  if ! fw_available; then warn "firewalld 没有运行，未修改防火墙；如使用 iptables 请自行放行 $port"; return 0; fi
  if firewall-cmd --permanent --query-port="$port/tcp" >/dev/null 2>&1; then
    info "firewalld 已放行 $port/tcp（不是本脚本添加的，卸载时不会移除）"
    return 0
  fi
  firewall-cmd --permanent --add-port="$port/tcp" >/dev/null
  firewall-cmd --reload >/dev/null
  ST[FIREWALL_OPENED]=1; ST[FIREWALL_PORT]=$port
  state_flush
  info "firewalld 已放行 $port/tcp（记录在状态文件，卸载时移除）"
}
fw_close() { # 只移除本脚本添加的规则
  local port=${ST[FIREWALL_PORT]:-}
  [ "${ST[FIREWALL_OPENED]:-0}" = 1 ] && [ -n "$port" ] || return 0
  if [ "$DRY_RUN" -eq 1 ]; then plan "firewalld 移除本脚本添加的 $port/tcp"; return 0; fi
  if fw_available; then
    firewall-cmd --permanent --remove-port="$port/tcp" >/dev/null 2>&1 || true
    firewall-cmd --reload >/dev/null 2>&1 || true
    info "firewalld 已移除本脚本添加的 $port/tcp"
  else
    warn "firewalld 没有运行，状态文件记录本脚本放行过 $port/tcp，请自行确认并移除"
  fi
  ST[FIREWALL_OPENED]=0; ST[FIREWALL_PORT]=""
  state_flush
}

# ---------------- 元数据库初始化 / 建表脚本（install --bootstrap、init-db、升级共用）----------------
# db_apply_flow dbt-admin 脚本根目录 模式
#   模式 init：用户显式要求初始化（--bootstrap / init-db）。库为空 → 基础结构 + 数据 + 扩展表脚本；非空 → 只执行幂等的扩展表脚本（--only-sql）；缺管理员再创建
#   模式 sql ：只执行 sql/*.sql 扩展表脚本（升级 / --apply-sql）；用建表账号失败是致命的，用运行账号失败只警告
ADMIN_BIN="" NEED_ADMIN=0
db_apply_flow() {
  local bin=$1 root=$2 mode=$3
  if [ "$mode" = init ]; then
    if [ "$DB_USERS" = absent ] || [ "$DB_USERS" = empty ]; then
      info "初始化元数据库：基础表 + 基础数据 + 扩展表脚本（dbt-admin bootstrap）"
      run_admin "$bin" "$ADMIN_DB_URL" bootstrap --sql-root "$root" --apply --yes \
        || die "bootstrap 失败（原因见上面的输出）。脚本幂等：修复后直接重跑即可；服务没有被改动"
    else
      info "元数据库里已有用户（用户表非空）：不重复初始化基础表与数据，只执行幂等的扩展表脚本（--only-sql）"
      run_admin "$bin" "$ADMIN_DB_URL" bootstrap --only-sql --sql-root "$root" --apply --yes \
        || die "执行 sql/*.sql 失败（原因见上面的输出；建表账号需要对库有 CREATE / ALTER 权限，可用 --ddl-database-url 指定）"
    fi
    if [ "$NEED_ADMIN" -eq 1 ]; then
      info "创建初始管理员 $ADMIN_USER"
      # 密码来自环境变量 DBT_ADMIN_PASSWORD（已在本进程环境里，随 env 传给 dbt-admin），或由 dbt-admin 在终端里无回显询问
      run_admin "$bin" "$ADMIN_DB_URL" admin create --username "$ADMIN_USER" --yes \
        || die "创建管理员失败（原因见上面的输出）。可稍后手工执行：$bin admin create --username $ADMIN_USER（连接串用环境变量 DATABASE_URL）"
    else
      info "已有启用的超级用户，跳过创建管理员"
    fi
    return 0
  fi
  # 模式 sql
  info "执行 sql/*.sql 扩展表脚本（dbt-admin bootstrap --only-sql，幂等）"
  if run_admin "$bin" "$ADMIN_DB_URL" bootstrap --only-sql --sql-root "$root" --apply --yes; then return 0; fi
  if [ -n "$DDL_URL" ]; then
    die "执行 sql/*.sql 失败（原因见上面的输出；建表账号需要对库有 CREATE / ALTER 权限）。服务没有被改动，修复后直接重跑即可"
  fi
  warn "用运行账号执行 sql/*.sql 建表脚本失败（它通常只有 DML 权限）。升级继续，但新版本新增的表不会被创建、对应功能降级；请稍后用建表账号执行：DDL_DATABASE_URL=... ./install.sh init-db，或 --ddl-database-url 重跑升级"
}

# 旧式：没有 dbt-admin 时用 mysql 客户端执行 sql/*.sql（沿用原做法）
apply_sql_with_mysql() {
  local root=$1 f url=$2
  parse_db_url "$url"
  case "$DB_PASS" in ENC\(*) DIE_CODE=2; die "没有 dbt-admin 时无法使用密码已加密（ENC(...)）的连接串：请用 --ddl-database-url 给出明文的建表账号连接串，或先安装 dbt-admin" ;; esac
  if [ -z "$DDL_URL" ]; then
    warn "没有提供 --ddl-database-url / DDL_DATABASE_URL：用运行账号 $DB_USER 建表，它需要对 $DB_NAME 有 CREATE 权限；生产环境建议用单独的建表账号，运行账号只给 DML 权限"
  fi
  if ! command -v mysql >/dev/null 2>&1; then
    info "安装 MySQL 客户端"
    pkg_install mysql || pkg_install mariadb || die "装不上 mysql 客户端，请手工执行 $root/sql/*.sql"
  fi
  for f in "$root"/sql/*.sql; do
    info "执行 $(basename "$f")"
    MYSQL_PWD="$DB_PASS" mysql -h "$DB_HOST" -P "$DB_PORT" -u "$DB_USER" "$DB_NAME" < "$f" \
      || die "执行 $f 失败（账号需要对 $DB_NAME 有 CREATE 权限）"
  done
}

# ---------------- 命令：version ----------------
cmd_version() {
  resolve_context
  compute_pkg_version
  echo "install.sh 所在目录: $SCRIPT_DIR"
  if [ -f "$SCRIPT_DIR/VERSION" ] || [ -f "$SCRIPT_DIR/dbtool" ]; then echo "安装包版本: $PKG_VER"; fi
  local c; c=$(current_id)
  if [ -n "$c" ]; then
    echo "已安装版本: $c（$PREFIX/current）"
    if [ -n "${ST[PREVIOUS_VERSION]:-}" ]; then echo "上一版本:   ${ST[PREVIOUS_VERSION]}"; fi
  else
    echo "已安装版本: （未安装）"
  fi
  return 0
}

# ---------------- 命令：status ----------------
cmd_status() {
  resolve_context
  cfg_load
  local c p r
  c=$(current_id)
  echo "dbtool 状态"
  echo "  安装目录   $PREFIX"
  echo "  当前版本   ${c:-（未安装）}${ST[PREVIOUS_VERSION]:+（上一版本 ${ST[PREVIOUS_VERSION]}）}"
  if [ -d "$PREFIX/releases" ]; then
    r=$(ls -1t "$PREFIX/releases" 2>/dev/null | grep -v '^\.' | tr '\n' ' ' || true)
    echo "  已保留版本 ${r:-（无）}"
  fi
  echo "  运行用户   $RUN_USER${ST[USER_CREATED]:+（由本脚本创建: ${ST[USER_CREATED]}）}"
  echo "  配置文件   $CONF_FILE、$ENV_FILE"
  echo "  状态文件   $STATE_FILE${ST[INSTALLED_AT]:+（首次安装 ${ST[INSTALLED_AT]}）}"
  echo "  监听端口   $CUR_PORT（${CUR_HOST:-所有网卡}）"
  if [ -n "${ST[FIREWALL_OPENED]:-}" ] && [ "${ST[FIREWALL_OPENED]}" = 1 ]; then echo "  防火墙     本脚本放行了 ${ST[FIREWALL_PORT]}/tcp"; fi
  if command -v systemctl >/dev/null 2>&1; then systemctl --no-pager status "$SERVICE" || true; fi
  p=$(health_host)
  { curl -fsS -m 3 "http://$p:$CUR_PORT/healthz" && echo; } || true
  { curl -fsS -m 3 "http://$p:$CUR_PORT/readyz" && echo; } || true
}

# ---------------- 命令：doctor ----------------
cmd_doctor() {
  resolve_context
  log_init
  locate_package soft || true
  if ! doctor_run full; then DIE_CODE=3; die "体检发现 FAIL 项，请先处理"; fi
}

# ---------------- 命令：install / upgrade ----------------
CFG_EXISTS=0 FRESH=1 HOST_N="" PORT_N=""
NEED_BACKFILL=0 TOML_MISSING=0 WILL_SWITCH=0
collect_fresh_config() {
  ask OPT_DB_URL "元数据库 DATABASE_URL（mysql://用户:密码@主机:3306/库名）"
  # --gen-secrets 且没有给 SECRET_KEY：稍后（二进制准备好之后）随机生成，这里不询问也不报缺
  if [ "$GEN_SECRETS" -eq 1 ] && [ -z "$SECRET_KEY" ]; then
    SECRETS_GENERATED=1
  else
    ask SECRET_KEY "SECRET_KEY（可加 --gen-secrets 自动生成）" "" secret
  fi
  ask PORT_N "监听端口" "8000"
  ask PUBLIC_URL "对外访问地址（用于通知里的链接，如 https://dba.example.com，可留空）" ""
  [ -n "$OPT_DB_URL" ] || { DIE_CODE=2; die "缺少 --database-url"; }
  if [ "$SECRETS_GENERATED" -eq 0 ] && [ -z "$SECRET_KEY" ]; then
    DIE_CODE=2; die "缺少 --secret-key（可加 --gen-secrets 自动生成）"
  fi
  parse_db_url "$OPT_DB_URL"
}

cmd_install() {
  local upgrade_only=$1 c r f i
  PLAN=("预检" "备份当前配置" "系统用户与目录" "安装新版本（解包 + sha256 校验）" "写入配置" "元数据库初始化 / 建表脚本" "部署 SOAR（SQL 优化工具）" "切换版本并重启服务" "防火墙" "健康检查" "收尾")
  STEP_TOTAL=${#PLAN[@]}
  require_root
  require_systemd
  resolve_context
  acquire_lock
  if [ "$DRY_RUN" -eq 1 ]; then echo "${c_ylw}dry-run：只打印计划，不改任何文件 / 服务 / 库${c_off}"; fi

  # ================= 1 预检 =================
  step
  cfg_load
  [ -f "$ENV_FILE" ] && CFG_EXISTS=1 || CFG_EXISTS=0
  FRESH=$((1 - CFG_EXISTS))
  CUR_ID=$(current_id)
  if [ "$upgrade_only" -eq 1 ] && [ "$CFG_EXISTS" -eq 0 ]; then
    DIE_CODE=2; die "没有发现已有安装（$ENV_FILE 不存在）。首次部署请用 install"
  fi
  if [ "$CFG_EXISTS" -eq 1 ]; then
    if [ "${ST[INSTALLED]:-0}" = 1 ] || [ -n "$CUR_ID" ]; then
      info "发现已有安装${CUR_ID:+（版本 $CUR_ID）}和配置 $ENV_FILE，按升级处理：配置、密钥、管理员、数据目录保持不变"
    else
      info "检测到旧配置 $ENV_FILE（之前卸载时保留），沿用它：不重新生成密钥、不覆盖已有值"
    fi
    if [ "${#GIVEN_CFG[@]}" -gt 0 ]; then
      warn "已有配置：本次给的 $(join_by ' ' "${GIVEN_CFG[@]}") 被忽略，没有改动配置。要变更请用：./install.sh reconfigure（--database-url / --secret-key / --public-url / --port / --listen / --timezone / --require-goinception）"
    fi
    if [ "$GEN_SECRETS" -eq 1 ]; then warn "--gen-secrets 只用于首次安装，不会改动已有的 SECRET_KEY / MIRAGE_*"; fi
    OPT_DB_URL="" SECRET_KEY="" PUBLIC_URL="" TZ_NAME="" LISTEN="" PORT_ARG="" REQUIRE_GI=""
    GEN_SECRETS=0
    CUR_TZ=${ENVV[TZ]:-Asia/Shanghai}
    RUN_DB_URL=${ENVV[DATABASE_URL]:-}
    PORT_N=""
  else
    # --listen 主机:端口 / --port 端口 → HOST_N、PORT_N（--port 优先）
    if [ -n "$LISTEN" ]; then
      case "$LISTEN" in *:*) HOST_N=${LISTEN%:*}; PORT_N=${LISTEN##*:} ;; *) PORT_N=$LISTEN ;; esac
    fi
    if [ -n "$PORT_ARG" ]; then PORT_N=$PORT_ARG; fi
    if [ -n "$PORT_N" ] && ! valid_port "$PORT_N"; then DIE_CODE=2; die "端口必须是 1–65535 之间的整数: $PORT_N"; fi
    collect_fresh_config
    if ! valid_port "${PORT_N:-8000}"; then DIE_CODE=2; die "端口必须是 1–65535 之间的整数: $PORT_N"; fi
    TZ_NAME=${TZ_NAME:-Asia/Shanghai}
    CUR_TZ=$TZ_NAME
    RUN_DB_URL=$OPT_DB_URL
  fi
  ADMIN_DB_URL=${DDL_URL:-$RUN_DB_URL}

  # 安装包
  locate_package
  if [ "$BUILD" -eq 0 ]; then check_elf "$BIN_SRC"
  elif [ "$DRY_RUN" -eq 0 ]; then build_from_source; fi
  resolve_release_id
  valid_release_id "$NEW_ID" || { DIE_CODE=2; die "版本号 $NEW_ID 含有不允许的字符"; }
  WILL_SWITCH=0
  [ "$NEW_ID" != "$CUR_ID" ] && WILL_SWITCH=1
  # 降级判定
  if [ -n "$CUR_ID" ] && [ "$WILL_SWITCH" -eq 1 ]; then
    local cur_label cur_bt
    cur_label=$(release_field "$CUR_ID" version); cur_label=${cur_label:-$CUR_ID}
    cur_bt=$(release_field "$CUR_ID" build_time)
    if is_downgrade "$cur_label" "$PKG_VER" "$cur_bt" "$PKG_BUILD_TIME"; then
      if [ "$ALLOW_DOWNGRADE" -ne 1 ]; then
        DIE_CODE=7; die "拒绝降级：当前 $cur_label，安装包是更低的 $PKG_VER。确需降级请加 --allow-downgrade（数据库脚本只增不改，通常向前兼容，但不保证；建议先备份元数据库）；回到上一版本请用 rollback"
      fi
      warn "降级：$cur_label → $PKG_VER（--allow-downgrade）。数据库脚本只增不改，旧版通常能运行在新版建出的表上，但不保证"
    fi
  fi

  # 预检（doctor 关键项）与 --bootstrap 的前置条件
  unit_compute
  if [ "$CFG_EXISTS" -eq 1 ]; then
    [ -f "$CONF_FILE" ] && TOML_MISSING=0 || TOML_MISSING=1
    NEED_BACKFILL=0
    env_has EXPORT_BASE_DIR || NEED_BACKFILL=1
    env_has ARCHIVE_SPOOL_DIR || NEED_BACKFILL=1
    env_has GOINCEPTION_CONFIG_FILE || NEED_BACKFILL=1
  fi
  if [ "$SKIP_PREFLIGHT" -eq 1 ]; then
    warn "已跳过预检（--skip-preflight）"
  else
    if ! doctor_run preflight; then DIE_CODE=3; die "预检失败（见上面的 FAIL 项）：没有改动任何东西。修复后重跑，或加 --skip-preflight 强制继续（不推荐）"; fi
  fi
  find_admin_bin
  [ -n "$DBT_ADMIN_SRC" ] || DBT_ADMIN_SRC=$ADMIN_BIN
  local want_db=0 db_mode=""
  if [ "$BOOTSTRAP" -eq 1 ]; then want_db=1; db_mode=init
  elif [ "$SKIP_SQL" -eq 0 ] && { [ "$APPLY_SQL" -eq 1 ] || [ "$CFG_EXISTS" -eq 1 ]; }; then want_db=1; db_mode=sql; fi
  NEED_ADMIN=0
  if [ "$want_db" -eq 1 ]; then
    if [ "$db_mode" = init ]; then
      [ -n "$DBT_ADMIN_SRC" ] || { DIE_CODE=2; die "--bootstrap 需要 dbt-admin：请使用 make dist 生成的安装包（里面带 dbt-admin），或加 --build 从源码编译"; }
      [ -n "$RUN_DB_URL" ] || { DIE_CODE=2; die "缺少 DATABASE_URL（--database-url）"; }
      if ! db_probe "$DBT_ADMIN_SRC" "$ADMIN_DB_URL"; then
        die "连不上元数据库，或库不存在（dbt-admin 不会自动建库）。请先建库并检查 DATABASE_URL / --ddl-database-url"
      fi
      [ "$DB_USERS" != "?" ] || die "无法判断元数据库状态（dbt-admin status 输出异常）"
      [ "$DB_SUPERS" != "?" ] || die "无法判断超级用户数（dbt-admin status 输出异常）"
      [ "$DB_SUPERS" -eq 0 ] && NEED_ADMIN=1
      if [ "$DB_USERS" = absent ] || [ "$DB_USERS" = empty ]; then
        if [ -z "$BOOT_SRC" ] && [ ! -d "$PREFIX/current/bootstrap" ]; then
          DIE_CODE=2; die "找不到 bootstrap/（基础表与基础数据脚本）。请使用 make dist 生成的安装包，或在源码仓库的 deploy/ 目录里运行"
        fi
      fi
      if [ "$NEED_ADMIN" -eq 1 ]; then
        if is_tty; then
          ask ADMIN_USER "初始管理员用户名" "admin"
        elif [ -z "$ADMIN_USER" ] || [ -z "${DBT_ADMIN_PASSWORD:-}" ]; then
          DIE_CODE=2; die "非交互环境下 --bootstrap 需要 --admin-user，并用环境变量 DBT_ADMIN_PASSWORD 提供管理员密码（不要写在命令行上）"
        fi
        case "$ADMIN_USER" in ''|*[[:space:]]*) DIE_CODE=2; die "--admin-user 不能为空或含空白字符" ;; esac
        [ "${#ADMIN_USER}" -le 30 ] || { DIE_CODE=2; die "--admin-user 最长 30 个字符"; }
      fi
    elif [ -n "$DBT_ADMIN_SRC" ] && [ -n "$ADMIN_DB_URL" ]; then
      # 升级（没有显式 --apply-sql）：库为空（还没初始化）时不自动建表，提示用 init-db
      if db_probe "$DBT_ADMIN_SRC" "$ADMIN_DB_URL"; then
        if { [ "$DB_USERS" = absent ] || [ "$DB_USERS" = empty ]; } && [ "$APPLY_SQL" -eq 0 ]; then
          warn "元数据库还是空的：本次不执行建表脚本。需要初始化请用 install --bootstrap 或 init-db"
          want_db=0
        fi
      else
        warn "dbt-admin status 连不上元数据库（上面是原因）：建表脚本步骤将直接尝试执行"
      fi
    fi
  fi
  step_end

  if [ "$CFG_EXISTS" -eq 1 ]; then
    echo "      计划：${CUR_ID:-（无）} → $NEW_ID$( [ "$REL_REUSE" -eq 1 ] && echo '（版本目录已存在，复用）' )"
  fi

  # ================= 2 备份当前配置 =================
  step
  local need_backup=0
  if [ "$CFG_EXISTS" -eq 1 ] && { [ "$WILL_SWITCH" -eq 1 ] || [ "$UNIT_NEEDS_WRITE" -eq 1 ] || [ "$NEED_BACKFILL" -eq 1 ] || [ "$TOML_MISSING" -eq 1 ]; }; then need_backup=1; fi
  if [ "$need_backup" -eq 1 ]; then backup_config
  elif [ "$FRESH" -eq 1 ]; then note "无需备份（首次安装，没有已有配置）"
  else note "无需备份（版本、单元文件与配置都没有变化）"; fi
  step_end

  # ================= 3 系统用户与目录 =================
  step
  local DATA_DIR=$PREFIX/data GI_DIR=$PREFIX/goinception missing=()
  if ! id "$RUN_USER" >/dev/null 2>&1; then
    if [ "$DRY_RUN" -eq 1 ]; then
      plan "创建系统用户 $RUN_USER（nologin，由本脚本创建，状态文件记录，卸载时删除）"
      if [ "${ST[DATA_RESET]:-0}" = 1 ]; then plan "上次卸载把保留的数据目录与配置属主改成了 root：把 $DATA_DIR 还给 $RUN_USER，配置文件组还原"; fi
    else
      info "创建系统用户 $RUN_USER"
      useradd --system --no-create-home --shell /sbin/nologin "$RUN_USER"
      ST[USER_CREATED]=1; ST[PREFIX]=$PREFIX; ST[RUN_USER]=$RUN_USER
      # 上次卸载删用户之前，把保留下来的数据目录与配置改成了 root 属主（避免留下无主 uid 被别的账号复用）：这里还原
      if [ "${ST[DATA_RESET]:-0}" = 1 ]; then
        if [ -d "$DATA_DIR" ]; then chown -R "$RUN_USER" "$DATA_DIR"; fi
        if [ -d "$GI_DIR" ]; then chown -R "$RUN_USER" "$GI_DIR"; fi
        if [ -f "$CONF_FILE" ]; then chown root:"$RUN_USER" "$CONF_FILE"; fi
        ST[DATA_RESET]=0
        info "已把保留的数据目录 $DATA_DIR 与配置文件的属主还原给 $RUN_USER"
      fi
      state_flush
    fi
  else
    note "运行用户 $RUN_USER 已存在"
  fi
  # 目录只在缺失时创建：已有目录的属主 / 权限 / 内容一概不动
  for f in "$DATA_DIR" "$DATA_DIR/export" "$DATA_DIR/archive-spool"; do [ -d "$f" ] || missing+=("$f"); done
  if [ "$DRY_RUN" -eq 1 ]; then
    plan "创建 $PREFIX、$PREFIX/releases、$CONF_DIR（缺失才建）"
    [ "${#missing[@]}" -gt 0 ] && plan "创建数据目录 ${missing[*]}（属主 $RUN_USER，0750）"
    [ -d "$GI_DIR" ] || plan "创建 goInception 配置目录 $GI_DIR（属主 $RUN_USER，0750）"
  else
    for f in "$PREFIX" "$PREFIX/releases" "$CONF_DIR"; do [ -d "$f" ] || install -d -m 0755 "$f"; done
    # 不对 $PREFIX 整体递归 chown：data/ 下是运行用户的导出文件与归档暂存，升级时不能被改回 root
    if [ "${#missing[@]}" -gt 0 ]; then install -d -o "$RUN_USER" -g "$RUN_USER" -m 0750 "${missing[@]}"; fi
    # goInception 配置目录：页面保存 config.toml 用“同目录临时文件 + rename”，运行用户需要对目录可写；位于 /opt 下，不受 ProtectSystem=full 影响
    if [ ! -d "$GI_DIR" ]; then install -d -o "$RUN_USER" -g "$RUN_USER" -m 0750 "$GI_DIR"; fi
  fi
  step_end

  # ================= 4 安装新版本 =================
  step
  if [ "$DRY_RUN" -eq 1 ]; then
    if [ "$BUILD" -eq 1 ]; then plan "go build 编译 dbtool / dbt-admin（版本 $PKG_VER）"; fi
    if [ "$REL_REUSE" -eq 1 ]; then plan "版本 $NEW_ID 已存在，复用"
    else plan "解包到 $PREFIX/releases/$NEW_ID 并用 SHA256SUMS 校验（dbtool、dbt-admin、sql、bootstrap、docs、VERSION）"; fi
  else
    stage_release
  fi
  step_end

  # ================= 5 写入配置 =================
  step
  if [ "$DRY_RUN" -eq 1 ]; then
    if [ "$FRESH" -eq 1 ]; then plan "生成 $ENV_FILE（0640 root:$RUN_USER）与 $CONF_FILE（监听 ${HOST_N:-0.0.0.0}:${PORT_N:-8000}）；密钥 $( [ "$SECRETS_GENERATED" -eq 1 ] && echo '随机生成，不回显' || echo '取自参数' )"
    else
      plan "已有配置不覆盖"
      [ "$NEED_BACKFILL" -eq 1 ] && plan "补充缺省项 EXPORT_BASE_DIR / ARCHIVE_SPOOL_DIR / GOINCEPTION_CONFIG_FILE（只补缺失的）"
      [ "$TOML_MISSING" -eq 1 ] && plan "补生成 $CONF_FILE"
    fi
    plan "部署 goInception 初始配置 $PREFIX/goinception/config.toml（文件不存在且 GOINCEPTION_CONFIG_FILE 指向它时；属主 $RUN_USER，0640）"
  else
    write_config_step
  fi
  step_end

  # ================= 6 元数据库 =================
  step
  if [ "$want_db" -eq 0 ]; then
    if [ "$SKIP_SQL" -eq 1 ]; then note "已指定 --skip-sql，不执行建表脚本（之后用 init-db 补）"
    elif [ "$FRESH" -eq 1 ]; then note "未执行建表脚本。使用通知渠道与定时执行前，请执行 ./install.sh init-db（全新空库用 install --bootstrap），或重跑本脚本并加 --apply-sql"
    else note "跳过"; fi
  elif [ "$DRY_RUN" -eq 1 ]; then
    if [ "$db_mode" = init ]; then
      if [ "$DB_USERS" = absent ] || [ "$DB_USERS" = empty ]; then plan "库为空：执行基础表 + 基础数据 + 扩展表脚本（dbt-admin bootstrap --apply）"
      else plan "库已有数据：只执行幂等的扩展表脚本（bootstrap --only-sql），不重复初始化"; fi
      [ "$NEED_ADMIN" -eq 1 ] && plan "创建管理员 $ADMIN_USER"
    else plan "执行幂等的扩展表脚本（bootstrap --only-sql）"; fi
  else
    local root=$PREFIX/releases/$NEW_ID admin=$PREFIX/releases/$NEW_ID/dbt-admin
    if [ -x "$admin" ]; then
      db_apply_flow "$admin" "$root" "$db_mode"
    elif [ "$db_mode" = init ]; then
      die "--bootstrap 需要 dbt-admin（包里没有）"
    else
      apply_sql_with_mysql "$root" "${DDL_URL:-$RUN_DB_URL}"
    fi
  fi
  step_end

  # ================= 7 部署 SOAR =================
  # 放在建表脚本之后：设置要写进 dbt_setting。整步都不致命（出错只警告），所以不会挡住升级
  step
  if [ "$DRY_RUN" -eq 1 ]; then soar_plan
  else soar_step "$PREFIX/releases/$NEW_ID/dbt-admin"; fi
  step_end

  # ================= 8 切换版本并重启 =================
  step
  local restart_needed=0 prev_id=$CUR_ID
  if [ "$WILL_SWITCH" -eq 1 ] || [ "$UNIT_NEEDS_WRITE" -eq 1 ] || [ "$NEED_BACKFILL" -eq 1 ] || [ "$FRESH" -eq 1 ]; then restart_needed=1; fi
  if [ "$DRY_RUN" -eq 1 ]; then
    [ "$WILL_SWITCH" -eq 1 ] && plan "原子切换 $PREFIX/current -> releases/$NEW_ID"
    write_unit || true
    if [ "$restart_needed" -eq 1 ]; then plan "systemctl enable + restart $SERVICE（会等待执行中的工单，最长 10 分钟）"; else plan "无变化，不重启（服务不健康时才重启）"; fi
  else
    if [ "$WILL_SWITCH" -eq 1 ]; then
      switch_current "$NEW_ID"
      info "current -> releases/$NEW_ID"
    fi
    write_unit || true
    systemctl enable "$SERVICE" >/dev/null 2>&1 || true
    if [ "$restart_needed" -eq 0 ]; then
      # 没有任何变化：服务健康就不打扰；不健康（例如上次没起来、用户手工修了配置）则重启
      if systemctl is-active --quiet "$SERVICE" 2>/dev/null && health_wait 1; then note "没有变化，服务健康，不重启"
      else restart_needed=1; note "服务当前不健康，重启一次"; fi
    fi
    if [ "$restart_needed" -eq 1 ]; then
      info "重启 $SERVICE（服务会等待执行中的 SQL 工单，最长 10 分钟：TimeoutStopSec=660）"
      systemctl restart "$SERVICE" || true
    fi
  fi
  step_end

  # ================= 9 防火墙 =================
  step
  if [ "$DRY_RUN" -eq 1 ]; then
    if [ "$OPEN_FW" -eq 1 ]; then fw_open "${PORT_N:-$CUR_PORT}"; else note "未指定 --open-firewall，不改防火墙"; fi
  else
    cfg_load
    if [ "$OPEN_FW" -eq 1 ]; then fw_open "$CUR_PORT"; else note "未指定 --open-firewall，不改防火墙"; fi
  fi
  step_end

  # ================= 10 健康检查（失败自动回滚）=================
  step
  if [ "$DRY_RUN" -eq 1 ]; then
    plan "等待 /healthz 与 /readyz（最长 $HEALTH_TIMEOUT 秒）；失败则自动回滚到 ${prev_id:-（无上一版本）}"
  else
    info "等待服务就绪（/healthz 与 /readyz，最长 $HEALTH_TIMEOUT 秒）"
    if ! health_wait "$HEALTH_TIMEOUT"; then
      journal_tail
      handle_unhealthy "$prev_id" "$NEW_ID" install
    fi
    if [ -n "$HEALTH_NOTE" ]; then warn "$HEALTH_NOTE"; fi
  fi
  step_end

  # ================= 11 收尾 =================
  step
  if [ "$DRY_RUN" -eq 1 ]; then
    plan "写状态文件 $STATE_FILE（版本 $NEW_ID、PREFIX、运行用户、端口、单元 sha256），保留最近 $KEEP 个版本"
    if [ "$want_db" -eq 1 ]; then plan "权限迁移（健康检查通过后）：dbt-admin perm migrate --apply（幂等、只增不减；失败只警告）"; fi
  else
    local now; printf -v now '%(%FT%T%z)T' -1
    if [ "${ST[INSTALLED]:-0}" != 1 ]; then ST[INSTALLED_AT]=$now; fi
    if [ -n "$prev_id" ] && [ "$prev_id" != "$NEW_ID" ]; then ST[PREVIOUS_VERSION]=$prev_id; fi
    ST[INSTALLED]=1 ST[VERSION]=$NEW_ID ST[PREFIX]=$PREFIX ST[RUN_USER]=$RUN_USER ST[PORT]=$CUR_PORT
    state_flush
    prune_releases
    if [ "$want_db" -eq 1 ] && [ -x "$PREFIX/releases/$NEW_ID/dbt-admin" ]; then perm_migrate_step "$PREFIX/releases/$NEW_ID/dbt-admin"; fi
  fi
  step_end
  print_summary "$prev_id"
}

# ---------------- 权限迁移（原权限码 → 按钮权限）----------------
# dbt-admin perm migrate --apply：把权限组 / 用户单独持有的原权限码一次性展开成按钮授权，并标记为已迁移。
# 幂等、只增不减；逐条撤销的 SQL 存在配置备份目录里（perm-migrate-rollback-<时间>.sql）。
# 放在健康检查通过之后：新版本没起来要回滚时，库里的权限还是迁移前的样子；迁移失败只警告，不挡升级（修好之前普通用户没有任何按钮权限，超级用户不受影响）。
perm_migrate_step() {
  local bin=$1 rb ts
  printf -v ts '%(%Y%m%d-%H%M%S)T' -1
  if [ -n "$BACKUP_DIR" ] && [ -d "$BACKUP_DIR" ]; then rb=$BACKUP_DIR
  else rb=$BACKUP_ROOT; install -d -m 0700 "$rb" 2>/dev/null || true; fi
  rb=$rb/perm-migrate-rollback-$ts.sql
  if [ "$SKIP_PERM" -eq 1 ]; then
    note "已指定 --skip-perm-migrate，不执行权限迁移：迁移完成前普通用户没有任何按钮权限（超级用户不受影响）；之后可手工执行 $bin perm migrate 预览，再加 --apply 执行"
    return 0
  fi
  info "权限迁移：把原权限码迁移成按钮授权（dbt-admin perm migrate，幂等、只增不减；最多等 2 分钟）"
  # 输出实时显示（迁移会列出每个权限组 / 用户的变化，卡住时也能看到停在哪一步）；dbt-admin 自己有 2 分钟的期限，超时报错退出
  if { run_admin "$bin" "$ADMIN_DB_URL" perm migrate --apply --yes --rollback-file "$rb" 2>&1 | sed 's/^/    /'; [ "${PIPESTATUS[0]}" -eq 0 ]; }; then
    if [ -f "$rb" ]; then note "撤销脚本：$rb"; fi
  else
    warn "权限迁移没有完成（上面是原因）：升级继续，但迁移完成前普通用户没有任何按钮权限（超级用户不受影响）；修好后执行 $bin perm migrate 预览，再加 --apply 执行"
  fi
}

# ---------------- SOAR（SQL 优化 / 改写工具）----------------
# 程序放在版本目录之外（<prefix>/plugins/soar）：路径固定，升级 / 回滚都不用改系统设置。设置页的校验要求程序是真实文件、
# 可执行、不能被其他用户改写（所以属主 root、0755）。配置 soar.yaml 里的 log-output 改写成 <prefix>/data/soar.log
# （运行用户可写的目录；包里原来的 /opt/dbtool/soar.log 在别的安装目录下写不进去）。
# 下面这几个函数都是“出错只警告”：返回非 0 表示这一步没做成，调用方不据此中断安装 / 升级（set -e 在 if / || 里不生效，
# 所以每个会失败的命令都显式判断）。

# 输出去掉 CR 的 soar.yaml，并把 log-output 指到 <prefix>/data/soar.log
soar_render_yaml() {
  tr -d '\r' < "$1" | LOGP="$PREFIX/data/soar.log" awk '/^log-output:/ { print "log-output: " ENVIRON["LOGP"]; next } { print }'
}

# 程序是不是 dbtool 要调用的那种 SOAR：二进制里同时有 dbtool 传给它的两个 DSN 参数名与 -allow-online-as-test（grep -a 按文本查二进制）
soar_looks_real() {
  grep -aq -e 'allow-online-as-test' "$1" 2>/dev/null && grep -aq -e 'online-dsn' "$1" 2>/dev/null && grep -aq -e 'test-dsn' "$1" 2>/dev/null
}

# dry-run：只打印计划
soar_plan() {
  if [ -z "$PLUGINS_SRC" ]; then plan "安装包里没有 plugins/soar：跳过 SOAR 部署"; return 0; fi
  plan "部署 SOAR 程序到 $PREFIX/plugins/soar（root:root 0755；内容相同则不动，不同则原子替换）"
  if [ -f "$PLUGINS_SRC/soar.yaml" ]; then plan "部署 $PREFIX/plugins/soar.yaml（已存在不覆盖；包内新版另存 soar.yaml.dist；log-output 改为 $PREFIX/data/soar.log）"; fi
  plan "系统设置 optimizer.soar_path / optimizer.soar_config 还没设置时指向上面两个文件（dbt-admin settings set；已设置的不改）"
}

# 部署文件（幂等）。0 = 已部署或已是最新；1 = 没有部署
soar_deploy() {
  local dir=$PREFIX/plugins src=$PLUGINS_SRC new cur tmp dist line
  if [ -z "$src" ]; then
    note "安装包里没有 plugins/soar，不部署 SOAR（已部署的 SOAR 与系统设置不受影响）"
    return 1
  fi
  # 内容核对：dbtool 调用 SOAR 时会传 -online-dsn / -test-dsn / -allow-online-as-test，真正的 SOAR（XiaoMi/soar）程序里一定有这些参数名；
  # 名字也叫 soar 的其他程序（例如同名的软件包管理器 soar）不能被部署成 SQL 优化工具
  if ! soar_looks_real "$src/soar"; then
    warn "安装包里的 plugins/soar 不像 SQL 优化工具 SOAR（里面没有 -online-dsn / -test-dsn / -allow-online-as-test 这些参数名；可能是同名的软件包管理器 soar）：不部署，也不改系统设置。请换成 XiaoMi/soar 的 linux 版程序后重新打包"
    return 1
  fi
  # 包完整性：SHA256SUMS 里有 plugins/soar 的记录就核对（预检已核对过整包；这里防 --skip-preflight）
  if [ -f "$SCRIPT_DIR/SHA256SUMS" ] && [ "$BUILD" -eq 0 ]; then
    line=$(grep -E '^[0-9a-f]{64} [ *]plugins/soar$' "$SCRIPT_DIR/SHA256SUMS" 2>/dev/null | head -n1 || true)
    if [ -n "$line" ] && ! (cd "$SCRIPT_DIR" && printf '%s\n' "$line" | sha256sum -c --quiet - >/dev/null 2>&1); then
      warn "安装包里的 plugins/soar 与 SHA256SUMS 不一致（包已损坏或被改动），不部署 SOAR；请重新获取安装包"
      return 1
    fi
  fi
  if ! install -d -m 0755 "$dir"; then warn "无法创建 $dir，跳过 SOAR 部署"; return 1; fi
  new=$(sha_of "$src/soar")
  cur=$(sha_of "$dir/soar")
  if [ -n "$cur" ] && [ "$cur" = "$new" ]; then
    note "SOAR 已是最新（$dir/soar，sha256 ${new:0:12}），不重复部署"
    chown root:root "$dir/soar" 2>/dev/null || true
    chmod 0755 "$dir/soar" 2>/dev/null || true
  else
    tmp=$dir/.soar.$$
    # 先写同目录临时文件再 rename：正在运行的 SOAR 不会受影响，也不会留下写了一半的程序
    if install -m 0755 "$src/soar" "$tmp" && chown root:root "$tmp" 2>/dev/null; then :; else rm -f "$tmp"; warn "写入 $tmp 失败，跳过 SOAR 部署"; return 1; fi
    if ! mv -f "$tmp" "$dir/soar"; then rm -f "$tmp"; warn "替换 $dir/soar 失败，跳过 SOAR 部署"; return 1; fi
    if [ -n "$cur" ]; then info "已更新 SOAR：$dir/soar（sha256 ${cur:0:12} → ${new:0:12}）"
    else info "已部署 SOAR：$dir/soar（root:root 0755，sha256 ${new:0:12}）"; fi
  fi
  if [ -f "$src/soar.yaml" ]; then
    dist=$dir/soar.yaml.dist
    if soar_render_yaml "$src/soar.yaml" > "$dist.tmp" && mv -f "$dist.tmp" "$dist"; then
      chmod 0644 "$dist" 2>/dev/null || true
      chown root:root "$dist" 2>/dev/null || true
      if [ ! -e "$dir/soar.yaml" ]; then
        if install -m 0644 "$dist" "$dir/soar.yaml"; then
          chown root:root "$dir/soar.yaml" 2>/dev/null || true
          info "已部署 SOAR 配置 $dir/soar.yaml（日志写到 $PREFIX/data/soar.log）"
        else warn "写入 $dir/soar.yaml 失败"; fi
      elif cmp -s "$dist" "$dir/soar.yaml"; then
        note "SOAR 配置 $dir/soar.yaml 已是最新"
      else
        note "已有 $dir/soar.yaml 且与包内版本不同：保持不动（包内新版在 $dir/soar.yaml.dist，可自行比对合并）"
      fi
    else
      rm -f "$dist.tmp"; warn "生成 $dist 失败，没有部署 SOAR 配置"
    fi
  else
    note "安装包里没有 plugins/soar.yaml：只部署程序（SOAR 用内置默认配置）"
  fi
  if command -v restorecon >/dev/null 2>&1; then restorecon -R "$dir" 2>/dev/null || true; fi
  return 0
}

# 系统设置 key 还没设置时设成 want；已有值（管理员配置过的）一律不改。0 = 无需再做 / 已设置；1 = 读写设置失败
soar_set_if_unset() {
  local admin=$1 key=$2 want=$3 out cur
  if ! out=$(run_admin "$admin" "$RUN_DB_URL" settings get --key "$key" 2>&1); then
    warn "读取系统设置 $key 失败，没有自动配置 SOAR：$(printf '%s' "$out" | head -n1 | cut -c1-160)。修复后重跑 ./install.sh upgrade，或到“平台设置 › 系统配置 › SQL 优化工具”手工填写 $want"
    return 1
  fi
  cur=$(printf '%s\n' "$out" | sed -n 's/^当前值：//p' | head -n1 || true)
  case "$cur" in
    "$want") note "系统设置 $key 已是 $want"; return 0 ;;
    ''|'-'|'—') ;;   # 未设置：dbt-admin settings get 在没有值时输出半角 “-”（internal/dbtadmin 的 orDash）
    *) note "系统设置 $key 已设置为 $cur，保持不变（要改用本次部署的 SOAR，改成 $want）"; return 0 ;;
  esac
  if out=$(run_admin "$admin" "$RUN_DB_URL" settings set --key "$key" --value "$want" --apply --yes 2>&1); then
    info "已设置系统设置 $key = $want（服务几秒内生效，不用重启）"
    return 0
  fi
  warn "写入系统设置 $key 失败：$(printf '%s' "$out" | head -n1 | cut -c1-160)。请到“平台设置 › 系统配置 › SQL 优化工具”手工填写 $want"
  return 1
}

# 部署完文件后把系统设置指向它们；测试库 DSN 还没配时提醒一次
soar_configure() {
  local admin=$1 out
  if [ ! -x "$admin" ] || [ -z "${RUN_DB_URL:-}" ]; then
    note "没有可用的 dbt-admin 或元数据库连接，跳过系统设置：请到“平台设置 › 系统配置 › SQL 优化工具”把 SOAR 程序路径填成 $PREFIX/plugins/soar"
    return 0
  fi
  if ! soar_set_if_unset "$admin" optimizer.soar_path "$PREFIX/plugins/soar"; then return 0; fi
  if [ -f "$PREFIX/plugins/soar.yaml" ]; then soar_set_if_unset "$admin" optimizer.soar_config "$PREFIX/plugins/soar.yaml" || true; fi
  out=$(run_admin "$admin" "$RUN_DB_URL" settings get --key optimizer.soar_test_dsn 2>/dev/null || true)
  case "$out" in
    *"当前值：（已设置）"*) ;;
    *) note "SOAR 还需要一个测试库（必须是非生产库，SOAR 会在其中建临时表）：到“平台设置 › 系统配置 › SQL 优化工具”填写“SOAR 测试环境 DSN”后，智能调优里的 SOAR 才可用" ;;
  esac
  return 0
}

soar_step() {
  local admin=$1
  if soar_deploy; then soar_configure "$admin"; fi
  return 0
}

# doctor：SOAR 部署情况（只读）
doctor_soar() {
  local f=$PREFIX/plugins/soar new cur
  if [ -f "$f" ]; then
    if [ -x "$f" ]; then d_ok "SOAR 已部署：$f（$(perm_of "$f")）"; else d_warn "SOAR $f 没有执行权限（install.sh upgrade 会重新部署）"; fi
    if [ -n "${PLUGINS_SRC:-}" ]; then
      new=$(sha_of "$PLUGINS_SRC/soar"); cur=$(sha_of "$f")
      if [ "$new" = "$cur" ]; then d_ok "SOAR 与安装包里的一致"
      else d_info "SOAR 与安装包里的不同：install.sh upgrade 会替换为包里的版本"; fi
    fi
  elif [ -n "${PLUGINS_SRC:-}" ]; then
    d_info "SOAR 尚未部署：安装包里带有 plugins/soar，install / upgrade 时会自动部署到 $PREFIX/plugins"
  fi
}

# 部署 goInception 初始配置 <prefix>/goinception/config.toml：只在 GOINCEPTION_CONFIG_FILE 指向这个默认位置且文件不存在时部署；
# 已有文件、或 env 里指向别处，一概不动。属主 运行用户，权限 0640（含备份库密码）。
deploy_goinception_config() {
  local want=$PREFIX/goinception/config.toml cur=${ENVV[GOINCEPTION_CONFIG_FILE]:-} src="" f
  if [ "$cur" != "$want" ]; then
    note "GOINCEPTION_CONFIG_FILE 指向 ${cur:-（未设置）}，不部署 goInception 初始配置"
    return 0
  fi
  if [ -e "$want" ]; then note "已有 $want，不覆盖"; return 0; fi
  for f in "$SCRIPT_DIR/goinception.config.toml" "$SCRIPT_DIR/../configs/goinception.config.toml"; do
    if [ -f "$f" ]; then src=$f; break; fi
  done
  if [ -z "$src" ]; then warn "安装包里没有 goinception.config.toml，跳过 goInception 初始配置"; return 0; fi
  install -d -o "$RUN_USER" -g "$RUN_USER" -m 0750 "$PREFIX/goinception"
  install -o "$RUN_USER" -g "$RUN_USER" -m 0640 "$src" "$want"
  info "已部署 goInception 初始配置 $want（0640，属主 $RUN_USER）；到“平台设置 › 审核引擎”页填写备份库等参数；Docker 运行 goInception 时挂载目录 $PREFIX/goinception"
}

# 写入配置：首次安装生成 env / toml；已有配置只补缺省、不覆盖
write_config_step() {
  local tmp f added=0 h p old
  if [ "$FRESH" -eq 1 ]; then
    # 随机生成密钥（--gen-secrets）：只写入 env 文件，不在终端显示；SECRET_KEY 已给出时不动 MIRAGE_*
    if [ "$SECRETS_GENERATED" -eq 1 ]; then
      gen_secrets_into_vars
      SECRET_KEY=$GEN_SK
      if [ -z "$MIRAGE_SECRET_KEY" ]; then
        MIRAGE_SECRET_KEY=$GEN_MK
        : "${MIRAGE_CIPHER_IV:=$GEN_MIV}"
        : "${MIRAGE_CIPHER_MODE:=$GEN_MM}"
      fi
      GEN_SK="" GEN_MK="" GEN_MIV="" GEN_MM=""
    fi
    # 连接串里的密码先加密再写进配置（密钥此时已确定）；加密不了就保持明文并提示，不中断安装
    ENC_DB_URL=$(enc_db_url "$OPT_DB_URL")
    tmp=$ENV_FILE.new.$$
    umask 077
    {
      echo "# dbtool 配置（由 install.sh 生成）。改动请用 ./install.sh reconfigure（会备份、原子替换），或编辑后 systemctl restart $SERVICE"
      if [ "$SECRETS_GENERATED" -eq 1 ]; then
        echo "# SECRET_KEY / MIRAGE_* 由 install.sh --gen-secrets 随机生成。请备份；MIRAGE_* 一旦使用不能再改，SECRET_KEY 同理"
      else
        echo "# SECRET_KEY / MIRAGE_* 由使用者提供。请备份；MIRAGE_* 一旦使用不能再改"
      fi
      if [ "$ENC_DB_URL" != "$OPT_DB_URL" ]; then echo "# DATABASE_URL 里的密码是 ENC(密文)：用 MIRAGE_SECRET_KEY（缺省 SECRET_KEY）加密。改了密钥要重新加密：./install.sh reconfigure --database-url ..."; fi
      echo "DATABASE_URL=$(q "$ENC_DB_URL")"
      echo "SECRET_KEY=$(q "$SECRET_KEY")"
      [ -n "$PUBLIC_URL" ]         && echo "PUBLIC_URL=$(q "$PUBLIC_URL")"
      [ "$REQUIRE_GI" = true ]     && echo "REQUIRE_GOINCEPTION=true"
      [ -n "$MIRAGE_SECRET_KEY" ]  && echo "MIRAGE_SECRET_KEY=$(q "$MIRAGE_SECRET_KEY")"
      [ -n "$MIRAGE_CIPHER_MODE" ] && echo "MIRAGE_CIPHER_MODE=$(q "$MIRAGE_CIPHER_MODE")"
      [ -n "$MIRAGE_CIPHER_IV" ]   && echo "MIRAGE_CIPHER_IV=$(q "$MIRAGE_CIPHER_IV")"
      echo "TZ=$(q "$TZ_NAME")"
    } > "$tmp"
    umask 022
    chown root:"$RUN_USER" "$tmp"; chmod 0640 "$tmp"
    mv -f "$tmp" "$ENV_FILE"
    info "已写入配置 $ENV_FILE（权限 0640，仅 root 与 $RUN_USER 可读）"
    if [ "$SECRETS_GENERATED" -eq 1 ]; then
      warn "已随机生成 SECRET_KEY 与 MIRAGE_* 并写入 $ENV_FILE（不在终端显示）。请立即备份该文件中的 SECRET_KEY 与 MIRAGE_*：丢失后实例账号密码等密文无法解密，且之后不能再更改"
    fi
    env_load
  fi
  # 数据目录的环境变量：env 文件里没有设置时才写入（已有值不覆盖；首次安装与升级都执行）
  if env_append EXPORT_BASE_DIR "$PREFIX/data/export"; then added=1; fi
  if env_append ARCHIVE_SPOOL_DIR "$PREFIX/data/archive-spool"; then added=1; fi
  if [ "$added" -eq 1 ]; then
    info "已在 $ENV_FILE 里补充 EXPORT_BASE_DIR / ARCHIVE_SPOOL_DIR（指向 $PREFIX/data，未设置的项才写入）"
  fi
  # goInception 配置文件路径：env 里没有才写入（已有值不覆盖）；文件不存在时部署初始模板
  if env_append GOINCEPTION_CONFIG_FILE "$PREFIX/goinception/config.toml"; then
    info "已在 $ENV_FILE 里补充 GOINCEPTION_CONFIG_FILE=$PREFIX/goinception/config.toml"
  fi
  deploy_goinception_config
  # 配置文件 dbtool.toml：只在不存在时生成，已有的一个字都不改
  if [ "$FRESH" -eq 1 ]; then
    write_conf "${HOST_N:-0.0.0.0}" "${PORT_N:-8000}"
    info "已写入 $CONF_FILE（监听 ${HOST_N:-0.0.0.0}:${PORT_N:-8000}；以后改端口用 ./install.sh reconfigure --port N）"
  elif [ ! -f "$CONF_FILE" ]; then
    # 旧版本把端口写在环境变量文件的 LISTEN= 里：生成等价的 dbtool.toml；env 里的 LISTEN 不动（它仍然优先，行为不变）
    old=${ENVV[LISTEN]:-}
    case "$old" in
      "")    h=0.0.0.0; p=8000 ;;
      *:*)   h=${old%:*}; p=${old##*:} ;;
      *)     h=0.0.0.0; p=$old ;;
    esac
    write_conf "${h:-0.0.0.0}" "$p"
    info "已补生成 $CONF_FILE（监听 ${h:-0.0.0.0}:$p，与现有配置一致）"
  fi
  if [ "$FRESH" -eq 0 ] && [ -n "${ENVV[LISTEN]:-}" ]; then
    warn "$ENV_FILE 里有 LISTEN=${ENVV[LISTEN]}，它优先于 $CONF_FILE；要改用配置文件里的端口：./install.sh reconfigure --port N（会注释掉 LISTEN 并备份）"
  fi
  cfg_load
}
# write_conf 主机 端口：生成 dbtool.toml（不含密钥，0644）
write_conf() {
  local tmp=$CONF_FILE.new.$$
  {
    echo "# dbtool 配置文件（由 install.sh 生成，TOML）。改端口 / 监听地址请用：./install.sh reconfigure --port N --listen 主机:端口"
    echo "# 完整说明见安装包里的 dbtool.example.toml；数据库密码、SECRET_KEY 等敏感项在 $ENV_FILE 里。"
    echo "# 同一项在环境变量里也有时，环境变量优先。"
    echo "[server]"
    echo "port = $2"
    echo "host = \"$1\""
  } > "$tmp"
  chmod 0644 "$tmp"
  mv -f "$tmp" "$CONF_FILE"
}

# 健康检查失败：能回滚就自动回滚到 prev_id 并重启 + 校验；退出码 5（已恢复）或 6（无法恢复）
handle_unhealthy() {
  local prev=$1 failed=$2 what=$3
  warn "新版本 $failed 健康检查失败：$HEALTH_REASON"
  if [ -z "$prev" ] || [ "$prev" = "$failed" ] || ! release_ok "$prev"; then
    ROLLBACK_NOTE="没有可回滚的上一版本，服务保持当前状态。修复配置后：systemctl restart $SERVICE，或重新运行本命令"
    DIE_CODE=6; die "服务不健康，且没有上一版本可回滚。看日志: journalctl -u $SERVICE -n 100；常见原因 DATABASE_URL 不对 / 库连不上 / 端口被占用，修改 $ENV_FILE（或 reconfigure）后重试"
  fi
  warn "自动回滚到上一版本 $prev ..."
  switch_current "$prev"
  systemctl restart "$SERVICE" || true
  if health_wait "$HEALTH_TIMEOUT"; then
    ST[VERSION]=$prev
    state_flush
    ROLLBACK_NOTE="已自动回滚到 $prev，服务健康。失败的版本 $failed 保留在 $PREFIX/releases/ 供排查（数据库脚本只增不改，不需要回滚库）"
    DIE_CODE=5; die "$what 失败：$failed 没有通过健康检查（$HEALTH_REASON），已回滚到 $prev 并恢复正常"
  fi
  ROLLBACK_NOTE="回滚到 $prev 后服务仍不健康（$HEALTH_REASON），需要人工处理：journalctl -u $SERVICE -n 100；配置备份在 $BACKUP_ROOT/"
  DIE_CODE=6; die "$failed 与回滚目标 $prev 都没有通过健康检查，需要人工处理"
}

print_summary() {
  local prev=$1 port url ip="" hn
  cfg_load
  port=$CUR_PORT
  url=${ENVV[PUBLIC_URL]:-}
  if [ -z "$url" ]; then
    hn=$(hostname -I 2>/dev/null || true)
    ip=${hn%% *}
    url=http://${ip:-<本机IP>}:$port
  fi
  cat <<EOF

${c_grn}dbtool 已启动${c_off}（版本 $NEW_ID）
  访问地址   $url
  配置文件   $CONF_FILE（监听端口等；改动用 ./install.sh reconfigure）
  环境变量   $ENV_FILE（数据库密码、SECRET_KEY 等敏感项）
  版本目录   $PREFIX/releases/*，当前 $PREFIX/current${prev:+（上一版本 $prev，回滚：./install.sh rollback）}
  状态文件   $STATE_FILE；升级前配置备份 $BACKUP_ROOT/<时间戳>/；安装日志 $LOG_FILE
  查看日志   journalctl -u $SERVICE -f
  常用命令   ./install.sh status | doctor | upgrade | rollback | reconfigure | uninstall [--purge]

下一步清单：
EOF
  if [ "$SECRETS_GENERATED" -eq 1 ]; then
    echo "  0. 立刻备份 $ENV_FILE 里的 SECRET_KEY 与 MIRAGE_*（密码库 + 离线保管）：丢失后密文无法解密，且之后不能再更改"
  fi
  cat <<EOF
  1. goInception：部署（Docker，见 docs/ops-manual.md 7.1），在“平台设置 › 审核引擎”里配置并测试连接；生产环境建议 ./install.sh reconfigure --require-goinception
  2. nginx / HTTPS：前面放反向代理，透传 X-Forwarded-Proto，并设置 PUBLIC_URL（./install.sh reconfigure --public-url https://dba.example.com，SSO 回调与通知里的工单链接都用它）
  3. 必配项（docs/ops-manual.md 8.3）：资源组、审批流、实例（目标库账号）、安全设置（通知渠道等按需配置）；上线前做 8.4 的抽样验证
  4. 数据目录 $PREFIX/data（导出文件、归档暂存）请纳入主机备份；归档暂存里可能有尚未上传的备份
EOF
}

# ---------------- 命令：rollback ----------------
cmd_rollback() {
  PLAN=("预检" "备份当前配置" "切换版本并重启服务" "健康检查" "收尾")
  STEP_TOTAL=${#PLAN[@]}
  require_root
  require_systemd
  resolve_context
  acquire_lock
  cfg_load
  local cur target
  cur=$(current_id)
  [ -n "$cur" ] || { DIE_CODE=2; die "没有 current 版本，无法回滚（未安装或旧布局；先用新安装包 install）"; }
  target=${TO_VERSION:-${ST[PREVIOUS_VERSION]:-}}
  [ -n "$target" ] || { DIE_CODE=2; die "没有记录上一版本，请用 --to <版本> 指定（已有版本：$(ls -1 "$PREFIX/releases" 2>/dev/null | grep -v '^\.' | tr '\n' ' ' || true)）"; }
  valid_release_id "$target" || { DIE_CODE=2; die "版本号不合法: $target"; }
  [ "$target" != "$cur" ] || { DIE_CODE=2; die "目标版本就是当前版本 $cur"; }
  release_ok "$target" || { DIE_CODE=2; die "版本 $target 不存在或不完整（$PREFIX/releases/$target）。已有版本：$(ls -1 "$PREFIX/releases" 2>/dev/null | grep -v '^\.' | tr '\n' ' ' || true)"; }

  step
  if verify_sums "$PREFIX/releases/$target"; then note "版本 $target 校验通过"
  else
    if [ -f "$PREFIX/releases/$target/SHA256SUMS" ]; then DIE_CODE=3; die "版本 $target 的文件与 SHA256SUMS 不一致，拒绝回滚到它"; fi
    note "版本 $target 没有 SHA256SUMS，跳过校验"
  fi
  note "回滚：$cur → $target（数据库脚本只增不改，旧版通常能运行在新表上；不回滚数据库）"
  if [ "$DRY_RUN" -eq 0 ]; then confirm "确认回滚到 $target？" || { info "已取消"; exit 0; }; fi
  step_end

  step
  if [ "$DRY_RUN" -eq 1 ]; then backup_config; else unit_compute; backup_config; fi
  step_end

  step
  if [ "$DRY_RUN" -eq 1 ]; then
    plan "原子切换 current -> releases/$target；重启 $SERVICE（会等待执行中的工单，最长 10 分钟）"
  else
    switch_current "$target"
    info "current -> releases/$target"
    systemctl enable "$SERVICE" >/dev/null 2>&1 || true
    info "重启 $SERVICE（会等待执行中的 SQL 工单，最长 10 分钟）"
    systemctl restart "$SERVICE" || true
  fi
  step_end

  step
  if [ "$DRY_RUN" -eq 1 ]; then plan "等待 /healthz 与 /readyz；失败则切回 $cur"
  else
    if ! health_wait "$HEALTH_TIMEOUT"; then
      journal_tail
      warn "回滚后的版本 $target 健康检查失败：$HEALTH_REASON；切回 $cur"
      switch_current "$cur"
      systemctl restart "$SERVICE" || true
      if health_wait "$HEALTH_TIMEOUT"; then
        ROLLBACK_NOTE="已切回 $cur，服务健康；$target 保留供排查"
        DIE_CODE=5; die "回滚到 $target 失败（$HEALTH_REASON），已恢复到 $cur"
      fi
      ROLLBACK_NOTE="两个版本都不健康，需要人工处理：journalctl -u $SERVICE -n 100"
      DIE_CODE=6; die "回滚失败且无法恢复"
    fi
  fi
  step_end

  step
  if [ "$DRY_RUN" -eq 0 ]; then
    ST[PREVIOUS_VERSION]=$cur ST[VERSION]=$target ST[INSTALLED]=1
    state_flush
    info "已回滚到 $target（再次 rollback 可回到 $cur）"
  else plan "状态文件：VERSION=$target，PREVIOUS_VERSION=$cur"; fi
  step_end
}

# ---------------- 命令：uninstall ----------------
cmd_uninstall() {
  PLAN=("停止并禁用服务" "移除 systemd 单元" "移除防火墙规则" "删除程序文件" "清理（--purge）" "记录状态")
  STEP_TOTAL=${#PLAN[@]}
  require_root
  resolve_context
  acquire_lock
  cfg_load
  local purge_list=() f cs
  if [ "$PURGE" -eq 1 ]; then
    purge_list=("$PREFIX（含 $PREFIX/data：导出文件与归档暂存，其中可能有尚未上传的备份；$PREFIX/goinception：goInception 配置，含备份库密码）" "$CONF_DIR（配置、SECRET_KEY / MIRAGE_*、状态文件与全部配置备份）")
    if [ "${ST[USER_CREATED]:-0}" = 1 ]; then purge_list+=("系统用户 $RUN_USER（由本脚本创建）"); fi
    echo "将要删除（--purge）："
    for f in "${purge_list[@]}"; do echo "  - $f"; done
    echo "不会删除：数据库里的任何东西、$UNIT_DIR/$SERVICE.service.d（drop-in）、$LOG_FILE。"
    if [ "$DRY_RUN" -eq 0 ]; then
      if [ "$ASSUME_YES" -eq 1 ]; then :
      elif tty_in; then
        local a=""
        read -r -p "确认删除上面的内容，请输入 PURGE: " a
        [ "$a" = PURGE ] || { info "已取消，没有删除任何东西"; exit 0; }
      else
        DIE_CODE=2; die "--purge 在非交互环境下必须同时加 --yes（会先打印上面的清单）"
      fi
    fi
  fi

  step
  if [ "$DRY_RUN" -eq 1 ]; then plan "systemctl disable --now $SERVICE（会等待执行中的工单，最长 10 分钟）"
  else
    info "停止并禁用服务（会等待执行中的 SQL 工单，最长 10 分钟）"
    systemctl disable --now "$SERVICE" 2>/dev/null || true
  fi
  step_end

  step
  cs=$(sha_of "$UNIT")
  if [ ! -f "$UNIT" ]; then note "没有单元文件"
  elif [ "$DRY_RUN" -eq 1 ]; then
    if [ -n "${ST[UNIT_SHA256]:-}" ] && [ "$cs" != "${ST[UNIT_SHA256]}" ]; then plan "单元文件被手工改过：备份为 $UNIT.bak 后删除"; else plan "删除 $UNIT"; fi
  else
    if [ -n "${ST[UNIT_SHA256]:-}" ] && [ "$cs" != "${ST[UNIT_SHA256]}" ]; then
      cp -f "$UNIT" "$UNIT.bak"
      warn "单元文件 $UNIT 被手工改过：已备份为 $UNIT.bak（重装后请把自定义项合并成 drop-in）"
    fi
    rm -f "$UNIT"
    systemctl daemon-reload || true
    info "已删除 $UNIT（drop-in 目录 $UNIT_DIR/$SERVICE.service.d 保持不动）"
  fi
  step_end

  step
  if [ "${ST[FIREWALL_OPENED]:-0}" = 1 ]; then fw_close; else note "防火墙规则不是本脚本添加的（或没有添加），不处理"; fi
  step_end

  step
  if [ "$DRY_RUN" -eq 1 ]; then
    plan "删除 $PREFIX/releases、$PREFIX/current、$PREFIX/plugins（SOAR，重装时自动重新部署；以及旧布局的 bin / sql / bootstrap）；保留 $PREFIX/data、$PREFIX/goinception 与 $CONF_DIR"
  else
    rm -rf "${PREFIX:?}/releases" "${PREFIX:?}/plugins" "${PREFIX:?}/bin" "${PREFIX:?}/sql" "${PREFIX:?}/bootstrap"
    rm -f "${PREFIX:?}/current"
    if [ -d "$PREFIX/data" ]; then
      info "保留数据目录 $PREFIX/data（导出文件、归档暂存）"
    else
      rmdir "$PREFIX" 2>/dev/null || true
    fi
  fi
  step_end

  step
  if [ "$PURGE" -eq 1 ]; then
    if [ "$DRY_RUN" -eq 1 ]; then plan "删除 ${purge_list[*]}"
    else
      rm -rf "${PREFIX:?}" "${CONF_DIR:?}"
      info "已删除 $PREFIX（含数据目录）与配置 $CONF_DIR"
      if [ "${ST[USER_CREATED]:-0}" = 1 ] && id "$RUN_USER" >/dev/null 2>&1; then
        userdel "$RUN_USER" 2>/dev/null && info "已删除由本脚本创建的系统用户 $RUN_USER" || warn "删除用户 $RUN_USER 失败，请手工处理"
      fi
    fi
  else
    note "保留配置 $CONF_DIR（含 SECRET_KEY 与库密码）与数据目录；需要一并删除请加 --purge"
    if [ "${ST[USER_CREATED]:-0}" = 1 ]; then
      if [ "$DRY_RUN" -eq 1 ]; then
        plan "删除由本脚本创建的系统用户 $RUN_USER（保留的数据目录与配置先改成 root 属主；重装时还原）"
      elif id "$RUN_USER" >/dev/null 2>&1; then
        # 先改属主再删用户：否则保留下来的文件会留下无主的 uid / gid，之后可能被别的账号复用
        if [ -d "$PREFIX/data" ]; then chown -R root:root "$PREFIX/data" 2>/dev/null || warn "改 $PREFIX/data 属主失败，请手工处理"; fi
        if [ -d "$PREFIX/goinception" ]; then chown -R root:root "$PREFIX/goinception" 2>/dev/null || warn "改 $PREFIX/goinception 属主失败，请手工处理"; fi
        if [ -f "$CONF_DIR/dbtool.toml" ]; then chown root:root "$CONF_DIR/dbtool.toml" 2>/dev/null || true; fi
        if userdel "$RUN_USER" 2>/dev/null; then
          info "已删除由本脚本创建的系统用户 $RUN_USER（保留的数据目录与配置改为 root 属主，重装时会还原给新建的用户）"
          ST[USER_CREATED]=0 ST[DATA_RESET]=1
        else
          warn "删除用户 $RUN_USER 失败，请手工处理"
        fi
      else
        ST[USER_CREATED]=0
      fi
    fi
  fi
  step_end

  step
  if [ "$DRY_RUN" -eq 1 ]; then plan "状态文件标记 INSTALLED=0（保留的配置与用户信息供重装复用）"
  elif [ "$PURGE" -eq 0 ]; then
    ST[INSTALLED]=0 ST[VERSION]="" ST[PREVIOUS_VERSION]="" ST[UNIT_SHA256]=""
    state_flush
    note "状态文件已更新（INSTALLED=0）；重新 install 会沿用保留的配置"
  fi
  step_end
  info "元数据库里的数据、dbt_ 开头的表都没有动"
}

# ---------------- 命令：init-db ----------------
cmd_init_db() {
  PLAN=("预检" "初始化 / 更新元数据库")
  STEP_TOTAL=${#PLAN[@]}
  require_root
  resolve_context
  acquire_lock
  cfg_load
  locate_package soft || true
  find_admin_bin
  local url root
  url=${OPT_DB_URL:-${ENVV[DATABASE_URL]:-}}
  CUR_TZ=${ENVV[TZ]:-${TZ_NAME:-Asia/Shanghai}}
  RUN_DB_URL=$url
  ADMIN_DB_URL=${DDL_URL:-$url}
  step
  [ -n "$url" ] || { DIE_CODE=2; die "没有连接串：请给 --database-url，或先 install 生成 $ENV_FILE"; }
  [ -n "$ADMIN_BIN" ] || { DIE_CODE=2; die "找不到 dbt-admin（已安装的 $PREFIX/current/dbt-admin 或安装包里的）"; }
  if [ -d "$PREFIX/current/bootstrap" ] || [ -d "$PREFIX/current/sql" ]; then root=$PREFIX/current
  elif [ -n "$BOOT_SRC" ] || [ -n "$SQL_SRC" ]; then root=$SCRIPT_DIR
    # 源码树：deploy/{sql,bootstrap} 与 SCRIPT_DIR 同级
  else DIE_CODE=2; die "找不到 sql/ 与 bootstrap/ 脚本目录"; fi
  if ! db_probe "$ADMIN_BIN" "$ADMIN_DB_URL"; then
    die "连不上元数据库，或库不存在（dbt-admin 不会自动建库）。请先建库并检查连接串 / --ddl-database-url"
  fi
  [ "$DB_USERS" != "?" ] || die "无法判断元数据库状态（dbt-admin status 输出异常）"
  [ "$DB_SUPERS" != "?" ] || die "无法判断超级用户数（dbt-admin status 输出异常）"
  note "库状态：$STATUS_USERS_KEY=$DB_USERS，启用的超级用户 $DB_SUPERS"
  NEED_ADMIN=0
  if [ "$DB_SUPERS" -eq 0 ] && { [ -n "$ADMIN_USER" ] || is_tty; }; then
    NEED_ADMIN=1
    if is_tty; then ask ADMIN_USER "初始管理员用户名" "admin"
    elif [ -z "${DBT_ADMIN_PASSWORD:-}" ]; then DIE_CODE=2; die "创建管理员需要用环境变量 DBT_ADMIN_PASSWORD 提供密码（不要写在命令行上）"; fi
    case "$ADMIN_USER" in ''|*[[:space:]]*) DIE_CODE=2; die "--admin-user 不能为空或含空白字符" ;; esac
  elif [ "$DB_SUPERS" -eq 0 ]; then
    note "还没有超级用户：加 --admin-user NAME（密码用环境变量 DBT_ADMIN_PASSWORD）可一并创建"
  fi
  step_end

  step
  if [ "$DRY_RUN" -eq 1 ]; then
    if [ "$DB_USERS" = absent ] || [ "$DB_USERS" = empty ]; then plan "库为空：执行基础表 + 基础数据 + 扩展表脚本（dbt-admin bootstrap --apply），脚本根目录 $root"
    else plan "库已有数据：只执行幂等的扩展表脚本（bootstrap --only-sql），不重复初始化"; fi
    [ "$NEED_ADMIN" -eq 1 ] && plan "创建管理员 $ADMIN_USER"
    plan "（不写库；dbt-admin bootstrap 不加 --apply 即为它自己的演练）"
  else
    db_apply_flow "$ADMIN_BIN" "$root" init
    if systemctl is-active --quiet "$SERVICE" 2>/dev/null; then
      note "服务运行中：018 / 019 / 050 / 070 / 080 等脚本执行后需要重启才生效：systemctl restart $SERVICE"
    fi
  fi
  step_end
}

# ---------------- 命令：reconfigure ----------------
# env_edit 源 目标 键=值...（值用 q() 引号）：纯 bash 逐行替换，值不进任何命令行；已有键就地替换（最后一处），没有则追加
RC_KEYS=() RC_VALS=() RC_SECRET=()
env_edit_file() {
  local src=$1 dst=$2 lines=() line i k idx found n
  mapfile -t lines < "$src"
  for ((n = 0; n < ${#RC_KEYS[@]}; n++)); do
    k=${RC_KEYS[$n]}; found=-1
    for ((i = 0; i < ${#lines[@]}; i++)); do
      line=${lines[$i]}
      line=${line#"${line%%[![:space:]]*}"}
      case "$line" in export[[:space:]]*) line=${line#export}; line=${line#"${line%%[![:space:]]*}"} ;; esac
      case "$line" in "$k="*) found=$i ;; esac
    done
    if [ "$found" -ge 0 ]; then lines[$found]="$k=$(q "${RC_VALS[$n]}")"
    else lines+=("$k=$(q "${RC_VALS[$n]}")"); fi
  done
  : > "$dst"
  for line in "${lines[@]}"; do printf '%s\n' "$line" >> "$dst"; done
}
rc_add() { # rc_add KEY VALUE [secret]
  local cur=${ENVV[$1]:-} has=0
  env_has "$1" && has=1
  if [ "$has" -eq 1 ] && [ "$cur" = "$2" ]; then note "$1：无变化"; return 0; fi
  RC_KEYS+=("$1"); RC_VALS+=("$2"); RC_SECRET+=("${3:-}")
  if [ -n "${3:-}" ]; then note "$1：$( [ "$has" -eq 1 ] && echo 修改 || echo 新增 )（值不显示）"
  else note "$1：${cur:-（未设置）} → $2"; fi
}

# encrypt-db-url：把 env 里 DATABASE_URL 的明文密码加密成 ENC(密文)；备份、原子替换。已经加密的什么也不做。
cmd_encrypt_db_url() {
  PLAN=("预检" "加密并写入")
  STEP_TOTAL=${#PLAN[@]}
  require_root
  resolve_context
  acquire_lock
  cfg_load
  [ -f "$ENV_FILE" ] || { DIE_CODE=2; die "没有发现已有配置 $ENV_FILE：首次部署请用 install"; }
  local url=${ENVV[DATABASE_URL]:-} new tmp
  step
  [ -n "$url" ] || { DIE_CODE=2; die "$ENV_FILE 里没有 DATABASE_URL"; }
  case "$url" in
    *':ENC('*) info "DATABASE_URL 里的密码已经是加密的，没有改动"; step_end; return 0 ;;
    mysql://*:*@*) ;;
    *) DIE_CODE=2; die "DATABASE_URL 里没有密码（格式应为 mysql://用户:密码@主机:3306/库名），无需加密" ;;
  esac
  find_admin_bin
  [ -n "$ADMIN_BIN" ] || { DIE_CODE=2; die "找不到 dbt-admin（已安装的 $PREFIX/current/dbt-admin 或安装包里的）"; }
  new=$(enc_db_url "$url")
  [ "$new" != "$url" ] || { DIE_CODE=2; die "没能加密（见上面的提示；常见原因：SECRET_KEY / MIRAGE_SECRET_KEY 还是公开缺省值或未设置）"; }
  step_end
  step
  if [ "$DRY_RUN" -eq 1 ]; then
    backup_config
    plan "把 $ENV_FILE 里的 DATABASE_URL 密码换成 ENC(密文)（原子替换，不重启）"
    step_end
    return 0
  fi
  backup_config
  RC_KEYS=(DATABASE_URL) RC_VALS=("$new") RC_SECRET=(secret)
  tmp=$ENV_FILE.new.$$
  umask 077
  env_edit_file "$ENV_FILE" "$tmp"
  umask 022
  chown root:"$RUN_USER" "$tmp"; chmod 0640 "$tmp"
  mv -f "$tmp" "$ENV_FILE"
  info "已把 $ENV_FILE 里 DATABASE_URL 的密码加密（备份在 $BACKUP_DIR/）。服务重新启动后读取：systemctl restart $SERVICE；换 SECRET_KEY / MIRAGE_SECRET_KEY 时需要重新加密"
  step_end
}

cmd_reconfigure() {
  PLAN=("预检" "备份当前配置" "应用修改" "重启与健康检查")
  STEP_TOTAL=${#PLAN[@]}
  require_root
  resolve_context
  acquire_lock
  cfg_load
  [ -f "$ENV_FILE" ] || { DIE_CODE=2; die "没有发现已有配置 $ENV_FILE：首次部署请用 install"; }
  local new_port="" new_host="" toml_change=0 secret_change=0 db_change=0 tmp
  RC_KEYS=() RC_VALS=() RC_SECRET=()
  step
  local db_was_enc=0
  case "${ENVV[DATABASE_URL]:-}" in *':ENC('*) db_was_enc=1 ;; esac
  if [ "$GIVEN_SECRET" -eq 1 ] && [ "$GIVEN_DB" -eq 0 ] && [ "$db_was_enc" -eq 1 ] && [ -z "${ENVV[MIRAGE_SECRET_KEY]:-}" ]; then
    DIE_CODE=2; die "DATABASE_URL 的密码是用当前 SECRET_KEY 加密的：只改 --secret-key 会让它解不开、服务起不来。请同时给 --database-url（明文连接串，脚本会用新密钥重新加密），或先在 env 里固定 MIRAGE_SECRET_KEY"
  fi
  if [ "$GIVEN_DB" -eq 1 ]; then
    parse_db_url "$OPT_DB_URL"
    db_change=1
    # 原来就是加密的，新连接串也加密（用现有 / 新给的密钥）；加密不了按明文写入并提示。下面的连通性验证仍用明文
    local newurl=$OPT_DB_URL
    if [ "$db_was_enc" -eq 1 ]; then
      newurl=$(enc_db_url "$OPT_DB_URL")
      [ "$newurl" != "$OPT_DB_URL" ] || warn "DATABASE_URL 原来是加密的，但这次没能加密新值（见上面的提示），将按明文写入"
    fi
    rc_add DATABASE_URL "$newurl" secret
  fi
  if [ "$GIVEN_SECRET" -eq 1 ]; then
    [ -n "$SECRET_KEY" ] || { DIE_CODE=2; die "--secret-key 不能为空"; }
    secret_change=1
    rc_add SECRET_KEY "$SECRET_KEY" secret
  fi
  if [ "$GIVEN_PUBLIC" -eq 1 ]; then
    case "$PUBLIC_URL" in ''|http://*|https://*) ;; *) DIE_CODE=2; die "--public-url 必须以 http:// 或 https:// 开头" ;; esac
    rc_add PUBLIC_URL "$PUBLIC_URL"
  fi
  if [ "$GIVEN_TZ" -eq 1 ]; then
    local tre='^[A-Za-z0-9_+/-]+$'
    [[ $TZ_NAME =~ $tre ]] || { DIE_CODE=2; die "--timezone 不是合法的时区名: $TZ_NAME"; }
    rc_add TZ "$TZ_NAME"
  fi
  if [ "$GIVEN_REQGI" -eq 1 ]; then rc_add REQUIRE_GOINCEPTION "$REQUIRE_GI"; fi
  if [ "$GIVEN_LISTEN" -eq 1 ]; then
    if [ -n "$LISTEN" ]; then
      case "$LISTEN" in *:*) new_host=${LISTEN%:*}; new_port=${LISTEN##*:} ;; *) new_port=$LISTEN ;; esac
    fi
    if [ -n "$PORT_ARG" ]; then new_port=$PORT_ARG; fi
    if [ -n "$new_port" ] && ! valid_port "$new_port"; then DIE_CODE=2; die "端口必须是 1–65535 之间的整数: $new_port"; fi
    if [ -n "$new_port" ] && [ "$new_port" != "$CUR_PORT" ]; then toml_change=1; note "监听端口：$CUR_PORT → $new_port（$CONF_FILE）"; fi
    if [ -n "$new_host" ] && [ "$new_host" != "$CUR_HOST" ]; then toml_change=1; note "监听地址：${CUR_HOST:-（所有网卡）} → $new_host（$CONF_FILE）"; fi
  fi
  if [ "${#RC_KEYS[@]}" -eq 0 ] && [ "$toml_change" -eq 0 ]; then
    if [ "$GIVEN_DB$GIVEN_SECRET$GIVEN_PUBLIC$GIVEN_TZ$GIVEN_REQGI$GIVEN_LISTEN" = 000000 ]; then
      DIE_CODE=2; die "没有要修改的项。可用：--database-url --secret-key --public-url --port --listen --timezone --require-goinception（或 --no-require-goinception）"
    fi
    info "配置已经是目标值，没有变化"
    exit 0
  fi
  if [ "$secret_change" -eq 1 ]; then
    warn "修改 SECRET_KEY 会使会话 / JWT 全部失效，并且实例账号密码、sql_config 密文、归档加密备份等凭它加解密的数据将无法解密（除非 MIRAGE_SECRET_KEY 已单独固定）。只在密钥泄露轮换且已按手册 9.8 处理时才改"
  fi
  # 新连接串先验证（连得上才允许改），除非 --no-verify
  if [ "$db_change" -eq 1 ] && [ "$NO_VERIFY" -eq 0 ]; then
    find_admin_bin
    if [ -n "$ADMIN_BIN" ]; then
      CUR_TZ=${ENVV[TZ]:-Asia/Shanghai}
      if db_probe "$ADMIN_BIN" "$OPT_DB_URL"; then note "新连接串验证通过：$(mask_url "$OPT_DB_URL")（$STATUS_USERS_KEY=$DB_USERS）"
      else DIE_CODE=3; die "新的 DATABASE_URL 连不上元数据库（原因见上面的输出）：没有改动配置。确认无误可加 --no-verify"; fi
    else
      warn "找不到 dbt-admin，无法验证新连接串"
    fi
  fi
  step_end

  step
  if [ "$DRY_RUN" -eq 1 ]; then
    backup_config
    plan "原子替换 $ENV_FILE$( [ "$toml_change" -eq 1 ] && echo " 与 $CONF_FILE" )；不重启（$( [ "$RESTART_OPT" = yes ] && echo '--restart：将重启并做健康检查，失败自动恢复备份' || echo '加 --restart 才重启' )）"
    step_end
    return 0
  fi
  if [ "$secret_change" -eq 1 ]; then confirm "确认修改 SECRET_KEY？" || { info "已取消，没有改动"; exit 0; }; fi
  backup_config
  step_end

  step
  if [ "${#RC_KEYS[@]}" -gt 0 ]; then
    tmp=$ENV_FILE.new.$$
    umask 077
    env_edit_file "$ENV_FILE" "$tmp"
    umask 022
    chown root:"$RUN_USER" "$tmp"; chmod 0640 "$tmp"
    mv -f "$tmp" "$ENV_FILE"
    info "已原子替换 $ENV_FILE（$(join_by ' ' "${RC_KEYS[@]}")）"
  fi
  if [ "$toml_change" -eq 1 ]; then
    apply_toml_change "$new_host" "$new_port"
    retire_env_listen
  fi
  env_load; toml_load
  if [ "$toml_change" -eq 1 ] && [ -n "$new_port" ]; then
    local oldp=${ST[PORT]:-}
    ST[PORT]=$new_port
    if [ "${ST[FIREWALL_OPENED]:-0}" = 1 ] && [ -n "$oldp" ] && [ "$oldp" != "$new_port" ]; then
      if [ "$OPEN_FW" -eq 1 ]; then
        fw_open "$new_port"
        if [ "${ST[FIREWALL_PORT]:-}" = "$new_port" ] && fw_available; then
          firewall-cmd --permanent --remove-port="$oldp/tcp" >/dev/null 2>&1 || true
          firewall-cmd --reload >/dev/null 2>&1 || true
          info "已移除本脚本之前放行的 $oldp/tcp"
        fi
      else
        warn "本脚本之前放行了 $oldp/tcp，新端口 $new_port 没有放行：加 --open-firewall 放行新端口并移除旧规则"
      fi
    elif [ "$OPEN_FW" -eq 1 ]; then
      fw_open "$new_port"
    fi
  elif [ "$OPEN_FW" -eq 1 ]; then
    fw_open "$CUR_PORT"
  fi
  state_flush
  step_end

  step
  local do_restart=0
  case "$RESTART_OPT" in
    yes) do_restart=1 ;;
    no) do_restart=0 ;;
    *) if tty_in && [ "$ASSUME_YES" -eq 0 ]; then confirm "现在重启 $SERVICE 使配置生效？" && do_restart=1; fi ;;
  esac
  if [ "$do_restart" -eq 0 ]; then
    note "配置已修改但尚未生效：systemctl restart $SERVICE（或再次运行并加 --restart）。备份在 $BACKUP_DIR/"
  else
    info "重启 $SERVICE（会等待执行中的 SQL 工单，最长 10 分钟）"
    systemctl restart "$SERVICE" || true
    if health_wait "$HEALTH_TIMEOUT"; then
      info "配置已生效，服务健康"
      [ -z "$HEALTH_NOTE" ] || warn "$HEALTH_NOTE"
    else
      journal_tail
      warn "改动后服务不健康（$HEALTH_REASON）：自动恢复备份的配置"
      restore_config_backup
      systemctl restart "$SERVICE" || true
      if health_wait "$HEALTH_TIMEOUT"; then
        ROLLBACK_NOTE="已恢复到修改前的配置（$BACKUP_DIR），服务健康"
        DIE_CODE=5; die "reconfigure 后服务不健康（$HEALTH_REASON），已恢复原配置并恢复正常"
      fi
      ROLLBACK_NOTE="恢复原配置后服务仍不健康，需要人工处理；备份在 $BACKUP_DIR/"
      DIE_CODE=6; die "reconfigure 失败且无法恢复"
    fi
  fi
  step_end
}
apply_toml_change() { # apply_toml_change 主机 端口
  local host=$1 port=$2 tmp=$CONF_FILE.new.$$
  if [ -f "$CONF_FILE" ]; then cp -p "$CONF_FILE" "$tmp"; else printf '[server]\n' > "$tmp"; fi
  if [ -n "$port" ]; then
    if grep -qE '^[[:space:]]*port[[:space:]]*=' "$tmp"; then sed -i -E "s/^([[:space:]]*port[[:space:]]*=[[:space:]]*)[0-9\"]+/\1$port/" "$tmp"
    elif grep -qE '^\[server\]' "$tmp"; then sed -i -E "/^\[server\]/a port = $port" "$tmp"
    else printf '\n[server]\nport = %s\n' "$port" >> "$tmp"; fi
  fi
  if [ -n "$host" ]; then
    if grep -qE '^[[:space:]]*#?[[:space:]]*host[[:space:]]*=' "$tmp"; then sed -i -E "s/^[[:space:]]*#?[[:space:]]*host[[:space:]]*=.*/host = \"$host\"/" "$tmp"
    elif grep -qE '^\[server\]' "$tmp"; then sed -i -E "/^\[server\]/a host = \"$host\"" "$tmp"
    else printf '\n[server]\nhost = "%s"\n' "$host" >> "$tmp"; fi
  fi
  chmod 0644 "$tmp"
  mv -f "$tmp" "$CONF_FILE"
  info "已原子替换 $CONF_FILE"
}
# env 里的 LISTEN 会覆盖配置文件：改端口时把它注释掉（已先整体备份）
retire_env_listen() {
  [ -n "${ENVV[LISTEN]:-}" ] || return 0
  local tmp=$ENV_FILE.new.$$ line lines=() i
  mapfile -t lines < "$ENV_FILE"
  umask 077
  : > "$tmp"
  for line in "${lines[@]}"; do
    case "$line" in LISTEN=*) printf '# 已迁移到 dbtool.toml（环境变量优先于配置文件）：%s\n' "$line" >> "$tmp" ;; *) printf '%s\n' "$line" >> "$tmp" ;; esac
  done
  umask 022
  chown root:"$RUN_USER" "$tmp"; chmod 0640 "$tmp"
  mv -f "$tmp" "$ENV_FILE"
  info "已注释 $ENV_FILE 中的 LISTEN 行，端口以 $CONF_FILE 为准"
}
restore_config_backup() {
  [ -n "$BACKUP_DIR" ] || return 0
  if [ -f "$BACKUP_DIR/dbtool.env" ]; then install -m 0640 -o root -g "$RUN_USER" "$BACKUP_DIR/dbtool.env" "$ENV_FILE.restore.$$"; mv -f "$ENV_FILE.restore.$$" "$ENV_FILE"; fi
  if [ -f "$BACKUP_DIR/dbtool.toml" ]; then install -m 0644 "$BACKUP_DIR/dbtool.toml" "$CONF_FILE.restore.$$"; mv -f "$CONF_FILE.restore.$$" "$CONF_FILE"; fi
}

# ---------------- 分发 ----------------
case "$CMD" in
  version)     cmd_version ;;
  status)      cmd_status ;;
  doctor)      cmd_doctor ;;
  install)     cmd_install 0 ;;
  upgrade)     cmd_install 1 ;;
  rollback)    cmd_rollback ;;
  uninstall)   cmd_uninstall ;;
  init-db)     cmd_init_db ;;
  reconfigure) cmd_reconfigure ;;
  encrypt-db-url) cmd_encrypt_db_url ;;
esac
