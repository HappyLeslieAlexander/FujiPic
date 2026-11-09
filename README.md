# FujiPic

轻量、简洁的多用户图床：网页登录上传 + API 上传，CDN 作为统一入口，私有 S3 兼容桶存储。

完整设计与验收标准见 **[PLAN.md](PLAN.md)**；CDN 配置见 **[docs/CDN.md](docs/CDN.md)**。

## 请求链路

```text
上传（网站/API）：
浏览器 / API 客户端 → CDN（POST bypass）→ Go 应用 → 私有 S3 桶

图片首次访问：
浏览器 → CDN（MISS）→ Go 应用 GET /i/{key} → S3 GetObject → 流式返回 → CDN 缓存

图片后续访问：
浏览器 → CDN（HIT）→ 浏览器（不再回源应用/S3）
```

S3 桶始终私有；CDN 不直接访问桶，只回源到应用。

## 已实现功能

- 网页：登录、邀请码注册（可选开放注册）、拖拽/粘贴/多选上传、图库、复制链接/Markdown、删除、配额条
- API：Bearer Token 上传、列表、详情、删除、`/me`；Token 创建/撤销/scope/过期
- 存储：S3 兼容私有桶（AWS SDK v2）；测试用内存驱动
- 图片：MIME/像素/尺寸/大小校验，拒绝 SVG；SHA-256 每用户去重；随机不可变对象键
- 回源：`/i/{key}` 流式读取，支持 `HEAD`、`Range`、`ETag`/`If-None-Match`、`If-Modified-Since`，返回 1 年 `immutable` 缓存头
- 配额：上传前原子预留，失败/删除归还；管理后台可调整
- 删除：软删除元数据 → 删除 S3 对象 → 入队 CDN 刷新；失败由后台 worker 重试
- 管理：用户列表、禁用/启用、配额、邀请码、审计日志、统计
- 安全：Argon2id 密码、Session Cookie + CSRF、Origin 校验、登录/上传限流、安全响应头、可选源站共享密钥
- 部署：单二进制、SQLite WAL、Docker distroless 镜像、开发 Compose（MinIO）、生产 Compose（Caddy）

## 快速开始（本地，无需 S3）

需要 Go 1.27+。

```bash
export APP_ENV=development
export APP_BASE_URL=http://localhost:8080
export PUBLIC_BASE_URL=http://localhost:8080
export DATABASE_PATH=./data/fujipic.db
export TMP_DIR=./data/tmp
export STORAGE_DRIVER=memory
export ALLOW_OPEN_REGISTRATION=true
export BOOTSTRAP_ADMIN_EMAIL=admin@example.com
export BOOTSTRAP_ADMIN_PASSWORD=admin-password-123

go run ./cmd/fujipic serve
```

打开 <http://localhost:8080>，注册普通用户或用上面的管理员登录。内存驱动重启后图片丢失，仅用于演示和测试；正式使用请配置 S3。

也可以使用 `.env.example` 作为模板：

```bash
cp .env.example .env
set -a; source .env; set +a
go run ./cmd/fujipic serve
```

## Docker Compose（MinIO 开发环境）

需要 Docker 与 Compose 插件：

```bash
make dev          # docker compose -f deploy/compose.dev.yml up -d --build
make logs
# http://localhost:8080  admin@example.com / admin-password-123
# MinIO 控制台 http://localhost:9001  minioadmin / minioadmin
make down
```

开发 Compose 会创建私有桶 `fujipic`（`mc anonymous set none`），应用通过 `S3_ENDPOINT=http://minio:9000` 访问。

> 当前开发环境未安装 Docker，Compose/MinIO 链路尚未在本机执行验证。

## 构建与测试

```bash
go build ./...          # 编译
go test ./...           # 单元 + HTTP 集成测试
go test -race ./...     # 竞态检测
go vet ./...            # 静态检查
make build              # 生成 bin/fujipic（CGO_ENABLED=0）
```

CI 见 `.github/workflows/ci.yml`，执行 `go build`、`go vet`、`go test -race`。

## API

认证方式：

- 网页登录：Cookie + `X-CSRF-Token`（或表单 `csrf`）
- API：`Authorization: Bearer fp_...`，Token 在“设置”页创建，支持 `upload,read,delete` scope 和过期时间

