# State 管理命令

`mgcl state` 是离线管理同一个 `mgcl.db` 中 workspace 与 state version 的命令组。它不启动 HTTP 服务、不需要 token，直接打开数据库。领域术语见 [CONTEXT.md](../CONTEXT.md)，加密格式见 [state-encryption.md](state-encryption.md)，写会话备份见 [database-backup.md](database-backup.md)。

## 1. 通用规则

### 1.1 State reference

所有命令的 workspace 参数都是 **State reference**：`<organization>/<workspace>`。字符串必须恰好包含一个 `/`，按第一个 `/` 拆分；organization 与 workspace 都不能为空，拆分后也不能再含 `/`。workspace 名称还必须满足 `name == url.PathEscape(name)`，与 OpenTofu 的 workspace 校验一致：`/`、空格、`%`、`?`、`#` 和非 ASCII 字符都非法，常规 OpenTofu 名称保持有效。不合法的引用在打开数据库前拒绝，且不会隐式创建 workspace。organization 规则不变，既有数据不做迁移。

### 1.2 配置

`state` 命令组继承 `mgcl run` 的配置解析：

- 持久 flag：`--db` / `-d`、`--encryption-key-file`、`--backup-to`；
- `<git-root>/.mgcl.yaml` 的 `db`、`encryption_key_file`、`backup_to`；在子目录运行也读取 Git 根配置，非 Git 目录不读 YAML；
- 环境变量 `MGCL_DB`、`MGCL_ENCRYPTION_KEY_FILE`。

优先级为显式 flag > `MGCL_*` > `.mgcl.yaml` > 内置默认值。`db` 没有内置默认值，必须提供；路径解析规则与 `mgcl run` 相同。命令不隐式创建缺失的数据库，数据库不存在时报错。

私钥带 passphrase 且 stdin 不是 TTY 时，必须通过 `MGCL_ENCRYPTION_KEY_PASSPHRASE` 提供口令；需要私钥的命令（`export` 与写命令）都适用，命令不会在非 TTY 上提示输入口令。

### 1.3 只读命令与写命令

| 命令 | 读写性质 | 私钥 |
|---|---|---|
| `list`、`stats`、`info` | 只读 | 不加载 |
| `export` | 只读 | 需要 Recipient 私钥 |
| `prune`、`delete`、`unlock`、`rename` | 写会话（`unlock` 仅锁定时） | 需要 Recipient 私钥 |

只读命令不创建、迁移或备份数据库，也不创建备份目标；数据库以 `mode=ro` 只读打开，未 checkpoint 的 WAL 数据仍可见。WAL 数据库缺少 `-shm` 时 SQLite 可能需要在数据库目录创建它，因此目录可能需要可写。

`prune`、`delete`、`unlock`、`rename` 是 **Write session**，与 `mgcl run`、`mgcl secret` 写命令复用同一备份层：配置了 `--backup-to` / `backup_to` 时按 [database-backup.md](database-backup.md) 同步，备份失败 fail-closed。它们先做只读预检，确认后才打开写会话和备份。`unlock` 只在确实清除 lock 时写库；workspace 未锁定时直接成功，不打开写会话、不触发备份。`rename` 不需要确认，只做只读预检后直接进入写会话。同一 `mgcl.db` 同时只应有一个写进程；备份层不做跨进程协调。

写命令只加载并验证加密私钥可用，不校验 Recipient，也不会为该私钥新增 Recipient 写授权。只有 `export` 在解密 current JSON blob 时校验对应 Lineage 的 Recipient；`prune`、`delete`、`unlock`、`rename` 不做解密。

数据库中存在旧版 secret schema（`secret_kvs` 缺少 `is_ephemeral`）时，所有 `state` 命令都会在打开或预检阶段报错并要求运行 `mgcl-migrate`；这发生在任何确认、写会话或备份之前。

### 1.4 Lock 与写进程

`prune`、`delete` 和 `rename` 在 workspace 存在 lock 时直接拒绝，不自动 unlock。遇到这种情况必须先停止所有写进程（包括 `mgcl run`、`mgcl secret` 写命令和另一次 `mgcl state` 写命令），再执行 `mgcl state unlock`，最后重试。`unlock` 只清除 lock，不检查锁的持有者。

## 2. `mgcl state list`

列出数据库中的全部 workspace，按 State reference 排序，逐个输出一行；没有任何 version 的 workspace 也要出现。

- `STATE`：完整的 State reference（`org/workspace`）。
- `VERSIONS`：该 workspace 的 finalized 行数。replacement 每次 finalize 都计入；同一 serial 出现多行时逐行计数，不按 serial 去重。
- `PENDING`：pending 行数，单独一列，不并入 `VERSIONS`。

只读，不加载私钥。

## 3. `mgcl state stats`

按 workspace 逐行输出，包含 State reference 与 `SIZE`；行集合与 `list` 一致，计数列含义也相同。

`SIZE` 是该 workspace 全部版本（含 pending 与 replacement）在 `state`、`json_state`、`json_state_outputs` 三列中实际存储的加密 blob 字节长度总和。它：

- 不包含 SQLite 页头、索引、空闲页、WAL 或备份文件；
- 因此不等于 `mgcl.db` 的文件大小，也不是明文 state 大小；
- 用 `humanize.IBytes` 输出 1024 进制的 IEC 文本（`B`、`KiB`、`MiB`、`GiB`）。

只读，不加载私钥。prune 后该值下降不代表数据库文件缩小。

