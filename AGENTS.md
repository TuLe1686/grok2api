# AGENTS.md —— grok2api-chenyme-v307

> 本文件是项目自身说明，现网状态最后核对于 2026-10-11，动手前以服务器实测为准。上级容器目录说明见 [../AGENTS.md](../AGENTS.md)。

## 项目定位

- chenyme/grok2api 的本地 fork：面向 Grok Build / Grok Web / Grok Console 三类账号池的多账号 API 网关（仓库 README 自述 "A multi-account API gateway for Grok Build, Grok Web, and Grok Console"）。
- 三类 Provider 的凭据、配额、健康度、冷却、并发与模型能力互相隔离；对外提供 OpenAI / Anthropic 兼容接口（Responses、Chat Completions、Anthropic Messages、Images、异步 Videos）。
- 本 fork 在 v3.0.7 基线上加了一条本地特性：Build 账号 chat permission-denied 处置（见「仓库与远端」）。
- 技术栈：后端 Go 1.26 + Gin + GORM；前端 React 19 + TypeScript + Vite + Tailwind + shadcn/ui，构建产物由 Go 服务同源托管；数据库 SQLite / PostgreSQL，运行时 Memory / Redis。

## 仓库与远端

| remote | 地址 | 用途 |
|---|---|---|
| origin | https://github.com/TuLe1686/grok2api.git | 推送目标 |
| upstream | https://github.com/chenyme/grok2api.git | 只拉不推 |

- 当前分支 `feature/build-chat-permission-denied-disable`；`VERSION` 文件为 `v3.0.7`（基线提交 `11bb5e2 chore: bump version to v3.0.7`）。
- 本分支的两个本地提交：
  - `f7ab9f5 feat: disable Build accounts on chat permission-denied`
  - `994bcfe feat(ui): add Build chat permission-denied settings fields`
- 改动语义：请求路径命中 Grok Build chat endpoint permission-denied（403）时可立即停用整个 Build 账号，让号池停止反复换号重试（`BuildChatPermissionDeniedRequestDisable`）；另可按周期巡检启用的 Build 账号并自动停用命中者（`InspectEnabled` / `InspectInterval` 1 分钟～24 小时 / `InspectConcurrency` 1–32，实际还受凭据刷新并发池限流）。只影响 Build，不影响 Web / Console。
- 实现落点：`backend/internal/application/account/build_chat_permission_denied.go`（新增，含单测）、`backend/internal/domain/settings/settings.go`、`backend/internal/application/{settings,gateway,account}/service.go`、`backend/internal/infra/config/config.go`、`backend/internal/transport/http/settings/handler.go`、`frontend/src/features/settings/*`、`frontend/src/shared/i18n/index.ts`。

## 部署形态或使用方式

- **当前无现网，本目录没有 `.deploy/` 或 `.agents/` 部署脚手架**：相关 grok 系域名（`grok2api` / `grok` / `jj-grok2api`）已于 2026-10-10 从 DNS 清理，原 us-biz 部署随主机停机一并下线。
- 如需部署，按仓库 README 自建：`cp config.example.yaml config.yaml` → 生成两个密钥（`openssl rand -hex 32` 填 `secrets.jwtSecret`、`openssl rand -base64 32` 填 `secrets.credentialEncryptionKey`）→ 改 `bootstrapAdmin.password` → `docker compose up -d`，管理端默认 `http://127.0.0.1:8000`。
- `docker-compose.yml`（项目名 `grok2api`）：单服务 + `${GROK2API_PORT:-8000}:8000`，`config.yaml` 只读挂载，SQLite 与本地媒体落命名卷 `grok2api-data`；`warp`（出口代理）与 `flaresolverr`（Cloudflare Clearance）在 profile 里，按需 `--profile` 启用。镜像默认 `ghcr.io/chenyme/grok2api:latest`。
- 源码运行：后端 `make run`（= `cd backend && go run ./cmd/grok2api --config <绝对路径 config.yaml>`）；前端 `cd frontend && pnpm install && pnpm dev`（默认 5173，代理到 8000）。
- 镜像由 `.github/workflows/ghcr-image.yml` 构建（push main / `v*.*.*` tag / PR 触发，amd64 + arm64）。

## 关键文件

- `VERSION`、`README.md` / `README.zh-CN.md`：版本号与官方说明（架构图、Provider 边界表、首次设置流程）。
- `config.example.yaml`：启动配置模板，逐项标注 [必须修改] / [按需修改] / [通常不要修改]（`server` / `auth` / `secrets` / `bootstrapAdmin` / `frontend` / `database` 等）。
- `backend/cmd/grok2api/main.go`：进程入口；`backend/internal/{domain,application,infra,transport}/`：领域模型 / 用例与调度 / 适配器与持久化 / HTTP 路由与 DTO；`backend/docs/`：swagger 产物。
- `frontend/src/{app,features,entities,shared}/`：路由与外壳 / 功能页 / 共享领域对象 / API 客户端与工具。
- `Makefile`：`run`（本地跑后端）、`swagger`（swag v1.16.6 生成 `backend/docs`）。
- `Dockerfile`、`docker/entrypoint.sh`、`.dockerignore`、`.gitignore`。

## 备注

- 首次设置顺序：用 `bootstrapAdmin` 登录 → 在 Upstream Accounts 加 Build/Web/Console 账号 → 等配额与模型能力同步 → 在 Model Routes 核对公开模型名 → 在 Client Keys 建 `g2a_` 开头 key → 调 `/v1/*`。
- `config.yaml` 只放启动配置；运行时可变参数走管理端 Runtime settings 热加载。
- `secrets.credentialEncryptionKey` 一旦有账号写入就必须长期保留，更换会导致既有凭据无法解密。
- 密钥只写服务器上的 `config.yaml` / `.env`，仓库与文档只写键名。
- 本目录不写部署流水账；该 fork 若将来再上线，先在 [../AGENTS.md](../AGENTS.md) 的域名表登记域名与机器，再按上面的自建流程部署。
