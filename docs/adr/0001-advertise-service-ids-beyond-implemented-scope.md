# 通告超出实现范围的 Service ID

mgcl 只实现了状态存储 API 子集，但 discovery document 同时通告 `state.v2`、`tfe.v2`、`tfe.v2.1` 三个 Service ID 指向同一 base URL。

原因：OpenTofu 客户端按接入方式查询不同 Service ID——remote backend 默认查 `tfe.v2.1`、设置 `TF_FORCE_LOCAL_BACKEND` 时查 `state.v2`、cloud 块查 `tfe.v2`；三者都只是 API base URL 的指针。全部通告后，两种客户端接入方式均无需额外环境变量即可工作；配合 workspace 固定 `execution-mode: "local"` 与 entitlement-set 返回 `operations: false`，客户端始终本地执行、只调用状态 API，不会触碰未实现的 runs 端点。

后果：如果未来 tofu 版本改变本地执行的判定逻辑，客户端可能调用未实现的 tfe.v2 端点并失败。届时应缩小通告范围或补齐实现，而不是惊讶于"为什么声称支持 tfe.v2"。
