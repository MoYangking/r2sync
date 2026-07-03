# r2sync

Standalone Cloudflare R2 file sync service written in Go.

r2sync replaces a GitHub-backed file sync workflow with direct Cloudflare R2 object storage. It is designed to be reused by other projects that need a small startup sync gate, scheduled file sync, and a protected management UI.

## English

### Features

- Syncs configured real filesystem paths in place.
- Stores only the current object for each target; no history keys are created.
- Defaults to a `5h` scheduled sync interval.
- Skips unchanged files using local size/mtime state, then SHA-256 only when needed.
- Restores from R2 when a local target is missing.
- Treats R2 as authoritative after remote initialization.
- Starts another program only after initial sync succeeds with `r2sync run -- <command>`.
- Provides a password-protected web UI/API on `0.0.0.0:5321` by default.
- Uses conservative cost guards: Standard storage only, 4 GiB default storage cap, request warning/block thresholds.

### Install

Download a release asset from GitHub Releases:

- `r2sync_<tag>_linux_amd64.tar.gz`
- `r2sync_<tag>_linux_arm64.tar.gz`
- `r2sync_<tag>_darwin_amd64.tar.gz`
- `r2sync_<tag>_darwin_arm64.tar.gz`
- `r2sync_<tag>_windows_amd64.zip`
- `r2sync_<tag>_windows_arm64.zip`

Verify the archive with `SHA256SUMS`, then place the `r2sync` binary on your `PATH`.

Build from source:

```powershell
go build -o dist\r2sync.exe ./cmd/r2sync
```

### Quick Start

Run the daemon and management UI:

```powershell
go run ./cmd/r2sync serve
```

On first start, if `R2SYNC_ADMIN_PASSWORD` is not set, r2sync generates an initial password and prints it once in the startup log. Only the password hash is stored in `.r2sync/state.json`.

Open:

```text
http://127.0.0.1:5321
```

Configure the bucket name and Cloudflare token in the UI, or use environment variables:

```powershell
$env:R2SYNC_BUCKET="my-r2-bucket"
$env:R2SYNC_TOKEN="cloudflare_api_token"
$env:R2SYNC_ACCOUNT_ID="cloudflare_account_id"
$env:R2SYNC_TARGETS="data/sophnet.db"
r2sync serve
```

### Startup Gate

Use `run --` when another program must not start until files have been restored or uploaded:

```powershell
r2sync run -- your-program --arg value
```

`run --` performs these steps:

1. Loads config and state.
2. Validates Cloudflare/R2 access.
3. Creates the bucket if it is missing and the token permits creation.
4. Runs the initial sync.
5. Starts the command only after sync succeeds.
6. Keeps scheduled sync and the management UI running while the command runs.
7. Exits with the child command exit code.

If initial sync fails, the child command is not started.

### Commands

```powershell
r2sync serve
r2sync sync
r2sync run -- <command> [args...]
r2sync config check
```

### Configuration

Configuration is loaded from `.r2sync/config.json` and environment variables. Environment variables override file values.

| Variable | Default | Purpose |
| --- | --- | --- |
| `R2SYNC_CONFIG` | `<state_dir>/config.json` | Config file path |
| `R2SYNC_BASE_DIR` | current directory | Base directory for relative targets |
| `R2SYNC_STATE_DIR` | `<base_dir>/.r2sync` | Local state directory |
| `R2SYNC_LISTEN_ADDR` | `0.0.0.0:5321` | Web UI/API listen address |
| `R2SYNC_BUCKET` | empty | R2 bucket name |
| `R2SYNC_TOKEN` | empty | Cloudflare R2 API token |
| `R2SYNC_ACCOUNT_ID` | auto-discovered when possible | Cloudflare account id used for token verification and R2 access |
| `R2SYNC_TARGETS` | `data/sophnet.db` | Comma/space separated target files |
| `R2SYNC_EXCLUDES` | system defaults | Comma/space separated excludes |
| `R2SYNC_SYNC_INTERVAL` | `5h` | Scheduled sync interval |
| `R2SYNC_STORAGE_CAP_BYTES` | `4294967296` | Default 4 GiB storage cap |
| `R2SYNC_ADMIN_PASSWORD` | generated | First-start admin password |
| `R2SYNC_STRICT_VERIFY` | `false` | Force remote metadata checks during sync |
| `R2SYNC_DISABLE_COST_GUARDS` | `false` | Disable storage/request guards |

### Cloudflare Token

Use a Cloudflare R2 API token with permissions for the target account and bucket. r2sync uses:

- Cloudflare REST API for account discovery, account-scoped token verification, and bucket creation.
- R2 S3-compatible API for object upload/download/head/delete.

Token verification uses `GET /accounts/{account_id}/tokens/verify` only. If `R2SYNC_ACCOUNT_ID` is omitted, r2sync first tries to discover one accessible account. If discovery is unavailable or the token can access multiple accounts, set `R2SYNC_ACCOUNT_ID` explicitly.

For API-created R2 tokens, Cloudflare documents that the S3 Access Key ID is the token id and the S3 Secret Access Key is the SHA-256 hash of the token value. r2sync derives those values at runtime and never returns the token in API responses.

