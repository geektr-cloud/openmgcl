# State Encryption v1 实施规范

本文是 `v1` 的实现契约，供后续编码任务直接执行。决策原因见 [ADR-0004](adr/0004-encrypt-state-by-lineage.md)，领域术语见 [CONTEXT.md](../CONTEXT.md)。

## 1. 产品边界

OpenMgCl 用于从零启动整套基础设施，目标场景是本地手动、交互式运行 OpenTofu，不以 CI 或无人值守运行为目标。

本功能只提供 **SQLite at-rest encryption**：

- 强制加密 `state_versions.state`、`json_state`、`json_state_outputs`；
- mgcl 在内存中解密，现有 API 对 OpenTofu 返回相同明文；
- workspace/organization、serial、lineage、MD5、状态、时间戳和密钥元数据保持明文；
- 现有 Bearer token、foreign upload/download 和锁语义不变；
- 不保护运行中的进程、已解密 HTTP 响应或私钥被本机攻击者读取的场景；
- 不防能改写 SQLite 的主动攻击者替换密钥元数据；wrapped key 没有额外的本地签名/MAC。

### 非目标

- CI、无人值守和 ssh-agent；
- 多 Recipient、grant/revoke CLI 或 API；
- 私钥轮换、rewrap、恢复；
- 按 HTTP 用户执行 Recipient 权限；
- 旧明文数据库迁移；
- 可靠撤销、旧备份或磁盘残留的安全擦除；
- 隐藏元数据或改变 OpenTofu 协议。

## 2. 核心不变量

1. 三个受保护字段只要在数据库中不是 SQL NULL，就必须是 v1 envelope，不能写入或返回明文。
2. 每个非空 **State Lineage** 对应一把随机 32-byte **Lineage Key**（DEK）。
3. 保留 lineage 的 workspace/副本共享 DEK；同一 workspace 写入新 lineage 时使用另一把 DEK。
4. 首次创建某个 lineage 的 state version 时自动创建 DEK，包括尚无 payload 的 pending state version。
5. 首版每把 DEK 只有一个 Recipient；一个 mgcl 进程只加载一把 SSH 私钥。
6. 启动时不扫描或解锁全部 lineage。无法解锁的 lineage 在实际 state 读写时失败，不阻塞其他 lineage。
7. SSH 私钥、转换后的 X25519 私钥、ECDH shared secret、明文 DEK/KEK、passphrase 均不得入库或写日志。
8. lineage 是客户端提供的公开元数据，不是秘密或认证凭证。

## 3. 配置与启动

### 3.1 私钥文件

新增：

| 来源 | 名称 |
|---|---|
| flag | `mgcl run --encryption-key-file <path>` |
| 环境变量 | `MGCL_ENCRYPTION_KEY_FILE` |
| `<git-root>/.mgcl.yaml` | `encryption_key_file: <path>` |
| 默认值 | `~/.ssh/id_ed25519` |

优先级沿用现有规则：显式 flag > 环境变量 > `<git-root>/.mgcl.yaml` > 默认路径。配置只在 Git 工作树内读取；非 Git 目录不读取配置文件。不要增加 `--encryption` 或任何 plaintext opt-out。

要求：

- 路径必须是 regular file；
- Unix 下 group/other 不得有读写执行权限，即 `mode.Perm() & 0o077 == 0`；
- 只接受普通 OpenSSH `ssh-ed25519` 私钥；RSA、ECDSA、FIDO key、certificate、agent 均明确拒绝；
- 在打开/创建 SQLite 和监听端口之前完成私钥加载与验证，失败时不得留下新 DB 或临时服务。

### 3.2 Passphrase

只支持：

- `MGCL_ENCRYPTION_KEY_PASSPHRASE`；
- 或未设置该变量时，从 TTY 无回显读取一次。

**不支持** `MGCL_ENCRYPTION_KEY_PASSPHRASE_FILE`，也不增加任何 passphrase CLI 参数或 YAML 配置。

解析顺序：

1. 调用 `ssh.ParseRawPrivateKey`；成功则继续。
2. 若错误不是 `*ssh.PassphraseMissingError`，返回错误。
3. 若需要 passphrase 且直接环境变量已设置（包括空值），调用 `ssh.ParseRawPrivateKeyWithPassphrase`；失败即结束，不再提示。
4. 若变量未设置且 stdin 是 TTY，无回显读取一次并解析；失败即结束。
5. 非交互环境且未设置变量时明确报错。

