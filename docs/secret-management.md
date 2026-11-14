# Bootstrap Secret Management v1 实施规范

> **状态：主仓范围已实现。** Secret store、`mgcl secret`、provider session API、`pkg/providerapi`、`cmd/mgcl` 入口和非 loopback 安全门已落地；独立仓库 `terraform-provider-mgcl` 的 ephemeral resource 已实现，Registry 尚未发布。实现状态与文件见第 13 节。

决策原因见 [ADR-0006](adr/0006-bootstrap-secrets-and-ephemeral-provider.md)，领域术语见 [CONTEXT.md](../CONTEXT.md)，state 加密见 [state-encryption.md](state-encryption.md) 与 [ADR-0004](adr/0004-encrypt-state-by-lineage.md)。

## 1. 范围与边界

mgcl 只接管首次运行 OpenTofu 前必须准备的 bootstrap credentials，例如 IAM access key、跨云凭据。它不自动导入、读取或接管 OpenTofu 创建的 state outputs；sensitive output 仍只由既有 state 加密保护。

Secret 是一个具名 KV 集合。每个 Secret 使用一把独立随机 **Secret DEK**；同一 Secret 的所有 KV 分别加密，但共用该 Secret 的 DEK。Secret DEK 与 State Lineage 的 **Lineage Key** 完全独立，绝不共用，也不从 lineage 派生。

Secret 与 state 复用同一个项目 `mgcl.db`、同一把 SSH Ed25519 Recipient 私钥和同一套 identity 解析规则，但使用独立表、独立 envelope label/AAD 和独立密钥元数据。备份 `mgcl.db` 和私钥即可恢复 Secret；私钥丢失则 Secret 不可恢复。首版不支持多 Recipient，也不做 DEK rotation。

只保留每个 KV 的最新值：`set` 覆盖旧值，没有历史版本。Secret 名称、KV key、时间戳等元数据可以明文保存；KV value 强制加密。SQLite、WAL、临时文件和备份都不承诺安全擦除。

### 非目标

- CI、无人值守运行、ssh-agent；
- 多 Recipient、grant/revoke、可靠撤销；
- Secret DEK rotation、rewrap、恢复；
- Secret 或 KV 历史版本、回滚；
- 磁盘、WAL、备份的安全擦除；
- 自动接管 state outputs；
- provider function、普通 data source、cloud workspace variables；
- 本地 registry 模拟或已发布的可无缝 `tofu init` provider。

## 2. 领域模型

| 概念 | 约束 |
|---|---|
| Secret | 不可变 `secret_id`，唯一且可读的 `name`；一个 Secret 对应一把 Secret DEK |
| KV | `<secret-name>/<key>` 引用；每个 KV 有独立 nonce 与 ciphertext；只保留最新值 |
| Secret DEK | 32-byte 随机数据密钥；每 Secret 一把；只保存由当前 Recipient 公钥包裹后的形式 |
| Provider session | `mgcl run -- <command>` 子进程使用的短期只读 token；只允许读取当前 db 的 Secret |

## 3. 配置与启动

### 3.1 数据库与私钥

`mgcl secret` 子命令与 `mgcl run` 共用以下配置和优先级：

| 配置 | flag | 环境变量 | `.mgcl.yaml` | 默认值 |
|---|---|---|---|---|
| SQLite 路径 | `--db` / `-d` | `MGCL_DB` | `db` | 无，必须提供 |
| SSH 私钥 | `--encryption-key-file` | `MGCL_ENCRYPTION_KEY_FILE` | `encryption_key_file` | `~/.ssh/id_ed25519` |

优先级固定为：显式 flag > 环境变量 > `<git-root>/.mgcl.yaml` > 默认值。只有配置文件中的 `db` 相对 Git 根解析；`encryption_key_file` 保持现有 `mgcl run` 行为，不做 Git 根锚定，相对路径按当前工作目录解析。flag/env 中的路径也相对当前工作目录。非 Git 目录不读取配置文件。

`mgcl secret` 命令是离线的：直接打开 SQLite，不启动 HTTP 服务，不监听端口，不需要 `--token`，也不读取 provider session。未配置 DB 路径时直接失败；不猜测默认数据库。

DB 与私钥的加载边界按命令区分：

- `list`、`show` 是只读元数据命令：不加载私钥，不创建缺失 DB；DB 文件不存在时返回 not found。
- `create` 是唯一可能初始化缺失 DB 的命令：先加载当前 Recipient，并用它绑定新 Secret DEK；随后按 store schema 建库并创建 Secret。
- `set`、`get`、`unset`、`delete` 不隐式创建缺失 DB；DB 文件不存在时返回 not found。

只读命令和 `get` 用 SQLite `mode=ro` 打开现有数据库，不使用 `immutable=1`。如果数据库是 WAL 模式且 `-shm` 不存在，SQLite 可能需要在数据库目录创建 `-shm`；因此该目录可能需要可写。mgcl 不把只读操作变成 `immutable`，以免读不到尚未 checkpoint 的已提交数据。

`create`、`set`、`get`、`unset`、`delete` 的私钥加载、passphrase 处理、权限检查和文件格式要求与 `mgcl run` 完全一致：只接受普通 OpenSSH Ed25519 私钥，Unix 下 group/other 不得有权限，passphrase 只来自 `MGCL_ENCRYPTION_KEY_PASSPHRASE` 或 TTY 无回显读取。私钥必须在打开或创建数据库之前完成加载与验证；失败不得创建或修改数据库。

`set --stdin` 会让 stdin 不再是 TTY；带 passphrase 的私钥必须通过 `MGCL_ENCRYPTION_KEY_PASSPHRASE` 提供 passphrase。未设置该变量且 stdin 为非 TTY 时沿用现有加载语义直接失败，不在 stdin 上提示。非 TTY 的 `delete --yes` 同理。

这些 Recipient 检查提供一致的操作边界和防误删，不是 OS 级授权。本机同用户仍可直接删除 `mgcl.db`、替换文件、读取进程内存或私钥。

#### 3.1.1 备份层接入

实现已按本节完成：`create`、`set`、`unset`、`delete` 复用 `internal/sqlitebackup`：

