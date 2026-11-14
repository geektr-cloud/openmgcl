# SQLite 备份目标 v1 实施规范

> **状态：已接受并实现。** `mgcl run` 与 `mgcl secret create/set/unset/delete` 的备份层均已实现。本文是 `sqlite:` / `path:` 备份目标的完整实现与验收合同。`--backup-to`、README、secret 命令和当前功能描述已同步。

决策原因见 [ADR-0007](adr/0007-sqlite-backup-destinations.md)，领域术语见 [CONTEXT.md](../CONTEXT.md)。Secret 接入约束见 [secret-management.md](secret-management.md)。

## 1. 范围与不变量

备份目标是写会话（write session）把当前 `mgcl.db` 覆盖复制到用户指定位置的可选功能。`mgcl run` 和 `mgcl secret create/set/unset/delete` 都接入同一备份层。只读命令和只读请求不触发备份。

不变量：

1. 备份源始终是同一个 `mgcl.db`；备份不会解密、重写或导出明文，内容沿用数据库现有的加密格式。
2. 每个备份目标都是“最新副本”的覆盖写入，没有历史版本、保留策略、轮换或内置恢复命令。恢复仍靠人工替换 `mgcl.db` 与私钥。
3. 目标只允许 `sqlite:<path>` 和 `path:<path>` 两种 scheme，语义固定，不能自动降级或互相替代。
4. 备份是异步的：写请求成功后目标可以暂时落后。只有干净关停（drain 成功）才承诺 sqlite 目标包含退出前已提交的写入，path 目标包含关停时的主数据库。
5. 备份失败 fail-closed：启动阶段失败不启动，运行阶段失败停止接受新写并终止写会话，退出码非零。不得静默 best-effort 或把失败当作成功。
6. 空备份列表不改变现有行为：不 probe、不创建 backup handle、不改变关闭顺序。

非目标：云端对象存储、压缩、额外加密、增量备份、版本保留、备份校验命令、自动恢复、跨进程备份协调、安全擦除（磁盘、WAL、目标文件）。

## 2. 术语与两种语义

**Write session（写会话）**：打开 `mgcl.db` 并可能提交写入的进程生命周期。`mgcl run` 是写会话，即使本次没有实际写入；`mgcl secret create/set/unset/delete` 也是写会话。`mgcl secret list/show/get` 等只读命令不是。

**Backup destination（备份目标）**：配置中的一个 `<scheme>:<path>` 条目。scheme 只允许 `sqlite` 和 `path`，首版不接受其他值。

| 行为 | `sqlite:<path>` | `path:<path>` |
|---|---|---|
| 同步机制 | SQLite Online Backup API | 普通文件 copy |
| 启动时 | 接受流量前做一次全量备份 | 只做同目录 RW 探测，不写正式目标；探测文件权限由 umask/默认 ACL 决定 |
| 运行中 | 每个成功写提交标 dirty，后台 worker 合并同步 | 不同步 |
| 关停时 | drain 最后一轮 dirty | 停止写、checkpoint、关主库后复制并 rename |
| 目标新鲜度 | 最多落后一个后台轮次；干净关停后为最新 | 上次干净关停时的快照；运行中始终是旧内容 |
| 适用环境 | 支持 SQLite 锁的本地文件系统 | 不支持 SQLite 锁的挂载，或只接受普通文件 I/O 的目标 |
| 目标形态 | 一个 SQLite 数据库文件 | 一个自包含的数据库文件，不依赖 sidecar |
| 失败暴露点 | 启动、运行中、drain | 启动 probe、关停 copy |
| 目标更新方式 | 原地覆盖 | 同目录临时文件 + rename 覆盖 |

两种 scheme 都是覆盖式最新副本，不做版本轮换。`sqlite:` 原地覆盖，失败后不保证目标仍等于上一份成功副本；`path:` 在 rename 前保留旧目标，rename 后若目录 fsync 失败，目标已是新内容但命令仍失败。

## 3. 配置

| 来源 | 名称 | 约束 |
|---|---|---|
| flag | `mgcl run --backup-to <destination>` / `mgcl secret ... --backup-to <destination>` | 可重复；出现至少一次时整体替换 YAML 数组 |
| `<git-root>/.mgcl.yaml` | `backup_to` | 字符串数组；相对路径按 Git 根目录解析 |
| 环境变量 | 无 | 首版不支持 `MGCL_BACKUP_TO`，设置了也不读取 |
| 默认值 | 无 | 缺失、`null` 或空数组表示不启用备份 |