`mgcl run --` 创建子进程环境时必须删除 `MGCL_ENCRYPTION_KEY_PASSPHRASE`，不得把 passphrase 传给 tofu 或其他命令。直接环境变量只通过 `os.LookupEnv` 读取，不能经 viper 落入配置文件。

### 3.3 私钥解析与校验

先用 `pem.Decode` 要求 PEM block 类型为 `OPENSSH PRIVATE KEY`，并确保剩余内容只有空白；这样不会意外接受 PKCS#8 等其他 Ed25519 编码。随后使用 `golang.org/x/crypto/ssh` 的 Raw API，以取得 Ed25519 seed。兼容返回 `ed25519.PrivateKey` 和 `*ed25519.PrivateKey`，并验证标准长度。

加载后必须：

1. 校验上述 OpenSSH PEM 包装；
2. 取 `seed := private.Seed()`；
3. 用 `ed25519.NewKeyFromSeed(seed)` 重建密钥；
4. 验证重建公钥与原私钥内嵌公钥一致；
5. 由 Ed25519 公钥创建 SSH public key，计算 Recipient fingerprint；
6. 完成下述 X25519 转换，并验证转换后的公私钥一致。

错误不得输出私钥、seed 或 passphrase；可以输出文件路径和公开 fingerprint。

## 4. 密钥与指纹

### 4.1 Recipient fingerprint

对原始 SSH wire-format public-key blob 计算：

```text
SHA256(ssh.PublicKey.Marshal())
```

展示和数据库编码使用 `ssh.FingerprintSHA256` 的格式：

```text
"SHA256:" + base64.RawStdEncoding(digest)  // 无 = padding
```

不要对裸 Ed25519 公钥、X25519 公钥、`.pub` 整行文本或私钥计算 fingerprint。

### 4.2 Lineage Key 与 Key fingerprint

新 DEK 由 `crypto/rand` 生成 32 bytes。其原始 fingerprint 为：

```text
SHA256("openmgcl/lineage-key-fingerprint/v1\x00" || DEK)
```

其中 `\x00` 是一个实际零字节。数据库中的文本编码同样为：

```text
"SHA256:" + base64.RawStdEncoding(raw_fingerprint)
```

payload envelope 保存原始 32-byte digest。解包 DEK 后必须重新计算并匹配存储的 fingerprint。

### 4.3 Ed25519 → X25519

私钥转换严格使用 **32-byte seed**，不能哈希整个 64-byte Go 私钥：

```text
h = SHA-512(seed)
x = h[0:32]
x[0]  &= 248
x[31] &= 127
x[31] |= 64
```

将 `x` 交给 `crypto/ecdh.X25519().NewPrivateKey`。

公钥转换使用 `filippo.io/edwards25519`：

1. `Point.SetBytes(ed25519PublicKey)`；
2. 重新编码并逐字节比较 `Point.Bytes()` 与原 Ed25519 公钥，不相等则拒绝非规范编码；
3. `Point.BytesMontgomery()`；
4. 验证它与转换后 X25519 private key 的 `PublicKey().Bytes()` 完全一致；
5. 拒绝转换后的全零公钥。

ECDH 必须处理全零 shared secret 错误，不得忽略。

## 5. 数据库结构

首版面向新建数据库，不实现旧明文数据迁移。v0.1.x 之前不保证历史 schema 兼容，也不提供自动或人工迁移；升级前请自行导出 state。

`state_versions` 不使用表级 `UNIQUE(workspace_id, serial)`，改为 finalized / pending 两个 partial unique index，允许同 serial 的新 pending 与旧 finalized 短暂共存。相关决策见 [ADR-0005](adr/0005-state-version-restore-and-replacement.md)。

新增表：

```sql
CREATE TABLE IF NOT EXISTS lineage_keys (
    lineage TEXT PRIMARY KEY,
    key_fingerprint TEXT NOT NULL UNIQUE,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);

CREATE TABLE IF NOT EXISTS wrapped_lineage_keys (
    key_fingerprint TEXT PRIMARY KEY REFERENCES lineage_keys(key_fingerprint),
    recipient_fingerprint TEXT NOT NULL,
    suite INTEGER NOT NULL,
    ephemeral_public_key BLOB NOT NULL,
    hkdf_salt BLOB NOT NULL,
    nonce BLOB NOT NULL,
    wrapped_key BLOB NOT NULL,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);

CREATE INDEX IF NOT EXISTS idx_wrapped_lineage_keys_recipient
    ON wrapped_lineage_keys(recipient_fingerprint);
```