- 四个写命令都是 **Write session**：按同一 `backup_to` / `--backup-to` 配置语义解析、校验目标，执行 `path:` probe 与 `sqlite:` 初始备份，成功提交后标记 dirty，退出前 drain/checkpoint/copy；任一目标失败时 fail-closed，命令非零退出，且不得声称备份成功。
- `list`、`show`、`get` 是只读命令，不触发备份、不 probe、不创建备份目标；provider session 的只读 `secrets:read` 同样不触发备份。
- 备份内容仍是同一个已加密 `mgcl.db`，备份层不得为了 secret 解密数据、导出明文或改变 envelope；恢复仍只依赖 `mgcl.db` 与私钥。
- 空 `backup_to` 保持现有行为；不得新增仅 secret 生效的旁路、默认目标或 `MGCL_BACKUP_TO`。
- 同一 `mgcl.db` 同时只应有一个写会话；并发 `mgcl run` 与 secret 写命令不在首版协调范围内，`path:` checkpoint 会因其他活跃连接 fail-close。

### 3.2 名称、引用与值

Secret 名称和 KV key 使用同一语法：

```text
^[a-z0-9][a-z0-9_-]{0,63}$
```

即 1 到 64 个 ASCII 小写字母、数字、`_` 或 `-`，首字符必须是字母或数字。名称和 key 均不能为空，不能包含 `/`、空白或其他字符；首版只接受小写形式。

KV 引用固定为 `<secret-name>/<key>`，按唯一一个 `/` 分割；因为名称和 key 都不允许 `/`，解析无歧义。出现零个或多个 `/` 时按用法错误拒绝。

KV value 必须是合法 UTF-8 文本，允许空字符串、换行和任意 Unicode 标量值；不裁剪、不做 Unicode 规范化、不改变换行。单 value 最大 64 KiB（按 UTF-8 字节计）。每个 Secret 最多 64 个 KV，全部 value 合计最多 256 KiB。超出限制在写入前拒绝。

## 4. CLI 契约

所有 `secret` 子命令：

- 只离线访问数据库，不访问 HTTP；`list`、`show` 不加载私钥；`create` 加载当前 Recipient 并用它包裹新 DEK；`set`、`get`、`unset`、`delete` 加载当前 Recipient 并要求既有 Secret DEK 的 recipient 匹配；
- 数据输出与诊断分离：`list`、`show`、`get` 的数据写 stdout，错误和诊断写 stderr；
- `create`、`set`、`unset`、`delete` 的成功提示写 stderr，stdout 为空；
- 任何命令都不得在错误、提示、日志或 debug 输出中打印 KV value、Secret DEK、wrapped DEK、私钥或 provider token；
- 非零退出码表示失败；不得用“成功但空输出”掩盖错误。

`--db` 和 `--encryption-key-file` 是 `mgcl secret` 的持久 flag，可放在子命令前后。

### 4.1 `mgcl secret list`

只读当前数据库中的明文 Secret 元数据，按名称排序。不加载私钥，不创建缺失 DB；DB 文件不存在时非零退出并报 not found。默认表格至少包含名称、创建时间、更新时间、KV 数量；不显示 value、DEK、wrapped key 或 nonce。没有 Secret 时输出表头或空列表，并正常退出。

### 4.2 `mgcl secret show <secret-name> [-o table|yaml|json]`

只读单个 Secret 的明文元数据和其 KV 元数据，默认 `table`。不加载私钥，不创建缺失 DB；DB 文件不存在或 Secret 不存在时非零退出并报 not found。表格是给人看的，不作为稳定脚本接口；`yaml` 和 `json` 的字段集合稳定，按下列 schema 输出：

```json
{
  "id": "0c2f6f5e-6f2a-4f1e-9d4f-6e0f1a2b3c4d",
  "name": "bootstrap",
  "created_at": "2026-11-13T12:00:00.000Z",
  "updated_at": "2026-11-13T12:05:00.000Z",
  "keys": [
    {
      "key": "access_key_id",
      "created_at": "2026-11-13T12:01:00.000Z",
      "updated_at": "2026-11-13T12:01:00.000Z"
    }
  ]
}
```

约束：

- `keys` 按 key 排序；空 Secret 输出 `"keys": []`，不是 `null`；
- 不包含 `values`、任何 value、raw DEK、wrapped DEK、nonce、ciphertext 或 provider token；
- 时间使用 UTC RFC 3339，带毫秒和 `Z`；
- 增加或删除字段属于 schema 变更，必须同步更新本文和测试；
- Secret 不存在时非零退出，stderr 报 not found，stdout 为空。

### 4.3 `mgcl secret create <secret-name>`

显式创建一个空 Secret。命令先加载并绑定当前 Recipient；在缺失 DB 时初始化数据库，然后在同一事务中生成 32-byte Secret DEK、按当前 Recipient 包裹、写入独立密钥元数据。创建后没有任何 KV。私钥加载或验证失败时不得创建数据库。

名称已存在时失败，不覆盖、不合并、不自动生成 DEK。名称语法错误时在写库前失败。成功只输出简短确认到 stderr。

### 4.4 `mgcl secret set <secret-name>/<key> <value>`

写入或覆盖一个 KV。Secret 必须已存在；缺失时失败，绝不因拼错名称而自动创建 Secret。命令加载并匹配当前 Recipient；解锁失败或 Recipient 不匹配时不得写入。DB 缺失时返回 not found，不隐式建库。

value 有三种来源，必须互斥：

1. 位置参数 `<value>`：保留该形式，但文档和 `--help` 必须明确警告：value 会出现在 shell history、进程 argv 和本机进程列表中。敏感值优先使用后两种方式。
2. `--stdin`：从 stdin 读取到 EOF，保留所有字节，包括多行和结尾换行；不裁剪。使用 `echo` 管道时，`echo` 附带的换行会成为 value 的一部分。
3. 省略 value 且 stdin 是 TTY：从 TTY 无回显读取一次，提示写 stderr。输入不显示、不确认回显。

同时给位置 value 与 `--stdin` 是用法错误。省略 value 且 stdin 不是 TTY 时直接失败。`--stdin` 在 TTY 上也允许，但会读到 EOF。

写入语义：

- 新 key：设置 `created_at` 和 `updated_at`；
- 已有 key：覆盖 ciphertext 和 nonce，保留 `created_at`，更新 `updated_at`；
- 每次写入生成新的随机 nonce；
- 不保留旧值或历史版本；
- 事务失败不得留下半写入的 KV 或错误的 `updated_at`。

### 4.5 `mgcl secret get <secret-name>/<key>`