上传：

```bash
curl -X POST https://pic.example.com/api/v1/upload \
  -H "Authorization: Bearer fp_..." \
  -F "file=@cat.png"
```

响应（201；去重时 200 且 `deduplicated=true`）：

```json
{
  "id": "img_...",
  "url": "https://img.example.com/i/2026/11/xxxxxxxxxxxxxxxxxxxxxxxxxx.png",
  "markdown": "![cat.png](https://img.example.com/i/...)",
  "html": "<img src=\"https://img.example.com/i/...\" alt=\"cat.png\">",
  "name": "cat.png",
  "mime": "image/png",
  "size": 12345,
  "width": 800,
  "height": 600,
  "sha256": "...",
  "deduplicated": false,
  "created_at": "2026-11-09T13:00:00Z"
}
```

主要端点：

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| POST | `/api/v1/upload` | 上传（multipart `file` 或原始 body + `X-Filename`） |
| GET | `/api/v1/images?limit=20&cursor=...` | 当前用户图片列表（游标分页） |
| GET | `/api/v1/images/{id}` | 图片元数据 |
| DELETE | `/api/v1/images/{id}` | 删除（204，幂等） |
| GET | `/api/v1/me` | 当前用户与用量 |
| GET/POST | `/api/v1/tokens` | 列出/创建 Token（仅 Cookie 登录） |
| DELETE | `/api/v1/tokens/{id}` | 撤销 Token（仅 Cookie 登录） |
| GET | `/i/{key}` | CDN 回源：图片字节、HEAD/Range/304 |
| GET | `/healthz`、`/readyz` | 存活/就绪检查 |

错误格式：

```json
{"error":{"code":"quota_exceeded","message":"存储配额不足"}}
```

常见状态码：401 未认证、403 CSRF/scope/配额、413 文件过大、415 类型不支持、422 像素超限、429 限流。

## 配置

所有配置通过环境变量读取，完整列表见 [`.env.example`](.env.example)。关键项：

| 变量 | 说明 |
| --- | --- |
| `APP_ENV` | `production` / `development` |
| `APP_BASE_URL` | 网站/API 的公网地址（CDN 域名） |
| `PUBLIC_BASE_URL` | 图片公网地址（CDN 图片域名，不能带路径） |
| `STORAGE_DRIVER` | `s3` / `memory` |
| `S3_ENDPOINT`、`S3_BUCKET`、`S3_ACCESS_KEY_ID`、`S3_SECRET_ACCESS_KEY` | 私有桶连接信息 |
| `S3_FORCE_PATH_STYLE` | MinIO 等需要 `true` |
| `MAX_UPLOAD_BYTES` | 单张上限，默认 10 MiB |
| `MAX_IMAGE_PIXELS`、`MAX_IMAGE_DIMENSION` | 像素/边长上限 |
| `DEFAULT_QUOTA_BYTES` | 新用户默认配额，默认 1 GiB |
| `ALLOW_OPEN_REGISTRATION` | 是否允许无邀请码注册，默认 `false` |
| `BOOTSTRAP_ADMIN_EMAIL` / `BOOTSTRAP_ADMIN_PASSWORD` | 空库首次启动创建管理员 |
| `CONTACT_EMAIL` | 页脚公开的滥用反馈邮箱（页脚 `mailto:` 链接）；留空则不展示 |
| `TRUST_PROXY`、`CLIENT_IP_HEADER` | 从 CDN 头获取真实 IP；连接来源不在可信代理网段内时忽略该头 |
| `TRUSTED_PROXY_CIDRS` | 可信反向代理 CIDR 列表；开启代理信任时必填 |
| `ORIGIN_SHARED_SECRET` | 要求回源请求带 `X-Origin-Secret` |
| `CDN_PURGE_PROVIDER` | `none` / `cloudflare` / `generic` |
| `CDN_ZONE_ID`、`CDN_PURGE_TOKEN`、`CDN_PURGE_ENDPOINT` | 刷新凭据 |

生产环境务必设置 `APP_ENV=production`（Cookie 加 `Secure`），并让源站只接受 CDN 回源。

## 运维

