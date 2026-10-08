# Teldrive

[![Ask DeepWiki](https://deepwiki.com/badge.svg)](https://deepwiki.com/zhz8888/teldrive)

**简体中文** · [English](README.md)

> **本仓库是 fork。** 由 [zhz8888](https://github.com/zhz8888) 维护在
> [zhz8888/teldrive](https://github.com/zhz8888/teldrive)，基于
> [divyam234](https://github.com/divyam234) 的
> [tgdrive/teldrive](https://github.com/tgdrive/teldrive)。本 fork 的发布、容器镜像、文档、
> issue 与 pull request 都在这里，而不是上游。

Teldrive 把 Telegram 变成自建的云存储：服务端把文件元数据放在 PostgreSQL，把文件内容放在
Telegram 频道，并由**同一个二进制**同时提供 Web 界面和 HTTP API。

## 主要特性

- **单二进制交付** —— React 前端被嵌入 Go 服务端，浏览器与 API 由同一个进程响应。
- **契约优先的 API** —— `typespec/` 下的 TypeSpec 是 HTTP 契约的唯一来源，OpenAPI 文档、
  Go 服务端、TypeScript 客户端类型与 API 参考文档都由它生成。
- **可续传上传与流式下载** —— 持久化的上传会话、分片级重试、HTTP Range 下载，以及从 URL
  或服务器路径发起的后台导入。
- **后台任务** —— 上传、清理与 bot 开通都以持久化的 River 任务运行，支持重试、队列以及界面
  上的暂停/恢复。
- **分享与访问控制** —— 公开分享链接、单文件授权、供外部客户端使用的 API Key，以及按角色
  控制的管理页面。
- **可选的内容加密** —— 分块且可随机定位的加密格式与版本化密钥，另有保护所存 Telegram 凭据
  的 data key。
- **可横向拆分** —— 单实例内联 worker 即可运行，也可把 API 与 worker 角色拆到不同实例；
  PostgreSQL 是唯一的共享状态。

## 环境要求

- 带持久化存储的 PostgreSQL
- 可访问 Telegram 的 Telegram 账号
- `security.signing-key` 与 `security.data-key`

面向公网部署时，请在 Teldrive 前面终止 HTTPS，并让 PostgreSQL 只处于内网。

## 快速开始

### 容器

[快速开始指南](https://zhz8888.github.io/teldrive/getting-started/quick-start)会生成 PostgreSQL
加 Teldrive 的 `compose.yaml`。启动前请备份 `security.data-key` 与所有内容加密密钥——丢失它们
可能导致受保护的数据无法恢复。

已有 PostgreSQL 时：

```bash
docker run --rm \
  -p 127.0.0.1:8080:8080 \
  -e TELDRIVE_HTTP_ADDRESS=0.0.0.0:8080 \
  -e TELDRIVE_DATABASE_URL='postgres://teldrive:password@db.example:5432/teldrive?sslmode=require' \
  -e TELDRIVE_SECURITY_SIGNING_KEY='YOUR_SIGNING_KEY' \
  -e TELDRIVE_SECURITY_DATA_KEY='YOUR_DATA_KEY' \
  ghcr.io/zhz8888/teldrive:v2
```

打开 <http://127.0.0.1:8080>，用 Telegram 登录，并上传一个小的测试文件。需要可控升级时请固定
发布标签（`ghcr.io/zhz8888/teldrive:vX.Y.Z`），不要使用 `latest`。

### 发布二进制

从本 fork 的 [GitHub Releases](https://github.com/zhz8888/teldrive/releases) 下载对应平台的
压缩包并安装 `teldrive`，然后指向 PostgreSQL：

```bash
teldrive check     # 校验配置、执行迁移、初始化依赖
teldrive run       # 提供 API 与界面（别名：serve）
teldrive version   # 打印构建信息
```

### 从源码构建

需要 Go 1.26、[Bun](https://bun.sh)、[Just](https://github.com/casey/just)，以及用于运行服务端的
PostgreSQL。

```bash
git clone https://github.com/zhz8888/teldrive.git
cd teldrive
just install-tools
just build
./bin/teldrive version
```

`just build` 会重新生成前端 API 客户端、构建 Vite/React 界面，并将其嵌入 Go 二进制。仓库同时
提供 Nix flake：`nix develop` 即可获得完整工具链。

## 配置

配置按**文件 → `TELDRIVE_*` 环境变量 → 命令行参数**的顺序覆盖。可从 `config.sample.yaml`
（或 `config.sample.toml`）开始，详见
[配置参考](https://zhz8888.github.io/teldrive/configuration/overview)。

`teldrive check` 会加载配置、执行迁移并初始化全部依赖后退出，适合作为部署前的预检。

## 开发

```bash
just dev         # 同时启动后端与 Vite 开发服务器
just test-unit   # 单元测试
just ui-check    # 前端 lint、类型检查、浏览器测试与构建
just check       # 完整门禁：生成、lint、测试、覆盖率、构建
```

集成测试与竞态测试需要容器运行时（Docker 或 Podman）：

```bash
just test-integration
just test-race
```

生成物由其来源文件决定：HTTP 契约来自 `typespec/*.tsp`（`just generate-api`、
`just generate-ui` 或 `just generate`），查询层来自 `db/queries/*.sql`（`just generate-db`）。
请勿手改 `openapi/`、`internal/api/gen/`、`internal/db/sqlcgen/` 与 `ui/src/api/schema.ts`；
仓库结构、生成规则与测试体系见 `AGENTS.md`。

## 文档

- 使用指南：<https://zhz8888.github.io/teldrive>
- API 参考：<https://zhz8888.github.io/teldrive/api/>
- rclone：本仓库**不包含 rclone 后端**。除非你另行获取，否则 `rclone config` 不会提供
  `teldrive` 类型；Teldrive 提供的是该后端所驱动的 HTTP API，认证使用在**设置 → API 密钥**中
  创建的密钥。参见 [rclone 配置](https://zhz8888.github.io/teldrive/rclone/setup)

## 最佳实践

**应当**

- 遵守 Telegram 的限制。Teldrive 只是你 Telegram 账号之上的一层封装，滥用会导致账号被封、
  频道被删。
- 把 `security.data-key`、内容加密密钥与数据库备份放在一起保管，并在真正需要之前演练一次恢复。
- 只存有用途的数据，并且有节制地升级。

**不要**

- 囤积数据：这既违反 Telegram 的条款，也没有任何收益。

## 参与贡献

欢迎在 <https://github.com/zhz8888/teldrive> 提交 issue 与 pull request。提交前请先跑通
`just check`；`AGENTS.md` 是参与本仓库开发的入口。

## 许可证

Teldrive 以 [MIT 许可证](LICENSE)发布。

- 原始项目 —— Copyright © 2024 [divyam234](https://github.com/divyam234)
  （[tgdrive/teldrive](https://github.com/tgdrive/teldrive)）。
- 本 fork 及其中的修改 —— Copyright © 2026 [zhz8888](https://github.com/zhz8888)。

## 致谢

本 fork 基于 [divyam234](https://github.com/divyam234) 与贡献者们创作的
[tgdrive/teldrive](https://github.com/tgdrive/teldrive)。上游的版权声明与 MIT 许可证原样保留；
本 fork 所带的每一项修复、特性与重写都建立在这份基础之上。