唯一读取明文 value 的 CLI。命令加载并匹配当前 Recipient；DB 缺失、Secret 或 key 不存在、解锁失败或认证失败时不输出明文。成功后把 value 的原始 UTF-8 字节写到 stdout：

- 不追加换行，不添加前缀、后缀、JSON、引号或日志；
- 不写 stderr（除非发生错误）；
- 错误只走 stderr，stdout 为空；
- 不因 TTY 或管道改变输出；
- 解密或认证失败时不输出任何部分明文。

### 4.6 `mgcl secret unset <secret-name>/<key>`

删除单个 KV 逻辑记录。命令加载并匹配当前 Recipient；在进入写事务前必须成功 unwrap 该 Secret 的 Secret DEK，wrong recipient 直接失败且不修改任何行。Secret 或 key 不存在时失败，便于发现拼写错误；不静默成功。删除后更新 Secret 的 `updated_at`。不做历史保留或安全擦除承诺。

### 4.7 `mgcl secret delete <secret-name>`

删除整个 Secret、其全部 KV 和 wrapped Secret DEK。

命令加载并匹配当前 Recipient；在进入删除写事务前必须成功 unwrap 该 Secret 的 Secret DEK，wrong recipient 直接失败且不删除任何行。非 TTY 的 `delete --yes` 如果私钥带 passphrase，必须通过 `MGCL_ENCRYPTION_KEY_PASSPHRASE` 提供，因为 stdin 不可用于无回显提示。

- stdin 是 TTY 时进行交互确认；提示包含 Secret 名称和 KV 数量，不包含任何 value；只有 `y` 或 `yes`（不区分大小写）才继续，其他输入取消并非零退出；
- stdin 不是 TTY 时必须显式传 `--yes`；没有 `--yes` 时在删除前失败；TTY 下显式给 `--yes` 也跳过交互确认；
- Secret 不存在时失败；DB 缺失时返回 not found，不隐式建库；
- 不做历史保留或安全擦除承诺。

## 5. `mgcl run` 与 provider session

### 5.0 用户 token

`mgcl run` 的 TACOS 用户 Bearer token 与 provider session 完全独立。未显式配置时，每次 run 都用 `crypto/rand` 生成新的至少 32-byte token，base64url 无 padding；不落库、不写日志：

- `mgcl run -- <command>` 只把 token 写入本次临时 CLI config 的 `credentials`，不打印；文件 0600，子进程退出后删除，服务随之关闭；
- 常驻 `mgcl run` 在 listener 启动并输出 `MgCl server listening on ...` 后，向 stderr 打印一次 `MgCl access token: <token>`，供外部 OpenTofu CLI config 使用；shutdown 后服务关闭，token 失效；
- 显式 `--token`、`MGCL_TOKEN` 或 `.mgcl.yaml` 的 `token` 保持固定值，不自动生成，也不因自动生成而打印。

空 token 不再表示免认证：TACOS 认证路由始终要求与当前 run token 匹配的 Bearer。`mgcl secret` 离线命令仍不需要用户 token。

### 5.1 生成与注入

首版 ephemeral provider 只支持 `mgcl run -- <command>` 的子进程。常驻 `mgcl run`（无子进程）不签发、不打印 provider token，因此不能使用 provider；首版不增加常驻 provider token 获取接口。

当 `mgcl run` 启动子进程时，为本次子进程生成一个 provider session：

- token 由 `crypto/rand` 生成至少 32 bytes，使用 base64url 无 padding 编码，对客户端不透明；
- 服务端只在内存保存 token 的 SHA-256 摘要和元数据，不落库、不写日志、不写临时文件；
- 有效期只由进程生命周期决定：从创建到子进程退出或 server shutdown，以较早者为准；不设固定有效期，避免长 apply 中途失效；
- 子进程退出或服务关闭后立即失效。

注入前先从继承环境中删除同名变量，再设置：

```text
MGCL_PROVIDER_ENDPOINT=http://127.0.0.1:1217
MGCL_PROVIDER_TOKEN=<opaque-token>
```

`MGCL_PROVIDER_ENDPOINT` 由实际 listener 地址推导，不使用 `--server` 对外 URL。listener 是 loopback 时使用实际 loopback 地址；wildcard 地址归一为 `127.0.0.1`；listener 是特定非 loopback 地址时使用该地址，因此非 loopback 安全门是必需的。base URL 不带结尾 `/`；provider 自行追加固定路径。`--server` 只影响 TACOS 对外 URL，不作为 provider endpoint。`MGCL_PROVIDER_TOKEN` 与用户 `MGCL_TOKEN`、SSH passphrase、Bearer token 完全独立，不复用。

### 5.2 权限与失效

provider token 只能：

- 认证 `POST /api/mgcl/v1/secrets:read`；
- 读取当前数据库中的任意已知 Secret 名称；
- 获得该 Secret 当前全部 KV 的明文。

provider token 不能：

- 被 TACOS state、workspace、lock、discovery 或其他用户 API 接受；
- 列出、创建、修改或删除 Secret；
- 读写 state，或执行任何管理操作；
- 在子进程退出或 server shutdown 后继续使用。

TACOS API 始终要求用户 Bearer token，空 token 不绕过认证；provider token 不是用户 token，不扩大也不替代用户 token 的权限。

撤销后的表现分两种：server 仍在监听时，旧 token 的 `secrets:read` 返回 401；进程退出后端口已关闭，客户端得到的是连接失败，而不是 HTTP 401。两者都表示 token 已失效，但测试和日志不应混为一谈。

### 5.3 信任边界

provider session 只向子进程环境注入 `MGCL_PROVIDER_ENDPOINT` 和 `MGCL_PROVIDER_TOKEN`，不注入 Secret value；`mgcl run` 现有的 `TF_CLOUD_HOSTNAME`、`TF_CLI_CONFIG_FILE` 等 TF_* 注入不变。Secret value 由 provider 通过 API/RPC 读取后只存在于 tofu/provider 内存；只有 HCL 显式把 value 传给 provisioner、connection、resource 参数等时，才会进入对应配置或进程。`mgcl run` 信任当前子进程树，但不构成同用户进程隔离边界。同一 OS 用户的其他进程可能读取环境、进程内存、临时文件或 `mgcl.db` 与私钥；日志、stdout/stderr、`TF_LOG`、崩溃报告和 provisioner 输出也可能泄露值。

