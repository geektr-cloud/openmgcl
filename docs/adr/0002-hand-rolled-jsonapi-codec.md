# 手写 JSON:API 编解码，不引入 hashicorp/jsonapi

TACOS API 的请求/响应 JSON:API 编解码用手写 struct（`data`/`type`/`attributes`），没有引入 OpenTofu 客户端同款的 `hashicorp/jsonapi` 库。

原因：服务端只需要编解码五种资源（workspace、state version、entitlement-set、错误、分页 meta），字段集小且稳定；引入 jsonapi 库会带入其标签 DSL 与反射规则，复杂度高于收益。这是刻意选择，不是偷懒——如果未来资源类型增多、需要关系（relationships）或稀疏字段集，应重新评估而不是直接认为"当时没人会做"。
