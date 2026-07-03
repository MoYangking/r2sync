# r2sync

r2sync 是一个用 Go 编写的独立 Cloudflare R2 文件同步服务。它把项目里的真实文件路径同步到 R2，例如 SQLite 数据库、配置文件、运行状态文件，并提供管理 UI/API 与启动门禁，让业务程序可以先完成云端恢复或初始化上传，再正式启动。

它的目标不是做 Git 历史备份，也不是做网盘式双向同步；它的目标是给其它项目提供一个小而明确的“当前状态文件同步层”。

English documentation is available in [English](#english).

## 它解决什么问题

很多容器、托管平台和轻量应用的本地磁盘不是长期可靠存储。程序重启、重建、迁移实例时，`data/*.db` 这类状态文件可能丢失，或者业务程序比恢复逻辑先启动，导致空数据库覆盖旧数据。

r2sync 解决的是这类问题：

- 在业务程序启动前，先从 R2 拉回已有状态文件。
- 如果 R2 还没有初始化，则把本地文件上传到 R2 作为初始版本。
- 程序运行期间按周期同步已变化的文件。
- 未变化的文件不上传，减少 R2 请求和写入开销。
- R2 里只保留每个目标文件的当前对象，不主动创建历史版本。
- 通过 Web UI/API 修改 bucket、token、同步间隔、目标文件和成本保护配置。

## 主要用途

r2sync 适合放在其它项目旁边作为一个可复用的同步服务：

- 容器或云平台中的轻量持久化：应用数据在本地，R2 作为恢复点。
- SQLite、BoltDB、小型 JSON/YAML 配置、运行状态文件的当前版本同步。
- ModelScope Studio、Docker、VPS、单机部署中，需要先恢复数据再启动业务服务的场景。
- 替代依赖 GitHub 仓库或 GitHub Releases 的文件同步逻辑。
- 给多个项目复用同一套“R2 配置 + 启动门禁 + 管理 UI”能力。

默认目标是 `data/sophnet.db`，这是为了兼容最初的 SophNet 使用场景；r2sync 本身不绑定 SophNet，任何项目都可以通过 `R2SYNC_TARGETS` 或管理 UI 修改目标文件。

## 不适合什么场景

r2sync 有意保持简单，不适合这些用途：

- 多设备实时双向同步。
- 多用户同时编辑同一个文件并自动合并冲突。
- 带版本保留、审计、回滚策略的完整备份系统。
- 大规模目录镜像、对象湖、CDN 资源分发。
- 数据库主从复制或高频事务复制。

如果你需要历史版本，请使用专门的备份系统，或开启 R2/外部系统自己的版本管理策略；r2sync 默认不会做这件事。

## 核心能力

- `r2sync serve`：启动同步守护进程和管理 UI/API。
- `r2sync sync`：执行一次前台手动同步。
- `r2sync run -- <command>`：先完成初始同步，再启动业务命令。
- `r2sync config check`：校验本地配置、Cloudflare token、R2 bucket 和 S3 访问。
- 默认每 `5h` 执行一次周期同步。
- 默认监听 `0.0.0.0:5321`。
- 默认状态目录是当前项目下的 `.r2sync`。
- 默认存储保护上限是 `4 GiB`，可以手动调整。
- 默认只使用 R2 Standard storage，不主动启用 public bucket、`r2.dev`、Infrequent Access、R2 Data Catalog、R2 SQL、Sippy 或 Super Slurper。

## 同步语义

r2sync 同步的是显式配置的文件，不会扫描整个目录。每个 target 会映射为一个确定的 R2 object key：

```text
<object_prefix>/<normalized-target-path>
```

例如：

```text
target: data/sophnet.db
object_prefix: prod
object key: prod/data/sophnet.db
```

### 初始同步

初始同步用于启动门禁，也是最重要的安全边界：

| 状态 | 行为 |
| --- | --- |
| R2 有对象，本地没有文件 | 从 R2 恢复到本地 |
| R2 有对象，本地也有文件且内容不同 | 先把本地文件复制到 `.r2sync/quarantine/...`，再用 R2 版本覆盖本地 |
| R2 没有对象，本地有文件 | 上传本地文件，初始化 R2 |
| R2 和本地都没有 | 创建父目录，标记为 missing |

也就是说：R2 已初始化后，默认以 R2 远端为准；只有 R2 远端还没有对象时，本地文件才作为初始来源。

### 周期同步和手动同步

周期同步的目标是少请求、少上传：

1. 如果本地文件的 size 和 mtime 没变，直接跳过，不计算 hash，也不访问 R2。
2. 如果 size 或 mtime 变了，计算本地 SHA-256。
3. 如果 SHA-256 没变，只刷新本地 metadata，不上传。
4. 如果 SHA-256 变了，检查成本保护，然后覆盖同一个 R2 object key。
5. 如果本地文件被删，而 R2 里有对象，默认从 R2 恢复，不会删除 R2。

删除远端对象只能通过明确确认的 UI/API 操作执行。本地删除文件不会被解释为“也删除云端唯一副本”。

## 安装

从 GitHub Releases 下载对应平台的产物：

```text
https://github.com/MoYangking/r2sync/releases
```

资产命名格式：

```text
r2sync_<tag>_linux_amd64.tar.gz
r2sync_<tag>_linux_arm64.tar.gz
r2sync_<tag>_darwin_amd64.tar.gz
r2sync_<tag>_darwin_arm64.tar.gz
r2sync_<tag>_windows_amd64.zip
r2sync_<tag>_windows_arm64.zip
SHA256SUMS
```

示例：

```powershell
# Windows: 下载 zip 后解压，把 r2sync.exe 放到 PATH 或项目目录
r2sync.exe --help
```

也可以从源码构建：

```powershell
go build -o dist\r2sync.exe ./cmd/r2sync
```

Linux 构建：

```bash
go build -o dist/r2sync ./cmd/r2sync
```

## 快速开始

启动管理 UI：

```powershell
r2sync serve
```

首次启动时，如果没有设置 `R2SYNC_ADMIN_PASSWORD`，r2sync 会生成一个初始管理密码，并且只在启动日志里打印一次。程序只保存密码哈希，不保存明文管理密码。

打开：

```text
http://127.0.0.1:5321
```

在 UI 里填写：

- R2 bucket 名称。
- Cloudflare R2 API token。
- Cloudflare account id，可选但推荐填写。
- 需要同步的 target 文件列表。
- 同步间隔和存储上限。

也可以用环境变量启动：

```powershell
$env:R2SYNC_BUCKET="my-r2-bucket"
$env:R2SYNC_TOKEN="cloudflare_api_token"
$env:R2SYNC_ACCOUNT_ID="cloudflare_account_id"
$env:R2SYNC_TARGETS="data/sophnet.db"
$env:R2SYNC_ADMIN_PASSWORD="change-me"
r2sync serve
```

校验配置：

```powershell
r2sync config check
```

手动同步一次：

```powershell
r2sync sync
```

## 启动门禁

如果你的业务程序必须等数据库或状态文件恢复完成后才能启动，请使用：

```powershell
r2sync run -- your-program --arg value
```

`run --` 会按顺序执行：

1. 加载配置和本地状态。
2. 初始化管理密码和会话密钥。
3. 校验 Cloudflare/R2 访问。
4. 如果 bucket 不存在且 token 有权限，则自动创建 bucket。
5. 执行初始同步。
6. 初始同步成功后才启动业务命令。
7. 业务命令运行期间继续保留周期同步和管理 UI。
8. 业务命令退出时，r2sync 返回相同退出码。

如果初始同步失败，业务命令不会启动。这能避免“空数据库先启动，然后覆盖远端旧数据”的问题。

Docker 中常见写法：

```bash
r2sync run -- ./my-service
```

## 配置

配置来源：

1. 默认值。
2. `.r2sync/config.json` 或 `R2SYNC_CONFIG` 指定的文件。
3. 环境变量，优先级最高。
4. Web UI 保存后的配置文件。

常用环境变量：

| 环境变量 | 默认值 | 说明 |
| --- | --- | --- |
| `R2SYNC_CONFIG` | `<state_dir>/config.json` | 配置文件路径 |
| `R2SYNC_BASE_DIR` | 当前工作目录 | 相对 target 的基准目录 |
| `R2SYNC_STATE_DIR` | `<base_dir>/.r2sync` | 本地状态目录 |
| `R2SYNC_LISTEN_ADDR` | `0.0.0.0:5321` | Web UI/API 监听地址 |
| `R2SYNC_BUCKET` / `R2SYNC_BUCKET_NAME` | 空 | R2 bucket 名称 |
| `R2SYNC_TOKEN` / `R2SYNC_CLOUDFLARE_TOKEN` | 空 | Cloudflare R2 API token |
| `R2SYNC_ACCOUNT_ID` | 自动发现 | Cloudflare account id；多账号时建议显式填写 |
| `R2SYNC_OBJECT_PREFIX` | 空 | R2 object key 前缀，用于同 bucket 隔离项目 |
| `R2SYNC_TARGETS` | `data/sophnet.db` | 逗号、分号、空格或换行分隔的目标文件 |
| `R2SYNC_EXCLUDES` | `.r2sync` 等系统项 | 保留/兼容字段；当前同步以显式 targets 为准 |
| `R2SYNC_SYNC_INTERVAL` | `5h` | 周期同步间隔，例如 `30m`、`5h` |
| `R2SYNC_STORAGE_CAP_BYTES` | `4294967296` | 默认 4 GiB 存储保护上限 |
| `R2SYNC_STRICT_VERIFY` | `false` | 每次同步强制检查远端 metadata |
| `R2SYNC_DISABLE_COST_GUARDS` | `false` | 关闭成本保护，不建议默认开启 |
| `R2SYNC_ADMIN_PASSWORD` | 自动生成 | 首次启动管理密码 |
| `R2SYNC_ADMIN_PASSWORD_HASH` | 空 | 已哈希的管理密码，适合高级部署 |

JSON 配置示例：

```json
{
  "base_dir": "/app",
  "state_dir": "/app/.r2sync",
  "listen_addr": "0.0.0.0:5321",
  "bucket_name": "my-r2-bucket",
  "account_id": "cloudflare_account_id",
  "cloudflare_token": "cloudflare_api_token",
  "object_prefix": "prod",
  "sync_interval": "5h",
  "targets": ["data/sophnet.db"],
  "storage_cap_bytes": 4294967296,
  "strict_verify": false,
  "disable_cost_guards": false
}
```

注意：Cloudflare token 如果通过 UI 或配置文件保存，会以明文保存在本地配置文件里。r2sync 不会在 API 响应里返回明文 token，也不会主动打印 token，但你仍然需要保护 `.r2sync/config.json` 和运行环境变量。

## Cloudflare R2 token

r2sync 使用两类 Cloudflare/R2 接口：

- Cloudflare REST API：账号发现、账号级 token 校验、bucket 创建。
- R2 S3-compatible API：对象 Head、Get、Put、Delete，以及大文件 multipart upload。

token 校验只使用账号级接口：

```text
GET /accounts/{account_id}/tokens/verify
```

如果没有设置 `R2SYNC_ACCOUNT_ID`，r2sync 会尝试发现 token 可访问的账号。只有一个账号时可以自动使用；没有账号或多个账号时，会要求你显式设置 `R2SYNC_ACCOUNT_ID`。

对于 Cloudflare API 创建的 R2 token，r2sync 会按 Cloudflare 文档派生 S3 凭据：

- S3 Access Key ID：token id。
- S3 Secret Access Key：token value 的 SHA-256。
- S3 endpoint：`https://<account_id>.r2.cloudflarestorage.com`。
- region：`auto`。

token 至少需要能访问目标账号的 R2，并且如果希望 r2sync 自动创建 bucket，需要有创建 bucket 的权限。

## 成本保护和免费额度

R2 不是“只能存 10GB”的硬限制。Cloudflare 按用量计费，免费层通常按 GB-month 和请求次数计算；超出免费层后可能产生费用。具体价格和额度以 Cloudflare 官方页面为准：

- R2 pricing: <https://developers.cloudflare.com/r2/pricing/>
- R2 limits: <https://developers.cloudflare.com/r2/platform/limits/>

r2sync 的默认策略是保守避免意外收费：

- 默认本地存储保护上限：`4 GiB`。
- 达到配置上限后阻止继续上传。
- Class A / Class B 请求计数按本地估算记录。
- 请求量达到免费额度估算的 80% 时预警。
- 请求量达到免费额度估算的 95% 时阻止继续执行相关操作，除非用户调整或关闭保护。
- 默认使用 Standard storage。
- 默认不启用 public bucket、`r2.dev`、Infrequent Access、R2 Data Catalog、R2 SQL、Sippy、Super Slurper。

`10 GB-month` 可以理解为“一个月内平均 10GB 左右的 Standard 存储用量”，不是 bucket 的硬容量上限。比如持续存 4GB 一整月通常低于 10 GB-month；短时间超过某个值也会按 Cloudflare 的计费口径折算进当月用量。r2sync 默认 4 GiB cap 是项目自己的软保护，不是 Cloudflare 强制限制。

请求计数是本地估算，不是 Cloudflare 账单的事实来源。它的用途是提前拦住错误循环、过低同步间隔或异常 UI/API 轮询。

## 管理 UI/API

默认地址：

```text
http://0.0.0.0:5321
```

本机访问通常使用：

```text
http://127.0.0.1:5321
```

UI/API 默认需要登录。登录成功后使用 HTTP-only cookie 保存会话。

主要 API：

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| `GET` | `/api/health` | 进程健康检查 |
| `GET` | `/api/ready` | 初始同步是否完成 |
| `POST` | `/api/login` | 管理密码登录 |
| `POST` | `/api/logout` | 退出登录 |
| `GET` | `/api/status` | 状态、目标文件、计数器、事件 |
| `GET` | `/api/config` | 读取脱敏配置 |
| `PUT` | `/api/config` | 更新配置 |
| `GET` | `/api/targets` | 读取 targets/excludes |
| `PUT` | `/api/targets` | 更新 targets/excludes |
| `POST` | `/api/sync` | 立即同步 |
| `POST` | `/api/verify` | 严格校验同步 |
| `POST` | `/api/objects/delete` | 删除远端对象，需要 `DELETE` 确认 |

如果管理 UI 暴露到公网，请放在反向代理、内网或访问控制之后，并设置强密码。

## 常见部署方式

### 作为独立守护进程

```bash
export R2SYNC_BUCKET="my-r2-bucket"
export R2SYNC_TOKEN="cloudflare_api_token"
export R2SYNC_ACCOUNT_ID="cloudflare_account_id"
export R2SYNC_TARGETS="data/app.db"
export R2SYNC_ADMIN_PASSWORD="change-me"

r2sync serve
```

### 作为业务程序启动门禁

```bash
export R2SYNC_BUCKET="my-r2-bucket"
export R2SYNC_TOKEN="cloudflare_api_token"
export R2SYNC_ACCOUNT_ID="cloudflare_account_id"
export R2SYNC_TARGETS="data/app.db"

r2sync run -- ./app
```

### 多项目共享同一个 bucket

用 `R2SYNC_OBJECT_PREFIX` 隔离 object key：

```bash
export R2SYNC_BUCKET="shared-state"
export R2SYNC_OBJECT_PREFIX="project-a/prod"
export R2SYNC_TARGETS="data/app.db config/runtime.json"
r2sync run -- ./app
```

这样会写入类似：

```text
project-a/prod/data/app.db
project-a/prod/config/runtime.json
```

## 故障排查

### 页面需要密码，但不知道密码

如果没有设置 `R2SYNC_ADMIN_PASSWORD`，首次启动日志会打印一次自动生成的密码。只会打印一次。找不到时，可以停止程序，删除或更换状态中的密码哈希，或者用新的 state dir 重新初始化。

生产环境建议显式设置：

```bash
R2SYNC_ADMIN_PASSWORD="your-strong-password"
```

### `attempt to write a readonly database`

这通常不是 R2 把文件变成只读，而是 r2sync 恢复文件时使用的系统用户和业务程序写数据库的系统用户不一致。

解决方式：

- 让 r2sync 和业务程序用同一个 UID/GID 运行。
- 或在启动脚本里对恢复后的数据目录执行 `chown`。
- Docker 中不要让 r2sync 用 root 恢复文件，再让非 root 业务进程写同一个 SQLite 文件。

### 本地数据库变成 0B

如果 R2 远端还没有初始化，而业务程序先创建了一个空数据库，r2sync 会把这个空文件当作本地初始版本上传。应使用 `r2sync run -- <command>` 保证初始同步先完成，再启动业务程序。

如果 R2 已经有对象，初始同步会以 R2 为准；本地 0B 文件会被复制到 quarantine，然后恢复 R2 版本。

### 看到 `0001-01-01T00:00:00Z`

这是旧版本中零值时间直接展示的问题。请升级到 `v0.1.1` 或更高版本；新版本会隐藏未设置的时间字段。

### bucket 创建失败

检查：

- bucket 名称是否符合 Cloudflare R2 规则。
- token 是否属于正确账号。
- `R2SYNC_ACCOUNT_ID` 是否正确。
- token 是否有创建 bucket 或访问目标 bucket 的权限。

如果不想给自动创建权限，可以先在 Cloudflare 控制台手动创建 bucket，再给 token 访问该 bucket 的权限。

### 同步间隔可以调多低

技术上 `R2SYNC_SYNC_INTERVAL` 只要求是 Go duration 且大于 0，例如 `5m`、`30m`、`5h`。但越低越容易因为频繁检查、错误循环或文件频繁变化而增加请求量。

默认 `5h` 是保守值。对于稳定的小文件，如果没有开启 strict verify 且文件 metadata 没变，周期同步会直接跳过，不访问 R2；但如果文件经常变，低间隔会增加上传和 Class A 请求。

## 开发

```bash
go test ./...
go vet ./...
go run ./cmd/r2sync --help
go run ./cmd/r2sync serve
```

Live R2 测试需要真实 Cloudflare 凭据，会产生少量请求和存储用量。只应使用可轮换的测试 token 和测试 bucket。

## 发布

GitHub Actions workflow 位于：

```text
.github/workflows/release.yml
```

它会：

- 在 pull request 和 push 到 `main`/`master` 时运行 `go test ./...` 和 `go vet ./...`。
- 为 Linux、macOS、Windows 构建 `amd64` 和 `arm64` 二进制。
- 生成 `SHA256SUMS`。
- 当推送 `v*` tag 或手动运行 workflow 时发布 GitHub Release。

创建 release：

```bash
git tag v0.1.1
git push origin v0.1.1
```

## 官方参考

- Cloudflare R2 pricing: <https://developers.cloudflare.com/r2/pricing/>
- Cloudflare R2 limits: <https://developers.cloudflare.com/r2/platform/limits/>
- Cloudflare R2 API tokens: <https://developers.cloudflare.com/r2/api/tokens/>
- Cloudflare R2 S3 API: <https://developers.cloudflare.com/r2/api/s3/api/>
- Cloudflare R2 S3 compatibility: <https://developers.cloudflare.com/r2/api/s3/>

---

## English

r2sync is a standalone Cloudflare R2 file synchronization service written in Go. It syncs real local files, such as SQLite databases, configuration files, and runtime state files, to Cloudflare R2. It also provides a protected management UI/API and a startup gate so your application can restore or initialize its files before it starts.

r2sync is not a Git history backup tool and not a Dropbox-style bidirectional sync client. It is a reusable "current state file sync layer" for small applications and container deployments.

## What It Does

r2sync is useful when an application stores important state on local disk, but the runtime environment is disposable or frequently rebuilt.

It can:

- Restore configured files from R2 before your application starts.
- Upload local files to initialize R2 when the remote object does not exist yet.
- Run scheduled syncs while the application is running.
- Skip unchanged files to reduce R2 requests and uploads.
- Store only the current object for each target key.
- Expose a web UI/API for bucket, token, targets, interval, and cost guard configuration.
- Create the R2 bucket automatically when the token has permission.

The default target is `data/sophnet.db` for compatibility with the original SophNet use case. r2sync itself is project-neutral and can be reused by any project.

## Good Use Cases

- Lightweight persistence for Docker, VPS, ModelScope Studio, and similar deployments.
- SQLite databases, small state files, and runtime configuration files.
- Replacing GitHub/GitHub Releases based sync flows with direct R2 object storage.
- Starting an application only after required files have been restored.
- Sharing one reusable R2 sync component across multiple projects.

## Not A Good Fit

r2sync is intentionally narrow. It is not designed for:

- Realtime multi-device bidirectional sync.
- Automatic merge conflict resolution.
- Historical backup retention or audit trails.
- Large directory mirroring or object-lake ingestion.
- Database replication.

## Commands

```bash
r2sync serve
r2sync sync
r2sync run -- <command> [args...]
r2sync config check
```

- `serve`: run the sync daemon and management UI/API.
- `sync`: run one foreground manual sync.
- `run --`: run initial sync first, then start the child command.
- `config check`: validate config, Cloudflare token, bucket setup, and R2 S3 access.

## Sync Rules

Each configured target maps to one deterministic R2 object key:

```text
<object_prefix>/<normalized-target-path>
```

r2sync overwrites that same object key on update. It does not create timestamped history objects.

Initial sync:

| State | Behavior |
| --- | --- |
| Remote exists, local missing | Restore from R2 |
| Remote exists, local differs | Copy local file to `.r2sync/quarantine/...`, then restore R2 version |
| Remote missing, local exists | Upload local file to initialize R2 |
| Both missing | Create parent directories and mark target missing |

After R2 has been initialized, R2 is authoritative during initial sync. Local wins only when the remote object does not exist.

Scheduled/manual sync:

1. If local size and mtime match local state, skip without hashing or calling R2.
2. If local metadata changed, calculate SHA-256.
3. If SHA-256 is unchanged, refresh local metadata only.
4. If SHA-256 changed, run cost guards and upload to the same R2 object key.
5. Local deletion restores from R2 when the remote object exists.

Local deletion never deletes the R2 object. Remote deletion requires an explicit confirmed UI/API action.

## Install

Download a release asset from:

```text
https://github.com/MoYangking/r2sync/releases
```

Asset names:

```text
r2sync_<tag>_linux_amd64.tar.gz
r2sync_<tag>_linux_arm64.tar.gz
r2sync_<tag>_darwin_amd64.tar.gz
r2sync_<tag>_darwin_arm64.tar.gz
r2sync_<tag>_windows_amd64.zip
r2sync_<tag>_windows_arm64.zip
SHA256SUMS
```

Build from source:

```bash
go build -o dist/r2sync ./cmd/r2sync
```

Windows:

```powershell
go build -o dist\r2sync.exe ./cmd/r2sync
```

## Quick Start

Run the daemon and management UI:

```bash
r2sync serve
```

If `R2SYNC_ADMIN_PASSWORD` is not set on first start, r2sync generates an initial password and prints it once in the startup log. Only the password hash is stored.

Open:

```text
http://127.0.0.1:5321
```

Configure the bucket name, Cloudflare token, optional account id, targets, sync interval, and storage cap in the UI.

Environment example:

```bash
export R2SYNC_BUCKET="my-r2-bucket"
export R2SYNC_TOKEN="cloudflare_api_token"
export R2SYNC_ACCOUNT_ID="cloudflare_account_id"
export R2SYNC_TARGETS="data/app.db"
export R2SYNC_ADMIN_PASSWORD="change-me"
r2sync serve
```

Validate:

```bash
r2sync config check
```

Manual sync:

```bash
r2sync sync
```

## Startup Gate

Use `run --` when your application must wait for files to be restored or initialized:

```bash
r2sync run -- ./app
```

`run --` loads config, validates R2, creates the bucket when allowed, runs initial sync, starts the child process only after sync succeeds, keeps scheduled sync and the UI running, and exits with the child process exit code.

If initial sync fails, the child process is not started.

## Configuration

Configuration is loaded from defaults, config file, environment variables, and UI updates. Environment variables override file values.

| Variable | Default | Purpose |
| --- | --- | --- |
| `R2SYNC_CONFIG` | `<state_dir>/config.json` | Config file path |
| `R2SYNC_BASE_DIR` | current working directory | Base directory for relative targets |
| `R2SYNC_STATE_DIR` | `<base_dir>/.r2sync` | Local state directory |
| `R2SYNC_LISTEN_ADDR` | `0.0.0.0:5321` | Web UI/API listen address |
| `R2SYNC_BUCKET` / `R2SYNC_BUCKET_NAME` | empty | R2 bucket name |
| `R2SYNC_TOKEN` / `R2SYNC_CLOUDFLARE_TOKEN` | empty | Cloudflare R2 API token |
| `R2SYNC_ACCOUNT_ID` | auto-discovered when possible | Cloudflare account id |
| `R2SYNC_OBJECT_PREFIX` | empty | R2 object key prefix |
| `R2SYNC_TARGETS` | `data/sophnet.db` | Comma/semicolon/space/newline separated target files |
| `R2SYNC_EXCLUDES` | system defaults | Reserved/compatibility field; explicit targets drive syncing |
| `R2SYNC_SYNC_INTERVAL` | `5h` | Scheduled sync interval |
| `R2SYNC_STORAGE_CAP_BYTES` | `4294967296` | Default 4 GiB storage cap |
| `R2SYNC_STRICT_VERIFY` | `false` | Force remote metadata checks during sync |
| `R2SYNC_DISABLE_COST_GUARDS` | `false` | Disable storage/request guards |
| `R2SYNC_ADMIN_PASSWORD` | generated | First-start admin password |
| `R2SYNC_ADMIN_PASSWORD_HASH` | empty | Pre-hashed admin password for advanced deployments |

Secrets submitted through the UI or config file are stored locally in the config file. r2sync masks tokens in API responses and logs, but you should still protect `.r2sync/config.json`.

## Cloudflare R2 Token

r2sync uses:

- Cloudflare REST API for account discovery, account-scoped token verification, and bucket creation.
- R2 S3-compatible API for object Head/Get/Put/Delete and multipart uploads.

Token verification uses:

```text
GET /accounts/{account_id}/tokens/verify
```

If `R2SYNC_ACCOUNT_ID` is omitted, r2sync tries to discover exactly one accessible account. If discovery fails or multiple accounts are available, set `R2SYNC_ACCOUNT_ID` explicitly.

For API-created R2 tokens, r2sync derives S3 credentials as documented by Cloudflare:

- S3 Access Key ID: token id.
- S3 Secret Access Key: SHA-256 of token value.
- Endpoint: `https://<account_id>.r2.cloudflarestorage.com`.
- Region: `auto`.

## Cost Guards

Cloudflare R2 does not behave like a hard 10 GB bucket quota. It is metered by storage duration and request usage; usage beyond the free tier can be billed. Check the official pages for current numbers:

- <https://developers.cloudflare.com/r2/pricing/>
- <https://developers.cloudflare.com/r2/platform/limits/>

r2sync defaults are conservative:

- Standard storage only.
- 4 GiB storage cap.
- Estimated Class A/Class B warnings at 80% of free-tier assumptions.
- Estimated Class A/Class B blocks at 95% unless adjusted or disabled.
- No public bucket access, `r2.dev`, Infrequent Access, R2 Data Catalog, R2 SQL, Sippy, or Super Slurper by default.

The counters are local estimates, not Cloudflare billing truth. They are meant to stop accidental loops or excessive polling early.

## Management API

The UI/API listens on `0.0.0.0:5321` by default and requires login.

| Method | Path | Purpose |
| --- | --- | --- |
| `GET` | `/api/health` | Process health |
| `GET` | `/api/ready` | Initial sync readiness |
| `POST` | `/api/login` | Password login |
| `POST` | `/api/logout` | Logout |
| `GET` | `/api/status` | Status, targets, counters, events |
| `GET` | `/api/config` | Masked config |
| `PUT` | `/api/config` | Update config |
| `GET` | `/api/targets` | Read targets/excludes |
| `PUT` | `/api/targets` | Update targets/excludes |
| `POST` | `/api/sync` | Sync now |
| `POST` | `/api/verify` | Strict verification sync |
| `POST` | `/api/objects/delete` | Confirmed remote delete |

If exposing the UI beyond localhost, put it behind network access control or a reverse proxy and use a strong admin password.

## Troubleshooting

### Unknown admin password

The generated first-start password is printed only once. For production, set:

```bash
R2SYNC_ADMIN_PASSWORD="your-strong-password"
```

### `attempt to write a readonly database`

This is usually a Unix ownership mismatch. Run r2sync and the application with the same UID/GID, or `chown` the data directory after restore.

### Local DB became 0 bytes

If R2 is not initialized yet and the application creates an empty database before r2sync runs, r2sync may upload that empty file as the initial remote object. Use `r2sync run -- <command>` so initial sync happens before the application starts.

### `0001-01-01T00:00:00Z`

This is a zero-time display issue from older versions. Upgrade to `v0.1.1` or later.

## Development

```bash
go test ./...
go vet ./...
go run ./cmd/r2sync --help
go run ./cmd/r2sync serve
```

Live R2 validation requires disposable Cloudflare credentials and may consume a small amount of storage/request usage.
