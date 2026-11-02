# OpenMgCl (mgcl)

OpenMgCl 是一个给 [OpenTofu](https://opentofu.org/) 用的本地 TACOS 状态后端：启动一个 HTTP 服务，把 workspace 和 state 存进本地 SQLite，让 `tofu` 的 `cloud` block 或 `remote` backend 把 state 保存在本机。

名字来自 MgCl₂（氯化镁）——豆腐点卤的凝固剂，呼应"给 OpenTofu 提供后端、让 tofu 成形"的定位。

## 状态

已经可以跑真实 OpenTofu 的 init / plan / apply / destroy / state / lock 流程。e2e 覆盖 remote backend、cloud block、lock 冲突和 `mgcl run --`。它只实现 TACOS 的状态存储子集，不是 Terraform Cloud 的替代品。

## 安装

需要 Go 1.25+：

```bash
go build -o mgcl .
```

或从 GitHub Releases 下载 `mgcl-linux-amd64` / `mgcl-linux-arm64`。

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
mgcl run --db ./mgcl.db --token dev-token -- \
  sh -c 'tofu init -input=false && tofu apply -auto-approve -input=false'
```

`mgcl run` 会启动服务，把 `TF_CLOUD_HOSTNAME` 和临时 `TF_CLI_CONFIG_FILE` 注入子进程，命令结束后关闭服务。state 留在 `mgcl.db`，下次继续用同一个 `--db` 即可。

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
mgcl run --host 127.0.0.1:1217 --db ./mgcl.db --token dev-token
```

另开一个终端，写 `tofu.rc`：

```hcl
host "127.0.0.1:1217" {
  services = {
    "state.v2" = "http://127.0.0.1:1217/api/v2/"
    "tfe.v2"   = "http://127.0.0.1:1217/api/v2/"
    "tfe.v2.1" = "http://127.0.0.1:1217/api/v2/"
  }
}

credentials "127.0.0.1:1217" {
  token = "dev-token"
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

## 配置

| flag | 默认值 | 说明 |
|---|---|---|
| `--host`, `-h` | `127.0.0.1:1217` | 监听地址；`127.0.0.1:0` 自动选端口 |
| `--server`, `-s` | 从 `--host` 推导 | 对外 base URL，用于 discovery 和上传/下载链接 |
| `--db`, `-d` | `mgcl.db` | SQLite 路径；WAL 模式，会生成 `-wal` / `-shm` |
| `--token`, `-t` | 空 | Bearer token；为空表示不认证 |

也可以使用 `config.yaml`（搜索 `./config.yaml` 和 `/etc/mgcl/config.yaml`）：

```yaml
host: "127.0.0.1:1217"
server: "http://127.0.0.1:1217"
db: "./mgcl.db"
token: "dev-token"
```

或环境变量 `MGCL_HOST`、`MGCL_SERVER`、`MGCL_DB`、`MGCL_TOKEN`。

## 已实现的 API

TACOS `state.v2` 子集：

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

`--token` 非空时，除 discovery、ping 和 foreign upload/download 外的 API 都需要 `Authorization: Bearer <token>`。

## 限制

- 只做 state 存储；没有 runs、variables、policies、teams、VCS 等 TFC 功能。
- workspace 固定 local 执行模式，entitlement-set 返回 `operations: false`。
- workspace 只支持创建、列表、按 name 查询和加锁；没有 update/delete。
- discovery 同时通告 `state.v2`、`tfe.v2`、`tfe.v2.1`，原因见 [ADR-0001](docs/adr/0001-advertise-service-ids-beyond-implemented-scope.md)。未实现的端点会返回 404。
- SQLite 单连接，面向本地单用户；不要拿它做多租户服务。
- 上传/下载 URL 按设计免认证，靠随机 ID 保护；不要暴露到不可信网络。
- `force` 跳过 serial 单调校验，但仍受 `UNIQUE(workspace_id, serial)` 约束。
- schema 迁移只有 `CREATE TABLE IF NOT EXISTS`；升级前备份 `mgcl.db`。

## 开发

```bash
go test ./...        # tofu 不在 PATH 时自动跳过 e2e
go test -short ./... # 显式跳过 e2e
go vet ./...
go build -o mgcl .
```

CI 会安装最新 OpenTofu 并跑完整 e2e；release 门禁同样会跑。

## License

[MIT](LICENSE)