优先级只有两层：显式 `--backup-to` 列表 > `.mgcl.yaml` 的 `backup_to` 数组。没有环境变量层，也没有默认目标。

规则：

- `--backup-to` 只接受分离形式（`--backup-to <value>`），可重复；`--` 之后的内容是子进程参数，不再解析为备份 flag。
- YAML `backup_to` 必须是字符串数组。标量、映射、含非字符串元素的数组都按配置错误拒绝；不能把单个字符串按空白拆成多个目标。
- 只要出现显式 `--backup-to`，就忽略 YAML 数组的条目，不做合并、追加或去重合并。YAML 文件本身仍须可读、可解析；`backup_to` 的类型错误仍是配置错误。
- 元素按第一个 `:` 拆成 scheme 与 path；scheme 大小写敏感，只接受 `sqlite` 和 `path`。没有冒号、空 scheme、空 path、含 NUL 的 path、未知 scheme 一律拒绝。path 本身可以包含 `:`。
- flag 中的相对 path 按进程当前工作目录解析；YAML 中的相对 path 按 Git 根目录解析。绝对 path 保持不变。非 Git 目录不读取 YAML，因此不存在 YAML 相对路径。
- 保持配置中的顺序；按规范化后的目标路径拒绝重复，而不是静默去重。同一路径分别写成 `sqlite:` 和 `path:` 也视为重复/冲突并拒绝。
- 所有目标校验和 path 探测在加载私钥、打开数据库、监听端口之前完成。配置或目标非法时不得创建/修改数据库、不得监听。

示例：

```yaml
# <git-root>/.mgcl.yaml
backup_to:
  - "sqlite:/srv/mgcl-backup/mgcl.db"
  - "path:/mnt/no-locks/backup/mgcl.db"
```

```bash
# flag 相对路径按当前工作目录
mgcl run --db ./mgcl.db \
  --backup-to sqlite:./backup/mgcl.db \
  --backup-to path:/mnt/no-locks/mgcl.db \
  -- tofu apply -auto-approve
```

## 4. 目标解析与启动校验

### 4.1 规范化

1. 按首次 `:` 拆分 scheme 与原始 path。
2. 相对 path 按来源基准转成绝对路径，再 `filepath.Clean`。
3. 目标存在时，尽量对完整路径做 `EvalSymlinks`；目标不存在时，对父目录做 `EvalSymlinks` 后拼回 basename。解析失败（例如父目录缺失）按配置错误处理。
4. 以“scheme + 规范化绝对路径”保存目标；顺序不变。用于重复和冲突检测的键是规范化路径，scheme 不参与键。
5. 目标 identity 还要覆盖它的 `-wal`、`-shm`、`-journal` 同目录路径，避免目标与源 sidecar 或另一目标的 sidecar 互相覆盖。

### 4.2 必须拒绝的配置

启动时对每个目标执行以下检查；任一失败都不得启动。备份目标被视为 mgcl 独占的覆盖副本，不应有其他进程同时打开或使用它：

- 目标路径等于源 `mgcl.db`，或等于源 `mgcl.db-wal`、`mgcl.db-shm`、`mgcl.db-journal`；源路径也按上述规范化规则比较，并在两者都存在时用 `os.SameFile` 检测同一文件。
- 目标与另一个目标规范化后相同；或目标路径/其 sidecar 与另一目标路径/其 sidecar 冲突。
- 目标是已存在的目录、symlink、设备、FIFO、socket 或其他非普通文件；目标的 `-wal`/`-shm`/`-journal` 是 symlink 或非普通文件同样拒绝。目标本身是 symlink 一律拒绝；父目录可以是 symlink，但按解析后的真实目录处理。
- 目标不存在但父目录不存在、不是目录，或不可访问。
- 目标已存在且是普通文件，但既不是空文件，也不是可打开的 SQLite 数据库。SQLite 校验至少在启动时执行 `PRAGMA quick_check` 并要求返回 `ok`；不能仅凭扩展名判断。对 `path:` 目标，校验不得依赖 SQLite 锁或创建目标 sidecar；使用 `immutable=1` 只读打开、header 检查或等价方式，不能因为挂载不支持 SQLite 锁而拒绝合法目标。
- 目标已存在且其 `-wal` 或 `-journal` 非空。`-shm` 不承载持久数据，但若存在非空 WAL，按前者拒绝；空 sidecar 不算数据依赖，可清理，非空 WAL 绝不能被当作可删除的缓存。
- Unix 上目标已存在且 `Nlink > 1`（硬链接），或与源/其他目标 `os.SameFile` 为同一文件；目标的 `-wal`/`-shm`/`-journal` 存在且 `Nlink > 1` 也拒绝。无法取得 `Nlink` 的平台跳过硬链接检查，但仍做路径和同文件检查。
- 目标或其目录的权限/ACL 导致当前进程无法完成所需操作（`path:` 需要目录的创建、写入和 rename 权限，`sqlite:` 需要目标可写）；具体探测方式见 4.3 和 5.1。不要求当前用户是目标文件的 owner。

