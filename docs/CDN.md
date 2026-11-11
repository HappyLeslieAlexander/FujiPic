# CDN 配置与验收

FujiPic 的 CDN 是唯一公网入口。应用只作为源站，S3 桶保持私有。本文给出通用配置步骤，Cloudflare / CloudFront / Bunny / 阿里云 CDN 的菜单名称不同，规则等价。

Cloudflare 的当前部署拓扑和具体文件见 [宿主机 Caddy 指南](DEPLOY-CLOUDFLARE-HOST.md)。

## 1. 域名与链路

| 域名 | 用途 | 回源 |
| --- | --- | --- |
| `pic.example.com` | 网站 + JSON API + 上传 | `origin.example.com`（FujiPic 应用） |
| `img.example.com` | 图片读取 | 同一个应用 |

```text
上传：浏览器/API → CDN（POST bypass）→ Go 应用 → 私有 S3
首次读：浏览器 → CDN（MISS）→ Go 应用 GET /i/{key} → S3 → CDN 缓存
后续读：浏览器 → CDN（HIT）→ 浏览器
```

`/i/{key}` 由应用从 S3 流式读取，支持 `HEAD`、`Range`、`If-None-Match`、`If-Modified-Since`。应用返回 1 年 `immutable` 缓存头，实际 TTL 由 CDN 规则决定。

## 2. 源站保护

源站不能直接暴露公网。任选其一或组合：

1. **防火墙白名单**：只允许 CDN 出口 IP 段访问源站 443/8080。
2. **共享密钥**：CDN 回源时注入 `X-Origin-Secret: <secret>`，应用设置 `ORIGIN_SHARED_SECRET` 后拒绝没有该头的请求（`/healthz`、`/readyz` 除外）。
3. **Cloudflare Tunnel / mTLS / 私有网络**：源站不监听公网地址。

如果使用共享密钥，CDN 必须在**所有**回源请求（包括 `/i/*`、`/static/*`）上注入该头。

## 3. 缓存规则

| 域名/路径 | 方法 | 缓存 | 说明 |
| --- | --- | --- | --- |
| `img.example.com/i/*` | GET/HEAD | 遵循源站，成功图片 1 年 | 默认 URL 缓存键；移除 Cookie/Authorization；不缓存 4xx/5xx |
| `pic.example.com/*` | 全部 | bypass | 覆盖 API、上传、静态资源、`/admin` 及 `/admin/*`；透传认证和 CSRF |
| `/healthz`、`/readyz` | GET | bypass | 建议只允许监控访问 |

**图片域名必须：**

- 剥离请求中的 `Cookie`、`Authorization`，避免缓存碎片和隐私泄露。
- 使用默认 URL 缓存键；应用生成的对象 URL 不带查询参数，无需额外配置忽略查询参数。
- 不要开启自动压缩、转码、裁剪、水印；保持原始字节和 `Content-Type`。
- 不要剥离 `Cache-Control`、`CDN-Cache-Control`、`ETag`、`Content-Length`、`Accept-Ranges`、`Content-Range`。
- 缓存完整 200，按 CDN 标准机制处理 206 与 304 重验证；不要用固定状态码 TTL 强制缓存所有响应。
- 不缓存 404、5xx、带 `Set-Cookie` 或 `no-store` 的响应。
- 优先缓存完整 200 并由 CDN 处理 Range；如果缓存 206，确认不会把部分内容当成完整对象返回。

**上传路径必须：**

- POST 全部 bypass，不能缓存上传响应。
- 请求体上限至少 `MAX_UPLOAD_BYTES + 1 MiB`（默认 12 MiB）。
- 回源超时至少 120 秒；慢速网络可提高上限或降低 `MAX_UPLOAD_BYTES`。
- 透传 `Authorization`、`Cookie`、`X-CSRF-Token`、`Content-Type`、`Content-Length`、`X-Filename`。
- 不记录请求体。WAF 误拦截时先核对安全事件中的规则和请求，不要全站放行这些 Content-Type。

### Cloudflare 规则要点

- 网站规则：`http.host eq "pic.example.com"` → Bypass cache。
- 图片规则：`http.host eq "img.example.com" and starts_with(http.request.uri.path, "/i/")`
  → Eligible for cache；Edge TTL 使用源站 Cache-Control（缺失时 bypass），Browser TTL 尊重源站。
  不加仅 GET 的方法条件，避免影响按 URL 刷新。
