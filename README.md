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
export LISTEN_ADDR=127.0.0.1:8080
export DATABASE_PATH=./data/fujipic.db
export TMP_DIR=./data/tmp
export STORAGE_DRIVER=memory
export ALLOW_OPEN_REGISTRATION=true
export BOOTSTRAP_ADMIN_EMAIL=admin@example.com
export BOOTSTRAP_ADMIN_PASSWORD=admin-password-123

go run ./cmd/fujipic serve
```

打开 <http://localhost:8080>，注册普通用户或用上面的管理员登录。内存驱动重启后图片丢失，仅用于演示和测试；正式使用请配置 S3。应用二进制的完整单机部署、反向代理及公网访问流程请见下一节。

## 完整部署教程：单机二进制与公网反向代理

本节适合一台 Linux 服务器已经具备公网 IP、打算让 FujiPic 只监听本机，再通过 Caddy 把公网 HTTPS 反向代理到它的部署方式：

```text
浏览器或 API 客户端
       │ HTTPS
       ▼
Caddy（公网域名 pic.example.com / img.example.com）
       │ 127.0.0.1:8080
       ▼
FujiPic 二进制服务
       │ 私有 S3/MinIO
       ▼
private S3-compatible bucket
```

应用直接以 `127.0.0.1:8080` 监听，所以不能被绕过反向代理从公网直接访问。文章以下所有具体域名均需替换为你自己的域名，然后按 DNS 解析、配置与验收顺序执行。

### 1. 服务器与域名准备

服务器建议使用 Ubuntu/Debian 22.04 及以上，x86_64 或 ARM64，至少预留 2 GB 磁盘空间和 1 GB 可用内存。公网域名解析至少需要：

| 记录 | 类型 | 指向 |
| --- | --- | --- |
| `pic.example.com` | A/AAAA | 服务器公网 IP |
| `img.example.com` | A/AAAA | 服务器公网 IP |

云防火墙需要放行 TCP 80 与 443，供 Caddy 自动申请和刷新 HTTPS 证书。8080 只在服务器本机监听，不要在云安全组、防火墙或 Docker 端口映射中对公网开放。

### 2. 安装系统依赖

二进制本身使用 `CGO_ENABLED=0` 交叉/静态构建，运行环境只需要 Git、curl、CA 和 Go 工具链：

```bash
sudo apt-get update
sudo apt-get install -y ca-certificates curl git

go version
```

`go.mod` 需要 Go 1.27 或更高版本。如果发行版仓库的 Go 版本过低，使用官方 Go 二进制安装包：

```bash
ARCH=$(uname -m)
case "$ARCH" in
  x86_64) GO_ARCH=amd64 ;;
  aarch64) GO_ARCH=arm64 ;;
  *) echo "unsupported architecture: $ARCH" >&2; exit 1 ;;
esac
curl -fsSL -o /tmp/go.tar.gz "https://go.dev/dl/go1.27.3.linux-${GO_ARCH}.tar.gz"
sudo rm -rf /usr/local/go
sudo tar -C /usr/local -xzf /tmp/go.tar.gz
export PATH="/usr/local/go/bin:$PATH"
go version
```

本机 MinIO 开发环境还需要 Docker 与 Docker Compose。官方 MinIO Docker 仓库和 MC 仓库已经下线；本仓库开发 Compose 已固定使用当前可用的 `alpine/minio:RELEASE.2025-10-15T17-29-55Z` 和 `bitnamilegacy/minio-client:2025.7.21-debian-12-r3`。

### 3. 创建目录并克隆仓库

```bash
sudo useradd -r -m -d /opt/fujipic -s /usr/sbin/nologin fujipic
sudo install -d -o fujipic -g fujipic -m 750 \
  /opt/fujipic /opt/fujipic/bin /opt/fujipic/env \
  /opt/fujipic/data /opt/fujipic/tmp /opt/fujipic/src
