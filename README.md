# FujiPic

轻量、多用户的自托管图床。Go 单二进制提供中文 Web 界面和 HTTP API，SQLite 保存元数据，私有 S3 兼容桶保存图片，Cloudflare 缓存公开图片与静态资源。

**部署入口：[Docker + Caddy + Cloudflare 完整教程](docs/DEPLOYMENT.md)**

教程从全新 Ubuntu、私有桶和域名开始，逐步完成 Docker 安装、证书、Cloudflare 规则、首次启动、管理员设置、可选 Turnstile、验收、备份恢复与升级回退。

## 功能

- **上传与图库**：拖拽、粘贴、多选上传，复制链接/Markdown，分页浏览与删除。
- **图片校验**：MIME、文件大小、像素和边长限制；拒绝 SVG；同用户 SHA-256 去重。
- **账号管理**：邀请码注册、管理员创建用户、禁用/启用、用户配额与审计日志。
- **会话与 Token**：查看登录会话、注销其他浏览器；独立 API Token、scope 与过期设置。
- **存储桶池**：管理员管理多个私有 S3 兼容桶，按优先级与容量分配；自定义桶凭据加密保存。
- **图片分发**：不可变随机对象路径，流式读取，支持 HEAD、Range、ETag 和条件请求。
- **删除与刷新**：删除对象、释放配额、排队刷新 CDN，后台重试失败任务。
- **可选功能**：Turnstile 登录/注册验证、额外图片域名、管理员全用户图库。

图片 URL 是公开链接。私有桶保护存储入口，不对已获得图片链接的访问者做登录认证；删除无法撤回浏览器缓存和已下载副本。

## 部署结构

```text
浏览器 / API → Cloudflare HTTPS :443
                    │ HTTPS → 服务器 TCP 443
                    ▼
           Docker Caddy（443:8443）
                    │ 内部 HTTP :8080
                    ▼
              Docker FujiPic
                ├── SQLite 数据卷
                └── 私有 AWS S3 / OCI / S3 兼容桶
```

Cloudflare 不直接访问存储桶；上传、首次读取都经过 FujiPic，缓存命中后由边缘直接返回图片。应用容器不发布公网端口，无需 Redis、外部任务队列或 Node.js 构建。

生产部署使用 [`deploy/compose.cloudflare.yml`](deploy/compose.cloudflare.yml) 和 [`deploy/cloudflare.env.example`](deploy/cloudflare.env.example)。不要跳过教程中的 Cloudflare 请求头、证书和缓存配置直接启动：应用需要有效的存储及源站认证配置。

### 教程导航

| 目标 | 章节 |
| --- | --- |
| 安装 Docker、准备私有桶 | [服务器](docs/DEPLOYMENT.md#step-2) · [存储](docs/DEPLOYMENT.md#step-3) |
| 配置域名、证书与缓存 | [DNS/TLS](docs/DEPLOYMENT.md#step-5) · [规则](docs/DEPLOYMENT.md#step-6) |
| 启动与管理 | [启动](docs/DEPLOYMENT.md#step-7) · [后台](docs/DEPLOYMENT.md#step-8) |
| 启用验证码 | [Turnstile](docs/DEPLOYMENT.md#step-9) |
| 验收与 API | [验收](docs/DEPLOYMENT.md#step-10) · [接口](docs/DEPLOYMENT.md#step-11) |
| 日常运维 | [备份恢复](docs/DEPLOYMENT.md#step-12) · [升级回退](docs/DEPLOYMENT.md#step-13) · [排障](docs/DEPLOYMENT.md#step-15) |

## 本地开发

需要 **Go 1.27.4+**。在仓库根目录运行以下命令可使用内存对象存储体验界面，无需云凭据：

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

打开 `http://localhost:8080`。上述密码仅用于本地体验。**内存驱动重启后丢失图片字节**，SQLite 元数据仍在；需要持久图片时配置真实的测试桶。仓库没有本地 S3 模拟器或开发 Compose。

完整环境变量示例见 [`.env.example`](.env.example)，代码默认值见 [`internal/config/config.go`](internal/config/config.go)。程序读取进程环境，不会自动加载 `.env`；生产 Compose 使用教程中的 `--env-file`。Compose 模板会覆盖部分代码默认值，例如登录会话有效期为 24 小时，而独立二进制默认 30 天。

### 构建与检查

```bash
go build ./...
go test ./...
go test -race ./...
go vet ./...
gofmt -l cmd internal migrations web
```

`gofmt -l` 应无输出。`make build` 将二进制写入 `bin/fujipic`。CLI 提供 `serve`、`backup --out`、`admin create`、`admin reset-password`、`version`；管理员 CLI 密码通过 `FUJIPIC_ADMIN_PASSWORD` 传入。

## 代码结构

```text
cmd/fujipic/       CLI 与服务入口
internal/         配置、认证、数据库、存储、上传、CDN、worker、HTTP 服务
migrations/       嵌入式 SQL 迁移
web/              嵌入式 HTML 模板、CSS、JavaScript
deploy/           Dockerfile、Compose、Caddy 与环境模板
scripts/          CDN 验收、Caddy 代理回归检查
docs/DEPLOYMENT.md 完整部署与运维教程
```

已有宿主机 Caddy 部署可以继续维护自己的 `compose.cloudflare-host.yml` 配置；新教程使用全容器方案。历史设计记录在 [`PLAN.md`](PLAN.md)，不作为当前操作指南。文档重建前的全部提交及旧文档保存在 [`backup20261112`](https://github.com/HappyLeslieAlexander/FujiPic/tree/backup20261112) 分支。

请勿提交真实环境配置、API Token、存储凭据、证书私钥、数据库或备份。部署者应使用自己的域名和凭据，并按教程完成实际云环境验收。