- 两域名所有路径覆盖注入 `X-Origin-Secret`；只在图片规则范围移除 Cookie/Authorization。
- Full (strict)，回源使用 443。当前宿主机部署不需要 8443 Origin Rule。
- 上传被托管 WAF 误拦截时，优先限定命中规则的例外；如套餐只允许跳过规则集，
  将例外收窄到网站主机名 + `POST` + 精确 `/api/v1/upload` 路径，仅跳过必要托管规则。
  不扩大到 `/admin`、整个网站或所有安全检查。API 上传不应要求交互式浏览器挑战。

## 4. 缓存刷新

删除图片时应用会：

1. 在同一数据库事务中软删除元数据、归还配额和桶容量，并将图片 URL 写入 `cdn_purges`；
2. 删除 S3 对象（失败进入 `pending_deletes` 重试）；
3. 后台 worker 调用 CDN 刷新 API（失败重试，最多 10 次）。

配置方式：

```bash
# Cloudflare
CDN_PURGE_PROVIDER=cloudflare
CDN_ZONE_ID=<zone id>
CDN_PURGE_TOKEN=<token with Cache Purge permission>

# 通用 CDN（POST JSON {"urls":["..."]}，Bearer 认证）
CDN_PURGE_PROVIDER=generic
CDN_PURGE_ENDPOINT=https://cdn.example.com/api/purge
CDN_PURGE_TOKEN=<token>
```

未配置刷新时，删除记录会保留在 `cdn_purges` 表中，管理员需要手工刷新。CDN 缓存删除是最终一致的：刷新完成前，已缓存的 URL 仍可能返回旧图。对象键唯一且不可变，不会串图。

## 5. 验收步骤

部署后按顺序执行，或运行 `scripts/verify-cdn.sh`：

```bash
./scripts/verify-cdn.sh https://pic.example.com https://img.example.com fp_xxxxxxxx ./cat.png
```

请使用可删除的全新测试图片，或专用测试账号。脚本会删除本次新建的图片来验证刷新；
若上传返回去重结果（HTTP 200 或 `deduplicated=true`），会立即停止并保留原图，
以非零状态退出，需更换测试图片后重试。

手工检查：

1. **上传经 CDN**：`POST /api/v1/upload` 返回 201；CDN 日志显示 bypass；源站有一条 POST。
2. **首次读 MISS**：用新上传且尚未预览的图片运行 `curl -sS -D - -o /dev/null "$IMAGE_URL"`，观察 `CF-Cache-Status`（或相应 CDN 缓存头）；浏览器已预览的图片首次手工请求也可能已经 HIT。
3. **第二次读 HIT**：重复同一个真实 GET，观察 HIT；结合源站日志确认命中请求没有回源。
4. **缓存头**：`Cache-Control: public, max-age=31536000, immutable`、`ETag`、`Content-Type` 正确。
5. **HEAD / Range / 304**：`curl -I`、`curl -H "Range: bytes=0-99"`、带 `If-None-Match` 的请求分别返回 200、206、304。
6. **删除 + 刷新**：删除后等待 worker；URL 返回 404，或 CDN MISS 后回源 404。
7. **源站保护**：直接访问 `https://origin.example.com/...` 被防火墙或 403 拒绝。
8. **API 不被缓存**：连续请求 `/api/v1/me`、`/login`，CDN 为 bypass，响应 `Cache-Control: no-store`。
9. **图片响应无 Cookie**：`curl -sSI` 不出现 `Set-Cookie`。

## 6. 常见问题

| 现象 | 检查 |
| --- | --- |
| 上传 413 | CDN 请求体上限小于 `MAX_UPLOAD_BYTES + 1 MiB`，或 Caddy `max_size` 太小 |
| 上传 504 | CDN 回源超时太短；降低 `MAX_UPLOAD_BYTES` 或提高超时 |
| 上传 401/403 | CDN 剥离了 `Authorization`/`Cookie`/`X-CSRF-Token`；或 `X-Origin-Secret` 不匹配 |
| 图片一直 MISS | 响应头被 CDN 剥离；缓存规则未覆盖 `/i/*`；Cookie/Authorization 进入了缓存键 |
| 图片一直 DYNAMIC | 检查缓存资格、源站 no-store/Set-Cookie、冲突规则与回源端口；当前部署使用宿主机 443，不能只靠调整 TTL 推断问题已解决 |
| 删除后仍 200 | CDN 未刷新或 TTL 未到；检查 `cdn_purges` 和刷新 API 凭据 |
| 图片 404 | 对象已删除；或 CDN 缓存了 404（应配置不缓存 4xx） |
| 登录后立刻掉线 | `APP_BASE_URL` 与浏览器地址不一致；Cookie Secure/SameSite 配置；CDN 未透传 Cookie |
| 限流把所有人当成一个 IP | `TRUST_PROXY=true` 但 `CLIENT_IP_HEADER` 配错，或 CDN 未注入真实 IP 头 |