## 4. `mgcl state info <ref>`

列出该 workspace 的全部版本，按创建顺序倒序（最新在前）；pending 与 replacement 版本都单独列出。每行包含 `ID`、`SERIAL`、`STATUS`（pending 或 finalized）、`CURRENT` 与 `CREATED`。workspace 不存在时报错；存在但没有版本时只输出表头。

`CURRENT` 是**创建顺序最新（rowid 最大）的 finalized 版本**，不是 `MAX(serial)`：serial 由客户端生成，服务端允许更低或相同 serial 的更新。只读，不加载私钥。

## 5. `mgcl state export <ref> [--outputs] [-f <path>]`

导出 current finalized 版本的 JSON state：

- 默认导出 `json_state`；`--outputs` 改为导出 `json_state_outputs`；
- 只导出 current finalized 版本，不导出 pending、历史版本或 replacement 旧行；
- 没有 raw `state` 回退：目标列为空或缺失时报错，不退回导出原始 state；workspace 或版本不存在同样报错；
- 写入 stdout 时输出原始 JSON 字节，不追加换行，也不添加包装或诊断文本。

`-f` / `--file <path>` 把同一份内容写到文件：创建新文件，权限 0600；目标已存在时拒绝覆盖；先写同目录临时文件再原子发布，失败时不留下半份目标文件。写文件时不向 stdout 输出 JSON。显式传入空的 `-f ""` / `--file ""` 是错误，不会回退到 stdout。

导出内容是解密后的明文，可能包含敏感值（尤其是 `--outputs`）。stdout 会进入终端、日志或管道，文件也不额外加密；按敏感数据处理。`export` 是只读命令，需要 Recipient 私钥解密，但不触发备份、不修改 schema。

## 6. `mgcl state prune <ref> --keeps <n>`

只删除 finalized 版本：按创建顺序保留最近 `n` 个，删除更早的 finalized。`--keeps` 是必填参数，`n` 必须 >= 1。

- pending 版本全部保持不变；
- current 永不删除（它是最近的 finalized，总在保留窗口内）；
- 同一 serial 的 replacement 旧行若落在保留窗口外，同样会被删除。

执行前显示目标 workspace 与将删除的版本数，并要求输入完整 State reference（字面 `org/workspace`）二次确认；输入不匹配则不执行。stdin 不是 TTY 时必须精确传入 `--yes-i-know-this-will-permanently-delete-this-state`，没有其他绕过方式。

workspace 有 lock 时拒绝执行，处理方式见 1.4。`prune` 是写会话，复用备份层。

它不 `VACUUM`：数据库文件不保证缩小，删除的空间可能留在空闲页、WAL 或既有备份中，不做安全擦除。它也不清理 Lineage Key：lineage 的密钥元数据保留，即使其全部旧版本被删除。

## 7. `mgcl state delete <ref>`

删除一个 workspace 及其全部 finalized 与 pending 版本。执行前显示目标 workspace 并要求输入完整 State reference 二次确认；stdin 不是 TTY 时同样必须精确传入 `--yes-i-know-this-will-permanently-delete-this-state`。

workspace 有 lock 时拒绝执行，处理方式见 1.4。`delete` 是写会话，复用备份层；不 `VACUUM`，文件不保证缩小，也不清理 Lineage Key。

## 8. `mgcl state unlock <ref>`

离线强制解锁：直接清除该 workspace 的 lock，不经过 TACOS API，也不需要服务端的 force-unlock 端点。执行前显示当前 lock 的信息（例如 lock ID、持有者与创建时间），并要求输入完整 State reference 二次确认。

- 锁定时必须使用交互终端；没有非交互绕过参数。stdin 不是 TTY 时直接拒绝；
- workspace 未锁定时幂等成功，不改库；即使 stdin 不是 TTY 也直接成功；
- 只清 lock，不终止任何进程，也不检查锁是否仍被某个活跃 `mgcl run` 持有。

只有确认后、且 workspace 确实有 lock 时，`unlock` 才打开写会话并复用备份层。它不会由 `prune`、`delete` 或 `rename` 自动触发；这些命令遇到 lock 只会拒绝。

## 9. `mgcl state rename <src> <dest>`

把 `<src>` 指向的 workspace 改名为 `<dest>`。两个参数都是完整 State reference；organization 与 workspace 名称可以同时改变。

- 只在一个 SQLite 事务中 `UPDATE` 原 `workspaces` 行的 `organization` 与 `name`：workspace ID、全部 finalized 与 pending 版本、version ID、`created_at`、加密 state blob、Lineage 与 Lineage Key 全部保留；不解密或重写 state。
- `<src>` 必须存在，否则报 not found；`<src>` 有 lock 时拒绝，并提示先执行 `mgcl state unlock <src>`。
- `<src>` 与 `<dest>` 相同时冲突；`<dest>` 已被任何 workspace 占用时同样冲突。两种情况都不覆盖、不合并、不改数据。
- 不合法的 State reference 在打开数据库前拒绝。
- 非破坏性操作：不做交互确认，不需要 `--yes`，可在非 TTY 执行，也不自动 unlock。
- `rename` 是写会话，复用备份层并遵循 fail-closed 错误报告：初始备份失败时不执行 rename；写会话关闭阶段的备份失败发生在数据库提交之后，只能保证命令返回错误，不能承诺回滚。成功后把 `Renamed state from <src> to <dest>.` 写到 stderr。