目标已存在且是有效 SQLite DB 时允许覆盖；用户显式选择该路径即表示接受它作为备份目标。非空且非 SQLite 的文件拒绝，避免把任意用户文件当备份覆盖。对已存在目标的 SQLite 校验必须只读，不得在启动校验或 `path:` probe 阶段创建、截断、清理或删除目标 sidecar；空 sidecar 的清理只能发生在后续确认可写的初始备份/复制步骤。

### 4.3 `path:` 启动探测

对每个 `path:` 目标，在目标同目录创建一个唯一旁路文件：

- `O_CREATE|O_EXCL|O_WRONLY`，请求 mode `0666`（实际权限由进程 umask 与目录默认 ACL 决定），文件名不占用正式目标名；
- 写入至少一个字节，`fsync`，关闭，删除；删除失败也算失败；
- 只要求当前进程具备创建、写入、关闭、sync 和删除该文件所需的权限；不要求当前用户是文件 owner（目录 default ACL 授权即可），应用不执行 `chmod`/`chown`；
- 探测期间绝不打开、创建或修改正式目标；
- 探测失败 fail-close，不启动，并清理自己创建的临时文件。

探测只验证目录的创建、写入、fsync、关闭和删除能力，不使用 SQLite，也不依赖目标文件系统支持 SQLite 锁。

### 4.4 启动顺序

写会话启动顺序固定：

1. 解析配置与备份列表，完成 4.1/4.2 的语法、重复、源冲突、目录、已有目标和 sidecar 检查。
2. 对所有 `path:` 目标执行 4.3 探测。
3. 加载私钥，打开源数据库（含迁移）。
4. 对所有 `sqlite:` 目标执行一次全量 Online Backup；每个目标独立尝试并聚合错误。
5. 只有全部目标成功后才允许监听端口/接受写流量。

第 1、2 步失败时不得加载私钥、打开数据库或监听。第 4 步失败时关闭数据库、不监听、非零退出；已经成功更新的其他目标不回滚，但不得因此启动。`path:` 目标在启动阶段不复制正式内容。

启动阶段失败走 `Abort`：不执行 path 复制，因此 `path:` 目标保持旧内容；但已经启动的 sqlite worker 可能完成已排队的同步，已经成功的 sqlite 备份不回滚。因此启动失败后 sqlite 与 path 目标可能停在不同时间点，不能视为同一份成功备份。

### 4.5 运行中复检与 TOCTOU

每次 sqlite 同步前、每次 path 复制/rename 前，都要重新做廉价安全检查：目标仍是普通文件（或不存在）、不是 symlink、不在启动后被替换成目录、硬链接数没有变成大于 1、不是源文件、已存在时仍是空文件或 SQLite 文件，且没有非空 `-wal`/`-journal`。

这些检查只能降低误覆盖风险；SQLite 和文件系统按路径打开，检查与使用之间仍有 TOCTOU 窗口。备份层不是同用户恶意进程的安全边界。

## 5. `sqlite:` 实现合同

### 5.1 初始全量备份

在监听前，对每个 `sqlite:` 目标执行一次完整备份：

- 通过 modernc SQLite 的 `NewBackup` 创建目标连接，`Step(-1)` 直到 `SQLITE_DONE`，然后 `Finish`；目标连接由 backup handle 关闭。
- 目标不存在时允许创建；目标为空文件时允许初始化为 SQLite DB；目标已存在时按 4.2 校验。
- 备份完成后目标必须是一个自包含的 SQLite 数据库：能被独立打开并读取源库的全部 schema 与数据；不能把非空 `-wal`/`-journal` 当作备份的一部分。若目标原本是 WAL 模式，实现必须 checkpoint 或使用不依赖 WAL 的目标连接，并在 `Finish` 后确认没有非空 sidecar；否则该目标失败。
- 新建目标在 `NewBackup` 前先以 `O_CREATE|O_EXCL` 和 `0600` 预创建空文件，避免文件继承 umask 留下 `0644`。既有目标（包括 `sqlite:` 目标）的权限保持不变，备份过程不对目标执行 `chmod`；需要更严格权限时由用户自行设置。
- 任一个目标失败则不启动；仍然尝试并报告其余目标。

