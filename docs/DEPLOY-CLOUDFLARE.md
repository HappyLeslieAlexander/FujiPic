# Cloudflare 部署入口

当前推荐链路：**Cloudflare → 宿主机 Caddy HTTPS 443 → Docker FujiPic 应用**。

- [部署、迁移、备份与升级](DEPLOY-CLOUDFLARE-HOST.md)
- [CDN 缓存、源站保护与验收](CDN.md)
- [后台管理、容量与多桶调度](ADMIN.md)
- [OCI 认证故障排查](OCI-TROUBLESHOOTING.md)

使用 `deploy/compose.cloudflare-host.yml` 和 `deploy/Caddyfile.cloudflare-host`。
网站与图片使用独立域名；网站绕过缓存，图片 `/i/*` 按源站响应头缓存。

旧 `deploy/compose.cloudflare.yml` 使用容器 Caddy 8443，步骤已归档到
[旧架构指南](archive/DEPLOY-CLOUDFLARE-8443.md)。实际部署中曾出现图片持续 DYNAMIC，
迁移到宿主机 443 后观察到 HIT；这不等于证明所有 8443 回源都无法缓存。

两份 Compose 使用同一项目名和数据卷。迁移完成后日常运维只使用宿主机方案文件，
**不要对旧文件运行 `down`，尤其不能运行 `down -v`**。
