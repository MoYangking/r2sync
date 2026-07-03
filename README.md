# r2sync

Standalone Cloudflare R2 file sync service written in Go.

It replaces the old GitHub-backed `sync` workflow with direct R2 object storage:

- syncs configured real filesystem paths in place;
- keeps only the current object for each target;
- defaults to a 5 hour sync interval;
- skips unchanged files using local size/mtime state, then SHA-256 only when needed;
- restores from R2 when a local target is missing;
- treats R2 as authoritative after remote initialization;
- starts other programs only after initial sync with `r2sync run -- <command>`;
- includes a password-protected web UI/API on `0.0.0.0:5321` by default.

## Quick Start

```powershell
go run ./cmd/r2sync serve
```

On first start, if `R2SYNC_ADMIN_PASSWORD` is not set, r2sync generates an initial password and prints it once in the startup log. The password hash is stored in `.r2sync/state.json`.

Open:

```text
http://127.0.0.1:5321
```

Configure the bucket name and Cloudflare token in the UI, or use env vars:

```powershell
$env:R2SYNC_BUCKET="my-r2-bucket"
$env:R2SYNC_TOKEN="cloudflare_api_token"
$env:R2SYNC_ACCOUNT_ID="optional_account_id"
$env:R2SYNC_TARGETS="data/sophnet.db"
go run ./cmd/r2sync serve
```

## Start Another Program After Sync

```powershell
r2sync run -- your-program --arg value
```

`run --` does this in order:

1. Loads config and state.
2. Validates Cloudflare/R2 access.
3. Creates the bucket if missing and permitted.
4. Runs initial sync.
5. Starts the command only after sync completes.
6. Keeps scheduled sync and the management UI running while the command runs.
7. Exits with the child command exit code.

If initial sync fails, the child command is not started.

## Configuration

Configuration is loaded from `.r2sync/config.json` and environment variables. Environment variables override file values.

Common variables:

| Variable | Default | Purpose |
| --- | --- | --- |
| `R2SYNC_CONFIG` | `<state_dir>/config.json` | Config file path |
| `R2SYNC_BASE_DIR` | current directory | Base for relative targets |
| `R2SYNC_STATE_DIR` | `<base_dir>/.r2sync` | Local state directory |
| `R2SYNC_LISTEN_ADDR` | `0.0.0.0:5321` | Web UI/API listen address |
| `R2SYNC_BUCKET` | empty | R2 bucket name |
| `R2SYNC_TOKEN` | empty | Cloudflare API token |
| `R2SYNC_ACCOUNT_ID` | auto-discovered | Cloudflare account id used for token verification and R2 access |
| `R2SYNC_TARGETS` | `data/sophnet.db` | Comma/space separated target files |
| `R2SYNC_EXCLUDES` | system defaults | Comma/space separated excludes |
| `R2SYNC_SYNC_INTERVAL` | `5h` | Scheduled sync interval |
| `R2SYNC_STORAGE_CAP_BYTES` | `4294967296` | Default 4 GiB storage cap |
| `R2SYNC_ADMIN_PASSWORD` | generated | First-start admin password |

## Cloudflare Token

Use a Cloudflare API token with R2 permissions for the target account. r2sync uses:

- Cloudflare REST API for account discovery, account-scoped token verification, and bucket creation.
- R2 S3-compatible API for object upload/download/head/delete.

For API-created R2 tokens, Cloudflare documents that the S3 Access Key ID is the token id and the S3 Secret Access Key is the SHA-256 hash of the token value. r2sync derives those values at runtime and never returns the token in API responses.

Token verification uses `GET /accounts/{account_id}/tokens/verify` only. If `R2SYNC_ACCOUNT_ID` is omitted, r2sync first tries to discover one accessible account; if the token can access multiple Cloudflare accounts, set `R2SYNC_ACCOUNT_ID` explicitly.

## Sync Semantics

Initial sync:

- Remote exists, local missing: restore local file from R2.
- Remote exists, local differs: copy local file to `.r2sync/quarantine/...`, then restore R2 version.
- Remote missing, local exists: upload local file to initialize R2.
- Both missing: create parent directories and mark target missing.

Periodic sync:

- Local file unchanged by size/mtime: skip without hashing or R2 calls.
- Local size/mtime changed: calculate SHA-256.
- Hash unchanged: update local metadata only.
- Hash changed: overwrite the same R2 object key.

Local deletion never deletes R2. Remote deletion is available only through an explicit confirmed UI/API action.

## Cost Guard Defaults

r2sync defaults to conservative free-tier protection:

- Standard storage only.
- 4 GiB local storage cap.
- Class A and Class B request estimates warn at 80% of free-tier allowance.
- Class A and Class B request estimates block at 95% unless adjusted.
- Public bucket access, `r2.dev`, Infrequent Access lifecycle, R2 Data Catalog, R2 SQL, Sippy, and Super Slurper are not enabled by this project.

The request counters are local estimates, not Cloudflare billing truth. They are intended to catch accidental loops and excessive polling before they become billable.

## Commands

```powershell
r2sync serve
r2sync sync
r2sync run -- <command> [args...]
r2sync config check
```

## Development

```powershell
go test ./...
go vet ./...
go run ./cmd/r2sync --help
```

Live R2 validation is manual. Do not run live tests unless you intentionally provide disposable Cloudflare credentials and accept the small request/storage usage.