### 5.2 dirty 标记与后台 worker

- 每个成功提交的写事务在提交返回后标记一次 dirty。标记必须发生在提交之后，不能在事务内；事务回滚不得标记。dirty 标记是幂等的合并信号，不携带数据、不阻塞写路径。
- 所有写路径都必须接到同一个标记点：初始备份之后，当前包括 workspace 创建、lock/unlock、state version 创建、state/JSON state 上传，以及 secret create/set/unset/delete；备份层启动后的 schema 变更同样接入。打开数据库时、初始备份之前完成的迁移/建表已经包含在初始备份中，不额外标记一轮。只读查询不标记。
- 只有一个后台 worker。dirty 标记唤醒 worker；worker 立即按配置顺序依次同步所有 `sqlite:` 目标。没有 ticker、没有固定周期；没有 dirty 时不复制。
- 写会话有独立的写 gate。Store 的每个公开写方法从关闭检查开始，到事务 commit 与 `MarkDirty` 都持读锁；`StopWrites` 获取写锁、设置 closed 并等待全部在途写完成后才返回。closed 后所有新写立即拒绝。`Store.Close`/`Abort` 自身也先执行 `StopWrites`，SecretStore 同样如此。
- Manager 的 `BeginShutdown` 关闭 dirty 标记入口；此后 `MarkDirty` 返回错误并记录 late dirty。`Shutdown` 在 worker drain 后和返回前都检查该标记，存在 late dirty 时返回错误，不能静默成功。正常 Store 路径在 gate 等待后不应出现 late 标记。
- 一轮开始时先清除 dirty，再同步；同步期间发生的新写会重新标记 dirty，并在本轮结束后触发下一轮。循环直到没有 dirty。不能丢标记、不能在 idle 时忙转。
- 写请求在源事务提交并标记 dirty 后返回，不等待目标同步。目标允许落后；不得把“写已返回”描述成“目标已同步”。

### 5.3 每轮与每目标的 handle

每一轮、每一个 `sqlite:` 目标都必须新建 backup handle：

1. 复检目标（4.5）。
2. 从备份层自己的 source connection 调用 modernc `NewBackup`，目标 DSN 使用正确百分号编码的 `file:` URI，不能把含 `?`、`#`、`%` 的裸路径直接当 URI。
3. 循环 `Step(-1)` 直到返回 DONE（`more == false` 且无错误）；不能只调用一次就假定完成。
4. 无论成功或失败都 `Finish`，释放 backup 对象和目标连接；不能复用已 Finish 的对象。
5. 成功后确认目标自包含、没有非空 sidecar；权限只由 5.1 的预创建决定，每轮备份不 `chmod`；失败记录该目标错误并继续本轮其余目标。

备份层不得占用主 store 的唯一连接执行长时间 `Raw` / `Step`：

- 主 store 当前是 `MaxOpenConns(1)`。备份必须在独立的 source handle 上取 `sql.Conn`，通过 `Raw` 断言 modernc 的 `NewBackup` 接口；不能把主池的唯一连接长期借给 worker，否则写请求会被同步阻塞，关闭时也容易与 drain 互锁。取连接和主库 checkpoint 都带有限超时；超时按对应备份错误处理，不能永久阻塞。
- source handle 可以使用同一数据库文件，但只读使用，不执行迁移或写入，并配置有限的 busy timeout；backup 期间遇到 busy 达到上限时按该目标失败，不无限阻塞 worker。它必须在 worker drain 后、checkpoint 前关闭，否则会被视为额外连接，导致 `path:` 的 checkpoint 失败。
- worker、写路径和关闭路径之间不能出现“等待对方释放唯一连接”的环形依赖。测试必须覆盖 race 和超时。

### 5.4 运行中失败

任一 `sqlite:` 目标在初始备份、运行轮次或最终 drain 中失败：