约束含义：

- `lineage_keys` 记录 lineage 当前唯一 DEK；
- `wrapped_lineage_keys.key_fingerprint` 为主键，明确首版单 Recipient；
- 同一 Recipient 可解锁多个 lineage，因此 `recipient_fingerprint` **不能唯一**；
- v1 `suite = 1`；
- 固定长度：`ephemeral_public_key` 32 bytes、`hkdf_salt` 32 bytes、`nonce` 12 bytes、`wrapped_key` 48 bytes；
- 不保存 lineage 私钥、DEK、KEK、SSH 私钥或 passphrase。

未来多 Recipient 需要把 wrapped 表迁移为 `(key_fingerprint, recipient_fingerprint)` 复合身份；首版不要假装已经支持。

## 6. Cryptographic suite v1

固定套件，不支持算法协商：

- Ed25519 → X25519 conversion v1；
- ephemeral-static X25519；
- HKDF-SHA-256；
- AES-256-GCM；
- 12-byte random nonce；
- v1 canonical AAD。

使用标准库 `crypto/ecdh`、`crypto/hkdf`、`crypto/aes`、`crypto/cipher`、`crypto/rand`。`hkdf.Key` 可一次完成 Extract+Expand。

### 6.1 Canonical byte encoding

文档中的 `field(s)` 定义为：

```text
uint32bigendian(len(s_bytes)) || s_bytes
```

ID、lineage 和字段名使用原始 UTF-8/ASCII bytes。不要使用 Go `gob`、map JSON、带歧义字符串拼接或宿主架构字节序。

### 6.2 包裹 Lineage Key

设当前 Recipient 的长期 X25519 公钥为 `pkR`：

1. 生成随机 DEK 和 key fingerprint；
2. 生成新的 ephemeral X25519 key pair `(skE, pkE)`；
3. `Z = X25519(skE, pkR)`；
4. 生成随机 32-byte `hkdf_salt`；
5. 构造 `wrap_info`：

```text
"openmgcl/wrap/v1\x00" ||
pkE ||
pkR ||
raw_recipient_fingerprint ||
field(lineage) ||
raw_key_fingerprint
```

6. `KEK = HKDF-SHA-256(Z, salt=hkdf_salt, info=wrap_info, length=32)`；
7. 生成随机 12-byte wrap nonce；
8. 构造 `wrap_aad`：

```text
"openmgcl/wrap-aad/v1\x00" ||
field(lineage) ||
raw_key_fingerprint ||
raw_recipient_fingerprint ||
pkE
```

9. `wrapped_key = AES-256-GCM(KEK, wrap_nonce, DEK, wrap_aad)`，结果为 32-byte ciphertext + 16-byte tag。

解包使用 `Z = X25519(skR, pkE)`，随后执行相同 HKDF 和 AEAD Open。解包前检查数据库 Recipient fingerprint 是否等于当前身份；不相等返回 key-access denied。解包后验证 DEK fingerprint。

### 6.3 State payload envelope

每个非 SQL NULL 字段独立加密。直接使用 lineage DEK 作为 AES-256-GCM key，不再派生 payload 子密钥。每次写入（包括重复上传相同内容）生成新的随机 12-byte nonce。

二进制格式：

| Offset | Size | 内容 |
|---:|---:|---|
| 0 | 4 | magic = `MGCL` |
| 4 | 1 | envelope version = `1` |
| 5 | 1 | payload suite = `1` |
| 6 | 32 | raw key fingerprint |
| 38 | 12 | payload nonce |
| 50 | 16 + plaintext length | ciphertext + GCM tag |

最小长度为 66 bytes。写入时，nil slice 表示字段不存在并存 SQL NULL；non-nil slice（包括 `len == 0`）一律加密，零长度明文也会形成 66-byte envelope。数据库中任何非 NULL 的零长度 BLOB 都按损坏处理，不能当作“没有内容”或直接返回。

`payload_aad`：

```text
"openmgcl/state/v1\x00" ||
field(workspace_id) ||
field(state_version_id) ||
field(lineage) ||
field(field_name) ||
raw_key_fingerprint
```

`field_name` 只能是：

- `state`
- `json_state`
- `json_state_outputs`

AAD 不包含 Recipient 或 wrapped-key 元数据，以便未来只 rewrap DEK 而不重加密 payload。workspace、state version、lineage 和字段名必须与当前数据库行一致，防止密文换位。

