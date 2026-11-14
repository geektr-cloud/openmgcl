# OpenMgCl

OpenMgCl（mgcl）是一个本地开发辅助工具，用于从零启动整套基础设施：在本地手动运行 OpenTofu 时提供 TACOS 状态存储，使 state 保存在本地。目标场景是本地交互式开发，不以 CI 或无人值守运行为目标。

## Language

**OpenMgCl**:
项目名称。MgCl₂（氯化镁）是豆腐点卤的凝固剂——把豆浆固化成 tofu 的物质，呼应本项目为 OpenTofu 提供后端、让 tofu "成形" 的定位。
_大小写规范_: 用户可见文案用 `MgCl`（如 `OpenMgCl`、`MgCl server listening on`）；二进制名与代码标识符用小写 `mgcl`；环境变量前缀用大写 `MGCL_`。
_Avoid_: OpenMGCL, OpenMGCL CLI, MetaGaming Cloud（旧写法，已废弃）

**TACOS**:
TF Automation and Collaboration Software 的缩写，指 OpenTofu/Terraform 的远程自动化协作服务协议族。本项目只实现其中的状态存储子集。
_Avoid_: Terraform Cloud, TFC, remote backend（这是客户端配置块的名字，不是服务）

**Discovery document**:
`/.well-known/terraform.json` 返回的 JSON 文档，把 Service ID 映射到服务 base URL，是客户端发现服务能力的入口。

**Service ID**:
discovery document 中的键，由服务类型与版本组成（如 `state.v2`、`tfe.v2`、`tfe.v2.1`），指向同一状态 API base URL。
_Avoid_: endpoint, API version

**Organization**:
workspace 的命名空间。任意字符串即隐式存在，无创建、无成员、无权限管理。

**Workspace**:
organization 下的具名状态容器。本项目一律为 local 执行模式，仅承载 state，不承载远程运行。
_Avoid_: project, environment

**State version**:
workspace 内一份 state 快照记录，携带 serial、lineage 与状态字节，生命周期为 pending 或 finalized。
_Avoid_: state file, snapshot

**Current state version**:
workspace 中按创建顺序最新的 finalized 版本；current-state-version 与下载指向它。
_Avoid_: latest serial, head

**Serial**:
由客户端生成、正常更新时递增的 state version 序号。服务端允许更低或相同 serial；current 取决于 finalized 版本的创建顺序，而不是最大 serial。

**State Lineage**:
标识一族 state 的不透明标识（通常为 UUID），由客户端生成与管理。正常更新保持不变；保留 lineage 的迁移或副本属于同一族，并共享加密密钥。
_Avoid_: Project, State Family

**Lineage Key**:
与一个 State Lineage 关联的数据密钥，用于保护该族 state version 的内容。同一 lineage 的历史及副本使用同一把密钥。

**Recipient**:
凭借对应 Ed25519 私钥解开 Lineage Key 或 Secret DEK 的主体，分别获得对应 lineage state 的读写能力或 Secret KV 的解密能力。Recipient 不是 HTTP 用户或 Bearer token 身份。

**Key fingerprint**:
Lineage Key 的标识，用于区分数据密钥及识别 state version 使用的密钥。
_Avoid_: recipient fingerprint, SSH fingerprint

**Recipient fingerprint**:
Recipient 的 Ed25519 公钥指纹，用于选择对应的私钥。
_Avoid_: key fingerprint

**Pending / Finalized**:
state version 的两个生命周期状态：已创建但状态字节未到（pending），状态字节已落库可下载（finalized）。同 serial 的新 pending 可与旧 finalized 共存，旧 finalized 在替换上传成功前仍是 current。

**Stale pending**:
已被更晚创建的 finalized 版本覆盖的 pending。其上传被拒绝，不能改变 current。

**State version replacement**:
同 workspace+serial 用新版本替换旧 finalized。modern 流程先创建 pending，旧 finalized 保持 current；上传成功后原子替换。

**Restore**:
用不同内容的 state 创建 replacement，使 current 回到该内容。restore 可由普通更新或 force push 触发；服务端按请求内容与冲突规则判断。
_Avoid_: rollback, force push（force push 是客户端选项）

**Foreign upload / download**:
客户端对服务端签发的上传/下载 URL 发起的原始字节请求（`application/octet-stream`），不携带 Bearer 凭据。

**Force push**:
OpenTofu `state push -force` 的客户端意图，用于跳过客户端本地 lineage/serial 检查。服务端不把该标志当作 restore 依据，也不允许它绕过 identical finalized 冲突；force unlock 是另一种锁操作。
_Avoid_: overwrite, force unlock（后者是锁操作）

**Lock**:
workspace 级互斥锁，防止并发写 state；有 lock / unlock / force-unlock 三个操作。

**Secret**:
首次运行 OpenTofu 前需要准备的一组 bootstrap 凭据（例如 IAM access key、跨云凭据），以具名 KV 集合保存。它只覆盖 mgcl 接管的 bootstrap credentials，不接管 OpenTofu 创建的 state outputs。
_Avoid_: state output, managed resource

**KV**:
Secret 内的一个具名字符串条目。每个 KV 的值单独加密，但同一 Secret 的所有 KV 共用该 Secret 的 Secret DEK；只保留最新值，没有历史版本。
_Avoid_: secret version, key-value file

**Secret DEK**:
保护单个 Secret 全部 KV 的独立数据密钥。它与 State Lineage 的 Lineage Key 完全独立，绝不共用；首版每把 Secret DEK 只有一个 Recipient，且不轮换。
_Avoid_: Lineage Key, state key

**User token**:
`mgcl run` 服务用于 TACOS API 的 Bearer 凭据。未显式配置时每次 run 生成新的随机值：子进程 run 只写入临时 CLI config、不打印；常驻 run 在监听后向 stderr 打印一次。它与 Provider session 完全独立。
_Avoid_: provider token, MGCL_TOKEN（环境变量名，不是概念）

**Provider session**:
`mgcl run` 为每次子进程启动签发的短期、只读凭证，只能读取当前数据库里的 Secret，不能读写 TACOS state 或执行管理操作。它不同于用户 Bearer token 和 SSH passphrase。
_Avoid_: MGCL_TOKEN, user token

**Backup destination**:
`mgcl run --backup-to` 或 `.mgcl.yaml` `backup_to` 的一个条目，格式 `<scheme>:<path>`。首版只允许两种语义固定的 scheme：`sqlite:<path>` 用 SQLite Online Backup API 在运行中覆盖最新副本；`path:<path>` 在写会话关闭并完成 WAL checkpoint 后用普通文件复制生成自包含副本，适用于不支持 SQLite 锁的挂载。两者都覆盖最新副本，不做版本轮换。
_Avoid_: backup target, snapshot, dump

**Write session**:
打开 `mgcl.db` 并可能提交写入的进程生命周期，例如 `mgcl run` 服务和 `mgcl secret create/set/unset/delete`。每个写会话必须复用同一备份层；只读命令不是写会话。
_Avoid_: backup job, sync process