1. 记录首个致命错误（目标、阶段、底层错误），继续尝试并报告本轮/本次关停的其他目标；不得对已失败目标无限重试。
2. 立即关闭写 gate（`StopWrites`），拒绝新的写请求并等待在途写完成；已完成或在途的写事务可以提交，但备份失败已经成立，不能回滚源事务。
3. 终止当前写会话：`mgcl run` 先终止子进程组，再关闭 HTTP；Secret 写命令中止后续操作。
4. 以备份错误退出 1，不以子进程信号码伪装主动终止。
5. 不自动切换 scheme、不静默禁用备份、不把错误降级为警告。

## 6. `path:` 实现合同

### 6.1 适用场景与新鲜度

`path:` 用于目标挂载不支持 SQLite 锁的情况。运行中完全不复制；目标在两次干净关停之间保持上一次成功副本。进程崩溃、SIGKILL 或断电不会更新目标，也不会给出错误提示。需要运行中新鲜度时使用 `sqlite:`。

### 6.2 关停顺序

`path:` 复制必须严格按以下顺序执行。即使本次写会话没有成功写入，只要配置了 `path:` 目标，关停时仍执行复制；不能因为 dirty 为空就跳过：

1. `mgcl run` 先等待子进程自行退出、处理终止信号或备份 fatal（必要时终止/等待子进程组）。随后调用 `Store.StopWrites` 关闭写 gate：获取写锁、设置 closed，并等待所有在途 Store 写完成；之后停止接受新写。再调用 HTTP shutdown，等待在途 handler。
2. drain `sqlite:` worker：完成当前轮，若仍有 dirty 再做最后一轮；随后关闭备份 source handle。
3. 对主库执行 `PRAGMA wal_checkpoint(TRUNCATE)`；要求返回的 busy 计数为 0。checkpoint 使用有限超时（30s）；超时、失败或 busy 非 0，所有 `path:` 目标失败并跳过复制。
4. 关闭主 store，释放主会话。
5. 验证主文件自包含：`db-wal` 不存在或大小为 0，`db-journal` 不存在；仍有非空 WAL、存在 rollback journal、或有其他活跃连接导致 checkpoint busy，所有 `path:` 目标失败并跳过复制。不得复制 `-wal`/`-shm`/`-journal` 作为备份的一部分。
6. 只有步骤 3、5 都成功后，才对每个 `path:` 目标独立执行 6.3 的复制与 rename；一个目标失败不跳过其余目标。
7. 所有 `path:` 目标成功后，才允许报告备份成功。任一失败，命令失败。

在子进程退出/信号处理完成到 `StopWrites` 返回之间，已经被 HTTP 接受并完成提交的写会被 `StopWrites` 等待，并由后续 `Store.Close` 的 sqlite drain 覆盖；尚未进入 Store 写路径的请求可能在 gate 关闭后被拒绝。`sqlite:` 的 drain 发生在 HTTP shutdown 之后、checkpoint 和 path copy 之前。

只有 `sqlite:` 目标时不需要为了备份执行 checkpoint；只有 `path:` 目标时没有 worker，但步骤 1、3、4、5、6 仍然执行。

### 6.3 纯文件复制与原子覆盖

对每个 `path:` 目标：

1. 复检目标（4.5），确认目标仍是允许覆盖的空文件或有效 SQLite 文件；目标或其 sidecar 不得与源冲突。
2. 在目标同目录创建唯一临时文件，`O_CREATE|O_EXCL|O_WRONLY`，请求 mode `0666`，实际权限由进程 umask 与目录默认 ACL 决定；应用不 `chmod`/`chown`，只要求完成 copy、fsync、close 和 rename 所需的权限；不能直接在正式目标上打开写入。
3. 从关闭后的主 `mgcl.db` 复制字节到临时文件；复制必须完整，短读/短写都算失败。只复制主文件，不复制 sidecar。
4. `fsync` 临时文件，关闭；任何错误删除临时文件并使该目标失败。
5. 用同目录 `rename` 原子覆盖正式目标；rename 失败时正式目标保持原内容，删除临时文件，该目标失败。
6. 尽可能 `fsync` 目标目录；目录 fsync 失败按 fail-close 处理。
7. 复制后的目标不得依赖任何 sidecar；旧目标的非空 `-wal`/`-journal` 已在 4.2 拒绝。空 sidecar 不参与复制，必要时清理。

rename 是一种文件替换：目标最终是刚创建的新文件，旧目标的 inode、owner、权限位和 ACL 不会被保留或复制，应用也不会在 rename 后改写目标权限。需要固定目标权限时，应通过目录的 default ACL 或后续由用户自行设置；实现不承诺保留旧目标属性。