解密时：

- magic/version/suite 不支持则报错；
- 长度不足则报错；
- 非 NULL 数据没有 v1 magic 时按“不支持的明文数据”报错，不得直接返回；
- AEAD authentication、fingerprint 或上下文不匹配则报错；
- 未知错误不能降级为明文读取。

随机 nonce 在同一 DEK 下不得主动重用；不要使用可能因事务/VM 回滚而回退的计数器。首版不实现分块。

## 7. Store 与 API 行为

### 7.1 创建 state version

`CreateStateVersion` 仍接收明文参数，加密应在 store/持久化边界完成，handler 不接触 DEK。

在同一数据库事务中：

1. 要求 `lineage != ""` 且 `serial >= 0`，否则返回 422；不强制 UUID 格式。
2. 处理同 workspace+serial 的既有行：
   - serial 由客户端生成；正常更新递增，服务端允许更低或相同 serial，以兼容 OpenTofu cloud backend 在 `state push -force` 时清掉 force 的行为。
   - 相同 serial+lineage+MD5 的 finalized 重复创建一律返回 409，不受 `force` 影响，包括历史 serial，以及已存在同 serial pending 的情况；不同内容可显式 restore 并成为 current。force 不是服务端识别 restore 的依据。
   - 同 serial 的 pending 可被新请求替换，用于上传失败后的安全重试；已有 finalized 不受影响。该替换发生在 identical finalized 检查之后。
   - modern create（无 inline state）为不同内容新建 pending，同时保留旧 finalized 作为 current。
   - inline state 是 finalized 写入，删除同 serial 的旧 pending/finalized 并插入新行在同一事务内原子完成。
3. 查询 `lineage_keys`：
   - 不存在：生成 DEK、wrap 记录和 lineage 映射；
   - 已存在：读取 wrapped key，验证当前 Recipient 并解出 DEK；失败则整个事务回滚。
4. 生成 state version ID。
5. 根据**明文** `len(state) > 0` 决定 pending/finalized，保持原语义。
6. 对三个非 nil 字段分别加密（包括 non-nil 的零长度值），AAD 使用上述真实 ID 和字段名；nil 字段写 SQL NULL。
7. 插入 state version、必要的 lineage key 和 wrapped key 后一起提交。modern pending 不删除旧 finalized；只有上传成功时才在同一事务里删除同 serial 旧 finalized 并 finalize 新行。
8. 仅在提交成功后把 DEK 放入进程缓存。

串行冲突、唯一约束或加密失败不得留下孤儿 key、pending row 或半加密记录。MD5 继续保存客户端提交的原始 state MD5，不重算为密文 MD5。

`json-state` 和 `json-state-outputs` 的 base64 解码错误必须与 `state` 一样返回 422，不能继续忽略错误后写空值。

### 7.2 Modern upload path

pending state version 创建时已经建立 lineage 与 DEK。`UploadState` 和 `UploadJSONState`：

1. 根据 state version ID 读取 workspace ID、serial、lineage、状态和 `rowid`；
2. pending 版本若已有更晚创建的 finalized 版本，则返回 409，避免过期上传假成功或让 current 倒退；被替换掉的旧 pending URL 因 row 不存在返回 404；
3. 解锁对应 DEK；
4. 使用 state version ID 和正确字段名加密上传明文；空 body 是 non-nil 零长度输入，也必须写成 envelope；
5. `state` 上传 finalize 时，若存在同 workspace+serial 的旧 finalized：lineage+MD5 相同则返回 409（防止遗留 pending 复活历史状态）；不同内容才在同一事务删除旧行，再把 pending 行更新为 finalized，上传失败则事务回滚，旧 current 保持可用；
6. `json_state` 上传只更新 pending 的 JSON 字段，不改变 current；无法解锁时不更新任何字节。

重复上传会生成新 nonce，这是预期行为，不要求数据库字节幂等。已 finalized 版本的重复上传维持原有的幂等覆盖行为。

### 7.3 读取与下载

Store 返回给现有 handler 的 `StateVersion` 仍是明文表示；数据库 row 在持久化边界解密。这样 `downloadStateHandler` 和 JSON:API builder 无需知道加密格式。`current-state-version` 按 `rowid DESC` 取最新 finalized 版本；`download` 依据 status 和字段是否存在判断可用性，因此合法 finalized 的空 body 返回 200 和零字节。

每个非 NULL BLOB 的读取顺序固定为：

