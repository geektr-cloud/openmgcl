# State version restore 与安全替换

OpenTofu cloud backend 在 `state push -force` 时会清掉 `force`，并发送客户端计算出的 serial，可能低于或等于当前 serial。服务端若强制 serial 单调、把 current 定义为 `MAX(serial)`，或依赖 `force` 识别 restore，都会拒绝合法恢复或让 current 指向旧状态。mgcl 因此把 serial 视为客户端生成：服务端接受更低或相同 serial，current 取创建顺序最新的 finalized 版本；同 serial 替换先保留旧 current，只有新 pending 上传成功后才在同一事务删除旧 finalized 并 finalize 新行。同 workspace+serial+lineage+MD5 的 finalized 重复创建一律返回 409，`force` 也不绕过该冲突；不同内容的 restore 可以成为 current。

`state_versions` 不使用表级 `UNIQUE(workspace_id, serial)`，而是 finalized / pending 两个 partial unique index，让同 serial 的新 pending 与旧 finalized 暂时共存。这样上传失败、413 或进程崩溃都不会丢失旧 current；被替换的旧 URL 在替换成功前仍可用，成功后返回 404。更晚创建的 finalized 会让旧 pending 变成 stale，其上传被拒绝。

考虑过严格 serial 单调、`current = MAX(serial)`、用 `force` 作为 restore 信号，以及先删除旧 finalized 再创建 pending。均拒绝：前两者会把 restore 的更低 serial 判为错误或让 current 回退到旧版本；cloud backend 已证明 `force` 不可信；先删除旧 finalized 会在上传中断时造成不可恢复的数据丢失。

后果：serial 不再保证单调，服务端也不能用 serial 判断先后；force 只代表客户端意图，不能作为服务端授权；旧版本 URL 会在替换成功后失效。v0.1.x 之前不处理历史 schema 兼容，也不提供迁移工具。
