# OpenMgCl (mgcl)

OpenMgCl 是一个给 [OpenTofu](https://opentofu.org/) 用的本地 TACOS 状态后端和 bootstrap Secret 提供者：启动一个 HTTP 服务，把 workspace、state 和 bootstrap 凭据存进本地 SQLite，让 `tofu` 的 `cloud` block 或 `remote` backend 把 state 保存在本机，并让 ephemeral provider 在 plan/apply 时读取凭据。

名字来自 MgCl₂（氯化镁）——豆腐点卤的凝固剂，呼应"给 OpenTofu 提供后端、让 tofu 成形"的定位。

> 从零开始的完整操作流程见 [UserStory.md](UserStory.md)。

## 状态

已经可以跑真实 OpenTofu 的 init / plan / apply / destroy / state / lock 流程。state、JSON state 和 outputs 在 SQLite 中强制加密；bootstrap Secret 也保存在同一个加密数据库中，并通过 `mgcl run --` 签发的短期只读 provider session 提供给独立的 ephemeral provider。e2e 覆盖 remote backend、cloud block、lock 冲突、`mgcl run --`、错误私钥被拒绝和 provider token 生命周期。它只实现 TACOS 的状态存储子集，不是 Terraform Cloud 的替代品。

目标场景是从零启动整套基础设施时的本地手动、交互式 OpenTofu 使用，不以 CI 或无人值守运行为目标。

## 安装

需要 Go 1.27.4+：

```bash
go build -o mgcl .
```

或：

```bash
go install github.com/geektr-cloud/openmgcl/cmd/mgcl@<version>
```

`cmd/mgcl` 与仓库根入口共用同一套退出码处理；安装出的命令名是 `mgcl`。也可以从 GitHub Releases 下载 `mgcl-linux-amd64` / `mgcl-linux-arm64`。

## 加密私钥

mgcl 强制加密 state 和 Secret，没有明文模式。默认读取 `~/.ssh/id_ed25519`；不存在、权限允许其他用户访问或不是 Ed25519 OpenSSH 私钥时会拒绝启动。

```bash
ls -l ~/.ssh/id_ed25519

# 没有默认私钥时：
ssh-keygen -t ed25519 -f ~/.ssh/id_ed25519

# 或显式指定另一把私钥：
mgcl run --db ./mgcl.db --encryption-key-file ~/.ssh/mgcl_ed25519 -- ...
```

私钥受 passphrase 保护时，优先使用 `MGCL_ENCRYPTION_KEY_PASSPHRASE`；未设置时会从 TTY 无回显提示。

> **私钥丢失后，对应的 state 和 Secret 都无法恢复。** `mgcl.db` 和私钥要一起备份。加密只保护静态数据库文件，mgcl 运行时会解密 state 和 Secret；它不保护已泄露的数据库副本、正在运行的进程、现有免认证下载端点，也不防能改写数据库的主动攻击者替换密钥元数据。

实现边界见 [docs/state-encryption.md](docs/state-encryption.md)，决策见 [ADR-0004](docs/adr/0004-encrypt-state-by-lineage.md)。

## Bootstrap Secret

OpenTofu 第一次 apply 之前需要的凭据（例如 IAM access key）可以存进同一个 `mgcl.db`。Secret 是具名 KV 集合；每个 Secret 有独立的 32-byte DEK，DEK 再由当前 SSH Ed25519 Recipient 包裹。KV value 用 AES-256-GCM 单独加密，SQLite 里只有密文和明文元数据。

```bash
# 先创建空 Secret
mgcl secret create bootstrap --db ./mgcl.db

# 敏感值优先从 stdin 或 TTY 无回显输入，避免进入 shell history / argv
printf '%s' "$AWS_ACCESS_KEY_ID" | \
  mgcl secret set bootstrap/access_key_id --stdin --db ./mgcl.db

mgcl secret set bootstrap/secret_access_key --db ./mgcl.db
# 没有位置 value 且 stdin 是 TTY 时，会从 TTY 无回显读取一次

mgcl secret list --db ./mgcl.db
mgcl secret show bootstrap --db ./mgcl.db -o json
mgcl secret get bootstrap/access_key_id --db ./mgcl.db
mgcl secret unset bootstrap/secret_access_key --db ./mgcl.db
mgcl secret delete bootstrap --yes --db ./mgcl.db
```

约束：

- Secret 名和 KV key 都匹配 `^[a-z0-9][a-z0-9_-]{0,63}$`，引用为 `<secret-name>/<key>`；
- value 必须是 UTF-8，单值最多 64 KiB，每个 Secret 最多 64 个 KV、合计最多 256 KiB；
- 只保留最新值，没有历史版本；`set` 覆盖，`unset`/`delete` 不做安全擦除；
- `list`、`show` 只读明文元数据，不加载私钥，也不会创建缺失的数据库；
- `create` 可以在缺失路径初始化数据库；`set`、`get`、`unset`、`delete` 不隐式建库；
- `list`、`show`、`get` 用 `mode=ro` 读取现有数据库，不使用 `immutable=1`。WAL 数据库缺少 `-shm` 时，SQLite 可能需要在数据库目录创建它，因此目录可能需要可写；这样才能读到尚未 checkpoint 的数据；
- `create`、`set`、`unset`、`delete` 是 write session，复用 `--backup-to` / `backup_to` 备份层；`list`、`show`、`get` 不触发备份；
- 私钥丢失后对应 Secret 也无法恢复。`mgcl.db` 与私钥必须一起备份。

`get` 把原始 UTF-8 字节写到 stdout，不追加换行；诊断和成功提示写 stderr。任何错误、日志或提示都不会打印 value、DEK、wrapped key、nonce 或 provider token。

完整语义见 [docs/secret-management.md](docs/secret-management.md)。

## 非 loopback 安全门

默认监听 `127.0.0.1` 不需要确认。合并后的 listen host 或显式 `--server` 只要不是字面值 `127.0.0.1`（`localhost`、`::1`、`0.0.0.0`、`127.0.0.2`、LAN IP、主机名等都算），`mgcl run` 会在加载私钥、打开数据库和监听前要求确认：

- stdin 是 TTY 时显示警告并只接受 `y` / `yes`；
- 非 TTY 时必须精确传入 `--yes-i-know-it-will-publish-state-and-secrets`；
- 这个 flag 不能来自环境变量或 `.mgcl.yaml`；
- 确认后服务会暴露到非 loopback。provider endpoint 是 HTTP；TACOS `--server` 可能是 HTTP 或 HTTPS，是否加密取决于你的配置。

该门禁只放宽网络暴露面，不关闭 provider token 认证。

## 快速开始

### 方式一：`mgcl run --` 包住 tofu（推荐）

`main.tf` 使用 cloud block，不需要写 hostname：

```hcl
terraform {
  cloud {
    organization = "local"
    workspaces {
      name = "demo"
    }
  }
}
```

```bash
mgcl run --db ./mgcl.db -- \
  sh -c 'tofu init -input=false && tofu apply -auto-approve -input=false'
```

`mgcl run` 会启动服务，把 `TF_CLOUD_HOSTNAME` 和临时 `TF_CLI_CONFIG_FILE` 注入子进程。临时配置会保留用户现有 CLI config：显式设置的 `TF_CLI_CONFIG_FILE` 优先，否则继承默认 `~/.tofurc`。命令结束后关闭服务。token 不需要配置：每次运行自动生成一个全新随机 token，只写进本次临时 CLI config，不打印。state 留在 `mgcl.db`，下次继续用同一个 `--db` 即可。子进程退出码原样作为 `mgcl` 退出码；被信号终止时为 `128+signal`。

如果使用 `remote` backend，OpenTofu 要求显式 `hostname`：

```hcl
terraform {
  backend "remote" {
    hostname     = "127.0.0.1:1217"
    organization = "local"
    workspaces {
      name = "demo"
    }
  }
}
```

> `mgcl run` 默认监听 `127.0.0.1:1217`；如果改了 `--host`，`hostname` 也要跟着改。

### 方式二：常驻服务 + 手写 CLI config

```bash
mgcl run --host 127.0.0.1:1217 --db ./mgcl.db
```

未指定 token 时，常驻 `mgcl run` 也会自动生成一个本次进程有效的 token，并在 `MgCl server listening on ...` 之后向 stderr 打印一次：

```text
MgCl access token: <random-token>
```

需要稳定 token 时再显式传 `--token`；显式 token 不会打印。另开一个终端，写 `tofu.rc`：

```hcl
host "127.0.0.1:1217" {
  services = {
    "state.v2" = "http://127.0.0.1:1217/api/v2/"
    "tfe.v2"   = "http://127.0.0.1:1217/api/v2/"
    "tfe.v2.1" = "http://127.0.0.1:1217/api/v2/"
  }
}

credentials "127.0.0.1:1217" {
  token = "<MgCl access token>"
}
```

```bash
export TF_CLI_CONFIG_FILE="$PWD/tofu.rc"

# cloud block 还需要：
export TF_CLOUD_HOSTNAME=127.0.0.1:1217

tofu init -input=false
tofu apply -auto-approve -input=false
```

`remote` backend 则把 `hostname = "127.0.0.1:1217"` 写进 `backend "remote"` 块。

### Provider（独立仓）

ephemeral provider 已在独立仓库 `github.com/geektr-cloud/terraform-provider-mgcl` 实现。`mgcl run -- <command>` 只在该子进程环境里注入：

```text
MGCL_PROVIDER_ENDPOINT=http://127.0.0.1:<实际端口>
MGCL_PROVIDER_TOKEN=<一次性随机 token>
```

常驻 `mgcl run`（没有子进程）不签发 provider token。provider token 只存在内存，服务端只保存 SHA-256 摘要；子进程退出或服务关闭后立即失效。server 仍在监听时旧 token 返回 401；进程退出后端口关闭，客户端看到连接失败。它只能调用 `POST /api/mgcl/v1/secrets:read`，不能访问 TACOS state、workspace、lock 或管理路由；用户 Bearer token 也不能调用 Secret API。endpoint 从实际 listener 推导，不使用 `--server`。用户 TACOS token 与它完全独立：未配置时由每次 run 自动生成，子进程模式不打印，常驻模式启动后打印一次。

provider 尚未发布到 OpenTofu Registry，需要 OpenTofu >=1.11。开发期在 provider 仓库构建 `terraform-provider-mgcl`，然后把 `dev_overrides` 写进 OpenTofu 的默认 CLI config `~/.tofurc`：

```hcl
# ~/.tofurc
provider_installation {
  dev_overrides {
    # 必须是包含 terraform-provider-mgcl 二进制的目录，不是二进制文件本身
    "geektr-cloud/mgcl" = "/path/to/terraform-provider-mgcl/bin"
  }
  direct {}
}
```

```bash
# 该目录下应直接放着 terraform-provider-mgcl
ls /path/to/terraform-provider-mgcl/bin/terraform-provider-mgcl

mgcl run --db ./mgcl.db -- tofu plan
```

`mgcl run` 会自动继承 `~/.tofurc` 并追加本地服务的 host block。设置了 `TF_CLI_CONFIG_FILE` 时改为读取该文件，优先于默认 `~/.tofurc`。`dev_overrides` 会跳过该 provider 的 `tofu init` 安装/校验；需要真正的 `tofu init` 时改用 filesystem mirror。

HCL 示例：

```hcl
terraform {
  required_providers {
    mgcl = { source = "geektr-cloud/mgcl" }
  }
}

provider "mgcl" {}

ephemeral "mgcl_secret" "bootstrap" {
  name = "bootstrap"
}

provider "aws" {
  region     = "us-east-1"
  access_key = ephemeral.mgcl_secret.bootstrap.values["access_key_id"]
  secret_key = ephemeral.mgcl_secret.bootstrap.values["secret_access_key"]
}
```

`pkg/providerapi` 是 stdlib-only 客户端合同，供 provider 仓库导入。首版没有 provider function、普通 data source、cloud workspace variables 或 HTTP 管理 API；`tofu init` 的无缝安装要等 Registry 发布。

## 配置

| flag | 默认值 | 说明 |
|---|---|---|
| `--host`, `-h` | `127.0.0.1:1217` | 监听地址；`127.0.0.1:0` 自动选端口 |
| `--server`, `-s` | 从 `--host` 推导 | 对外 base URL，用于 discovery 和上传/下载链接 |
| `--db`, `-d` | 无（必填） | SQLite 路径；WAL 模式，会生成 `-wal` / `-shm` |
| `--token`, `-t` | 每次 run 自动生成 | TACOS Bearer token；子进程 run 自动生成且不打印，常驻 run 自动生成并在启动后打印一次；显式配置保持固定值且不打印 |
| `--encryption-key-file` | `~/.ssh/id_ed25519` | 强制加密使用的 Ed25519 OpenSSH 私钥 |
| `--backup-to` | 无 | 备份目标；可重复，`sqlite:<path>` 或 `path:<path>`；出现时整体替换 YAML `backup_to`；`mgcl run` 和 `mgcl secret` 写命令使用 |

在 Git 工作树内运行时，mgcl 从当前目录定位最近的 Git 根目录（支持 worktree 和 submodule），读取 `<git-root>/.mgcl.yaml`。在子目录运行也会读取根目录的配置。非 Git 目录不读取任何 `.mgcl.yaml`，也不会回退到当前目录。

```yaml
# <git-root>/.mgcl.yaml
host: "127.0.0.1:1217"
server: "http://127.0.0.1:1217"
db: "mgcl.db"
encryption_key_file: "/home/you/.ssh/id_ed25519"
backup_to:
  - "sqlite:/srv/mgcl-backup/mgcl.db"
  - "path:/mnt/no-locks/backup/mgcl.db"
```

`db` 来自 `.mgcl.yaml` 时，相对路径按 Git 根目录解析；来自 `--db` 或 `MGCL_DB` 时，相对路径按进程当前工作目录解析。绝对路径保持不变。`.mgcl.yaml` 只放配置和路径，不要写真实凭据、Secret value、私钥内容或 token。

也可以使用环境变量 `MGCL_HOST`、`MGCL_SERVER`、`MGCL_DB`、`MGCL_TOKEN`、`MGCL_ENCRYPTION_KEY_FILE`。`MGCL_ENCRYPTION_KEY_PASSPHRASE` 只从环境或 TTY 读取，不能写进配置。

配置优先级：显式 flag（包括显式空串）> `MGCL_*` 环境变量 > `<git-root>/.mgcl.yaml` > 内置默认值。`db` 没有内置默认值：最终 db 为空时，mgcl 会在加载私钥、打开数据库和监听端口之前报错退出。即使 `--db` 或 `MGCL_DB` 已提供，Git 内仍会读取 `.mgcl.yaml` 以获取其他配置；配置损坏或不可读会直接失败。

### 数据库备份

`--backup-to` 可重复，用于在 `mgcl run` 或 `mgcl secret create/set/unset/delete` 期间把同一个 `mgcl.db` 覆盖复制到显式配置的位置。YAML 使用 `backup_to` 字符串数组；只要出现 `--backup-to`，整份列表就替换 YAML，不做合并。不读取 `MGCL_BACKUP_TO`。`--backup-to` 的相对路径按当前工作目录解析，YAML 的相对路径按 Git 根目录解析。`mgcl secret list/show/get` 不触发备份。

```bash
mgcl run --db ./mgcl.db \
  --backup-to sqlite:./backup/mgcl.db \
  --backup-to path:/mnt/no-locks/backup/mgcl.db \
  -- tofu apply -auto-approve
```

两种 scheme 不能互换：

- `sqlite:<path>`：启动时先做一次完整的 SQLite Online Backup，之后每次成功写提交都会唤醒唯一后台 worker 合并同步；干净关停前会 drain 最后一轮。目标要求文件系统支持 SQLite 锁。
- `path:<path>`：启动时只在目标同目录做可写探测（探测文件权限由 umask/默认 ACL 决定）；运行中不复制。停止写、drain、checkpoint 并关闭主库后，才把自包含的主文件复制到同目录临时文件并 rename 覆盖。目标不依赖 `-wal`/`-shm`/`-journal`，适合不支持 SQLite 锁的挂载。临时文件和最终目标以 0666 请求创建，实际权限由 umask/目录默认 ACL 决定，应用不改写权限。

备份失败 fail-closed：启动校验、探测或初始备份失败不会启动服务；运行中 sqlite 同步失败会停止接受新写并终止子进程；关停时在子进程退出或信号处理完成后，先关闭写 gate、等待在途写完成，再 drain/checkpoint/copy，path 复制失败同样令命令失败。子进程自己的非零退出码会保留；子进程成功但备份失败时退出 1；备份主动终止 run 时不会伪装成 `128+signal`。已经提交的源事务不会回滚。

对于 `mgcl secret`：Secret 写命令遇到非法备份配置会在加载私钥或打开数据库前失败；只读的 `list/show/get` 不 probe、不创建备份目标；Secret 写命令的备份失败一律非零退出，且不打印成功提示。

备份内容沿用数据库现有加密格式，仍需和私钥一起保存。完整语义、校验规则与退出码见 [docs/database-backup.md](docs/database-backup.md)，决策见 [ADR-0007](docs/adr/0007-sqlite-backup-destinations.md)。

## 已实现的 API

TACOS `state.v2` 子集与 provider Secret API：

- `GET /.well-known/terraform.json`
- `GET /api/v2/ping`
- `GET /api/v2/organizations/{org}/entitlement-set`
- `GET|POST /api/v2/organizations/{org}/workspaces`
- `GET /api/v2/organizations/{org}/workspaces/{name}`
- `POST /api/v2/workspaces/{id}/actions/lock|unlock|force-unlock`
- `POST /api/v2/workspaces/{id}/state-versions`
- `GET /api/v2/workspaces/{id}/current-state-version`
- `GET /api/v2/state-versions/{id}`
- `PUT /api/v2/state-versions/{id}/state-upload`（免认证）
- `PUT /api/v2/state-versions/{id}/json-state-upload`（免认证）
- `GET /api/v2/state-versions/{id}/download`（免认证）
- `POST /api/mgcl/v1/secrets:read`（provider session token；`Cache-Control: no-store`）

TACOS API 除 discovery、ping 和 foreign upload/download 外始终要求 `Authorization: Bearer <token>`；空 token 不表示免认证。子进程 `mgcl run --` 自动把当次 token 写入临时 CLI config，常驻 run 自动生成并打印一次，显式 `--token`/`MGCL_TOKEN`/YAML token 保持固定值。provider session token 只接受于 `secrets:read`，用于其他任何 TACOS 路由都返回 401；用户 token 也不能访问 `secrets:read`。没有活跃 provider session 时该端点一律 401。

## 限制

- 只做 state 存储；没有 runs、variables、policies、teams、VCS 等 TFC 功能。
- workspace 固定 local 执行模式，entitlement-set 返回 `operations: false`。
- workspace 只支持创建、列表、按 name 查询和加锁；没有 update/delete。
- discovery 同时通告 `state.v2`、`tfe.v2`、`tfe.v2.1`，原因见 [ADR-0001](docs/adr/0001-advertise-service-ids-beyond-implemented-scope.md)。未实现的端点会返回 404。
- SQLite 单连接，面向本地单用户；不要拿它做多租户服务。
- 上传/下载 URL 按设计免认证，靠随机 ID 保护；不要暴露到不可信网络。
- API 不发送 CORS 头，浏览器页面不能跨源读取 state；OpenTofu CLI 不需要 CORS。
- serial 由客户端生成：正常更新递增，服务端也允许更低或相同 serial。`current-state-version` 按创建顺序返回最新 finalized 版本，不用 `MAX(serial)`。任何更晚创建的 finalized 版本都会使旧 pending 变成 stale。
- modern create 的 pending 与同 serial 旧 finalized 可共存：上传成功前 current/download 仍是旧版本，成功后同一事务删除旧 finalized 并 finalize 新行；上传失败或放弃不影响旧 current。替换成功后旧 URL 返回 404。设计见 [ADR-0005](docs/adr/0005-state-version-restore-and-replacement.md)。
- 相同 serial、lineage 和 MD5 的 finalized 重复创建一律返回 409，即使 `force=true` 或已有同 serial pending 也不会替换；upload finalize 对这种 pending 同样返回 409。不同内容的显式 restore 可以成为 current。pending 可同 serial 重试并替换旧上传 URL；pending 在更新版本 finalized 后视为过期，其上传返回 409。
- `state_versions` 没有表级 `UNIQUE(workspace_id, serial)`；finalized 与 pending 各有一个同列 partial unique index。v0.1.x 之前不保证历史 schema 兼容，也不提供自动或人工迁移；升级前建议备份 `mgcl.db`、`-wal`、`-shm` 和私钥，必要时自行导出 state。
- 上传 raw state / JSON state 上限约 100MiB；含 base64 inline state 的 create 请求留有 base64 余量；workspace 和 lock 请求上限 1MiB；超限返回 413。
- 加密范围只有 `state`、`json_state`、`json_state_outputs`；workspace 名称、serial、lineage、MD5 等元数据保持明文。
- 每个 State Lineage 一把密钥，首版每个 lineage 只登记一个 Recipient；没有 grant、rotate、rewrap 或恢复命令。
- 每个 Secret 一把独立 Secret DEK，首版只包裹给一个 Recipient；没有 rotation、rewrap 或多 Recipient。Secret 单值 64 KiB、每 Secret 64 KV、合计 256 KiB，只保留最新值。
- provider session 只由 `mgcl run --` 子进程获得，常驻 run 没有 provider token。子进程模式下自动生成的用户 token 只存在于本次临时 CLI config 和进程生命周期；常驻模式会打印一次供外部配置。provider token 是短期只读凭证，不是 TACOS 用户 token，也不能管理 Secret。
- Secret 写入与 `mgcl run` 共用同一个 `mgcl.db` 和备份层；同一数据库不应同时运行多个写会话。备份不含明文 value，恢复仍只依赖 `mgcl.db` 和私钥。
- 非 loopback provider endpoint 或 serverURL 需要显式确认。provider endpoint 是 HTTP；`--server` 的加密取决于其 scheme。
- 一个进程只加载一把私钥；无法解锁的 lineage 在 state 读写时返回 403，其他 lineage 不受影响。
- 不支持旧明文数据库迁移。遇到既有明文 state 会明确报错，不会静默返回明文。

## 开发

```bash
go test ./...        # tofu 不在 PATH 时自动跳过 e2e
go test -short ./... # 显式跳过 e2e
go vet ./...
go build -o mgcl .
go build -o mgcl ./cmd/mgcl
```

CI 会安装最新 OpenTofu 并跑完整 e2e；release 门禁同样会跑。

## License

[MIT](LICENSE)