不得使用 `VACUUM INTO`、SQLite backup API 或任何会打开目标数据库的机制来完成 `path:`；它必须是普通文件 copy。多个 `path:` 目标独立尝试并聚合错误。

## 7. 生命周期与退出码

### 7.1 `mgcl run` 关停

正常路径：

1. 子进程退出或收到终止信号；调用 `Store.StopWrites` 等待在途写完成并停止接受新写。
2. HTTP shutdown 完成，等待在途请求；子进程树已退出或已被终止/等待。
3. drain sqlite worker，完成最后一轮 dirty。
4. 若配置了 `path:` 目标：按 6.2 执行 checkpoint、关闭主 store、验证 WAL sidecar 和逐个复制；checkpoint/验证失败时跳过复制，但命令失败。
5. 所有目标成功才正常报告备份成功；否则报告每个失败目标，命令失败。

子进程退出到 `StopWrites` 返回之间已经接受并提交的写会被 `StopWrites` 等待，并由步骤 3 的 drain 覆盖；尚未进入 Store 写路径的请求可能在 gate 关闭后被拒绝。

若运行中 sqlite 目标已经 fatal，仍执行关停流程：drain 最后一轮即使已有 fatal 也继续尝试/重试本轮 dirty 涉及的目标并保留首个 fatal 错误，同时尝试所有 path 目标；最终以 1 退出。

### 7.2 退出码优先级

| 场景 | 退出码 |
|---|---|
| 配置/目标校验/probe/初始备份失败，未启动 | 1 |
| 子进程自行退出 0，所有备份成功 | 0 |
| 子进程自行退出 N（N 非 0），所有备份成功 | N |
| 子进程自行退出 0，任一备份失败 | 1 |
| 子进程自行退出 N（N 非 0），备份也失败 | N；保留子进程码，但必须报告备份失败且不得声称备份成功 |
| 备份 fail-closed 主动终止 run（无论子进程随后如何退出） | 1；不得伪装成 128+signal |
| 用户信号导致关停，备份成功 | 128+signal（沿用现有行为） |
| 用户信号导致关停，备份失败 | 128+signal；报告备份失败，不得声称成功 |

没有子进程的 secret 写命令：备份失败一律 1。任何情况下，只要任一目标失败，都不能输出“备份成功”的总结。

## 8. 写会话与复用层

所有写会话必须复用同一备份层，不能各自实现复制逻辑：

- 当前 `mgcl run` 已接入；`mgcl secret create/set/unset/delete` 也已接入，同为写会话。
- `mgcl secret list/show/get`、TACOS 只读查询、provider session 的只读读取不触发备份，不 probe，不创建备份目标。
- 空备份列表时备份层完全禁用：不改变启动、写入、关闭顺序，也不产生额外文件或日志。
- 同一 `mgcl.db` 同时只应有一个写会话。备份层不提供跨进程协调；外部进程同时写库时，`sqlite:` 可能遇到 busy 并失败，`path:` 的 checkpoint 会因 busy 失败。并发写会话不在首版保证内。

建议把备份层放在独立包（已实现为 `internal/sqlitebackup`），由它负责：目标解析/校验、path 探测、初始 sqlite 备份、dirty 合并 worker、fatal 信号、drain、checkpoint 协作、path 复制和错误聚合。TACOS Store 与 SecretStore 只在成功提交后调用同一个 “mark dirty” 钩子，不直接操作备份目标。

## 9. 失败边界与安全

- 备份失败不能回滚已经提交的源事务；源数据库可能领先于所有目标。实现只能报告失败并停止后续写入，不能假装恢复了一致性。
- 运行中 fail-closed 会终止子进程组，可能中断正在执行的 `tofu apply`；这是避免继续产生无法备份写入的代价。
- `sqlite:` 目标原地覆盖，没有版本轮换；失败或崩溃后，目标可能不是上一份成功副本。初始备份失败可能留下不完整或不可信的目标文件；mgcl 不自动删除正式目标，下次启动会按目标校验拒绝或再次覆盖，不能把该目标当作成功备份。`path:` 用临时文件 + rename；rename 前失败时旧目标不变，rename 后目录 fsync 失败时目标已是新内容但仍按失败处理。检查与 rename 之间仍有 TOCTOU。
- 进程被 SIGKILL、崩溃或断电时无法 drain；sqlite 目标停在上一轮，path 目标停在上一份干净关停的副本。不提供 crash-consistent 备份。
- 备份内容沿用源库的加密格式，不额外解密，不把 state/Secret 明文写入目标；仍包含明文元数据。`sqlite:` 新建目标预创建为 0600；`path:` 的 probe 与复制临时文件请求 0666，实际权限由 umask 与目录默认 ACL 决定。应用不 `chmod`/`chown` 任何目标，也不要求当前用户是 owner；既有 `sqlite:` 目标的权限保持不变。不承诺磁盘/WAL/目标的安全擦除。
- symlink、硬链接、同文件检查只做尽力检测；同用户攻击者仍可在检查与打开之间替换路径。备份层不是进程隔离或攻击者防护边界。
- `path:` 的 checkpoint 要求没有其他活跃连接。发现 busy、非空 WAL 或 rollback journal 时失败关闭，不复制可能不完整的数据库。
- 不提供备份恢复命令、版本列表、校验命令、远程目标、压缩、保留策略或 `MGCL_BACKUP_TO`。

