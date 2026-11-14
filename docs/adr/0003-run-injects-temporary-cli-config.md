# run 命令通过临时 CLI config 接入本地服务器

`mgcl run -- <cmd>` 需要让 OpenTofu 在子进程里访问一个 HTTP 本地服务。实现选择是：`TF_CLOUD_HOSTNAME` 只注入 `host[:port]`（OpenTofu 只接受这种格式），同时生成一个临时 CLI config，用 `host` block 把 `state.v2`、`tfe.v2`、`tfe.v2.1` 指向 `http://host:port/api/v2/`，把当次 TACOS 用户 token 写入 `credentials`，再通过 `TF_CLI_CONFIG_FILE` 注入；用户已有的 CLI config 会被读取并合并，同一个 host 的 `host` block 以用户配置为准。未显式配置 token 时每次 run 自动生成新 token；子进程模式不打印，常驻模式在监听后打印一次供外部客户端使用。

考虑过但未采用：给 mgcl 加自签 HTTPS 并让客户端信任（需要额外处理证书生成、分发和信任，复杂度明显更高）；或者只保留常驻服务、让用户手写 CLI config（放弃 `run --` 的一步式体验）。这个决定的代价是临时文件里包含当次 token，因此文件必须 0600 且子进程退出后删除。

后果：`cloud` block 可以不写 hostname；`remote` backend 仍必须在 `backend "remote"` 块里写 `hostname`，因为 OpenTofu 不会用 `TF_CLOUD_HOSTNAME` 填充 remote backend 的 hostname。
