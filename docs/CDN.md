# CDN 配置与验收

FujiPic 的 CDN 是唯一公网入口。应用只作为源站，S3 桶保持私有。本文给出通用配置步骤，Cloudflare / CloudFront / Bunny / 阿里云 CDN 的菜单名称不同，规则等价。

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
| `img.example.com/i/*` | GET/HEAD | 1 年 | 缓存键只用 URL；忽略 Cookie/Authorization；缓存 200/206/304；不缓存 4xx/5xx |
| `pic.example.com/api/*` | 全部 | bypass | 透传 Authorization/Cookie/CSRF；响应 `no-store` |
| `pic.example.com/upload` | POST | bypass | 透传 multipart，不缓存 |
| `pic.example.com/`、`/dashboard`、`/login`、`/register`、`/settings`、`/admin`、`/logout` | 全部 | bypass | 透传 Cookie/CSRF；响应 `no-store` |
| `pic.example.com/static/*` | GET/HEAD | 7 天 | 静态资源 |
| `/healthz`、`/readyz` | GET | bypass | 建议只允许监控访问 |

**图片域名必须：**

- 剥离请求中的 `Cookie`、`Authorization`，避免缓存碎片和隐私泄露。
- 缓存键只使用 URL，不包含查询参数（对象 URL 不带查询参数）。
- 不要开启自动压缩、转码、裁剪、水印；保持原始字节和 `Content-Type`。
- 不要剥离 `Cache-Control`、`CDN-Cache-Control`、`ETag`、`Content-Length`、`Accept-Ranges`、`Content-Range`。
- 缓存 200、206、304；不要缓存 404、5xx、带 `Set-Cookie` 的响应。
- 优先缓存完整 200 并由 CDN 处理 Range；如果缓存 206，确认不会把部分内容当成完整对象返回。

**上传路径必须：**

- POST 全部 bypass，不能缓存上传响应。
- 请求体上限至少 `MAX_UPLOAD_BYTES + 1 MiB`（默认 12 MiB）。
- 回源超时至少 120 秒；慢速网络可提高上限或降低 `MAX_UPLOAD_BYTES`。
- 透传 `Authorization`、`Cookie`、`X-CSRF-Token`、`Content-Type`、`Content-Length`、`X-Filename`。
- 不记录请求体；WAF 不要拦截 `multipart/form-data` 和 `application/octet-stream`。

## 4. 缓存刷新

删除图片时应用会：

1. 软删除元数据并归还配额；
2. 删除 S3 对象（失败进入 `pending_deletes` 重试）；
3. 把图片 URL 写入 `cdn_purges`，由后台 worker 调用 CDN 刷新 API（失败重试，最多 10 次）。

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

手工检查：

1. **上传经 CDN**：`POST /api/v1/upload` 返回 201；CDN 日志显示 bypass；源站有一条 POST。
2. **首次读 MISS**：`curl -sSI https://img.example.com/i/...` 看到 `cf-cache-status: MISS` 或 `x-cache: Miss`；源站有一条 GET。
3. **第二次读 HIT**：同样请求看到 HIT；源站没有新增 GET；S3 没有新增 `GetObject`。
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
| 删除后仍 200 | CDN 未刷新或 TTL 未到；检查 `cdn_purges` 和刷新 API 凭据 |
| 图片 404 | 对象已删除；或 CDN 缓存了 404（应配置不缓存 4xx） |
| 登录后立刻掉线 | `APP_BASE_URL` 与浏览器地址不一致；Cookie Secure/SameSite 配置；CDN 未透传 Cookie |
| 限流把所有人当成一个 IP | `TRUST_PROXY=true` 但 `CLIENT_IP_HEADER` 配错，或 CDN 未注入真实 IP 头 |
