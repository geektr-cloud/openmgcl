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
workspace 内一份 state 快照记录，携带 serial、lineage 与状态字节。
_Avoid_: state file, snapshot

**Serial**:
workspace 内单调递增的 state version 序号，客户端递增、服务端强制（除非 force push）。

**State Lineage**:
标识一族 state 的不透明标识（通常为 UUID），由客户端生成与管理。正常更新保持不变；保留 lineage 的迁移或副本属于同一族，并共享加密密钥。
_Avoid_: Project, State Family

**Lineage Key**:
与一个 State Lineage 关联的数据密钥，用于保护该族 state version 的内容。同一 lineage 的历史及副本使用同一把密钥。

**Recipient**:
凭借对应 Ed25519 私钥解开 Lineage Key、从而获得该 lineage state 读写能力的主体。Recipient 不是 HTTP 用户或 Bearer token 身份。

**Key fingerprint**:
Lineage Key 的标识，用于区分数据密钥及识别 state version 使用的密钥。
_Avoid_: recipient fingerprint, SSH fingerprint

**Recipient fingerprint**:
Recipient 的 Ed25519 公钥指纹，用于选择对应的私钥。
_Avoid_: key fingerprint

**Pending / Finalized**:
state version 的两个生命周期状态：已创建但状态字节未到（pending），状态字节已落库可下载（finalized）。

**Foreign upload / download**:
客户端对服务端签发的上传/下载 URL 发起的原始字节请求（`application/octet-stream`），不携带 Bearer 凭据。

**Force push**:
创建 state version 时跳过 serial 单调校验的强制写入标志。
_Avoid_: overwrite, force unlock（后者是锁操作）

**Lock**:
workspace 级互斥锁，防止并发写 state；有 lock / unlock / force-unlock 三个操作。