## 10. 验收测试

### 10.1 配置

- `--backup-to` 可重复；YAML `backup_to` 字符串数组；显式 flag 列表整体替换 YAML，即使 flag 只有一个而 YAML 有多个。
- flag 相对路径按 cwd、YAML 相对路径按 Git 根、绝对路径不变；子目录运行仍按 Git 根解析 YAML。
- 空/缺失 `backup_to` 不改变原行为；空数组不 probe、不创建文件。
- 未知 scheme、缺少冒号、空 path、空 scheme、NUL path、YAML 标量/映射/非字符串元素均报配置错误。
- 按规范化路径检测重复，包括 `..`、symlink 父目录、同一路径不同 scheme；重复不得静默去重。
- 目标等于源 db、源 `-wal`/`-shm`/`-journal`，或目标 sidecar 与源冲突时拒绝；运行时目标被换成源文件/硬链接也拒绝。
- 目标为目录、缺失父目录、父路径不是目录、symlink、非普通文件、已有非空非 SQLite 文件时拒绝。
- 配置错误发生在加载私钥、打开数据库和监听之前：不得创建 DB、不得监听。
- `MGCL_BACKUP_TO` 设置后不生效。

### 10.2 启动与探测

- `sqlite:` 初始全量备份在监听前完成；独立打开目标能看到源库 schema 与数据。
- `sqlite:` 初始备份失败（只读目录、目标被替换、空间不足等）不启动、不监听、非零退出；其他目标尽量尝试并报告。
- 初始 sqlite 备份失败留下的目标不会被当作成功；下次启动按目标校验拒绝或重新覆盖，不自动删除正式目标。
- 新建 `sqlite:` 目标由预创建得到 0600；既有 `sqlite:` 目标的自定义权限在初始备份、运行同步和关停后保持不变。
- `path:` probe 在目标同目录创建请求 mode 0666 的临时文件（实际权限由 umask/默认 ACL 决定），写入/关闭/删除；目标本身未被创建或修改。
- probe 创建、写入、fsync、关闭或删除失败均不启动；临时探测文件被清理。
- 启动时已有非空目标 `-wal` 或 `-journal` 被拒绝；空 sidecar 不被当作数据。
- 只有 `path:` 目标时启动阶段不复制正式内容，旧目标保持不变。

### 10.3 `sqlite:` 运行语义

- 成功写提交后 worker 被唤醒并在合理时间内把新数据同步到每个 sqlite 目标；无写入时不发生重复复制。
- 写请求返回不等待目标同步；在 worker 同步期间继续写入，结束后目标最终包含所有已提交数据。
- 同步期间的新写合并到后续轮次；用测试钩子/计数证明发生了多轮，且没有丢 dirty。
- 每个目标每轮使用新的 backup handle；DONE 后不能复用旧 handle；`Step` 未到 DONE 时不能只调用一次就 Finish。
- 运行中目标被删除且父目录存在时可重建；目标被替换为 symlink/目录/非 SQLite 文件时该目标失败。
- 写期间 worker 失败：记录首个致命错误、拒绝新写、终止 run、退出 1；其他目标仍被尝试和报告。
- 最后一次写提交后立即触发关停，drain 最后一轮必须包含该写；不能因为 worker 已空闲而跳过。
- 源事务已提交但备份失败：源数据仍存在，目标可能落后；命令退出 1，不出现成功提示。
- 单连接池下并发写、同步、关闭不发生死锁；`go test -race` 通过，测试带超时。

### 10.4 `path:` 运行与关停