provider token 是短期、只读的，能缩小泄露窗口和影响面，但不能阻止同用户攻击者。provider endpoint 是从实际 listener 推导的 HTTP URL；当它为非 loopback 时，Secret value 和 provider token 在传输中不加密。`--server` 可能是 HTTP 或 HTTPS；只有其实际 host 为非 loopback 且 scheme 为 HTTP 时，TACOS state 和认证流量才经明文传输。是否承担这些风险由启用安全门的用户决定。

## 6. Provider API

### 6.1 端点

```http
POST /api/mgcl/v1/secrets:read
Authorization: Bearer <provider-token>
Content-Type: application/json
Cache-Control: no-store

{"name":"bootstrap"}
```

成功响应：

```json
{
  "name": "bootstrap",
  "values": {
    "access_key_id": "AKIA...",
    "secret_access_key": "..."
  }
}
```

约束：

- `values` 是当前最新 KV 的 JSON object；空 Secret 返回 `{}`，不是 `null`；
- value 原样作为 UTF-8 JSON string，不 base64、不裁剪、不做规范化；
- 响应头必须包含 `Cache-Control: no-store`；可附加 `Pragma: no-cache`、`X-Content-Type-Options: nosniff`；
- 请求体限制 8 KiB，读取时使用 `http.MaxBytesReader`；
- 响应受第 3.2 节的大小限制约束；客户端可设 2 MiB 上限，以容纳 JSON 转义膨胀；
- 服务端不缓存 Secret；每次请求读取当前数据库快照并解密最新值；
- 不记录请求体、响应体、Authorization、token 或 value；可以记录方法、路径、状态码、耗时和 Secret 名称等非秘密元数据；
- 不提供 Secret 列表或管理 API。管理只通过离线 CLI。

### 6.2 错误

| 状态 | 场景 | 响应 |
|---|---|---|
| 400 | JSON 非法、缺少 name、name 语法错误 | 简短错误，不回显 value/token |
| 401 | token 缺失、格式错误或已失效 | 简短错误 |
| 404 | Secret 不存在 | 简短错误 |
| 405 | 非 POST | 简短错误 |
| 413 | 请求体超过限制 | 简短错误 |
| 429 | 可选限流 | 首版可不启用 |
| 500 | 解密失败、envelope 损坏、数据库错误 | 通用错误，不返回明文或密钥材料 |

错误响应使用简短稳定 JSON，例如 `{"error":"secret not found"}`。错误正文不得包含 token、value、ciphertext、nonce、DEK 或 wrapped key。

## 7. 非 loopback 安全门

`mgcl run` 在加载私钥、打开数据库和监听端口之前，检查 flag、环境变量与 `.mgcl.yaml` 合并后的有效 listen host 和有效 serverURL，而不是只检查显式 `--host`。

检查范围：

- 有效 listen host：合并后的 `--host`/环境变量/`.mgcl.yaml` 值，按 `host:port` 或 `[host]:port` 提取 host；
- 有效 serverURL：显式 `--server`/环境变量/`.mgcl.yaml` 的 `server` 值；为空时由 listener 推导，不再额外检查；非空时按现有 `hostFromServerURL` 规则取 host[:port]，再去掉端口得到 host；无法解析按配置错误处理；
- `--server` 只是 TACOS 对外 URL（discovery、upload/download 等），不用于 `MGCL_PROVIDER_ENDPOINT`，但它的 host 暴露同样受安全门约束。

只有 listen host 和 serverURL host 都逐字节等于 `127.0.0.1` 时免确认。以下都视为非 loopback：`localhost`、`::1`、`0.0.0.0`、`::`、`127.0.0.2`、空 host、LAN IP、公网 IP、任意主机名。因此 `--host 127.0.0.1:0 --server http://192.0.2.10:1217` 必须触发安全门，不能只检查 loopback listener。

对非 loopback：

1. 如果精确传入 `--yes-i-know-it-will-publish-state-and-secrets`，跳过交互确认，但仍向 stderr 打印公开 state 与 secrets 的警告；
2. 否则，如果 stdin 是 TTY，向 stderr 显示将公开 state 与 secrets 的警告，要求用户确认；只有 `y` 或 `yes`（不区分大小写）才继续；
3. 否则（非 TTY 且没有跳过 flag）直接失败，不加载私钥、不打开数据库、不监听端口；
4. 无法解析的 listen host 或 serverURL 按配置错误失败，不进入确认流程。

跳过 flag 保持用户选定的精确名称 `--yes-i-know-it-will-publish-state-and-secrets`，没有短形式，不能来自环境变量或 `.mgcl.yaml`。提示和错误只显示有效 host，不显示 Secret、token、私钥或数据库内容。

该安全门放宽的是网络暴露面、token 被截获和网络明文风险，不是认证：Secret API 仍要求有效 provider token，不应描述成无认证公开 Secret。provider endpoint 始终是 HTTP；`--server` 的明文风险取决于其实际 scheme。该安全门明确允许经确认后的非 loopback 服务，替代早期“secret API 永远只限 loopback”的提议，但不把非 loopback 变成安全默认。

## 8. 加密设计

### 8.1 不变量

1. 数据库中的每个 KV value 只要存在，就必须是 AES-256-GCM 密文；不能写明文，也不能把解密失败降级为明文返回。
2. 每个 Secret 对应一把独立 32-byte 随机 Secret DEK；不同 Secret 不复用 DEK。
3. 同一 Secret 的所有 KV 共用该 Secret 的 DEK，但每次加密使用独立随机 12-byte nonce。
4. Secret DEK 绝不复用 State Lineage 的 Lineage Key，也不从 state 密钥、workspace 或 lineage 派生。
5. 首版每个 Secret DEK 只包裹给一个 Recipient，即当前加载的 SSH 身份。
6. Secret 名称、KV key、时间戳、fingerprint 等元数据可以明文；value 必须加密。
7. 只保留最新 KV；覆盖和删除都不产生历史。
8. 认证失败、错误 Recipient、损坏 envelope 必须失败关闭，不能返回部分明文或旧值。

### 8.2 建议表模型

以下 schema 已实现；表与 TACOS state 表共用 `mgcl.db`，但密钥元数据独立：

