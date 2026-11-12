# FujiPic 文档

## 部署和使用

| 文档 | 用途 |
| --- | --- |
| [Cloudflare 部署入口](DEPLOY-CLOUDFLARE.md) | 选择当前部署方式 |
| [宿主机 Caddy + Docker](DEPLOY-CLOUDFLARE-HOST.md) | 新部署、旧架构迁移、备份、升级和回退 |
| [CDN](CDN.md) | 缓存规则、源站保护、刷新和验收 |
| [后台管理](ADMIN.md) | 创建用户、管理全部图片、桶容量和调度 |
| [Turnstile 人机验证](TURNSTILE.md) | 登录/注册的可选托管验证、私有环境配置和验收 |
| [中国大陆优化图片域名](IMAGE-DOMAINS.md) | 双域名链接/Markdown、Cloudflare 自定义主机名与多域名刷新 |
| [OCI 排查](OCI-TROUBLESHOOTING.md) | 参数检查、独立只读签名测试和已验证结论 |
| [OpenAPI](openapi.yaml) | JSON API 与图片回源接口；管理后台使用独立 HTML 表单 |

本地开发、二进制部署和环境变量见仓库根目录 [README](../README.md)。
[PLAN](../PLAN.md) 保留设计与验收背景；实际运行方式以当前代码及上述运维文档为准。

## 历史记录

- [容器 Caddy 8443 部署](archive/DEPLOY-CLOUDFLARE-8443.md)：仅供旧部署维护与回退。
- [2026-11-10 代码审查](archive/AUDIT-2026-11-10.md)
- [2026-11-11 对抗性审查与修复](archive/ADVERSARIAL-REVIEW-2026-11-11.md)
- [2026-11-11 管理功能、图片别名与全库复查](archive/REVIEW-ADMIN-ALIASES-2026-11-11.md)

审查记录是对应提交的历史快照，不是对当前版本或所有云环境的安全保证。
