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
- 存储：管理员配置的全局多存储桶池（AWS S3、OCI Object Storage），支持容量上限、优先级与自动溢出
- 部署：单二进制、SQLite WAL、Docker distroless 镜像、生产 Compose（Caddy）

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
       │ 私有 S3 桶
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

只有使用 Docker 部署时才需要 Docker 与 Docker Compose。本地开发不需要 S3 模拟器：界面/API 调试可用内存存储，真实上传链路请使用 AWS S3 或 OCI Object Storage 测试桶。

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

此单机示例依靠 **FujiPic 仅监听 `127.0.0.1:8080`** 隔离源站，下面的
`ORIGIN_SHARED_SECRET` 必须保持为空；上面的 Caddy 配置没有注入
`X-Origin-Secret`。不要单独把该变量改为非空，否则包括登录、上传和静态资源在内的
请求都会返回 403，而健康检查仍可能正常。

如果在 Caddy 前再部署 CDN，并需要共享密钥保护，必须先按
[源站保护](docs/CDN.md#2-源站保护)让 CDN 在**所有回源请求**中覆盖注入
`X-Origin-Secret`，再把应用的 `ORIGIN_SHARED_SECRET` 设置为相同的随机值。
密钥应仅保存在代理/CDN 和服务端配置中，不能放进网页或浏览器请求。

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
# AWS S3 留空使用官方端点；OCI 填 S3 兼容端点
S3_ENDPOINT=
S3_REGION=ap-northeast-1
S3_BUCKET=my-fujipic-bucket
S3_ACCESS_KEY_ID=replace-with-least-privilege-access-key
S3_SECRET_ACCESS_KEY=replace-with-least-privilege-secret
S3_FORCE_PATH_STYLE=false
# 可选：启用管理员添加自定义存储桶（openssl rand -base64 32）
STORAGE_ENCRYPTION_KEY=

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
# 本教程采用回环监听隔离；Caddy 未注入共享密钥，所以必须留空。
ORIGIN_SHARED_SECRET=
LOG_LEVEL=info
EOF
sudo chown fujipic:fujipic /opt/fujipic/env/fujipic.env
sudo chmod 600 /opt/fujipic/env/fujipic.env
```

示例使用 AWS S3。OCI Object Storage 需要把 `S3_ENDPOINT` 换成 `https://<namespace>.compat.objectstorage.<region>.oraclecloud.com`，并设置 `S3_FORCE_PATH_STYLE=true`；更多说明见“全局存储桶池”一节。应用不会自动创建 bucket，也不会修改桶策略，所以 bucket 必须先存在并保持私有。

`STORAGE_ENCRYPTION_KEY` 用于加密管理员添加的自定义桶凭据；留空时服务器默认桶仍正常工作，但不能再添加其他桶。生成方式见“全局存储桶池”一节。

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
# 必须额外验证普通页面：健康检查不受 ORIGIN_SHARED_SECRET 保护。
curl -fsS -o /dev/null -w 'login HTTP %{http_code}\n' https://pic.example.com/login
```

最后一条应显示 `login HTTP 200`。如果显示 403，检查应用是否设置了非空
`ORIGIN_SHARED_SECRET`，以及代理/CDN 是否真的注入了相同的 `X-Origin-Secret`。

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

最后用没有凭据的请求直访桶内对应对象或桶的公开端点，预期返回 403/AccessDenied；桶策略不要允许匿名访问。

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

如果使用云存储桶，先确认端点、区域和凭据仍然有效，再重启 FujiPic。备份数据库：

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

## 全局存储桶池（AWS S3 与 OCI Object Storage）

FujiPic 只使用云上私有 S3 兼容桶，不包含本地 S3 模拟器，也不会自动创建桶。所有存储桶由管理员在 `/admin` 的“存储桶池”中统一配置，普通用户不能添加、删除或选择存储桶。

### 工作方式

- `S3_*` 配置的服务器默认桶始终存在于池中（固定 ID `bkt_default`），默认优先级 `0`。
- 管理员可以添加任意数量的 S3 兼容桶，例如美国、德国、东京各一个；新桶默认优先级 `100`。
- 升级会将默认桶原来的优先级 `1000` 调整为 `0`；其他已保存的优先级保持不变。默认桶容量为 `0`（不限）且启用时，通常会一直优先使用它。
- 优先级数字越小越先使用。上传时服务端按 `优先级 ASC` 找到第一个启用且仍有容量的桶，自动写入。
- 每个桶可设置最大容量；`0` 表示不限制。容量在写入对象前原子预留，写入失败会归还。
- 当前桶达到容量上限后，下一次上传自动使用下一个有剩余容量的桶；所有桶都满时 API 返回 `507 storage_full`，网页提示“所有存储桶容量已满”。
- 图片软删除时归还用户配额和桶容量；服务启动时会根据活跃图片重新校准每个桶的 `used_bytes`。
- 删除桶前必须先删除桶内所有图片、清空待重试删除任务并等待进行中的上传完成；服务器默认桶不能删除。

### AWS S3

1. 创建私有桶，开启 Block Public Access，不要开启公开读。
2. 为应用创建最小权限 IAM 用户，至少允许该桶的 `s3:PutObject`、`s3:GetObject`、`s3:DeleteObject`；添加自定义桶时的探针还会写入并删除 `.fujipic-probe/` 下的临时对象。
3. 服务器默认桶配置：

```bash
STORAGE_DRIVER=s3
S3_ENDPOINT=
S3_REGION=ap-northeast-1
S3_BUCKET=my-fujipic-bucket
S3_ACCESS_KEY_ID=...
S3_SECRET_ACCESS_KEY=...
S3_FORCE_PATH_STYLE=false
```

AWS 使用虚拟主机风格端点，`S3_ENDPOINT` 留空即可；`S3_REGION` 必须与桶所在区域一致。默认桶的容量和优先级可以在管理后台修改，凭据仍只来自环境变量。

### OCI Object Storage

OCI 通过 S3 Compatibility API 接入：

```bash
STORAGE_DRIVER=s3
S3_ENDPOINT=https://<namespace>.compat.objectstorage.ap-tokyo-1.oraclecloud.com
S3_REGION=ap-tokyo-1
S3_BUCKET=my-fujipic-bucket
S3_ACCESS_KEY_ID=...
S3_SECRET_ACCESS_KEY=...
S3_FORCE_PATH_STYLE=true
```

在 OCI 控制台创建私有桶和 Customer Secret Key，并授予该桶的对象读写/删除权限。端点必须可从 FujiPic 服务器访问；生产环境要求 HTTPS。默认桶同样可以参与容量和优先级调度。

### 添加更多存储桶

1. 生成 32 字节密钥并写入环境变量（只需设置一次，之后不要更换）：

```bash
openssl rand -base64 32
# STORAGE_ENCRYPTION_KEY=<输出>
```

2. 重启 FujiPic。管理员登录后进入 `/admin`，在“存储桶池 → 添加存储桶”填写名称、端点、区域、桶名、凭据、最大容量和优先级。
3. 保存前 FujiPic 会执行 Put/Get/Delete 探针，验证端点、凭据和读写权限；失败不会保存。
4. 凭据使用 AES-256-GCM 加密后写入 SQLite；端点解析和实际拨号都会拒绝回环、内网、链路本地和保留地址，避免 SSRF。
5. 添加完成后无需重启，上传会立即按新的优先级和容量调度。

未设置 `STORAGE_ENCRYPTION_KEY` 时，只能使用 `S3_*` 默认桶；管理员仍可修改默认桶的容量、优先级和启用状态。

### 本地开发

本地不需要 S3 模拟器。只验证界面和 API 时使用内存驱动（重启即丢失数据）：

```bash
APP_ENV=development \
APP_BASE_URL=http://localhost:8080 \
PUBLIC_BASE_URL=http://localhost:8080 \
DATABASE_PATH=./data/fujipic.db \
TMP_DIR=./data/tmp \
STORAGE_DRIVER=memory \
ALLOW_OPEN_REGISTRATION=true \
BOOTSTRAP_ADMIN_EMAIL=admin@example.com \
BOOTSTRAP_ADMIN_PASSWORD=admin-password-123 \
go run ./cmd/fujipic serve
```

需要验证真实上传/回源链路时，直接使用上面的 AWS S3 或 OCI 测试桶；不要把生产桶凭据提交到仓库。

### 注意事项

- 桶必须保持私有；CDN 只回源到 FujiPic 的 `/i/{key}`，不能直连桶。
- 应用不会自动建桶，也不会修改桶策略。
- `STORAGE_ENCRYPTION_KEY` 丢失或更换后，已保存的自定义桶凭据无法解密，需要管理员重新添加这些桶。
- 当前仓库的自动化测试使用内存存储和 HTTP 测试替身；真实 AWS/OCI 与 CDN 验收需要实际凭据和部署环境。

## 构建与测试

```bash
go build ./...          # 编译
go test ./...           # 单元 + HTTP 集成测试
go test -race ./...     # 竞态检测
go vet ./...            # 静态检查
make build              # 生成 bin/fujipic（CGO_ENABLED=0）
python3 scripts/test-browser.py # 浏览器上传回归；需要 Chrome/Chromium，可设置 CHROME_BIN
```

CI 见 `.github/workflows/ci.yml`，执行 `go build`、`go vet`、`go test -race`。

浏览器回归直接加载真实 `web/static/app.js`，验证中文/emoji 文件名、空 MIME 类型、
上传队列、去重和图库计数；不引入 Node 构建步骤。HTTP 响应使用测试替身，
文件名请求头校验使用浏览器原生 XMLHttpRequest。

## API

认证方式：

- 网页登录：Cookie + `X-CSRF-Token`（或表单 `csrf`）
- API：`Authorization: Bearer fp_...`，Token 在“设置”页创建，支持 `upload,read,delete` scope 和过期时间

上传（存储桶由服务端自动选择，客户端不能指定）：

```bash
curl -X POST https://pic.example.com/api/v1/upload \
  -H "Authorization: Bearer fp_..." \
  -F "file=@cat.png"
```

响应（201；去重时 200 且 `deduplicated=true`）：

```json
{
  "id": "img_...",
  "bucket_id": "bkt_default",
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
| POST | `/api/v1/upload` | 上传（multipart `file` 或原始 body + `X-Filename`）；服务端自动选择存储桶 |
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

常见状态码：401 未认证、403 CSRF/scope/配额、413 文件过大、415 类型不支持、422 像素超限、429 限流、507 所有存储桶容量已满。

## 配置

所有配置通过环境变量读取，完整列表见 [`.env.example`](.env.example)。关键项：

| 变量 | 说明 |
| --- | --- |
| `APP_ENV` | `production` / `development` |
| `APP_BASE_URL` | 网站/API 的公网地址（CDN 域名） |
| `PUBLIC_BASE_URL` | 图片公网地址（CDN 图片域名，不能带路径） |
| `STORAGE_DRIVER` | `s3` / `memory` |
| `S3_ENDPOINT`、`S3_REGION`、`S3_BUCKET`、`S3_ACCESS_KEY_ID`、`S3_SECRET_ACCESS_KEY` | 服务器默认私有桶连接信息 |
| `S3_FORCE_PATH_STYLE` | OCI 等 path-style 端点需要 `true`；AWS S3 为 `false` |
| `STORAGE_ENCRYPTION_KEY` | 可选，32 字节 base64/hex 密钥；启用管理员添加自定义存储桶 |
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
internal/bucket/      全局存储桶池、优先级选择、容量预留、SSRF 防护、探针
internal/secrets/     AES-256-GCM 凭据加密
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
- 全局存储桶池已通过内存/HTTP 测试替身验证：添加探针、凭据加密、优先级自动选择、容量上限与溢出、满池 507、按桶回源/删除、删除保护
- 真实 AWS S3 / OCI Object Storage 与 CDN 缓存/刷新仍待具备凭据和部署环境的验收
