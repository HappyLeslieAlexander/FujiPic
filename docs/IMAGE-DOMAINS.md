# 第二图片域名与 Cloudflare 自定义主机名

## 应用配置

```dotenv
PUBLIC_BASE_URL=https://img.example.com
PUBLIC_BASE_ALIASES=https://img.second.example
```

`PUBLIC_BASE_URL` 是默认图片域名；`PUBLIC_BASE_ALIASES` 是可选的逗号分隔额外域名列表。
生产别名必须为 HTTPS origin，不含路径、查询参数、片段或凭据，也不能使用网站域名。
冲突检查按 hostname 执行，忽略端口、大小写和 DNS 尾点；别名不能与网站、主图片域名
或另一别名使用相同 hostname。留空保持单域名行为。

- 两域名访问相同 `/i/...` 路径、相同私有桶对象，不复制图片，已有图片同样支持。
- API 上传、列表和详情增加可选 `alternate_urls` 数组；现有 `url`、Markdown 和 HTML
  继续使用主域名，不破坏既有客户端。
- 普通图库、管理员图库和刚上传的图片卡片提供“复制备用域名链接”。
- 别名 Host 仅允许 GET/HEAD `/i/*`；登录、管理、API 和写请求返回 404，且不续期 Session。
  隔离规则同样覆盖显式端口、大小写变化和 DNS 尾点的 Host 写法。
- URL 由配置生成，不采信请求的 Host 或 X-Forwarded-Host。
- 删除图片时，主域名及全部配置别名的 URL 在同一事务中写入 CDN 刷新队列。
  任一 URL 入队失败会回滚软删除及配额变更。后台按现有机制重试刷新。

Compose 模板均传入 `PUBLIC_BASE_ALIASES`。已有服务器若保留了手工 Compose 文件，
需要补上 `PUBLIC_BASE_ALIASES: ${PUBLIC_BASE_ALIASES:-}` 才会读取环境文件的新变量。
这些配置只准备应用行为，不自动建立 DNS、证书或 Cloudflare 自定义主机名。

## Cloudflare for SaaS 接入准备

以下是部署时的核对步骤；仅修改应用代码并不会让第二个公网域名立即可用。

1. 在提供服务的 Cloudflare zone 启用 Cloudflare for SaaS，设置指向源站的橙云 fallback origin，
   确认其 Active；按需建立供客户 CNAME 指向的 SaaS target。
2. 添加第二域名为 Custom Hostname，完成域名所有权与边缘证书验证。
   `status` 和 `ssl.status` 均需为 `active`，DNS 应指向 SaaS target。
3. 明确回源 Host/SNI：默认 fallback 路由发送原自定义主机名作为 Host 和 SNI；
   不能假定 CNAME 会把它们改成现有图片域名。Caddy 必须匹配第二域名，源站证书必须覆盖
   实际 SNI。边缘签发的自定义主机名证书不会自动安装到源站，原域名的通配证书通常不覆盖另一域。
4. 可按 `deploy/Caddyfile.image-alias` 新增图片站点，使用已准备好的有效证书和私钥。
   该模板依赖 `deploy/Caddyfile.cloudflare-host` 的 snippets；若生产站点为拆分文件，
   先合并等价 snippets 或显式展开，避免重复站点。保留来源 IP 校验和图片头清理。
5. 若改用 custom origin、Host override 或 SNI rewrite，先检查账户功能支持和官方路由规则；
   不假定所有套餐可改写 SNI，也不降低 Full (strict) 来绕过证书问题。
6. 扩展图片缓存规则、Cookie/Authorization 清理和 `X-Origin-Secret` 覆盖注入规则，
   使它们也匹配第二域名。网站 bypass 保持按网站域名限定。源站不自行注入共享密钥。
7. 应用仍使用一套 `CDN_ZONE_ID` / `CDN_PURGE_TOKEN`。本功能针对同一 SaaS 提供方 zone
   管理的主域名和 Custom Hostname；如果是独立 CDN zone 或存在 O2O 额外缓存层，
   必须验证该 zone 的按 URL 刷新覆盖两域名，必要时使用可路由多个 zone 的 generic purge 服务。

删除域名配置前，先清理该域名的已有缓存；不在配置中的旧别名不会再为后续删除生成刷新任务。

## 上线验收

- 两域名 GET 返回相同字节，HEAD/Range/ETag 正常，均无 Set-Cookie。
- 第二域名 `/login`、`/admin`、`/api/v1/me` 及 POST `/i/*` 均拒绝。
- 两域名分别验证 MISS/HIT；不能用一个域名的 HIT 推断另一个域名。
- 使用专门测试图片验证删除后两个 URL 均最终 404，且刷新队列无失败遗留。
- 验证原有网站登录/上传不受影响。

本地测试覆盖 URL 生成、别名路由隔离和多域名刷新事务；真实 Cloudflare SaaS、证书和
双域名缓存/刷新需要部署时验收。

## 官方参考

- [Cloudflare for SaaS 初始设置](https://developers.cloudflare.com/cloudflare-for-platforms/cloudflare-for-saas/start/getting-started/)
- [回源 Host 与 SNI](https://developers.cloudflare.com/cloudflare-for-platforms/cloudflare-for-saas/reference/connection-details/)
- [Custom origin 与 SNI rewrites](https://developers.cloudflare.com/cloudflare-for-platforms/cloudflare-for-saas/start/advanced-settings/custom-origin/)
- [Cache for SaaS](https://developers.cloudflare.com/cloudflare-for-platforms/cloudflare-for-saas/performance/cache-for-saas/)