### Sync Semantics

Initial sync:

- Remote exists, local missing: restore local file from R2.
- Remote exists, local differs: copy local file to `.r2sync/quarantine/...`, then restore R2 version.
- Remote missing, local exists: upload local file to initialize R2.
- Both missing: create parent directories and mark target missing.

Periodic/manual sync:

- Local file unchanged by size/mtime: skip without hashing or R2 calls.
- Local size/mtime changed: calculate SHA-256.
- Hash unchanged: update local metadata only.
- Hash changed: overwrite the same R2 object key.

Local deletion never deletes R2. Remote deletion is available only through an explicit confirmed UI/API action.

### Cost Guard Defaults

r2sync defaults to conservative free-tier protection:

- Standard storage only.
- 4 GiB local storage cap.
- Class A and Class B request estimates warn at 80% of free-tier allowance.
- Class A and Class B request estimates block at 95% unless adjusted.
- Public bucket access, `r2.dev`, Infrequent Access lifecycle, R2 Data Catalog, R2 SQL, Sippy, and Super Slurper are not enabled by this project.

The request counters are local estimates, not Cloudflare billing truth. They are intended to catch accidental loops and excessive polling before they become billable.

### GitHub Actions Release

The release workflow lives at `.github/workflows/release.yml`.

It does the following:

- Runs `go test ./...` and `go vet ./...` for pull requests and pushes to `main`/`master`.
- Builds release binaries for Linux, macOS, and Windows on `amd64` and `arm64`.
- Uploads build artifacts to the workflow run.
- Publishes or updates a GitHub Release when a tag like `v0.1.0` is pushed.
- Supports manual release through `workflow_dispatch` with a `tag` input.
- Generates `SHA256SUMS` for all release archives.

Create a release by pushing a version tag:

```powershell
git tag v0.1.0
git push origin v0.1.0
```

Or run the `Release` workflow manually in GitHub Actions and provide a tag such as `v0.1.0`.

### Development

```powershell
go test ./...
go vet ./...
go run ./cmd/r2sync --help
```

Live R2 validation is manual. Do not run live tests unless you intentionally provide disposable Cloudflare credentials and accept the small request/storage usage.

## 中文

### 功能

- 直接同步配置的真实文件路径，不迁移目录，不创建符号链接。
- 每个目标文件在 R2 里只保留当前对象，不主动保存历史版本。
- 默认每 `5h` 周期同步一次。
- 先用本地 size/mtime 判断是否变化，必要时才计算 SHA-256，减少远端请求和上传开销。
- 本地目标文件缺失时，默认从 R2 恢复。
- 远端已经初始化后，默认以 R2 为准。
- 通过 `r2sync run -- <command>` 先完成初始化同步，再启动你的业务程序。
- 默认在 `0.0.0.0:5321` 提供带密码保护的 Web 管理 UI/API。
- 默认启用保守成本保护：Standard 存储、4 GiB 存储上限、请求量预警和阻断阈值。

### 安装

从 GitHub Releases 下载对应平台的产物：

- `r2sync_<tag>_linux_amd64.tar.gz`
- `r2sync_<tag>_linux_arm64.tar.gz`
- `r2sync_<tag>_darwin_amd64.tar.gz`
- `r2sync_<tag>_darwin_arm64.tar.gz`
- `r2sync_<tag>_windows_amd64.zip`
- `r2sync_<tag>_windows_arm64.zip`

可以用 `SHA256SUMS` 校验压缩包，然后把 `r2sync` 可执行文件放到 `PATH` 里。

从源码构建：

```powershell
go build -o dist\r2sync.exe ./cmd/r2sync
```

### 快速开始

启动守护进程和管理 UI：

```powershell
go run ./cmd/r2sync serve
```

首次启动时，如果没有设置 `R2SYNC_ADMIN_PASSWORD`，程序会生成一个初始管理密码，并且只在启动日志里打印一次。本地 `.r2sync/state.json` 只保存密码哈希。

打开：

```text
http://127.0.0.1:5321
```

可以在 UI 里配置 bucket 名称和 Cloudflare token，也可以使用环境变量：

```powershell
$env:R2SYNC_BUCKET="my-r2-bucket"
$env:R2SYNC_TOKEN="cloudflare_api_token"
$env:R2SYNC_ACCOUNT_ID="cloudflare_account_id"
$env:R2SYNC_TARGETS="data/sophnet.db"
r2sync serve
```

### 启动门禁

如果业务程序必须等待文件先从 R2 恢复或上传完成，再启动，请使用：

```powershell
r2sync run -- your-program --arg value
```

`run --` 会按顺序执行：

1. 加载配置和本地状态。
2. 验证 Cloudflare/R2 访问。
3. 如果 bucket 不存在，并且 token 有权限，则自动创建 bucket。
4. 执行初始化同步。
5. 初始化同步成功后才启动业务命令。
6. 业务命令运行期间继续保留周期同步和管理 UI。
7. 业务命令退出时，返回同样的退出码。

如果初始化同步失败，业务命令不会启动。

### 命令