```bash
# 在线备份（VACUUM INTO；备份文件以 0600 权限创建）
go run ./cmd/fujipic backup --out ./backup/fujipic-$(date +%F).db

# 创建管理员 / 重置密码（使用 DATABASE_PATH）
FUJIPIC_ADMIN_PASSWORD='strong-password' go run ./cmd/fujipic admin create --email admin@example.com
FUJIPIC_ADMIN_PASSWORD='new-password' go run ./cmd/fujipic admin reset-password --email admin@example.com
```

备份包含密码哈希与 Token 哈希等敏感数据：CLI 以 `0600` 创建备份文件，父目录为 `0750`；静态加密（`age`/`gpg`/加密文件系统）请在备份落盘后完成。删除图片后，若配置了 CDN 刷新，worker 会自动调用；未配置时记录保留在 `cdn_purges` 表，需手工刷新。`pending_deletes` 保存删除失败的 S3 对象键并自动重试。

## 部署

```bash
docker build -f deploy/Dockerfile -t fujipic:local .
docker compose -f deploy/compose.prod.yml up -d
```

生产 Compose 使用 Caddy 作为源站反向代理，CDN 放在其前面终止 TLS。需要的变量见 `deploy/compose.prod.yml` 与 `.env.example`。验收脚本：

```bash
./scripts/verify-cdn.sh https://pic.example.com https://img.example.com fp_TOKEN ./cat.png
```

## 常见问题

| 现象 | 检查 |
| --- | --- |
| 上传 413 | CDN 请求体上限小于 `MAX_UPLOAD_BYTES + 1 MiB`，或 Caddy `max_size` 太小 |
| 上传 504 | CDN 回源超时太短；降低 `MAX_UPLOAD_BYTES` 或提高超时 |
| 上传 401/403 | CDN 剥离了 `Authorization`/`Cookie`/`X-CSRF-Token`；或 `X-Origin-Secret` 不匹配 |
| 图片一直 MISS | 响应头被 CDN 剥离；缓存规则未覆盖 `/i/*`；Cookie/Authorization 进入了缓存键 |
| 删除后仍 200 | CDN 未刷新或 TTL 未到；检查 `cdn_purges` 表与刷新 API 凭据 |
| 图片 404 | 对象已删除；或 CDN 缓存了 404（应配置不缓存 4xx） |
| 登录后立刻掉线 | `APP_BASE_URL` 与浏览器地址不一致；Cookie Secure/SameSite 配置；CDN 未透传 Cookie |
| 限流把所有人当成一个 IP | `TRUST_PROXY=true` 但 `CLIENT_IP_HEADER` 配错，或 CDN 未注入真实 IP 头 |
| 注册需邀请码 | 默认 `ALLOW_OPEN_REGISTRATION=false`；管理员在 `/admin` 创建邀请码，或临时开启后关闭 |

## 目录

```text
cmd/fujipic/          单二进制入口（serve/backup/admin/version）
internal/config/      环境变量配置与校验
internal/db/          SQLite 打开与嵌入式迁移
internal/store/       SQL 查询与事务
internal/auth/        Argon2id、Token、scope
internal/storage/     Storage 接口、S3、内存实现
internal/imagefile/   图片校验、临时文件、SHA-256
internal/upload/      上传/去重/配额/删除业务
internal/cdn/         CDN 刷新适配器
internal/worker/      删除重试与 CDN 刷新 worker
internal/server/      HTTP 路由、Web UI、JSON API、回源
web/                  html/template 与静态资源
migrations/           SQL 迁移
deploy/               Dockerfile、Compose、Caddyfile
docs/CDN.md           CDN 配置与验收
scripts/verify-cdn.sh CDN 链路验收脚本
```

## 当前状态

- M0–M4 已实现；`go build`、`go test`、`go test -race`、`go vet`、`gofmt` 通过
- 本地内存存储端到端已验证：注册、登录、网页/API 上传、回源、HEAD/Range/304、去重、删除、配额归还、Token 权限
- CLI 已验证：`admin create` / `reset-password` / `backup`（临时库验证，含重复创建失败、未知用户失败、`VACUUM INTO` 备份一致性）
- `govulncheck` 已验证：代码无可达漏洞（1 个未调用的依赖告警）
- 待验收：Docker Compose + MinIO、真实 S3、真实 CDN 的缓存/刷新