1. 从 envelope 取出 key fingerprint，解析并校验版本、套件和固定长度；
2. 以该 fingerprint 查询 `wrapped_lineage_keys`，并 join `lineage_keys`；
3. 要求 join 得到的 lineage 与当前 state version row 的 lineage 完全一致；记录缺失或不一致均按数据损坏返回 500；
4. 检查 wrapped record 的固定长度和 Recipient；只有结构有效但 Recipient 不同才返回 403；
5. 使用当前 row 的 lineage 解包 DEK，验证 DEK fingerprint；
6. 使用当前 row 的 workspace ID、state version ID、lineage、字段名和 envelope fingerprint 构造 AAD 并解密。

不能先按 row lineage 任意选择另一把 DEK，也不能在上下文不一致时继续按 envelope 解密。

其他要求：

- `GetStateVersion` / `GetCurrentStateVersion` 不得把数据库密文当作明文 state 返回；
- `json_state_outputs` 虽无公开读取端点，仍必须经过同一加解密路径并有 store 测试；
- workspace 列表、workspace 元数据和锁操作不增加 Recipient 权限层。

### 7.4 已存在 lineage 与新 lineage

- 同一 lineage 出现在另一个 workspace：复用同一 DEK，payload AAD 使用新 workspace/state version ID。
- 同一 workspace 出现新 lineage：创建新 DEK；旧版本仍通过各自 envelope 的 key fingerprint 找旧 DEK。
- 已存在 lineage 的 wrapped key 属于另一 Recipient：当前进程不能创建、上传或读取该 lineage 的受保护内容。
- 当前 Recipient 无权访问某 lineage时，仍可为其他全新 lineage 创建 DEK。

## 8. 错误契约

| 场景 | 行为 |
|---|---|
| key 文件不存在、权限过宽、格式不支持、passphrase 错误 | 启动失败；不得创建 DB 或监听端口 |
| 新 state version 的 lineage 为空 | HTTP 422，事务无写入 |
| 当前 Recipient 与 wrapped key 不匹配 | HTTP 403；不得读写受保护内容 |
| envelope 是明文、非 NULL 零长度 BLOB、未知版本、截断、tag/AAD 校验失败 | HTTP 500；不得降级或返回原始 BLOB |
| wrapped key 缺失或长度错误 | HTTP 500，按数据损坏处理 |
| base64 inline payload 非法 | HTTP 422 |
| 私钥丢失 | 无恢复路径；明确报错，不尝试 fingerprint 反推 |

对 OpenTofu 的错误正文使用稳定、简短的英文；不要包含 secret、完整 state 或 wrapped key 字节。Recipient mismatch 与数据损坏要区分，不能都伪装成“not found”。

## 9. 进程内缓存

允许按 raw key fingerprint 缓存已解锁 DEK，避免每个字段重复 ECDH：

- 使用 mutex 保护并发读写；
- 缓存只在成功解包或事务提交后更新；
- 缓存键必须包含 key fingerprint，不能只按 lineage；
- 不把 DEK 返回给 handler 或暴露调试接口；
- 不承诺 Go 中可靠的内存清零，只要求不序列化、不记录日志、不无必要复制。

服务启动不预热缓存，以满足“部分 lineage 不可解锁也能启动”。

## 10. 建议的最小实现划分

避免新建通用加密框架，保持项目内最小模块化：

- `internal/tacos/identity.go`：SSH 私钥解析、passphrase、Ed/X25519 转换、fingerprint；
- `internal/tacos/envelope.go`：DEK wrap/unwrap、payload seal/open、canonical AAD、格式校验；
- `internal/tacos/store.go`：schema、事务内 key lookup/create、三个字段透明加解密、DEK cache；
- `cmd/run.go`：key-file flag/config、启动前加载 identity、剥离子进程 passphrase；
- 测试优先放在同包，以便验证原始 BLOB 和内部格式，不增加公共 API。

`NewRouter` 和现有 HTTP handler 的公开形状尽量不变。可以改变 `tacos.Open` 的构造参数以传入已验证 identity/key manager，并集中更新测试；不要提供 nil identity 的明文快捷路径。

新增直接依赖预计为：

- `golang.org/x/crypto`（`ssh`）；
- `golang.org/x/term`（TTY 无回显输入）；
- `filippo.io/edwards25519`（公钥转换与一致性校验）。

HKDF、ECDH、AES-GCM 使用 Go 标准库，不再添加第三套 crypto 实现。