sudo -u fujipic git clone https://github.com/HappyLeslieAlexander/FujiPic.git /opt/fujipic/src
cd /opt/fujipic/src
sudo -u fujipic git switch main
```

如果仓库已经存在，改为：

```bash
cd /opt/fujipic/src
sudo -u fujipic git pull --ff-only
```

### 4. 编译并安装二进制

```bash
cd /opt/fujipic/src
make build
sudo install -o fujipic -g fujipic -m 0755 bin/fujipic /opt/fujipic/bin/fujipic
/opt/fujipic/bin/fujipic version
```

`make build` 会生成 `CGO_ENABLED=0` 的静态可执行文件 `bin/fujipic`。随后安装到 `/opt/fujipic/bin/fujipic`，服务本身不需要 Go 运行时。

### 5. 配置反向代理与公网地址

公网域名 `pic.example.com` 负责网站和 API，`img.example.com` 负责图片返回 URL。两者都可以由同一个 Caddy 站点配置反代到 `127.0.0.1:8080`。

先安装 Caddy：

```bash
sudo apt-get install -y caddy
```

`/etc/caddy/Caddyfile` 示例：

```caddy
pic.example.com, img.example.com {
    encode zstd gzip

    request_body {
        max_size 12MB
    }

    # 只转发可信连接信息，不转发客户端直接注入的伪造 CF 头。
    request_header {
        -CF-Connecting-IP
    }

    reverse_proxy 127.0.0.1:8080
}
```

Caddy 会为域名自动申请 Let’s Encrypt 证书并监听 HTTPS 443。调整后验证并重载：

```bash
caddy validate --config /etc/caddy/Caddyfile
sudo systemctl reload caddy
```

也可以用同网段的 Cloudflare Tunnel 代替 Caddy，但 Tunnel 的源站地址应为 `http://127.0.0.1:8080`，不要直接把应用的 8080 端口暴露到公网。

### 6. 编写生产环境配置

创建 `/opt/fujipic/env/fujipic.env`，权限只保留给服务用户：

```bash
sudo tee /opt/fujipic/env/fujipic.env >/dev/null <<'EOF'
APP_ENV=production
APP_BASE_URL=https://pic.example.com
PUBLIC_BASE_URL=https://img.example.com
LISTEN_ADDR=127.0.0.1:8080

DATABASE_PATH=/opt/fujipic/data/fujipic.db
TMP_DIR=/opt/fujipic/tmp

STORAGE_DRIVER=s3
S3_ENDPOINT=http://127.0.0.1:9000
S3_REGION=us-east-1
S3_BUCKET=fujipic
S3_ACCESS_KEY_ID=replace-with-minio-root-or-limited-user
S3_SECRET_ACCESS_KEY=replace-with-minio-root-or-limited-secret
S3_FORCE_PATH_STYLE=true

MAX_UPLOAD_BYTES=10485760
MAX_IMAGE_PIXELS=50000000
MAX_IMAGE_DIMENSION=12000
DEFAULT_QUOTA_BYTES=1073741824
ALLOW_OPEN_REGISTRATION=false

BOOTSTRAP_ADMIN_EMAIL=admin@example.com
BOOTSTRAP_ADMIN_PASSWORD=replace-with-a-strong-password
CONTACT_EMAIL=abuse@example.com

TRUST_PROXY=true
TRUSTED_PROXY_CIDRS=127.0.0.0/8,::1/128,172.16.0.0/12,192.168.0.0/16,10.0.0.0/8
CLIENT_IP_HEADER=X-Forwarded-For
ORIGIN_SHARED_SECRET=replace-with-a-long-random-secret
LOG_LEVEL=info
EOF
sudo chown fujipic:fujipic /opt/fujipic/env/fujipic.env
sudo chmod 600 /opt/fujipic/env/fujipic.env
```

这份示例中的 S3 端点是“本机 MinIO”模式。生产环境可把 `S3_ENDPOINT`、区域、Bucket 和 credentials 换成你的私有云厂商 S3/R2 地址；MinIO 需要 `S3_FORCE_PATH_STYLE=true`。应用本身不会自动创建 bucket，所以 bucket 必须先存在。

