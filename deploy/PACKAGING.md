# 发布包打包说明

> **适用版本**：以 `deploy/RELEASE_VERSION` 为准；打包脚本为 `deploy/package-centos7.sh`。
> **读者**：负责构建与发布 dbtool 安装包的开发者 / 发布工程师。
> **文档类型**：构建与发布文档（环境要求、打包命令、流程、包内容、自查清单、排障）。
> **相关文档**：[../docs/one-click-deploy.md](../docs/one-click-deploy.md)（包里的 `install.sh`）· [../docs/ops-manual.md](../docs/ops-manual.md)（全新部署）· [../docs/deployment.md](../docs/deployment.md)（运行期配置与运维）· [../docs/release-notes.md](../docs/release-notes.md)（发布说明）。

## 目录

- [1. 概述](#1-概述)
- [2. 环境要求](#2-环境要求)
- [3. 生成发布包](#3-生成发布包)
- [4. 打包流程](#4-打包流程)
- [5. 发布包内容](#5-发布包内容)
- [6. 在 CentOS 上安装与升级](#6-在-centos-上安装与升级)
- [7. 发布前自查清单](#7-发布前自查清单)
- [8. 常见问题与排障](#8-常见问题与排障)
- [9. 验证状态与已知限制](#9-验证状态与已知限制)

## 1. 概述

`deploy/package-centos7.sh` 生成 Linux（CentOS 7 / 8 / 9、RHEL、Rocky、AlmaLinux 等带 systemd 的系统）的发布包：检查 → 交叉编译 → 复制文件 → 自检 → 打 tar.gz →（可选）生成自解压 `.run` 与 sha256 → 解压后验收。`make dist` 只是调用这个脚本，两种方式产物一致。

纯 Go、`CGO_ENABLED=0`：产物是静态二进制，不依赖服务器上的 C 库；前端模板与静态文件已嵌入 `dbtool` 二进制，包里没有单独的前端目录。

## 2. 环境要求

| 项 | 要求 | 说明 |
|---|---|---|
| Go | 1.25 或更高 | 脚本设置 `GOTOOLCHAIN=local`，用本机工具链；`SKIP_BUILD=1` 且有可复用的二进制时可以不装 Go。`go.mod` 声明 `go 1.25.0` |
| Shell 与工具 | `bash`、GNU `tar`、`gzip`、`sha256sum`、`sed`、`od` | 脚本用到 `tar --owner / --mode`，需要 GNU tar；Windows 用 Git Bash，macOS 需要 `brew install gnu-tar` |
| 可选 | `git`（版本号回退 `git describe`、写入 commit）、`file`（校验 `dbtool` 的 ELF 架构）、`make` | 没有 `make` 也没关系：直接用脚本 |
| 工作目录 | 项目根目录 | 脚本会检查必要文件是否存在 |

## 3. 生成发布包

有 `make` 时：

```bash
make dist
```

arm64 服务器（默认 amd64 / x86_64）：

```bash
make dist ARCH=arm64
```

指定版本号：

```bash
make dist VERSION=1.39
```

没有 `make`（如 Windows Git Bash）时用脚本，产物相同：

```bash
bash deploy/package-centos7.sh
```

打包前先跑 `go vet ./...` 与 `go test -count=1 ./...`，失败就不打包：

```bash
RUN_TESTS=1 bash deploy/package-centos7.sh
```

额外生成自解压 `.run`：

```bash
MAKE_RUN=1 bash deploy/package-centos7.sh
```

只改了文档 / 脚本时复用上次编译的二进制（**改了 Go 代码、模板、静态文件不要用**）：

```bash
SKIP_BUILD=1 bash deploy/package-centos7.sh
```

### 3.1 环境变量

| 变量 | 作用 | 默认 |
|---|---|---|
| `ARCH` | `amd64` 或 `arm64`，其它值直接报错 | `amd64` |
| `VERSION` | 版本号（脚本的位置参数优先）；只保留 `A-Za-z0-9._+-`，其余字符换成 `-` | 见下 |
| `RUN_TESTS=1` | 打包前运行 `go vet ./...` 与 `go test -count=1 ./...` | 不运行 |
| `MAKE_RUN=1` | 额外生成 `.run` | 不生成 |
| `SKIP_BUILD=1` | 不重新编译，复用 `dist/.build-linux-<arch>/` 里上一次编译的 `dbtool` 与 `dbt-admin`（其中的版本号是上次的值）；没有可复用的二进制时仍会重新编译并给出警告 | 重新编译 |

### 3.2 版本号

按下面的顺序取第一个非空的：

1. 脚本的位置参数（`bash deploy/package-centos7.sh 2.0`）；
2. 环境变量 `VERSION`（`make dist VERSION=…` 传的就是它）；
3. `deploy/RELEASE_VERSION`：格式必须是“主版本.两位小数”（如 `1.38`），**打包成功后自动加 0.01 写回**（`1.38` → `1.39`，`1.99` → `2.00`；失败不加）；
4. `git describe --tags --always --dirty`；
5. `0.0.0-<日期>`（没有提交 / 没有 git 时）。

显式指定（第 1、2 项）时不读也不改 `deploy/RELEASE_VERSION`。版本号经 `-ldflags "-X main.version=<版本>"` 注入 `dbtool` 与 `dbt-admin`（`dbtool -version`、`dbt-admin version` 可打印），并写入包内 `VERSION`。

### 3.3 输出（`dist/`，已在 `.gitignore` 里）

| 文件 | 说明 |
|---|---|
| `dbtool-<版本>-linux-<arch>.tar.gz` | 发布包（部署用）。解压得到 `dbtool/`，运行 `./install.sh` |
| `dbtool-<版本>-linux-<arch>.run` | **默认不生成**；`MAKE_RUN=1` 才额外生成的自解压单文件（同一份载荷）。`--info` / `--check` / `--extract DIR`；其余参数原样传给包内 `install.sh` |
| `dbtool-<版本>-linux-<arch>.sha256` | `tar.gz`（有 `.run` 时还有 `.run`）的 sha256 |

产物文件名都带版本号，历史版本保留在 `dist/`。发布包文件名统一用 `dbtool`（不带 `-`）；包内目录 `dbtool/`、二进制 `dbtool`、安装目录 `/opt/dbtool` 不变。

## 4. 打包流程

| 步骤 | 做什么 | 失败时 |
|---|---|---|
| 0 检查 | 必要文件存在（`cmd/server/main.go`、`cmd/dbt-admin/main.go`、`deploy/install.sh`、`deploy/run-header.sh`、`deploy/bootstrap/001`、`002`、`deploy/sql/*.sql`、`configs/` 下三个配置示例、`docs/{deployment,user-guide,ops-manual}.md`）；`tar` 是 GNU tar；`go` 存在（`SKIP_BUILD=1` 例外）；`install.sh` / `run-header.sh` 用 `bash -n` 检查语法；这几个脚本（含 `package-centos7.sh`）不能含 CRLF（CentOS 上会报 `bad interpreter`）；可选 `RUN_TESTS=1` | 中止 |
| 1 编译 | `GOOS=linux GOARCH=<arch> CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=<版本>"`，编译 `dbtool`（`./cmd/server`）与 `dbt-admin`（`./cmd/dbt-admin`）两个二进制；检查 ELF 魔数 | 中止 |
| 2 复制 | `install.sh`、`sql/*.sql`、`bootstrap/*.sql`、三个配置示例、`docs/`（见第 5 节）；生成发布包版 `Dockerfile` 与 `VERSION` | 中止 |
| 3 自检 | 关键文件齐全（含 `docs/release-notes.md`）；没有 `*.pem` / `*.key` / `.env*` / `*.log` / `dbtool.toml` / `.install-state` 混进包；`bash -n install.sh`；生成 `SHA256SUMS`（包内每个文件的 sha256） | 中止 |
| 4 打 tar.gz | 属主 0:0；`dbtool` / `dbt-admin` / `install.sh` 固定 0755，其余 0644 / 目录 0755（Windows 的 Git Bash 里 `chmod` 对 Linux 二进制不生效，所以可执行文件单独追加并指定模式）；`gzip -9 -n` | 中止 |
| 5 sha256（与可选的 `.run`） | 记录 tar.gz 的 sha256；`MAKE_RUN=1` 时先用 `deploy/run-header.sh`（占位符 `@VERSION@` 等替换）+ tar.gz 拼成 `.run`，两个文件一起记录 | 中止 |
| 6 验收 | 解压 tar.gz 重新校验 `SHA256SUMS`；检查可执行文件的模式与属主；两个二进制是 ELF，`dbtool` 的架构与 `ARCH` 一致（有 `file` 命令时）；`sha256sum -c`（`MAKE_RUN=1` 时还有 `.run --check`） | 中止 |

任何一项失败都不会留下“看起来成功”的产物。

## 5. 发布包内容

解压后目录名固定为 `dbtool/`：

| 文件 / 目录 | 来源 | 说明 |
|---|---|---|
| `dbtool` | `cmd/server` | 主程序（含前端模板与静态文件） |
| `dbt-admin` | `cmd/dbt-admin` | 元数据库初始化 / 管理员 / 密钥 / 系统设置工具（`bootstrap`、`status`、`admin`、`gen-secrets` …） |
| `install.sh` | `deploy/install.sh` | 一键安装 / 升级 / 回滚 / 卸载 / 体检 / 初始化库 / 改配置 |
| `bootstrap/` | `deploy/bootstrap/*.sql` | 48 张基础表与基础数据（001、002） |
| `sql/` | `deploy/sql/*.sql` | 扩展表脚本（35 个，幂等） |
| `env.example`、`dbtool.example.toml` | `configs/` | 配置示例（取值与代码的差异见 [../docs/ops-manual.md](../docs/ops-manual.md) 5.4） |
| `plugins/soar`、`plugins/soar.yaml` | 仓库根目录 `plugins/` | **可选**：SOAR 程序（Linux 静态可执行文件，打包时核对 ELF 与 `ARCH` 一致）与配置；没有 `plugins/soar` 就不带。`install.sh` 每次 install / upgrade 部署到 `<安装目录>/plugins/` 并把系统设置指向它 |
| `goinception.config.toml` | `configs/` | goInception 初始配置，`install.sh` 部署到 `<安装目录>/goinception/config.toml`（已存在不覆盖） |
| `docs/` | `docs/` | `deployment.md`、`user-guide.md`、`ops-manual.md`（必带）；存在时再带 `index.md`、`bootstrap-design.md`、`admin-cli.md`、`one-click-deploy.md`、`charset-policy.md`、`release-notes.md`、`settings-design.md`、`settings-reference.md` |
| `Dockerfile` | 脚本内置 | **发布包版**：`COPY` 包内 `dbtool`，不需要源码与 Go 环境（CentOS 上用 `install.sh`，不需要它）。仓库里的 `deploy/Dockerfile` 是源码多阶段构建版，两者不要混用 |
| `VERSION` | 打包时生成 | `version= / build_time= / commit= / arch=` |
| `SHA256SUMS` | 打包时生成 | 包内每个文件的 sha256，`.run --check` 与 `install.sh` 使用 |

体积参考（取自本机 1.58 的 amd64 构建产物 `dist/`，随版本变化）：`dbtool` 74,150,072 字节（约 70.7 MiB，含多引擎驱动）、`dbt-admin` 7,155,896 字节（约 6.8 MiB）、`tar.gz` 30,540,250 字节（约 29.1 MiB，含 SOAR 等外部工具）。

> **注意**：仓库的 `Makefile` 里 `build` 目标只编译 `bin/dbtool`，不含 `dbt-admin`；要得到完整的两个二进制请用 `dist`（或 `install.sh --build`）。

## 6. 在 CentOS 上安装与升级

`tar.gz` 与 `.sha256` 放在同一目录，先校验传输完整：

```bash
sha256sum -c dbtool-<版本>-linux-amd64.sha256
```

解压并进入包目录（之后都在 `dbtool/` 目录里运行 `install.sh`）：

```bash
tar xzf dbtool-<版本>-linux-amd64.tar.gz && cd dbtool
```

只读体检（系统 / systemd / 端口 / 磁盘 / 依赖命令 / 连库）：

```bash
sudo ./install.sh doctor
```

全新安装（管理员密码用环境变量 `DBT_ADMIN_PASSWORD`；完整步骤见 [../docs/ops-manual.md](../docs/ops-manual.md)）：

```bash
sudo --preserve-env=DBT_ADMIN_PASSWORD ./install.sh install --gen-secrets --bootstrap --admin-user admin --database-url 'mysql://<用户>:<URL 编码的密码>@<主机>:3306/db_tool' --public-url https://dba.example.com --open-firewall
```

升级（保留配置与数据，健康检查失败自动回滚）：

```bash
sudo ./install.sh upgrade
```

首次部署前请先读 `docs/release-notes.md` 的“升级前必读”（全部表加 `dbt_` 前缀、已有配置时忽略配置类选项、卸载会删除脚本创建的运行用户等）。安装 / 升级 / 回滚 / 卸载的完整选项见 [../docs/one-click-deploy.md](../docs/one-click-deploy.md) 与 `./install.sh --help`。

## 7. 发布前自查清单

1. `RUN_TESTS=1 bash deploy/package-centos7.sh <版本号>`（或先 `go vet ./... && go test -count=1 ./...`）；
2. 改过 `deploy/install.sh` 的话：`bash scripts/install-test/run.sh`（假命令 + 临时目录；耗时见 [../docs/one-click-deploy.md](../docs/one-click-deploy.md) 第 9 节）；
3. 检查输出的 sha256，把 `.tar.gz` 与 `.sha256` 一起保存 / 上传；
4. 在**与生产同版本的 CentOS** 上先 `doctor`，再在演练库完整走一遍“备份 → 建库 → `install --bootstrap` → 体检 → 抽样验证”（抽样验证见 [../docs/ops-manual.md](../docs/ops-manual.md) 8.4）；
5. 升级后 `./install.sh status` 确认版本，浏览器 Ctrl+F5 后检查登录与工单流程。

## 8. 常见问题与排障

| 现象 | 原因 | 处理 |
|---|---|---|
| `go: invalid GOTOOLCHAIN` | 工具链设置不合法 | 脚本已设置 `GOTOOLCHAIN=local`；手动 `go build` 时也要加；先检查 `go env GOTOOLCHAIN` |
| “需要 GNU tar” | 脚本用了 `--owner` / `--mode`，macOS 自带的 bsdtar 不行 | `brew install gnu-tar`，并把 `gtar` 所在目录放到 `PATH` 最前面（或建 `tar` 别名到 gtar 的软链接） |
| “含有 CRLF 换行” | Windows 上检出 / 编辑后脚本变成了 CRLF，CentOS 上会 `bad interpreter` | `sed -i 's/\r$//' deploy/install.sh`；仓库的 `.gitattributes` 已规定 `*.sh` 为 LF |
| “dbtool 架构与 ARCH 不一致” | 用了错误的 `ARCH` | 按服务器 `uname -m`（`x86_64` / `aarch64`）选择 `ARCH` |
| “不是 Linux 可执行文件（ELF）” | 在 Windows / macOS 上编译时 `GOOS` 被覆盖 | 检查 `go env` 与 `GOOS`；脚本编译时固定 `GOOS=linux` |
| “发布包缺少：docs/release-notes.md” | 步骤 3 把 `release-notes.md` 当作必需文件检查 | 确认 `docs/release-notes.md` 存在 |
| “deploy/RELEASE_VERSION 的内容必须是‘主版本.两位小数’” | 该文件内容不是 `1.38` 这种格式 | 修正文件内容，或显式指定版本号 |

清理：

```bash
rm -rf dist/dbtool dist/.build-linux-*
```

（保留 `.tar.gz`、`.sha256`，以及 `MAKE_RUN=1` 生成过的 `.run`。）

## 9. 验证状态与已知限制

| 项 | 状态 | 说明 |
|---|---|---|
| 打包脚本在 Windows（Git Bash）上生成 amd64 包并通过自检 / 验收 | 已验证 | 本机构建过 1.58（`dist/dbtool-1.58-linux-amd64.tar.gz`，解压后重新校验 `SHA256SUMS` 通过） |
| 包在真实 CentOS 上安装运行 | 未验证 | 本机是 Windows，没有 Linux 环境；`install.sh` 的行为由 `scripts/install-test/run.sh`（假命令）覆盖，真实 systemd / firewalld / SELinux 需要在演练机上走一遍 |
| 包里的 `Dockerfile` | 未验证 | 没有 `docker build` 过 |
| `ARCH=arm64` | 部分验证 | 只验证了交叉编译与 ELF 架构检查，没有在 arm64 机器上运行过 |
| `MAKE_RUN=1` 生成的 `.run` | 部分验证 | 打包脚本里有 `.run --check` 验收；`install.sh` 测试里覆盖 `--info / --check / --extract` 与参数透传（假命令环境） |