## 11. 必须完成的测试

### 11.1 Identity 与 crypto 单元测试

- 未加密和 passphrase 加密的 OpenSSH Ed25519 私钥均能加载；
- 错误 passphrase、RSA、损坏文件、权限过宽均失败；
- seed 与内嵌公钥不一致时失败；
- Ed25519→X25519 使用权威公开测试向量，不能只测试自己与自己互操作；
- Recipient fingerprint 与 `ssh-keygen -l -E sha256` 结果一致；
- wrap/unwrap round trip；错误 Recipient、任何关键字段篡改、fingerprint 不匹配均失败；
- payload round trip（包括零长度明文）；相同明文两次加密产生不同 nonce/ciphertext；数据库中的非 NULL 零长度 BLOB 被拒绝；
- workspace/state-version/lineage/field/key-fingerprint 任一 AAD 改变都会解密失败；
- plaintext、错误 magic/version/suite、截断、篡改 tag 均明确失败；
- 固定 DEK、ephemeral private key、salt、nonce 和上下文的 **golden test vectors**，断言精确字节，防止格式无意漂移。

### 11.2 Store 测试

- 三个字段在 SQLite 原始 BLOB 中都有 v1 header，不包含可识别明文，Store 读回与原始明文一致；
- inline 和 pending upload 两条路径都覆盖；
- 新 lineage 自动创建一条 lineage key 和一条 wrapped key；
- 同 lineage 跨 workspace 复用 fingerprint；同 workspace 换 lineage 使用新 fingerprint；
- 空 lineage 返回 422 且无 key/state 残留；
- 第二把私钥不能读写既有 lineage，但可以创建和读写新 lineage；
- 服务/Store 打开含不可解锁 lineage 的 DB 不失败，只有对应操作失败；
- serial、current、pending/finalized 替换生命周期、MD5、lock 行为符合 [ADR-0005](adr/0005-state-version-restore-and-replacement.md)；`force` 不绕过 identical finalized 冲突；
- 事务失败不留下孤儿 key；
- 旧明文 BLOB 不迁移、不返回，明确报错。

### 11.3 CLI 与真实 OpenTofu e2e

- 所有现有测试使用临时生成的 0600 Ed25519 key，不读取开发者真实 `~/.ssh/id_ed25519`；
- 默认路径解析、flag/env 覆盖、缺失 key 启动失败且无 DB 残留；
- passphrase 环境变量不出现在 `mgcl run --` 子进程环境中；
- 现有 remote backend、cloud block、lock、apply/destroy e2e 全部保持通过；
- 用第二把私钥启动后，既有 lineage 的 `tofu state list` 失败；
- 原始 SQL 检查 `state`、`json_state`、`json_state_outputs` 均为 envelope；
- `go vet ./...`、`go test -count=1 ./...` 全量通过。

测试不得依赖网络、开发者 SSH 配置或固定 Home 中的真实私钥。

## 12. 实现后必须同步的文档

编码完成后同步更新：

- `README.md`：强制加密、私钥配置、at-rest 边界、pre-v0.1 无 schema 兼容/迁移承诺、私钥丢失风险；
- `UserStory.md`：保留最简流程，只补充默认私钥、可选 key-file 和“私钥丢失 = state 不可恢复”；
- release notes：标明这是破坏数据库格式与启动要求的变化，不要静默作为普通 patch；
- `--help`：增加 key-file 参数，不增加 passphrase 或关闭加密参数。

若实际实现偏离本文的字节格式、AAD、schema 或错误语义，应先修改本规范并记录原因，不能用实现与文档各说一套。

## 13. 权威参考

- [RFC 7748: X25519](https://www.rfc-editor.org/rfc/rfc7748.html)
- [RFC 8032: Ed25519](https://www.rfc-editor.org/rfc/rfc8032.html)
- [RFC 5116: AEAD 接口与 nonce 要求](https://www.rfc-editor.org/rfc/rfc5116.html)
- [Go crypto/ecdh](https://pkg.go.dev/crypto/ecdh)
- [Go crypto/hkdf](https://pkg.go.dev/crypto/hkdf)
- [Go x/crypto/ssh](https://pkg.go.dev/golang.org/x/crypto/ssh)
- [filippo.io/edwards25519](https://pkg.go.dev/filippo.io/edwards25519)
- [libsodium Ed25519→Curve25519 转换说明](https://doc.libsodium.org/advanced/ed25519-curve25519)