```sql
CREATE TABLE IF NOT EXISTS secrets (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL UNIQUE,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS secret_deks (
    secret_id TEXT PRIMARY KEY REFERENCES secrets(id) ON DELETE CASCADE,
    dek_fingerprint TEXT NOT NULL UNIQUE,
    recipient_fingerprint TEXT NOT NULL,
    suite INTEGER NOT NULL,
    ephemeral_public_key BLOB NOT NULL,
    hkdf_salt BLOB NOT NULL,
    nonce BLOB NOT NULL,
    wrapped_dek BLOB NOT NULL,
    created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS secret_kvs (
    secret_id TEXT NOT NULL REFERENCES secrets(id) ON DELETE CASCADE,
    key TEXT NOT NULL,
    suite INTEGER NOT NULL,
    nonce BLOB NOT NULL,
    ciphertext BLOB NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    PRIMARY KEY (secret_id, key)
);
```

约束：

- `secrets.id` 是不可变 UUID，`name` 唯一；首版不提供 rename；
- `secret_deks.dek_fingerprint` 唯一，`recipient_fingerprint` 不唯一；
- v1 `suite = 1`；
- fixed length：`ephemeral_public_key` 32 bytes、`hkdf_salt` 32 bytes、`nonce` 12 bytes、`wrapped_dek` 48 bytes；KV `nonce` 12 bytes，`ciphertext` 至少 16 bytes（GCM tag）；
- `secret_kvs` 不保存明文 value、DEK、私钥或 passphrase；
- 启用 SQLite foreign keys；删除 Secret 时级联删除 DEK 与 KV。

### 8.3 Secret DEK 与包裹

Secret DEK 由 `crypto/rand` 生成 32 bytes。fingerprint 使用独立 domain label：

```text
SHA256("openmgcl/secret-dek-fingerprint/v1\x00" || DEK)
```

文本编码沿用 `SHA256:` + base64 RawStdEncoding，不带 padding。解包后必须重新计算并匹配数据库中的 fingerprint。

Ed25519 → X25519、ephemeral-static ECDH、HKDF-SHA-256、AES-256-GCM 和 canonical field 编码沿用 [state-encryption.md](state-encryption.md) 第 4 至 6 节，但必须使用独立的 Secret label，不能复用 state 的 wrap/payload label：

```text
wrap_info = "openmgcl/secret-wrap/v1\x00" ||
            pkE || pkR || raw_recipient_fingerprint ||
            field(secret_id) || raw_dek_fingerprint

wrap_aad  = "openmgcl/secret-wrap-aad/v1\x00" ||
            field(secret_id) || raw_dek_fingerprint ||
            raw_recipient_fingerprint || pkE
```

`KEK = HKDF-SHA-256(Z, salt=hkdf_salt, info=wrap_info, length=32)`；`wrapped_dek = AES-256-GCM(KEK, nonce, DEK, wrap_aad)`，其中 nonce 为随机 12 bytes，结果为 32-byte ciphertext + 16-byte tag。

解包前要求数据库中的 `recipient_fingerprint` 与当前身份一致；不一致返回 key-access denied。解包后验证 DEK fingerprint，失败按数据损坏处理。不得尝试用错误 Recipient 继续解密 KV。

### 8.4 KV 加密

KV 直接使用 Secret DEK 作为 AES-256-GCM key，不再派生 payload 子密钥。每次写入生成新的随机 12-byte nonce。AAD 必须绑定 Secret 身份、KV key 和版本标签：

```text
value_aad = "openmgcl/secret-value/v1\x00" ||
            field(secret_id) ||
            field(kv_key) ||
            field("v1")
```

`ciphertext` 为 GCM 输出加 16-byte tag。解密时：

- 要求 `suite = 1`、nonce 长度为 12、ciphertext 至少 16 bytes；
- 使用当前数据库行的 `secret_id`、`key` 和固定版本标签构造 AAD；
- AEAD 认证失败、长度错误、suite 不支持、上下文不匹配都返回损坏/认证错误；
- 不得返回明文、部分明文、旧版本或空值替代；
- 允许零长度 value，对应 16-byte tag 的密文。

AAD 不包含 Secret name：name 不是身份，`secret_id` 才是不变身份，即使未来支持 rename 也不影响密文。把密文移动到另一个 Secret 或 key 会因 AAD 不匹配而失败。

### 8.5 事务与并发

所有写操作在单个 SQLite 事务中完成，使用 `BEGIN IMMEDIATE` 和有限 busy timeout，避免 CLI 与运行中的 server 并发写时静默丢失更新。

- `list`、`show`：只读明文元数据；不加载私钥，不创建缺失 DB，不读取或解密 value。
- `create`：加载当前 Recipient；缺失 DB 时初始化 schema；检查名称唯一；生成 DEK；按当前 Recipient 包裹；插入 `secrets` 与 `secret_deks`；提交。重名、唯一约束或包裹失败时整体回滚，不留下孤儿行。
- `set`：按名称读取 Secret 和 DEK；解锁并验证 Recipient；加密；插入或覆盖 `secret_kvs`；更新 `secrets.updated_at`；提交。Recipient 不匹配或解锁失败时不写入。Secret 不存在时失败，不自动创建。并发覆盖同一 key 时后提交者获胜，不保留历史。
- `get`：在一致快照中读取 Secret、DEK 与 KV；解锁并验证 Recipient，再认证解密；不写库。行在读取前被删除时返回 not found，不能返回其他 Secret 的值。
- `unset`：在进入写事务前读取并成功解锁目标 Secret 的 DEK，验证 Recipient；随后删除目标 KV 并更新 Secret `updated_at`。wrong recipient 直接失败，不执行删除。Secret 或 key 不存在时失败并回滚。
- `delete`：在进入删除写事务前读取并成功解锁目标 Secret 的 DEK，验证 Recipient；随后删除 Secret，级联删除 DEK 与 KV。wrong recipient 直接失败，不执行删除。不存在时失败。
- 并发 `create` 同名 Secret 时由唯一约束保证只有一个成功；另一个返回冲突。
- 加密失败、认证失败或事务失败不得提交部分写入，也不得留下未包裹的 DEK。

允许进程内缓存已解锁的 Secret DEK，但必须：

- 以 `secret_id` + `dek_fingerprint` 为键，不能只按 name；
- 只在成功解包或事务提交后更新；
- 不把 DEK 返回给 CLI 输出、HTTP handler 或调试接口；
- 不承诺 Go 内存清零，只要求不序列化、不记录、不无必要复制。

