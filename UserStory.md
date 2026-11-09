# UserStory：用 OpenMgCl 在本地管理 OpenTofu state

> 假设 `mgcl` 已经安装好，并且在项目目录里、同一个终端中执行命令。

## 一句话理解

`mgcl` 是一个本地 state 后端：OpenTofu 以为自己在连远程服务，实际 state 被写进当前目录的 `mgcl.db`。

```text
tofu init / plan / apply
        │
        │  cloud block
        ▼
mgcl run 启动的本地服务
        │
        ▼
./mgcl.db   ← 你的 state
```

## 故事

作为一个在本地写 OpenTofu 的开发者，我想把 state 留在项目目录里，而不是提交到 git 或放到云端。这样我可以正常 apply / destroy，又不用维护远程账号。

## 1. 建一个最小项目

```bash
mkdir demo && cd demo
```

`main.tf`：

```hcl
terraform {
  cloud {
    organization = "local"
    workspaces {
      name = "demo"
    }
  }
}

resource "terraform_data" "hello" {
  input = "v1"
}
```

`cloud` block 是关键：它让 OpenTofu 走"远程 state 服务"的协议，mgcl 负责在本地接住这些请求。不需要在 `main.tf` 里写 mgcl 的地址。

把本地 state 排除出版本控制：

```bash
printf '.terraform/\nmgcl.db*\n' > .gitignore
```

`mgcl.db` 就是你的 state，地位等同于原来的 `terraform.tfstate`。

OpenTofu 的 cloud backend 启动时要求有一个 token。这里给一个本地固定值即可，mgcl 会把它同时配给本地服务和 OpenTofu：

```bash
export MGCL_TOKEN=dev-token
```

## 2. 第一次 init + apply

```bash
mgcl run -- tofu init
mgcl run -- tofu apply -auto-approve
```

每条命令发生时：

1. `mgcl run` 在 `127.0.0.1:1217` 启动一个临时本地服务；
2. OpenTofu 把 `local/demo` 当作远程 workspace，自动创建它；
3. apply 产生的 state 被写进当前目录的 `mgcl.db`；
4. tofu 命令结束后，mgcl 关闭服务。

所以每条命令都是独立的一次"起服务 → 跑 tofu → 关服务"，但 `mgcl.db` 会一直保留，state 是连续的。

预期看到：

```text
MgCl server listening on 127.0.0.1:1217
...
Apply complete! Resources: 1 added, 0 changed, 0 destroyed.
```

## 3. 确认 state 在本地

```bash
mgcl run -- tofu state list
# terraform_data.hello

ls -A
# .gitignore
# .terraform
# main.tf
# mgcl.db
```

`mgcl.db` 里保存了 workspace 和 state 历史。删掉它 = 丢失本地 state；要备份就复制它。

## 4. 改配置，再 apply

把 `input` 改成 `"v2"`，然后：

```bash
mgcl run -- tofu apply -auto-approve
mgcl run -- tofu plan
```

预期：

```text
Apply complete! Resources: 0 added, 1 changed, 0 destroyed.
No changes. Your infrastructure matches the configuration.
```

每次 apply 都会在 `mgcl.db` 里留下一个状态版本，旧版本不会被覆盖。

## 5. 收尾

```bash
mgcl run -- tofu destroy -auto-approve
rm -f mgcl.db mgcl.db-wal mgcl.db-shm
```

`destroy` 只销毁资源，不会删除 state 数据库。确认不再需要这份 state 后，再删 `mgcl.db*`。

## 记住三件事

1. **`mgcl.db` 就是 state**：不要提交到 git，备份和删除都要像对待 `terraform.tfstate` 一样谨慎。
2. **每条 `mgcl run -- ...` 都是临时服务**：在同一个项目目录、用同一个 `mgcl.db`，state 就连续。
3. **mgcl 只做 state 存储**：没有远程运行、变量、策略、团队权限等远程协作功能。

## 补充：如果用的是 remote backend

把 `cloud` block 换成 `backend "remote"`，并在里面加一行 `hostname = "127.0.0.1:1217"`；其他命令不变。`cloud` block 不需要显式 hostname。