如果是使用本机 Docker MinIO，浏览器入口和控制台都应只绑定到回环地址，然后 Fujipic 经 `127.0.0.1:9000` 访问，不要把 9000/9001 暴露到公网。MinIO 容器中的数据卷应使用 MinIO 镜像用户的 UID/GID 100:101 写入。

### 7. 用 systemd 运行二进制

```ini
# /etc/systemd/system/fujipic.service
[Unit]
Description=FujiPic image host
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=fujipic
Group=fujipic
WorkingDirectory=/opt/fujipic/src
EnvironmentFile=/opt/fujipic/env/fujipic.env
ExecStart=/opt/fujipic/bin/fujipic serve
Restart=on-failure
RestartSec=3s
PrivateTmp=true
NoNewPrivileges=true
ProtectHome=true
ProtectSystem=strict
ReadWritePaths=/opt/fujipic/data /opt/fujipic/tmp

[Install]
WantedBy=multi-user.target
```

`ProtectSystem=strict` 会使根文件系统只读，因此服务可写路径需要由 `[Service]` 中的 `ReadWritePaths=/opt/fujipic/data /opt/fujipic/tmp` 明确开放。启动并查看日志：

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now fujipic
systemctl status fujipic --no-pager
sudo journalctl -u fujipic -f
ss -ltnp | grep 8080
```

`ss` 应只看到 `127.0.0.1:8080` 这一监听地址。Caddy 负责外部的 80/443，应用不直接暴露。

### 8. 首次验证

本机和公网各做一次健康检查：

```bash
curl -fsS http://127.0.0.1:8080/healthz
curl -fsS http://127.0.0.1:8080/readyz
curl -fsS https://pic.example.com/healthz
curl -fsS https://pic.example.com/readyz
```

然后使用 `BOOTSTRAP_ADMIN_EMAIL` 密码登录网页，进入“设置”生成 API Token，并验证：

```bash
curl -X POST https://pic.example.com/api/v1/upload \
  -H "Authorization: Bearer fp_..." \
  -F "file=@cat.png"
```

用返回的 URL 做：

```bash
curl -I "$IMAGE_URL"
curl -H 'Range: bytes=0-99' -o /tmp/range.bin "$IMAGE_URL"
curl -X DELETE "https://pic.example.com/api/v1/images/<image-id>" \
  -H "Authorization: Bearer fp_..."
