# 按 State Lineage 强制加密 state

mgcl 在 SQLite 中强制加密 `state`、`json_state`、`json_state_outputs`，不提供明文模式。每个 **State Lineage** 使用一把独立的随机 **Lineage Key**；数据库只保存由当前 Recipient 的 Ed25519 公钥包裹后的 Lineage Key，不保存 SSH 私钥或明文数据密钥。首次写入 lineage 时自动建钥，空 lineage 拒绝写入。

选择 lineage 而不是 workspace 作为密钥作用域，是因为需要保护的是同一族 state 的历史及迁移连续性。正常更新保留 lineage；保留 lineage 的副本也共享 Lineage Key，因此 lineage 不是 workspace 唯一标识，更不是认证凭证。元数据与 API 协议保持明文和兼容，本设计只提供 at-rest protection，不提供按 HTTP 用户隔离的访问控制。

首版只支持单 Recipient，不实现授权、轮换、恢复或旧明文库迁移。更换或丢失私钥会使既有 lineage 无法读写；这是刻意收紧的首版边界。未来增加多 Recipient 需要迁移表结构，并重新评估撤销语义，不能假设删除 wrapped key 就能召回旧备份或已解出的密钥。

考虑过 workspace 级密钥、明文兼容模式和首版多 Recipient，均未采用：workspace 级密钥不能自然跟随保留 lineage 的 state 副本；明文兼容会破坏强制加密不变量；多 Recipient 会扩大授权和撤销范围。详细格式与实现约束见 [state-encryption.md](../state-encryption.md)。
