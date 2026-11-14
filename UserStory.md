# UserStory：用 OpenMgCl 在本地管理 OpenTofu state

> 假设 `mgcl` 已经安装好；在 Git 仓库内、项目目录里执行命令。

## 一句话理解

`mgcl` 是一个本地 state 后端：OpenTofu 以为自己在连远程服务，实际 state 被写进项目根目录的 `mgcl.db`。

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
git init
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

只把本地状态产物排除出版本控制：

```bash
printf '.terraform/\nmgcl.db*\nbackup/\n' > .gitignore
```

`.mgcl.yaml` 不忽略：它是项目配置，应该提交。里面只放配置和路径；示例 `token` 只是仅 loopback 使用的本地开发 token，真实凭据、Secret value 和私钥内容都不写入 YAML。

## 2. 一份 `.mgcl.yaml` 管住项目配置

在仓库根写配置，之后所有命令都读它：

```yaml
# .mgcl.yaml
db: mgcl.db
encryption_key_file: /home/you/.ssh/id_ed25519
token: dev-token
backup_to:
  - sqlite:backup/mgcl.db
```

```bash
mkdir -p backup
```

- `db` 没有默认值，必须配；相对路径按 Git 根目录解析。
- `encryption_key_file` 只写路径，不写私钥内容；换成你的实际路径。
- `token` 是 OpenTofu cloud block 启动时要求的 Bearer token；示例值是仅 loopback 使用的本地开发 token，不是真实凭据，可以提交。真实凭据、Secret value 和私钥内容不要写进 YAML。
- mgcl 只在 Git 工作树内读取 `<git-root>/.mgcl.yaml`，在子目录运行也读根目录这一份。
- 完整配置字段见 [README 配置](README.md#配置)。

mgcl 强制使用 SSH Ed25519 私钥。没有默认私钥时先创建：

```bash
ssh-keygen -t ed25519 -f ~/.ssh/id_ed25519
```

默认会读 `~/.ssh/id_ed25519`；YAML 里写的是绝对路径，要和你实际用的那把一致。

## 3. 第一次 init + apply

```bash
mgcl run -- tofu init
mgcl run -- tofu apply -auto-approve
```

每条命令发生时：

1. `mgcl run` 在 `127.0.0.1:1217` 启动一个临时本地服务；
2. OpenTofu 把 `local/demo` 当作远程 workspace，自动创建它；
3. apply 产生的 state 被写进 `mgcl.db`，内容在数据库里加密；
4. tofu 命令结束后，mgcl 关闭服务。

所以每条命令都是独立的一次"起服务 → 跑 tofu → 关服务"，但 `mgcl.db` 会一直保留，state 是连续的。

预期看到：

```text
MgCl server listening on 127.0.0.1:1217
...
Apply complete! Resources: 1 added, 0 changed, 0 destroyed.
```

## 4. 确认 state 在本地

```bash
mgcl run -- tofu state list
# terraform_data.hello

ls -A
# .gitignore
# .mgcl.yaml
# .terraform
# backup
# main.tf
# mgcl.db
```

`mgcl.db` 里保存了 workspace 和 state 历史。删掉它 = 丢失本地 state。

## 5. 把 bootstrap 凭据交给 provider

第一次用真实 provider apply 前需要的凭据（例如 AWS access key）也存进同一个 `mgcl.db`。`mgcl secret` 读同一份 `.mgcl.yaml`，不用再传其他参数：

```bash
mgcl secret create bootstrap

printf '%s' "$AWS_ACCESS_KEY_ID" | \
  mgcl secret set bootstrap/access_key_id --stdin

mgcl secret set bootstrap/secret_access_key
# 不带 value 且 stdin 是 TTY 时，无回显读取一次
```

**value 作为位置参数会进入 shell history 和进程列表（`ps`）；敏感值优先 `--stdin` 或 TTY 无回显。** Secret 的 value 在数据库里单独加密。可以检查元数据：

```bash
mgcl secret list
mgcl secret show bootstrap
```

`main.tf` 用独立的 ephemeral provider 读取：

```hcl
terraform {
  required_providers {
    mgcl = { source = "geektr-cloud/mgcl" }
  }
}

provider "mgcl" {}

ephemeral "mgcl_secret" "bootstrap" {
  name = "bootstrap"
}

provider "aws" {
  region     = "us-east-1"
  access_key = ephemeral.mgcl_secret.bootstrap.values["access_key_id"]
  secret_key = ephemeral.mgcl_secret.bootstrap.values["secret_access_key"]
}
```

provider 已实现但还没发布到 Registry；开发期需要 provider 仓库构建的二进制和 `dev_overrides`：

```hcl
# dev.tfrc
provider_installation {
  dev_overrides {
    "geektr-cloud/mgcl" = "/path/to/terraform-provider-mgcl/bin"
  }
  direct {}
}
```

```bash
export TF_CLI_CONFIG_FILE=/path/to/dev.tfrc
mgcl run -- tofu validate
mgcl run -- tofu plan
mgcl run -- tofu apply -auto-approve
```

`dev_overrides` 路径会跳过 `tofu init`；需要 init 时改用 filesystem mirror。`dev_overrides` 只能通过 `TF_CLI_CONFIG_FILE` 交给 tofu；`mgcl run` 会保留这份内容并追加本地服务的 host block。provider 需要 OpenTofu >=1.11。

只有 `mgcl run -- <command>` 会签发 `MGCL_PROVIDER_ENDPOINT` / `MGCL_PROVIDER_TOKEN`；常驻 `mgcl run` 没有 token。Secret 每次 plan/apply 由 provider 重新读取，所以更新 Secret 后下一条命令能读到新值。

## 6. 改配置，再 apply

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

## 7. 备份与恢复

`backup_to` 已经在 `.mgcl.yaml` 里，`mgcl run` 和 `mgcl secret` 的写命令都会自动使用，不需要额外参数：

- `sqlite:<path>`：运行中持续同步最新副本；
- `path:<path>`：只在干净关停时复制一份自包含文件，适合不支持 SQLite 锁的挂载。

数组可以放多个目标。两种目标都只是 `mgcl.db` 的副本：**备份时必须连私钥一起备份**，否则 state 和 Secret 都无法解密。恢复就是把副本放回 `db` 指向的位置，并确保 YAML 里的私钥可用。

细节见 [docs/database-backup.md](docs/database-backup.md)。

## 8. 收尾

```bash
mgcl run -- tofu destroy -auto-approve
rm -f mgcl.db mgcl.db-wal mgcl.db-shm
```

`destroy` 只销毁资源，不会删除 state 数据库。确认不再需要这份 state 后，再删 `mgcl.db*`。

## 记住四件事

1. **`.mgcl.yaml` 可以提交**：只放配置和路径；示例 `token` 只是仅 loopback 使用的本地开发 token。真实凭据和 Secret value 用 `mgcl secret` 写进加密的 `mgcl.db`，私钥内容永远不进仓库。
2. **`mgcl.db` 就是 state 和 bootstrap Secret**：不要提交到 git，备份和删除都要像对待 `terraform.tfstate` 一样谨慎。私钥丢失后 state 和 Secret 都无法恢复，备份时必须同时备份私钥。
3. **每条 `mgcl run -- ...` 都是临时服务**：在同一个项目目录、用同一个 `mgcl.db`，state 和 Secret 就连续。provider token 只在该子进程存活期间有效。
4. **mgcl 只做 state 存储和 bootstrap Secret**：provider 只提供 bootstrap Secret，不导出 outputs；也没有远程 runs、variables、策略、团队权限等协作功能。provider 尚未发布 Registry 时用 `dev_overrides`。

## 补充：如果用的是 remote backend

把 `cloud` block 换成 `backend "remote"`，并在里面加一行 `hostname = "127.0.0.1:1217"`；其他命令不变。`cloud` block 不需要显式 hostname。