```

最后用没有凭据的请求直访 MinIO bucket 中对应对象，预期返回 403/AccessDenied；MinIO 控制台也不要对公网开放。

### 9. 更新与备份

更新应用：

```bash
cd /opt/fujipic/src
sudo -u fujipic git pull --ff-only
make build
sudo install -o fujipic -g fujipic -m 0755 bin/fujipic /opt/fujipic/bin/fujipic
sudo systemctl restart fujipic
curl -fsS https://pic.example.com/readyz
```

如果本机有 MinIO Docker 容器，先确认它们健康，再重启 FujiPic。备份数据库：

```bash
set -a
. /opt/fujipic/env/fujipic.env
set +a
sudo -u fujipic /opt/fujipic/bin/fujipic backup --out /opt/fujipic/data/backup/fujipic.db
```

备份包含密码和 Token 哈希，应保持 0600 权限并按需要加密到对象存储或加密备份介质。

也可以使用 `.env.example` 作为模板：

```bash
cp .env.example /opt/fujipic/env/fujipic.env
set -a; source .env; set +a
go run ./cmd/fujipic serve
```

## Docker Compose（MinIO 开发环境）

这部分是一份面向新手的本地开发流程：在同一台电脑或开发服务器上启动 **FujiPic 应用 + MinIO S3 存储**，用于完整验证登录、上传、读取和删除链路。Docker Compose 的生产部署在“Docker 部署”部分单独说明。

### 1. 准备 Docker

macOS 请先安装 Docker Desktop，确保 Docker daemon 正在运行；服务器请按发行版安装 Docker Engine 与 Compose 插件。验证：

```bash
docker --version
docker compose version
docker ps
```

如果 `docker ps` 提示权限不足，Linux 上通常要把当前用户加入 `docker` 组，然后重新登录 shell：

```bash
sudo usermod -aG docker "$USER"
newgrp docker
docker ps
```

### 2. 克隆并进入仓库

```bash
git clone https://github.com/HappyLeslieAlexander/FujiPic.git
cd FujiPic
```

### 3. 启动开发环境

```bash
make dev
```

等价于：

```bash
docker compose -f deploy/compose.dev.yml up -d --build
```

第一次运行需要拉取镜像并构建 FujiPic 镜像，可能需要几分钟。构建和启动完成后查看容器。开发 Compose 有三个服务：

| 服务 | 作用 |
| --- | --- |
| `minio` | 在 Docker 内部模拟 S3 存储，默认保存 MinIO 数据卷 |
| `createbucket` | MinIO 健康后运行一次，创建私有桶 `fujipic`，并执行 `mc anonymous set none` |
| `app` | 构建并运行 FujiPic，使用 S3/MinIO 存储上传图片，SQLite 和临时文件保存在 `app-data` 卷 |

查看容器状态、镜像和端口：

```bash
docker compose -f deploy/compose.dev.yml ps
```

正常情况下能看到：

```text
NAME                  STATUS                  PORTS
fujipic-dev-app-1     Up                      0.0.0.0:8080->8080/tcp
fujipic-dev-createbucket-1 Exited (0)         （一次性初始化任务，已完成）
fujipic-dev-minio-1   Up (healthy)            0.0.0.0:9000-9001->9000-9001/tcp
```

### 4. 验证应用、登录和上传

应用地址：

```text
http://localhost:8080
```

开发环境默认管理员：

```text
邮箱：admin@example.com
密码：admin-password-123
```

先验证健康检查：

```bash
curl -fsS http://localhost:8080/healthz
curl -fsS http://localhost:8080/readyz
```

`readyz` 应包含 `storage: true`，这表示应用已经连接上 MinIO。然后打开 `http://localhost:8080` 登录并上传任意 JPG/PNG/GIF/WebP。也可以在设置页创建 API Token，再用登录会话或 Bearer Token 上传。

MinIO 控制台地址：

```text
http://localhost:9001
账号：minioadmin
密码：minioadmin
```

控制台里应能看到私有桶 `fujipic`；但不要在未登录的情况下通过对应 MinIO 对象 URL 直接下载图片。正确做法是先在 FujiPic 应用中上传，再打开返回的图片 URL。未认证的 MinIO 直访应得到 403 或 AccessDenied。

### 5. 日志、重启和停止

查看全部服务日志：

```bash
make logs
```

只看应用日志：

```bash
docker compose -f deploy/compose.dev.yml logs -f app
```

重启开发环境：

```bash
docker compose -f deploy/compose.dev.yml restart app
docker compose -f deploy/compose.dev.yml restart minio
```

停止开发环境，保留容器数据卷：

```bash
make down
```

如果开发 MinIO 和应用数据也不再需要，可显式删除卷：

```bash
docker compose -f deploy/compose.dev.yml down -v
```

该命令会删除 `minio-data`、`app-data` 以及它们保存的本地上传和数据库，仅应在舍弃本地开发数据时使用。

### 6. 开发环境注意事项

- `deploy/compose.dev.yml` 是开发学习配置，不是生产配置；默认管理员和 MinIO 凭据必须在公开或长期运行前修改。
- 开发环境开启了开放注册，且应用以 HTTP 直接发布到 `8080`；不要把这个本机 MinIO 开发配置直接当作公网生产服务。
- 如果本机 Docker 尚未安装，可安装 Docker Desktop 或在服务器上按照官方渠道安装 Docker Engine 和 Compose 插件。
- 需要正式公网访问时，请改用本文前面的“单机二进制与公网反向代理”部署方式，或使用生产 Docker Compose 与 CDN 配置。

该 Compose 已在 ARM64 服务器上实际启动验证：MinIO 私有桶、登录、上传、图片回读和匿名对象访问拒绝均已通过检查。

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
- ARM64 服务器已验证 Docker Compose、MinIO、私有桶写入和匿名拒绝；真实 CDN 缓存/刷新仍待部署环境验证