```powershell
r2sync serve
r2sync sync
r2sync run -- <command> [args...]
r2sync config check
```

### 配置

配置从 `.r2sync/config.json` 和环境变量读取。环境变量优先级高于配置文件。

| 环境变量 | 默认值 | 说明 |
| --- | --- | --- |
| `R2SYNC_CONFIG` | `<state_dir>/config.json` | 配置文件路径 |
| `R2SYNC_BASE_DIR` | 当前目录 | 相对目标路径的基准目录 |
| `R2SYNC_STATE_DIR` | `<base_dir>/.r2sync` | 本地状态目录 |
| `R2SYNC_LISTEN_ADDR` | `0.0.0.0:5321` | Web UI/API 监听地址 |
| `R2SYNC_BUCKET` | 空 | R2 bucket 名称 |
| `R2SYNC_TOKEN` | 空 | Cloudflare R2 API token |
| `R2SYNC_ACCOUNT_ID` | 尽量自动发现 | Cloudflare account id，用于 token 校验和 R2 访问 |
| `R2SYNC_TARGETS` | `data/sophnet.db` | 逗号或空格分隔的目标文件 |
| `R2SYNC_EXCLUDES` | 系统默认值 | 逗号或空格分隔的排除项 |
| `R2SYNC_SYNC_INTERVAL` | `5h` | 周期同步间隔 |
| `R2SYNC_STORAGE_CAP_BYTES` | `4294967296` | 默认 4 GiB 存储上限 |
| `R2SYNC_ADMIN_PASSWORD` | 自动生成 | 首次启动管理密码 |
| `R2SYNC_STRICT_VERIFY` | `false` | 同步时强制检查远端 metadata |
| `R2SYNC_DISABLE_COST_GUARDS` | `false` | 关闭存储和请求成本保护 |

### Cloudflare Token

请使用目标账号和 bucket 有权限的 Cloudflare R2 API token。r2sync 会使用：

- Cloudflare REST API 做账号发现、账号级 token 校验、bucket 创建。
- R2 S3 兼容 API 做对象上传、下载、Head、删除。

token 校验只使用 `GET /accounts/{account_id}/tokens/verify`。如果没有设置 `R2SYNC_ACCOUNT_ID`，程序会先尝试自动发现唯一可访问账号；如果无法发现或 token 能访问多个账号，请显式设置 `R2SYNC_ACCOUNT_ID`。

对于 API 创建的 R2 token，Cloudflare 文档说明 S3 Access Key ID 是 token id，S3 Secret Access Key 是 token value 的 SHA-256。r2sync 会运行时派生这些值，并且不会在 API 响应里返回明文 token。

### 同步语义

初始化同步：

- 远端存在、本地缺失：从 R2 恢复本地文件。
- 远端存在、本地也存在但内容不同：先把本地文件复制到 `.r2sync/quarantine/...`，再恢复 R2 版本。
- 远端缺失、本地存在：上传本地文件，用本地文件初始化 R2。
- 两边都缺失：创建父目录并标记为 missing。

周期/手动同步：

- 本地 size/mtime 没变化：跳过，不计算 hash，不请求 R2。
- 本地 size/mtime 变化：计算 SHA-256。
- hash 没变化：只刷新本地 metadata。
- hash 变化：覆盖同一个 R2 object key。

本地删除不会删除 R2。远端删除只能通过明确确认的 UI/API 操作执行。

### 成本保护默认值

r2sync 默认采用保守的免费额度保护策略：

- 只使用 Standard 存储。
- 默认本地存储上限为 4 GiB。
- Class A 和 Class B 请求估算量达到免费额度 80% 时预警。
- Class A 和 Class B 请求估算量达到免费额度 95% 时阻断，除非用户调整配置。
- 默认不启用 public bucket、`r2.dev`、Infrequent Access lifecycle、R2 Data Catalog、R2 SQL、Sippy、Super Slurper。

请求计数是本地估算，不是 Cloudflare 账单事实来源。它的目的主要是提前拦住意外循环或过高频率轮询。

### GitHub Actions 发布

发布工作流位于 `.github/workflows/release.yml`。

它会执行：

- 对 pull request 以及推送到 `main`/`master` 的提交运行 `go test ./...` 和 `go vet ./...`。
- 为 Linux、macOS、Windows 构建 `amd64` 和 `arm64` 二进制。
- 把构建产物上传到 workflow run。
- 当推送 `v0.1.0` 这种 tag 时，自动发布或更新 GitHub Release。
- 支持在 GitHub Actions 页面手动运行 `Release` workflow，并输入 tag。
- 为所有压缩包生成 `SHA256SUMS`。

通过推送 tag 发布：

```powershell
git tag v0.1.0
git push origin v0.1.0
```

也可以在 GitHub Actions 页面手动运行 `Release` workflow，并填写例如 `v0.1.0` 的 tag。

### 开发

```powershell
go test ./...
go vet ./...
go run ./cmd/r2sync --help
```

Live R2 验证是手动操作。只有在你明确提供可轮换的测试凭据，并接受少量请求和存储用量时，才应该运行 live 测试。
