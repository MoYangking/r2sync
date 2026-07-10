# r2sync

r2sync 是一个用 Go 编写的独立**状态文件同步服务**。它把项目里的真实文件路径同步到远端，并提供管理 UI/API 与启动门禁，让业务程序可以先完成云端恢复或初始化上传，再正式启动。

支持两种同步后端（`R2SYNC_SYNC_METHOD`）：

| 方法 | 值 | 说明 |
| --- | --- | --- |
| Cloudflare R2 | `r2`（默认） | 按目标文件映射到 R2 对象，单对象覆盖上传/下载 |
| GitHub 仓库 | `github` | 把目标迁移到本地 git 仓库目录，原路径改软链接，整仓 pull / commit / push |

它的目标不是做网盘式双向同步，也不是完整备份系统；它是给其它项目复用的“当前状态文件同步层”。

English documentation is available in [English](#english).

## 它解决什么问题

很多容器、托管平台和轻量应用的本地磁盘不是长期可靠存储。程序重启、重建、迁移实例时，`data/*.db` 这类状态文件可能丢失，或者业务程序比恢复逻辑先启动，导致空数据库覆盖旧数据。

r2sync 解决的是这类问题：

- 在业务程序启动前，先从远端拉回已有状态文件。
- 如果远端还没有初始化，则把本地文件上传到远端作为初始版本。
- 程序运行期间按周期同步已变化的内容。
- 未变化的内容尽量跳过，减少请求和写入开销。
- 通过 Web UI/API 切换同步方式、修改凭据、目标路径、同步间隔和存储保护配置。

## 主要用途

r2sync 适合放在其它项目旁边作为一个可复用的同步服务：

- 容器或云平台中的轻量持久化：应用数据在本地，R2 或私有 GitHub 仓库作为恢复点。
- SQLite、BoltDB、小型 JSON/YAML 配置、运行状态文件的当前版本同步。
- ModelScope Studio、Docker、VPS、单机部署中，需要先恢复数据再启动业务服务的场景。
- 给多个项目复用同一套“同步配置 + 启动门禁 + 管理 UI”能力。

默认目标是 `data/sophnet.db`，这是为了兼容最初的 SophNet 使用场景；r2sync 本身不绑定 SophNet，任何项目都可以通过 `R2SYNC_TARGETS` 或管理 UI 修改目标文件。

## 不适合什么场景

r2sync 有意保持简单，不适合这些用途：

- 多设备实时双向同步。
- 多用户同时编辑同一个文件并自动合并冲突。
- 带版本保留、审计、回滚策略的完整备份系统。
- 大规模目录镜像、对象湖、CDN 资源分发。
- 数据库主从复制或高频事务复制。
- **GitHub 方法**：单文件超过 100 MiB（GitHub 硬限制）；需要系统已安装 `git`。
- **R2 方法**：不支持把整个目录当作一个 target（只同步显式配置的文件路径）。

如果你需要历史版本，请使用专门的备份系统，或依赖远端自身的版本能力；r2sync 默认不会替你做完整历史备份。

## 核心能力

- `r2sync serve`：启动同步守护进程和管理 UI/API。
- `r2sync sync`：执行一次前台手动同步。
- `r2sync run -- <command>`：先完成初始同步，再启动业务命令。
- `r2sync config check`：校验本地配置与所选后端的连通性。
- `r2sync version`：打印版本。
- 默认同步方式：`r2`。
- 默认每 `5h` 执行一次周期同步。
- 默认监听 `0.0.0.0:5321`。
- 默认状态目录是当前项目下的 `.r2sync`。
- 默认存储保护上限是 `4 GiB`，可以手动调整。
- R2 模式默认只使用 Standard storage，不主动启用 public bucket、`r2.dev`、Infrequent Access 等附加能力。

## 同步方式

### Cloudflare R2（`r2`）

每个 target 映射为一个确定的 R2 object key：

```text
<object_prefix>/<normalized-target-path>
```

例如：

```text
target: data/sophnet.db
object_prefix: prod
object key: prod/data/sophnet.db
```

R2 模式只同步**文件**，不扫描整个目录。更新时覆盖同一个 object key，不主动创建时间戳历史对象。

### GitHub 仓库（`github`）

GitHub 模式会：

1. 在 `repo_dir`（默认 `<state_dir>/repo`）维护一个本地 git 仓库。
2. 把配置的 target **迁移**进仓库目录（保持相对路径）。
3. 在原始路径留下**软链接**指向仓库内副本，业务程序继续读写原路径，无感知。
4. 通过 `git commit` + `push` 把变更推到 `owner/name` 的指定分支。
5. 初始对齐时以远端为准；冲突时本地副本会先进入 `.r2sync/quarantine/`，再采用远端版本。

特点：

- 需要 PATH 中有 `git`。
- Token 以每次命令的 HTTP header 传递，**不会**写入 `.git/config` 或 origin URL。
- 单文件不能超过 **100 MiB**（GitHub 限制）；更大文件请用 R2 方法。
- Target 可以是文件，也可以是以 `/` 结尾的目录路径。
- 空目录会写入 `.gitkeep` 以便被 git 跟踪。
- 周期同步会先提交本地变更，再 push；若远端有更新则 rebase 后重试。

## 同步语义

### 初始同步（两种方式共通原则）

初始同步用于启动门禁，也是最重要的安全边界：

| 状态 | 行为 |
| --- | --- |
| 远端有数据，本地没有 | 从远端恢复到本地 |
| 远端有数据，本地也有且内容不同 | 先把本地复制到 `.r2sync/quarantine/...`，再采用远端版本 |
| 远端没有，本地有 | 上传/推送本地内容，初始化远端 |
| 远端和本地都没有 | 创建父目录/占位，标记为 missing |

也就是说：**远端已初始化后，初始同步默认以远端为准**；只有远端还没有内容时，本地才作为初始来源。

### R2：周期同步和手动同步

1. 如果本地文件的 size 和 mtime 没变，直接跳过，不计算 hash，也不访问 R2。
2. 如果 size 或 mtime 变了，计算本地 SHA-256。
3. 如果 SHA-256 没变，只刷新本地 metadata，不上传。
4. 如果 SHA-256 变了，检查成本保护，然后覆盖同一个 R2 object key。
5. 如果本地文件被删，而 R2 里有对象，默认从 R2 恢复，不会删除 R2。

删除远端对象只能通过明确确认的 UI/API 操作执行。本地删除文件不会被解释为“也删除云端唯一副本”。

### GitHub：周期同步和手动同步

1. 确保各 target 仍是指向仓库内的软链接；若被真实文件/目录替换，视为更新本地状态并迁回仓库。
2. 检测 worktree 变更；有变更则检查存储保护后 `git add -A` 并 commit。
3. `git push`；若远端已前进则 `pull --rebase` 后重试一次。
4. 初始阶段会 `fetch` 并对齐 `origin/<branch>`；历史分叉时远端获胜，本地差异进入 quarantine。

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

Linux / macOS：

```bash
go build -o dist/r2sync ./cmd/r2sync
```

> 使用 GitHub 同步方法时，运行环境还需安装 `git`。

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

在 UI「同步连接」中选择：

- **Cloudflare R2**：bucket、token、可选 account id、对象前缀。
- **GitHub 仓库**：`owner/name`、具有 repo 读写权限的 Token、分支（默认 `main`）。

再配置目标文件列表、同步间隔和存储上限。

### R2 环境变量示例

```powershell
$env:R2SYNC_SYNC_METHOD="r2"
$env:R2SYNC_BUCKET="my-r2-bucket"
$env:R2SYNC_TOKEN="cloudflare_api_token"
$env:R2SYNC_ACCOUNT_ID="cloudflare_account_id"
$env:R2SYNC_TARGETS="data/sophnet.db"
$env:R2SYNC_ADMIN_PASSWORD="change-me"
r2sync serve
```

### GitHub 环境变量示例

```powershell
$env:R2SYNC_SYNC_METHOD="github"
$env:R2SYNC_GITHUB_REPO="your-name/state-backup"
$env:R2SYNC_GITHUB_PAT="ghp_xxxxxxxx"
$env:R2SYNC_GITHUB_BRANCH="main"
$env:R2SYNC_TARGETS="data/sophnet.db"
$env:R2SYNC_ADMIN_PASSWORD="change-me"
r2sync serve
```

`R2SYNC_GITHUB_TOKEN` 与 `R2SYNC_GITHUB_PAT` 等价，任选其一。

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
3. 按所选方式校验远端访问（R2 或 GitHub）。
4. R2：若 bucket 不存在且 token 有权限，则自动创建 bucket。
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
| `R2SYNC_SYNC_METHOD` | `r2` | `r2` 或 `github` |
| `R2SYNC_BUCKET` / `R2SYNC_BUCKET_NAME` | 空 | R2 bucket 名称 |
| `R2SYNC_TOKEN` / `R2SYNC_CLOUDFLARE_TOKEN` | 空 | Cloudflare R2 API token |
| `R2SYNC_ACCOUNT_ID` | 自动发现 | Cloudflare account id；多账号时建议显式填写 |
| `R2SYNC_OBJECT_PREFIX` | 空 | R2 object key 前缀，用于同 bucket 隔离项目 |
| `R2SYNC_GITHUB_REPO` | 空 | GitHub 仓库，`owner/name` |
| `R2SYNC_GITHUB_TOKEN` / `R2SYNC_GITHUB_PAT` | 空 | GitHub PAT，需要 repo 读写权限 |
| `R2SYNC_GITHUB_BRANCH` | `main` | 推送/拉取的分支 |
| `R2SYNC_REPO_DIR` | `<state_dir>/repo` | GitHub 模式下本地仓库目录 |
| `R2SYNC_TARGETS` | `data/sophnet.db` | 逗号、分号、空格或换行分隔的目标路径 |
| `R2SYNC_EXCLUDES` | `.r2sync` 等系统项 | GitHub 模式写入本地 `info/exclude`；R2 模式以显式 targets 为准 |
| `R2SYNC_SYNC_INTERVAL` | `5h` | 周期同步间隔，例如 `30m`、`5h` |
| `R2SYNC_STORAGE_CAP_BYTES` | `4294967296` | 默认 4 GiB 存储保护上限 |
| `R2SYNC_STRICT_VERIFY` | `false` | 严格校验：R2 强制检查远端 metadata；GitHub 推送前先 fetch |
| `R2SYNC_DISABLE_COST_GUARDS` | `false` | 关闭成本/存储保护，不建议默认开启 |
| `R2SYNC_ADMIN_PASSWORD` | 自动生成 | 首次启动管理密码 |
| `R2SYNC_ADMIN_PASSWORD_HASH` | 空 | 已哈希的管理密码，适合高级部署 |

JSON 配置示例（R2）：

```json
{
  "base_dir": "/app",
  "state_dir": "/app/.r2sync",
  "listen_addr": "0.0.0.0:5321",
  "sync_method": "r2",
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

JSON 配置示例（GitHub）：

```json
{
  "base_dir": "/app",
  "state_dir": "/app/.r2sync",
  "listen_addr": "0.0.0.0:5321",
  "sync_method": "github",
  "github_repo": "your-name/state-backup",
  "github_token": "ghp_xxxxxxxx",
  "github_branch": "main",
  "repo_dir": "/app/.r2sync/repo",
  "sync_interval": "5h",
  "targets": ["data/sophnet.db", "config/"],
  "storage_cap_bytes": 4294967296
}
```

注意：Cloudflare token 或 GitHub token 如果通过 UI 或配置文件保存，会以明文保存在本地配置文件里。r2sync 不会在 API 响应里返回明文 token，也不会主动打印 token，但你仍然需要保护 `.r2sync/config.json` 和运行环境变量。

## Cloudflare R2 token

R2 模式使用两类 Cloudflare/R2 接口：

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

## GitHub token

GitHub 模式需要：

- 仓库格式：`owner/name`（建议私有仓库）。
- 具有该仓库 **contents 读写** 权限的 PAT（经典 token 的 `repo` 范围，或 fine-grained token 的 Contents: Read and write）。
- 运行环境已安装 `git`。

远程地址为：

```text
https://github.com/<owner>/<name>.git
```

认证通过临时 `http.extraheader` 注入 `Authorization: Basic`（`x-access-token:<PAT>`），不会把 token 持久化到仓库配置。

## 成本保护和免费额度

### R2 模式

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

`10 GB-month` 可以理解为“一个月内平均 10GB 左右的 Standard 存储用量”，不是 bucket 的硬容量上限。r2sync 默认 4 GiB cap 是项目自己的软保护，不是 Cloudflare 强制限制。

请求计数是本地估算，不是 Cloudflare 账单的事实来源。它的用途是提前拦住错误循环、过低同步间隔或异常 UI/API 轮询。

### GitHub 模式

GitHub 模式没有 Class A/B 请求计数。仍会检查目标总大小是否超过 `storage_cap_bytes`（除非关闭 cost guards）。超过 100 MiB 的单文件会记录警告，且推送通常会被 GitHub 拒绝。

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
| `GET` | `/api/health` | 进程健康检查（含版本） |
| `GET` | `/api/ready` | 初始同步是否完成 |
| `POST` | `/api/login` | 管理密码登录 |
| `POST` | `/api/logout` | 退出登录 |
| `POST` | `/api/password` | 修改管理密码 |
| `GET` | `/api/progress` | 当前同步进度 |
| `GET` | `/api/status` | 状态、目标文件、计数器、事件 |
| `GET` | `/api/config` | 读取脱敏配置 |
| `PUT` | `/api/config` | 更新配置（含 `sync_method` 与对应凭据） |
| `GET` | `/api/targets` | 读取 targets/excludes |
| `PUT` | `/api/targets` | 更新 targets/excludes |
| `POST` | `/api/sync` | 立即同步 |
| `POST` | `/api/verify` | 严格校验同步 |
| `POST` | `/api/objects/delete` | 删除远端对象/仓库中对应路径，需要 body 确认 `DELETE` |

如果管理 UI 暴露到公网，请放在反向代理、内网或访问控制之后，并设置强密码。

## 常见部署方式

### R2：独立守护进程

```bash
export R2SYNC_SYNC_METHOD="r2"
export R2SYNC_BUCKET="my-r2-bucket"
export R2SYNC_TOKEN="cloudflare_api_token"
export R2SYNC_ACCOUNT_ID="cloudflare_account_id"
export R2SYNC_TARGETS="data/app.db"
export R2SYNC_ADMIN_PASSWORD="change-me"

r2sync serve
```

### GitHub：启动门禁 + 业务程序

```bash
export R2SYNC_SYNC_METHOD="github"
export R2SYNC_GITHUB_REPO="your-name/state-backup"
export R2SYNC_GITHUB_PAT="ghp_xxxxxxxx"
export R2SYNC_GITHUB_BRANCH="main"
export R2SYNC_TARGETS="data/app.db"
export R2SYNC_ADMIN_PASSWORD="change-me"

r2sync run -- ./app
```

### 多项目共享同一个 R2 bucket

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

### 多项目使用不同 GitHub 仓库或分支

每个项目使用独立的 `R2SYNC_GITHUB_REPO` / `R2SYNC_GITHUB_BRANCH`，或同一仓库的不同分支，并使用各自的 `R2SYNC_STATE_DIR`。

## 故障排查

### 页面需要密码，但不知道密码

如果没有设置 `R2SYNC_ADMIN_PASSWORD`，首次启动日志会打印一次自动生成的密码。只会打印一次。找不到时，可以停止程序，删除或更换状态中的密码哈希，或者用新的 state dir 重新初始化。

生产环境建议显式设置：

```bash
R2SYNC_ADMIN_PASSWORD="your-strong-password"
```

### `attempt to write a readonly database`

这通常不是远端把文件变成只读，而是 r2sync 恢复文件时使用的系统用户和业务程序写数据库的系统用户不一致。

解决方式：

- 让 r2sync 和业务程序用同一个 UID/GID 运行。
- 或在启动脚本里对恢复后的数据目录执行 `chown`。
- Docker 中不要让 r2sync 用 root 恢复文件，再让非 root 业务进程写同一个 SQLite 文件。

### 本地数据库变成 0B

如果远端还没有初始化，而业务程序先创建了一个空数据库，r2sync 会把这个空文件当作本地初始版本上传。应使用 `r2sync run -- <command>` 保证初始同步先完成，再启动业务程序。

如果远端已经有数据，初始同步会以远端为准；本地 0B 文件会被复制到 quarantine，然后恢复远端版本。

### 看到 `0001-01-01T00:00:00Z`

这是旧版本中零值时间直接展示的问题。请升级到 `v0.1.1` 或更高版本；新版本会隐藏未设置的时间字段。

### R2 bucket 创建失败

检查：

- bucket 名称是否符合 Cloudflare R2 规则。
- token 是否属于正确账号。
- `R2SYNC_ACCOUNT_ID` 是否正确。
- token 是否有创建 bucket 或访问目标 bucket 的权限。

如果不想给自动创建权限，可以先在 Cloudflare 控制台手动创建 bucket，再给 token 访问该 bucket 的权限。

### GitHub：`git binary not found`

安装 git 并确保在 PATH 中，然后重试 `r2sync config check`。

### GitHub：推送被拒绝 / 文件过大

- 确认 PAT 对目标仓库有写权限。
- 单文件不得超过 100 MiB；大文件请改用 R2 方法。
- 确认分支名与 `R2SYNC_GITHUB_BRANCH` 一致。

### GitHub：软链接相关问题

Windows 上创建符号链接可能需要开发者模式或管理员权限。Linux 容器中需注意挂载卷是否支持 symlink，以及业务进程是否跟随链接读写。

### 同步间隔可以调多低

技术上 `R2SYNC_SYNC_INTERVAL` 只要求是 Go duration 且大于 0，例如 `5m`、`30m`、`5h`。但越低越容易因为频繁检查、错误循环或文件频繁变化而增加请求量（R2）或 git 操作频率（GitHub）。

默认 `5h` 是保守值。

## 开发

```bash
go test ./...
go vet ./...
go run ./cmd/r2sync --help
go run ./cmd/r2sync serve
```

Live R2 / 真实 GitHub 测试需要真实凭据，会产生少量请求、存储或仓库写入。只应使用可轮换的测试 token 和测试 bucket/仓库。

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
- GitHub REST / PAT: <https://docs.github.com/en/authentication/keeping-your-account-and-data-secure/managing-your-personal-access-tokens>

---

## English

r2sync is a standalone **state-file sync service** written in Go. It syncs real local paths to a remote backend and provides a protected management UI/API plus a startup gate so your application can restore or initialize files before it starts.

It supports two backends (`R2SYNC_SYNC_METHOD`):

| Method | Value | Behavior |
| --- | --- | --- |
| Cloudflare R2 | `r2` (default) | Map each target file to one R2 object; overwrite on update |
| GitHub repository | `github` | Move targets into a local git repo dir, leave symlinks at original paths, pull/commit/push the repo |

r2sync is not a Dropbox-style bidirectional client and not a full backup system. It is a reusable “current state file sync layer” for small apps and container deployments.

## What It Does

- Restore configured paths from the remote before your application starts.
- Upload/push local content to initialize the remote when nothing exists yet.
- Run scheduled syncs while the application is running.
- Skip unchanged content when possible.
- Expose a web UI/API for method, credentials, targets, interval, and storage guards.
- For R2: create the bucket automatically when the token allows it.
- For GitHub: require `git` on PATH; never embed the token in the remote URL.

The default target is `data/sophnet.db` for compatibility with the original SophNet use case. r2sync itself is project-neutral.

## Good Use Cases

- Lightweight persistence for Docker, VPS, ModelScope Studio, and similar deployments.
- SQLite databases, small state files, and runtime configuration.
- Starting an application only after required files have been restored.
- Sharing one reusable sync component across multiple projects.
- Choosing R2 for large objects, or a private GitHub repo for small state with git history.

## Not A Good Fit

- Realtime multi-device bidirectional sync.
- Automatic merge conflict resolution.
- Full historical backup retention or audit trails.
- Large directory mirroring or object-lake ingestion.
- Database replication.
- **GitHub method**: files larger than 100 MiB; environments without `git`.
- **R2 method**: directory targets (files only).

## Commands

```bash
r2sync serve
r2sync sync
r2sync run -- <command> [args...]
r2sync config check
r2sync version
```

- `serve`: run the sync daemon and management UI/API.
- `sync`: run one foreground manual sync.
- `run --`: run initial sync first, then start the child command.
- `config check`: validate config and connectivity for the selected method.
- `version`: print build version.

## Sync Methods

### R2

Each target maps to:

```text
<object_prefix>/<normalized-target-path>
```

Only files are synced. Updates overwrite the same object key.

### GitHub

Targets are migrated into `repo_dir` (default `<state_dir>/repo`) and replaced with symlinks. The daemon commits local changes and pushes to `owner/name` on `github_branch` (default `main`). Auth uses a per-command HTTP header so the PAT is never stored in `.git/config`. Initial align is remote-wins with local quarantine on conflict.

## Sync Rules

Initial sync (both methods):

| State | Behavior |
| --- | --- |
| Remote exists, local missing | Restore from remote |
| Remote exists, local differs | Quarantine local copy under `.r2sync/quarantine/...`, then take remote |
| Remote missing, local exists | Upload/push local content to initialize remote |
| Both missing | Create parents/placeholders and mark missing |

After the remote is initialized, the remote is authoritative during initial sync.

R2 scheduled/manual:

1. Skip when local size/mtime match stored state (no hash, no R2 call).
2. If metadata changed, compute SHA-256.
3. If hash unchanged, refresh local metadata only.
4. If hash changed, run cost guards and upload.
5. Local deletion restores from R2 when the remote object exists.

GitHub scheduled/manual:

1. Repair broken symlinks; re-migrate if a real path replaced the link.
2. Commit worktree changes when present (after storage-cap checks).
3. Push; on remote divergence, rebase once and retry.
4. Local remote-delete materializes content back to the original path, removes it from the repo, and pushes.

## Install

Download a release asset from:

```text
https://github.com/MoYangking/r2sync/releases
```

Build from source:

```bash
go build -o dist/r2sync ./cmd/r2sync
```

Windows:

```powershell
go build -o dist\r2sync.exe ./cmd/r2sync
```

The GitHub method also requires `git` installed on the host.

## Quick Start

```bash
r2sync serve
```

If `R2SYNC_ADMIN_PASSWORD` is not set on first start, r2sync generates an initial password and prints it once. Only the password hash is stored.

Open `http://127.0.0.1:5321` and configure the connection method, credentials, targets, interval, and storage cap.

R2:

```bash
export R2SYNC_SYNC_METHOD="r2"
export R2SYNC_BUCKET="my-r2-bucket"
export R2SYNC_TOKEN="cloudflare_api_token"
export R2SYNC_ACCOUNT_ID="cloudflare_account_id"
export R2SYNC_TARGETS="data/app.db"
export R2SYNC_ADMIN_PASSWORD="change-me"
r2sync serve
```

GitHub:

```bash
export R2SYNC_SYNC_METHOD="github"
export R2SYNC_GITHUB_REPO="your-name/state-backup"
export R2SYNC_GITHUB_PAT="ghp_xxxxxxxx"
export R2SYNC_GITHUB_BRANCH="main"
export R2SYNC_TARGETS="data/app.db"
export R2SYNC_ADMIN_PASSWORD="change-me"
r2sync serve
```

`R2SYNC_GITHUB_TOKEN` and `R2SYNC_GITHUB_PAT` are aliases.

```bash
r2sync config check
r2sync sync
```

## Startup Gate

```bash
r2sync run -- ./app
```

`run --` loads config, validates the selected backend, runs initial sync, starts the child only after success, keeps scheduled sync and the UI running, and exits with the child exit code.

## Configuration

Loaded from defaults, config file, environment variables (highest priority), and UI saves.

| Variable | Default | Purpose |
| --- | --- | --- |
| `R2SYNC_CONFIG` | `<state_dir>/config.json` | Config file path |
| `R2SYNC_BASE_DIR` | cwd | Base directory for relative targets |
| `R2SYNC_STATE_DIR` | `<base_dir>/.r2sync` | Local state directory |
| `R2SYNC_LISTEN_ADDR` | `0.0.0.0:5321` | Web UI/API listen address |
| `R2SYNC_SYNC_METHOD` | `r2` | `r2` or `github` |
| `R2SYNC_BUCKET` / `R2SYNC_BUCKET_NAME` | empty | R2 bucket name |
| `R2SYNC_TOKEN` / `R2SYNC_CLOUDFLARE_TOKEN` | empty | Cloudflare R2 API token |
| `R2SYNC_ACCOUNT_ID` | auto-discovered when possible | Cloudflare account id |
| `R2SYNC_OBJECT_PREFIX` | empty | R2 object key prefix |
| `R2SYNC_GITHUB_REPO` | empty | `owner/name` |
| `R2SYNC_GITHUB_TOKEN` / `R2SYNC_GITHUB_PAT` | empty | GitHub PAT with repo R/W |
| `R2SYNC_GITHUB_BRANCH` | `main` | Branch to pull/push |
| `R2SYNC_REPO_DIR` | `<state_dir>/repo` | Local git working tree for GitHub mode |
| `R2SYNC_TARGETS` | `data/sophnet.db` | Target paths |
| `R2SYNC_EXCLUDES` | system defaults | Git exclude list for GitHub mode |
| `R2SYNC_SYNC_INTERVAL` | `5h` | Scheduled sync interval |
| `R2SYNC_STORAGE_CAP_BYTES` | `4294967296` | Storage cap (4 GiB) |
| `R2SYNC_STRICT_VERIFY` | `false` | Stricter remote checks |
| `R2SYNC_DISABLE_COST_GUARDS` | `false` | Disable storage/request guards |
| `R2SYNC_ADMIN_PASSWORD` | generated | First-start admin password |
| `R2SYNC_ADMIN_PASSWORD_HASH` | empty | Pre-hashed admin password |

Secrets saved via the UI or config file are stored locally in plaintext in the config file. Tokens are masked in API responses and logs; still protect `.r2sync/config.json`.

## Cost Guards

**R2:** Standard storage only; 4 GiB cap; estimated Class A/B warn at 80% and block at 95% of free-tier assumptions; counters are local estimates, not Cloudflare billing.

**GitHub:** storage-cap checks only; warn when a file exceeds 100 MiB (GitHub hard limit).

Official R2 docs:

- <https://developers.cloudflare.com/r2/pricing/>
- <https://developers.cloudflare.com/r2/platform/limits/>

## Management API

Listens on `0.0.0.0:5321` by default; requires login (except health/ready/login).

| Method | Path | Purpose |
| --- | --- | --- |
| `GET` | `/api/health` | Process health + version |
| `GET` | `/api/ready` | Initial sync readiness |
| `POST` | `/api/login` | Password login |
| `POST` | `/api/logout` | Logout |
| `POST` | `/api/password` | Change admin password |
| `GET` | `/api/progress` | Sync progress |
| `GET` | `/api/status` | Status, targets, counters, events |
| `GET` | `/api/config` | Masked config |
| `PUT` | `/api/config` | Update config |
| `GET` | `/api/targets` | Read targets/excludes |
| `PUT` | `/api/targets` | Update targets/excludes |
| `POST` | `/api/sync` | Sync now |
| `POST` | `/api/verify` | Strict verification sync |
| `POST` | `/api/objects/delete` | Confirmed remote delete |

## Troubleshooting

### Unknown admin password

Generated first-start password is printed only once. Prefer:

```bash
R2SYNC_ADMIN_PASSWORD="your-strong-password"
```

### `attempt to write a readonly database`

Usually a UID/GID mismatch after restore. Run r2sync and the app as the same user, or `chown` the data directory.

### Local DB became 0 bytes

If the remote is empty and the app creates an empty DB first, r2sync may upload that empty file. Use `r2sync run -- <command>`.

### GitHub: `git binary not found`

Install `git` and ensure it is on `PATH`.

### GitHub: push rejected / file too large

PAT needs write access; files must be ≤ 100 MiB; use R2 for larger objects.

### Symlinks on Windows

Creating symlinks may require Developer Mode or elevated privileges.

## Development

```bash
go test ./...
go vet ./...
go run ./cmd/r2sync --help
go run ./cmd/r2sync serve
```

Live validation against real R2 or GitHub consumes real credentials and a small amount of storage/request/repo usage.