- 运行中目标不更新；关停后目标包含关闭前最后一次成功写入的数据，可独立打开。
- 复制的是主文件字节，不使用 `VACUUM INTO`/SQLite backup API；目标不依赖 `-wal`/`-shm`/`-journal`。
- 临时文件在目标同目录、唯一，请求 0666（实际权限由 umask/默认 ACL 决定）；失败注入时正式目标保持旧内容，临时文件被清理。
- 同一 umask 下用 0666 创建参考文件时，path 备份最终文件权限与参考文件一致，不被强制为 0600；应用不 chmod/chown，不要求当前用户是 owner。
- rename 前失败不直接覆盖目标；rename 成功后目标内容完整。rename 后目录 fsync 失败仍报错，不得声称旧目标仍在或备份成功。
- checkpoint busy（其他连接持有事务）、非空 WAL、rollback journal 存在时 path 目标失败，命令非零退出；不得复制 sidecar 或跳过目标。
- 目标目录不可写、fsync 失败、rename 失败时该目标失败；多个 path 目标独立尝试并聚合错误。
- 已有空文件可覆盖；已有有效 SQLite DB 可覆盖；已有非空非 SQLite 文件拒绝。
- 在模拟不支持 SQLite 锁、只允许普通文件 I/O 的目录上，`path:` 复制仍成功；`sqlite:` 在同类目标上按失败关闭。
- 只有 path 目标时无 worker，但仍按关停顺序 checkpoint、关库、验证、复制。

### 10.5 生命周期、退出码与多目标

- 多 sqlite + path 目标全部成功才报告成功。
- 子进程 0 且所有备份成功退出 0；子进程 0 但任一备份失败退出 1。
- 子进程自行非零且备份成功保留原子进程码；子进程自行非零且备份失败仍保留原子进程码，但报告备份失败。
- 备份 fatal 主动终止 run 时退出 1，即使子进程随后被信号终止也不返回 128+signal。
- 用户信号关停保留 128+signal；备份失败仍报告但不伪装成功。
- 无子进程的 `mgcl run` 在备份 fatal 时退出 1；信号路径保留 128+signal。
- 多目标中一个失败时其他目标仍被尝试；错误信息包含目标和阶段，不包含 state/secret 内容。

### 10.6 回归与并发

- 未配置备份时 `mgcl run` 的现有 e2e、子进程退出码、信号处理和关闭顺序不变。
- 备份 worker 与 MarkDirty、HTTP shutdown、store close 并发时无数据竞争、无丢标记、无死锁。
- `go vet ./...`、`go test -count=1 ./...`、`go test -race ./...` 通过；涉及备份的测试使用临时目录和 0600 私钥，不依赖开发者真实环境。

## 11. 实现顺序

1. 目标解析、规范化、校验与配置测试。
2. `sqlite:` 初始备份、独立 source handle、dirty worker 与 `mgcl run` 集成。
3. `path:` probe、关停 checkpoint/close/copy、退出码与错误聚合。
4. 全量验收测试；之后才把 `backup_to` 写入 README/UserStory/`--help` 的当前功能描述。
5. `mgcl secret` 写命令已接入同一层；[secret-management.md](secret-management.md) 已同步实现状态。

## 12. 实现状态说明

- 备份层位于 `internal/sqlitebackup`，不依赖 TACOS handler；`internal/tacos.Store` 和 `internal/tacos.SecretStore` 只在成功提交后标记 dirty，并在 `Close` 中执行 drain/checkpoint/copy 顺序。
- `mgcl run` 已接入 `--backup-to` 和 YAML `backup_to`；`mgcl secret create/set/unset/delete` 也已接入。`mgcl secret list/show/get` 不触发备份。
- 已存在 `sqlite:` 目标的可写性由初始 Online Backup 实际验证；它仍发生在监听端口和接受流量之前，失败按启动失败处理。
- 为了从 `Step(-1)` 到 DONE 的循环、每轮新 handle、临时文件/rename 和固定关闭顺序，实现与本文合同一致；没有引入 `VACUUM INTO`、周期 ticker 或 `MGCL_BACKUP_TO`。

## 13. 参考

- [ADR-0007：SQLite 备份目标采用 sqlite: 与 path: 两种不可互换语义](adr/0007-sqlite-backup-destinations.md)
- [CONTEXT.md](../CONTEXT.md)
- [Secret Management v1 实施规范](secret-management.md)
- [State Encryption v1 实施规范](state-encryption.md)
- [ADR-0004：按 State Lineage 强制加密 state](adr/0004-encrypt-state-by-lineage.md)