首版 DEK 不轮换，因此不需要跨进程缓存失效协议；删除后同名重建会得到新的 `secret_id` 和 DEK，旧缓存不会被新 Secret 命中。

### 8.6 错误边界

| 场景 | 行为 |
|---|---|
| `list`/`show` 的 DB 不存在 | not found；不加载私钥，不建库 |
| `set`/`get`/`unset`/`delete` 的 DB 不存在 | not found；不隐式建库 |
| Secret 或 KV 不存在 | CLI 非零退出；API 404；stdout 为空 |
| `create` 重名 | 冲突错误；不覆盖 |
| `set` 时 Secret 不存在 | 错误；不自动创建 |
| 当前 Recipient 与 wrapped DEK 不匹配 | key-access denied；不返回明文；`unset`/`delete` 不修改 |
| wrapped DEK 缺失、长度错误、fingerprint 不匹配 | 数据损坏/密钥错误；不回退 |
| KV nonce 长度错误、ciphertext 过短、AEAD/AAD 失败 | 数据损坏；不返回明文 |
| value 不是合法 UTF-8 或超过大小限制 | 写入前拒绝 |
| 数据库路径未配置 | 命令失败，不建库 |
| `create`/`set`/`get`/`unset`/`delete` 的私钥缺失、权限过宽、格式错误、passphrase 错误 | 命令失败，不打开/创建数据库；`list`/`show` 不受影响 |
| 删除未确认或非 TTY 未给 `--yes` | 取消删除并非零退出 |
| provider token 缺失、非法或已失效 | API 401 |
| provider token 用于 state 或管理路由 | 不被接受 |

错误消息可以显示 Secret name、KV key、文件路径和公开 fingerprint，但不得显示 value、DEK、wrapped key、nonce、token 或私钥。

## 9. Provider 设计合同

### 9.1 OpenTofu 调研事实

- 本地 `tofu plan` / `tofu apply` 不读取 cloud workspace variables，不能靠 cloud variables 提供 bootstrap credentials。
- 普通 `sensitive` variable 只控制 UI 展示，不保证值被排除出 plan/state；当值进入持久化上下文时会以明文保存，因此不能安全传递 bootstrap credentials。
- provider function 的返回值不能标记为 sensitive 或 ephemeral，可能被写入 plan/state。
- 普通 data source 的值会进入 state。
- ephemeral resource 需要 OpenTofu >=1.11。它的值可以用于其他 provider 配置、provisioner、connection、write-only 属性和 ephemeral 输出；核心会阻止把 ephemeral 值写入 plan/state，并在非 ephemeral 上下文误用时直接报错。
- ephemeral resource 的生命周期是 Validate → Open → Close；`tofu validate` 只做配置校验，不应触发 Secret 读取。plan 和 apply 各自会在需要时 Open，因此每次都能取得当时的最新值。

### 9.2 HCL 示例

以下 AWS 配置只是示例，设计本身跨云：

```hcl
terraform {
  required_providers {
    mgcl = {
      source = "geektr-cloud/mgcl"
    }
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

其他云 provider 同样从 `ephemeral.mgcl_secret.bootstrap.values["<key>"]` 读取。provider 不应要求把 token 写入 HCL；token 只从环境读取。

### 9.3 provider 行为

独立 provider 仓库为 `github.com/geektr-cloud/terraform-provider-mgcl`，provider local name 为 `mgcl`。

- 首版只在 `mgcl run -- <command>` 子进程中工作；常驻 `mgcl run` 不签发 provider token，因此不能使用 provider；
- 只提供 ephemeral resource `mgcl_secret`，不注册同名普通 resource 或 data source；
- `provider "mgcl" {}` 不需要必填参数；从 `MGCL_PROVIDER_ENDPOINT` 和 `MGCL_PROVIDER_TOKEN` 读取连接信息；Secret value 不来自环境；
- provider `Configure` 不访问网络；缺少 endpoint/token 时允许 `tofu validate` 通过，由 Open 返回清晰错误；
- `mgcl_secret` schema：`name` 为必填 string，`values` 为 computed map(string)，并标记 sensitive；
- Validate 不调用 Secret API；name 值 known 时校验语法，unknown 时跳过语法检查；Open 时再要求 name known 且合法；
- Open 调用一次 `secrets:read`，返回当前值；不写数据库、不缓存跨 Open 的值；
- Close 无持久化操作；
- `resource "mgcl_secret"` 或因误用 ephemeral 值而无法满足普通 resource 参数时，应由 provider 未注册该 resource 或 OpenTofu 核心直接报错；
- provider 错误诊断不得包含 token 或 value。

### 9.4 `pkg/providerapi` 合同

主仓已提供 `pkg/providerapi`，供独立 provider 仓库导入。该包 stdlib-only，不依赖 mgcl 的内部包，也不引入第三方 HTTP 客户端。

至少包含：

```go
const (
    EnvEndpoint     = "MGCL_PROVIDER_ENDPOINT"
    EnvToken        = "MGCL_PROVIDER_TOKEN"
    SecretsReadPath = "/api/mgcl/v1/secrets:read"
)

type ReadSecretRequest struct {
    Name string `json:"name"`
}

type ReadSecretResponse struct {
    Name   string            `json:"name"`
    Values map[string]string `json:"values"`
}

type Client struct {
    Endpoint   string
    Token      string
    HTTPClient *http.Client
}

