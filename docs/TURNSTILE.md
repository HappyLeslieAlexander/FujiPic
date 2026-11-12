# 登录与注册的人机验证

FujiPic 可选集成 Cloudflare Turnstile **Managed（托管）模式**，同时保护：

| 表单 | 服务端 action |
| --- | --- |
| `POST /login` | `login` |
| `POST /register` | `signup` |

## 配置

在 Cloudflare 控制台的 Turnstile 中创建组件，选择 Managed 模式，添加自己的
**网站域名**（不包含协议、端口或路径），取得 Site Key 和 Secret Key。
无需添加图片域名。一般不需要启用 pre-clearance。

将下面三个变量写入部署使用的私有环境文件，例如 `/etc/fujipic/fujipic.env`，
文件权限设为 `0600`；不要提交实际凭据：

```dotenv
TURNSTILE_SITE_KEY=
TURNSTILE_SECRET=
TURNSTILE_HOSTNAMES=
```

- `TURNSTILE_SITE_KEY`：组件公开的 Site Key，会出现在登录和注册页面。
- `TURNSTILE_SECRET`：组件私钥，只在 Go 后端向 Siteverify 发出请求时使用。
- `TURNSTILE_HOSTNAMES`：精确的前端域名白名单，例如 `pic.example.com`；多个域名用逗号分隔。
  不支持通配符、URL 或端口。生产环境禁止 localhost、回环地址及官方测试密钥。

三项全部为空时保持原来的登录与注册流程。只填写部分配置会拒绝启动，防止配置遗漏时
误以为保护已经启用。仓库中的三套 Compose 文件均支持这三个变量；修改环境文件后，
需要重新创建应用容器，单纯重启容器不会更新环境变量。

**应用不需要 Cloudflare 账户 ID 或管理 API Token。** 它们只用于配置组件，
不要作为应用配置加入 Docker，也不要提交到 GitHub。撤销管理 API Token 不影响组件运行。

## 验证行为

浏览器从 `challenges.cloudflare.com` 加载组件，将 `cf-turnstile-response` 随原生表单提交。
后端在密码校验/创建账号前调用 Cloudflare Siteverify，并要求：

1. `success` 为 `true`；
2. `action` 与当前表单匹配；
3. `hostname` 精确匹配配置的白名单，不根据请求的 Host 或转发头决定白名单。

缺失、重复字段、超长或无效令牌、域名/action 不匹配、验证服务网络错误、超时和无效响应
都会拒绝请求。网络验证最多等待 10 秒，不会因为 Cloudflare 不可用而跳过验证。
原有 CSRF 校验、登录 IP/邮箱限流和注册 IP 限流继续生效。

启用后需要浏览器允许 JavaScript 和 Cloudflare 验证组件。CSP 仅在登录与注册页面开放
Cloudflare 的脚本、iframe 和连接来源，不添加 `unsafe-inline` 或 `unsafe-eval`。
原生表单每次提交后重新渲染页面，错误重试会生成新组件和新令牌。

Turnstile 令牌有效期为五分钟，按单次使用设计。Cloudflare 的重放检测存在传播延迟，
不要将其视为业务操作的严格幂等锁。FujiPic 不缓存成功的验证结果，每次提交都调用 Siteverify。
不会在日志中记录验证令牌、Secret Key 或上游响应正文。

## 验收与排查

- 先在测试部署验证组件加载，以及无令牌/伪造令牌提交被拒绝。
- 使用真实组件取得新令牌，完成一次正常登录和注册，再验证重放被拒绝。
- 密码错误后重新验证再提交，确认不会复用已经消费的令牌。
- 确认线上前端域名同时在组件域名列表及 `TURNSTILE_HOSTNAMES` 中。
- 后端需要能通过 HTTPS 访问 `challenges.cloudflare.com`。
- 验证失败不应创建账号或登录会话；已登录会话、API Token 和图片直链不要求重复验证。

本地真实组件测试可以添加 `localhost`、`127.0.0.1` 到组件域名列表，并仅在
`APP_ENV=development` 的部署白名单中使用它们。生产白名单只放正式网站域名。
自动化回归使用模拟 Siteverify，不能替代真实浏览器令牌验收。

参考：[Cloudflare 服务端校验文档](https://developers.cloudflare.com/turnstile/get-started/server-side-validation/)。