func (c *Client) ReadSecret(ctx context.Context, name string) (*ReadSecretResponse, error)
```

最小 client 要求：

- 使用 `Authorization: Bearer`，设置 JSON `Content-Type`；
- 不跟随重定向，避免把 token 发往其他 host；
- 默认超时有限，例如 10 秒；
- 限制响应体大小并关闭 body；
- 非 2xx 返回不含 token/value 的错误；
- 不记录请求或响应中的秘密。

设计已实现 `cmd/mgcl` 入口；安装命令为 `go install github.com/geektr-cloud/openmgcl/cmd/mgcl@<version>`，安装出的命令名是 `mgcl`。仓库根 `main.go` 与 `cmd/mgcl` 共用 `cmd.Main`，避免退出码处理漂移。

### 9.5 开发期集成与发布

当前仓不采用 monorepo、provider 内嵌或模拟 registry。开发期在独立 provider 仓库中：

1. 用 `go install`/`go build` 生成 `terraform-provider-mgcl` 二进制。
2. 快速开发使用 `dev_overrides`：CLI config 把 `geektr-cloud/mgcl` 指向包含该二进制的目录。OpenTofu 会直接使用本地二进制，并对该 provider 跳过 `tofu init` 的安装/校验。

   ```hcl
   # dev.tfrc
   provider_installation {
     dev_overrides {
       "geektr-cloud/mgcl" = "/home/me/src/terraform-provider-mgcl/bin"
     }
     direct {}
   }
   ```

   ```text
   /home/me/src/terraform-provider-mgcl/bin/terraform-provider-mgcl
   ```

3. 需要走 `tofu init` 的集成测试使用符合 OpenTofu 布局的 filesystem mirror，而不是把目录简单指向二进制。最小 unpacked 布局：

   ```text
   /opt/tofu-mirror/registry.opentofu.org/geektr-cloud/mgcl/0.1.0/linux_amd64/terraform-provider-mgcl_v0.1.0
   ```

   或 packed 布局：

   ```text
   /opt/tofu-mirror/registry.opentofu.org/geektr-cloud/mgcl/terraform-provider-mgcl_0.1.0_linux_amd64.zip
   ```

   ```hcl
   # dev.tfrc（filesystem mirror）
   provider_installation {
     filesystem_mirror {
       path    = "/opt/tofu-mirror"
       include = ["registry.opentofu.org/geektr-cloud/mgcl"]
     }
     direct {
       exclude = ["registry.opentofu.org/geektr-cloud/mgcl"]
     }
   }
   ```

4. 在 `mgcl run -- <command>` 场景必须显式设置 `TF_CLI_CONFIG_FILE`：

   ```bash
   export TF_CLI_CONFIG_FILE=/path/to/dev.tfrc
   mgcl run --db ./mgcl.db -- tofu plan
   ```

   mgcl 只从 `TF_CLI_CONFIG_FILE` 读取并合并现有 CLI config，不读取默认 `~/.tofurc`；仅依赖默认文件不会让 `provider_installation` 生效。

5. 用真实 OpenTofu >=1.11 跑 provider 合同测试和 e2e。
6. 不实现本地 registry 服务：`dev_overrides` 覆盖未发布 provider 并跳过 `tofu init`，filesystem mirror 覆盖需要 `tofu init` 的集成路径。

暂不发布到 OpenTofu Registry。未来发布 GitHub Release 并进入 Registry 后，才提供正式的无缝 `tofu init` 体验；在此之前，文档和错误提示必须明确需要 `dev_overrides` 或 filesystem mirror。

## 10. 验收与阶段

### 10.1 阶段

1. **主仓 secret store + CLI**：已完成。
2. **主仓 provider session API + `pkg/providerapi`**：已完成。
3. **独立 provider ephemeral resource**：已在独立仓实现，不属于主仓范围。
4. **真实 OpenTofu e2e**：provider 仓库负责；主仓已覆盖进程生命周期和 API 合同。
5. **Registry 发布**：未发布，不阻塞主仓功能。

### 10.2 必须完成的测试

CLI 与存储：

- `create` 空 Secret；`set` 新建/覆盖；`get` 输出与写入字节完全一致，不附加换行；
- 位置参数、`--stdin` 多行、TTY 无回显三种输入方式；互斥和非 TTY 无值报错；
- `list`/`show` 不加载私钥、不创建缺失 DB，缺失 DB 返回 not found；错误的 `--encryption-key-file` 不影响元数据读取；
- `create` 在指定路径缺失时可初始化 DB 并创建 Secret；`set`/`get`/`unset`/`delete` 在 DB 缺失时 not found 且不创建；
- `create` 加载当前 Recipient；`set`/`get`/`unset`/`delete` 加载并匹配当前 Recipient；错误 Recipient 时 `get` 不解密，`set`/`unset`/`delete` 不修改；
- `unset`/`delete` 在写事务前成功 unwrap；wrong recipient 不执行 DELETE/UPDATE；
- 带 passphrase 的私钥配合 `set --stdin` 时，未设置 `MGCL_ENCRYPTION_KEY_PASSPHRASE` 必须失败；设置后成功；非 TTY `delete --yes` 同理；
- `list`/`show` 不显示 value；`show` 的 JSON/YAML schema 稳定且没有 value/DEK/wrapped key；
- `unset`、`delete` 行为；`delete` 的 TTY 确认和非 TTY `--yes`；
- 名称/key/value 语法、长度、KV 数量和总大小限制；
- SQLite 原始 BLOB 不含可识别明文；`get` 与原文逐字节一致；
- 相同明文两次写入产生不同 nonce/ciphertext；AAD 任一字段改变都会解密失败；
- 错误 Recipient 无法读取既有 Secret，但可以创建和读取新 Secret；
- wrapped DEK/KV 任一长度、fingerprint、tag、nonce 损坏都失败关闭；
- golden test vectors 固定 DEK、ephemeral key、salt、nonce、AAD 和精确密文字节，防止格式漂移；
- 事务失败不留下孤儿 Secret、DEK 或 KV；
- 删除/覆盖不产生历史行；备份恢复只依赖 db + 私钥；
- secret 写命令实现后，`create`/`set`/`unset`/`delete` 复用 [database-backup.md](database-backup.md) 的备份层；`list`/`show`/`get` 不触发备份；备份失败 fail-closed 且不声称成功；空 `backup_to` 不改变原行为。

Provider session 与 API：

- token 缺失、非法、子进程退出后、server shutdown 后均返回 401；失效边界只由进程生命周期决定；
- token 不能访问 state、workspace、lock 或其他管理路由；
- 成功读取返回最新值；空 Secret 返回 `{}`；未知 Secret 404；
- 请求体超限、JSON 非法、name 非法返回 400/413；
- 响应 `Cache-Control: no-store`；日志不含 token/value；
- provider endpoint 从实际 listener 推导，不使用 `--server`；loopback、wildcard、特定非 loopback listener 三种情况分别验证；
- 非 loopback 门：`localhost`、`::1`、`0.0.0.0`、LAN IP、配置文件中的 host 都触发；`--host 127.0.0.1:0 --server http://<LAN-IP>/...` 也触发；非 TTY 无 flag 在加载私钥/DB 前失败；TTY 确认流程；精确 flag 可跳过；提示和错误不含秘密；
- 未配置 DB 时 secret CLI 失败；`list`/`show` 在 DB 缺失时 not found 且不创建；私钥错误时 `create`/`set`/`get`/`unset`/`delete` 不创建/修改 DB；
- CLI 输出和错误不泄露 value/token。

真实 OpenTofu e2e：

- provider 认证可用；
- `tofu validate` 不调用 `secrets:read`；unknown name 跳过语法检查，Open 时再要求 known/合法；
- 每次 `tofu plan` 和 `tofu apply` 都重新 Open，并在两次之间更新 Secret 后读到新值；
- Secret value 不出现在 plan 文件、state 文件、lock 文件、stdout、stderr 或 `TF_LOG` 输出中；
- 把 `mgcl_secret` 误用为普通 resource，或把 ephemeral 值用于不允许的普通参数时失败；
- 非 loopback 安全门在真实 `mgcl run` 启动路径上生效，包括 `--host 127.0.0.1:0 --server http://<LAN-IP>/...` 组合；
- token 失效和权限限制在 API 测试及真实进程生命周期中验证：子进程退出后、server shutdown 后均 401；
- `dev_overrides` 路径跳过 `tofu init`；filesystem mirror 的 packed 或 unpacked 布局能完成真实 `tofu init`；`mgcl run -- ...` 通过显式 `TF_CLI_CONFIG_FILE` 使用这些配置；
- e2e 使用临时生成的 0600 Ed25519 私钥，不依赖开发者真实 `~/.ssh/id_ed25519`、网络或固定 Home。

通用检查：

- 主仓 `go vet ./...`、`go test -count=1 ./...` 通过；
- provider 仓库的合同测试和真实 OpenTofu e2e 通过；
- 涉及格式、AAD、schema 或错误语义的实现偏离，必须先修改本文再改代码，不能让实现与文档各说一套。

### 10.3 实现完成后必须同步的文档

主仓实现已完成，下列文件已同步：

- `README.md`：Secret/Provider session 的用户可见流程、`mgcl secret` 命令、`mgcl run --` 限制、非 loopback 安全门和风险边界；
- `UserStory.md`：从创建 bootstrap Secret 到 `tofu plan/apply` 的最简流程，以及私钥丢失和备份要求；
- `mgcl --help` 和各子命令 `--help`：`secret` 子命令、`--stdin`、`--yes`、`--yes-i-know-it-will-publish-state-and-secrets` 和 `--encryption-key-file`；
- release notes：主仓没有既有 changelog/release-notes 规范，因此未新建发布系统；发布说明应注明这是新增功能、provider 需要 OpenTofu >=1.11，且不自动接管 state outputs。

## 11. 明确超出范围

- CI、无人值守、ssh-agent，或通用云凭据环境变量注入；
- 多 Recipient、授权、撤销、grant/revoke API；
- Secret DEK rotation、rewrap、跨私钥恢复；
- Secret/KV 历史、版本、回滚；
- SQLite/WAL/备份/磁盘的安全擦除；
- 自动导入或接管 state outputs；
- provider function、普通 data source、cloud workspace variables；
- 本地 registry 模拟、provider 内嵌、monorepo 集成；
- HTTP Secret 管理 API；
- 在 Registry 发布前宣称可直接 `tofu init`。

## 12. 参考

- [ADR-0004：按 State Lineage 强制加密 state](adr/0004-encrypt-state-by-lineage.md)
- [ADR-0006：Bootstrap secrets 由独立 ephemeral provider 提供](adr/0006-bootstrap-secrets-and-ephemeral-provider.md)
- [ADR-0007：SQLite 备份目标采用 sqlite: 与 path: 两种不可互换语义](adr/0007-sqlite-backup-destinations.md)
- [Database Backup v1 实施规范](database-backup.md)
- [State Encryption v1 实施规范](state-encryption.md)
- [OpenTofu Ephemerality](https://opentofu.org/docs/v1.11/language/ephemerality/)
- [OpenTofu Ephemeral resources](https://opentofu.org/docs/v1.11/language/ephemerality/ephemeral-resources/)
- [OpenTofu Provider Configuration](https://opentofu.org/docs/v1.11/language/providers/configuration/)

## 13. 主仓实现状态

已实现（主仓）：

- 存储：`internal/tacos/secret_store.go`、`internal/tacos/secret_envelope.go`。schema 为 `secrets` / `secret_deks` / `secret_kvs`，与 TACOS state 共用 `mgcl.db`、SSH identity 和迁移，但 label/AAD/fingerprint 独立。写路径使用 `BEGIN IMMEDIATE`、唯一约束、外键级联和事务。
- CLI：`cmd/secret.go`、`cmd/secret_output.go`。`list/show/create/set/get/unset/delete`，固定 flag、配置优先级、TTY 无回显、`--stdin`、`--yes`、备份接入和缺失 DB 边界。
- Provider session：`internal/tacos/provider_session.go`、`internal/tacos/provider.go`。`POST /api/mgcl/v1/secrets:read`，8 KiB 请求限制、`Cache-Control: no-store`、200/400/401/404/405/413/500，token 只存 SHA-256 摘要；provider token 被 TACOS 路由拒绝，用户 token 不能读 Secret。
- `mgcl run`：只对有子进程的运行签发 provider session，注入 `MGCL_PROVIDER_ENDPOINT` / `MGCL_PROVIDER_TOKEN`，endpoint 从实际 listener 推导；用户 TACOS token 未配置时每次 run 自动生成，子进程模式只写入临时 CLI config，常驻模式在监听后打印一次；非 loopback 安全门在加载私钥、打开 DB、监听前执行。
- 客户端：`pkg/providerapi`（stdlib-only、有限超时、不跟随重定向、2 MiB 响应上限）；`cmd/mgcl` 安装入口与根入口共用 `cmd.Main`。

依赖外部仓库：

- `github.com/geektr-cloud/terraform-provider-mgcl` 的 `mgcl_secret` ephemeral resource、Validate/Open/Close 和真实 OpenTofu e2e 已在独立仓实现；主仓不修改该仓库。
- 未发布 OpenTofu Registry；开发期使用 `dev_overrides` 或 filesystem mirror，`mgcl run --` 通过显式 `TF_CLI_CONFIG_FILE` 使用对应配置。

首版明确未实现：多 Recipient、DEK rotation/rewrap、历史、HTTP 管理 API、provider function/普通 data source/cloud variables、本地 registry。
