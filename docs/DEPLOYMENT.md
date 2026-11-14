# 从零部署 FujiPic：Docker、Caddy 与 Cloudflare

这份教程面向在自己的服务器和域名上部署 FujiPic 的维护者。完成后，网站、上传 API 和图片均通过 Cloudflare 访问；FujiPic 与 Caddy 各运行在一个 Docker 容器中，图片存放在私有 S3 兼容桶，SQLite 保存在 Docker 数据卷。

命令以 **Ubuntu 24.04/26.04 LTS、Bash、root shell** 为例。AMD64 与 ARM64 均可从源码构建。建议至少 2 GB 内存并保留构建镜像与备份的磁盘空间；模板为应用设置 2 GB、Caddy 设置 256 MB 内存上限，按实际负载调整。

## 目录

1. [理解部署结构](#step-1)
2. [准备服务器与 Docker](#step-2)
3. [准备私有存储桶](#step-3)
4. [获取代码与私有配置](#step-4)
5. [配置 Cloudflare DNS 与证书](#step-5)
6. [配置源站认证与缓存](#step-6)
7. [构建和启动容器](#step-7)
8. [首次登录与后台管理](#step-8)
9. [可选：Turnstile](#step-9)
10. [验收部署](#step-10)
11. [API 使用](#step-11)
12. [备份与恢复](#step-12)
13. [升级与回退](#step-13)
14. [扩展图片域名](#step-14)
15. [故障排查与维护](#step-15)

<a id="step-1"></a>
## 1. 理解部署结构

准备一个接入 Cloudflare DNS 的域名和两个子域名。全文使用保留示例域名，操作时替换成自己的：

| 示例 | 用途 |
| --- | --- |
| `pic.example.com` | 网站、登录、后台、上传、JSON API |
| `img.example.com` | 公开图片，只接受 GET/HEAD `/i/*` |
| `203.0.113.10` | 示例源站 IPv4，替换为自己的服务器地址 |

```text
浏览器 / API 客户端
        │ HTTPS :443
        ▼
Cloudflare（DNS 橙云、边缘证书、图片/静态文件缓存）
        │ HTTPS，Full (strict)，目标服务器 TCP 443
        ▼
Docker 端口映射 443:8443 → Caddy 容器 :8443
        │ Origin CA 证书 + Cloudflare TCP 来源 IP 校验
        │ HTTP，Docker 网络
        ▼
FujiPic 容器 :8080（不发布宿主机端口）
        ├── /data → Docker 数据卷：SQLite、临时文件
        └── HTTPS → 私有 S3 兼容桶：图片原文件
```

**Cloudflare 的回源端口是 443。** 8443 只在容器内部使用，让非 root 的 Caddy 无需低端口权限。访客 URL 不带端口，Cloudflare 不需要端口覆盖 Origin Rule。若曾配置回源 8443，请删除或禁用与这两个域名匹配的旧端口覆盖规则。

上传必须经过应用校验；Cloudflare 只回源 Caddy/FujiPic，不直接访问桶。图片链接公开且长期可缓存，私有桶并不意味着拿到图片 URL 后还要登录。

本教程使用独立文件 `deploy/compose.cloudflare.yml`。仓库中的 `compose.cloudflare-host.yml` 是复用宿主机 Caddy 的另一种拓扑，`compose.prod.yml` 是 HTTP 源站模板；不要叠加使用。已有部署应先核对端口、卷名、网络、证书和备份再迁移。旧文档和完整修改前历史保存在分支 `backup20261112`。

<a id="step-2"></a>
## 2. 准备服务器与 Docker

### 2.1 登录与网络

以有 sudo 权限的用户 SSH 登录服务器，进入 root Bash：

```bash
sudo -i
```

后文服务器命令均在这个 shell 执行。确认 TCP 443 没有被其他服务占用：

```bash
ss -ltnp 'sport = :443'
```

如果已有网站占用 443，先规划反向代理复用；直接启动本模板会端口冲突。云安全组/OCI NSG 和主机防火墙至少要允许：

- 自己的管理 IP → SSH 端口。
- Cloudflare [官方 IPv4/IPv6 网段](https://www.cloudflare.com/ips/) → TCP 443。
- 服务器向外访问 DNS、HTTPS（拉取镜像、下载依赖、S3、Cloudflare API、可选 Turnstile）。

模板不发布 80、8080、8443，也不需要开放 UDP 443。HTTP 到 HTTPS 的跳转由 Cloudflare 边缘完成。Docker 发布端口可能绕过 UFW 常规规则，应在云安全组或正确的 Docker 防火墙链配置访问限制；Caddy 另有实际 TCP 对端的 Cloudflare IP 校验。

### 2.2 安装 Docker Engine 和 Compose 插件

全新 Ubuntu 可执行以下官方 APT 仓库方式。若系统已有 Docker 或冲突软件包，先按 [Docker Ubuntu 安装说明](https://docs.docker.com/engine/install/ubuntu/)核对；不要直接移除现有业务依赖。

```bash
apt-get update
apt-get install -y ca-certificates curl git openssl python3 sqlite3 nano
install -m 0755 -d /etc/apt/keyrings
curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc
chmod a+r /etc/apt/keyrings/docker.asc
cat > /etc/apt/sources.list.d/docker.sources <<EOF
Types: deb
URIs: https://download.docker.com/linux/ubuntu
Suites: $(. /etc/os-release && echo "${UBUNTU_CODENAME:-$VERSION_CODENAME}")
Components: stable
Architectures: $(dpkg --print-architecture)
Signed-By: /etc/apt/keyrings/docker.asc
EOF
apt-get update
apt-get install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
systemctl enable --now docker
docker version
docker compose version
docker run --rm hello-world
```

使用 `docker compose`（带空格的插件命令）。服务器无需安装 Go、Node.js 或 Caddy：Go 编译在构建容器中完成。

<a id="step-3"></a>
## 3. 准备私有存储桶

先创建一个默认桶，再为应用创建仅能访问该桶的访问密钥。保留桶名、区域、Endpoint、Access Key ID 和 Secret Access Key。桶须关闭公开读取和公开写入，不启用静态网站托管；浏览器不会直连桶，无需为上传配置桶 CORS。

### AWS S3

- 开启 Block Public Access，选择实际区域，例如 `ap-northeast-1`。
- 默认 AWS endpoint 可留空，`S3_FORCE_PATH_STYLE=false`。
- 对目标桶内对象授予 `s3:GetObject`、`s3:PutObject`、`s3:DeleteObject`；若运维工具需要列举桶内容，再授予目标桶的 `s3:ListBucket`。使用 SSE-KMS 时另按密钥策略配置必要 KMS 权限。
- 对象级策略资源是 `arn:aws:s3:::YOUR_BUCKET/*`，桶级资源是 `arn:aws:s3:::YOUR_BUCKET`，不要混用。

### OCI Object Storage

- 创建私有桶，记录 **Object Storage namespace**、区域和桶名。
- 为具有目标桶对象读/写/删除权限的 IAM 用户创建 **Customer Secret Key**。
- 使用 S3 Compatibility API endpoint，例如：
  `https://YOUR_NAMESPACE.compat.objectstorage.ap-tokyo-1.oraclecloud.com`。
- 区域填写 `ap-tokyo-1`，`S3_FORCE_PATH_STYLE=true`。按 OCI 控制台显示的当前区域/realm endpoint 填写，域名中不附加桶名或对象路径。
- OCI 控制台密码、Auth Token、OCI API signing key 不能替代 Customer Secret Key。

其他 S3 兼容服务按厂商提供的 endpoint、签名区域和 path-style 要求配置。应用会执行 Put/Get/Delete；新增后台桶时还会做短生命周期对象探针，因此只读凭据不够。

图片恢复依赖桶对象备份/版本管理，SQLite 备份不包含对象字节。按自己的保留需求配置对象版本、复制或独立备份；不要设置会自动删除仍在使用图片的生命周期规则。

<a id="step-4"></a>
## 4. 获取代码与私有配置

### 4.1 拉取代码

```bash
install -d -m 0755 /opt/fujipic
git clone https://github.com/HappyLeslieAlexander/FujiPic.git /opt/fujipic/src
cd /opt/fujipic/src
git switch main
git rev-parse HEAD
install -d -m 0700 /etc/fujipic
install -m 0600 deploy/cloudflare.env.example /etc/fujipic/fujipic.env
nano /etc/fujipic/fujipic.env
```

只在首次部署复制模板，后续升级保留自己的配置文件。私有配置位于仓库外，避免随源码或构建上下文上传。

### 4.2 填写配置

| 配置项 | 填写方式 |
| --- | --- |
| `APP_HOST` / `IMAGE_HOST` | `pic.example.com` / `img.example.com`，不带协议、端口、路径 |
| `ORIGIN_CERT_DIR` | 保持 `/etc/fujipic/origin`，下一步创建 |
| `S3_ENDPOINT` | OCI/S3 兼容服务填完整 HTTPS endpoint；AWS 默认可留空 |
| `S3_REGION` / `S3_BUCKET` | 实际区域和私有桶名 |
| `S3_ACCESS_KEY_ID` / `S3_SECRET_ACCESS_KEY` | 桶访问凭据 |
| `S3_FORCE_PATH_STYLE` | OCI `true`，AWS `false` |
| `STORAGE_ENCRYPTION_KEY` | 单独生成的 32 字节 Base64 密钥，用于加密后台自定义桶凭据 |
| `ORIGIN_SHARED_SECRET` | 单独生成的随机共享密钥，稍后也填入 Cloudflare 规则 |
| `BOOTSTRAP_ADMIN_EMAIL` / `BOOTSTRAP_ADMIN_PASSWORD` | 初始管理员邮箱与 10–200 字符密码 |
| `CDN_ZONE_ID` / `CDN_PURGE_TOKEN` | 下一节创建；用于删除图片后刷新 Cloudflare 缓存 |
| `CONTACT_EMAIL` | 可选公开联系邮箱 |
| `SESSION_TTL` | 模板为 `24h`，例如也可用 `168h` |
| `PUBLIC_BASE_ALIASES` | 首次部署留空；见扩展图片域名章节 |
| 三个 `TURNSTILE_*` | 首次可全部留空；见 Turnstile 章节 |

分别运行，分别保存结果，三个值不要相同：

```bash
openssl rand -base64 32
openssl rand -hex 32
openssl rand -hex 24
```

依次用于存储加密密钥、源站共享密钥、初始管理员密码。`STORAGE_ENCRYPTION_KEY` 必须随数据库一起备份，丢失或随意替换后无法解密已保存的自定义桶凭据。

这是 **Compose env 文件**。值中有 `$`、`#` 等特殊字符时使用单引号包围，例如 `S3_SECRET_ACCESS_KEY='实际值'`，避免 Compose 插值；不要把它当 shell 脚本 `source`。

默认单文件上限 10 MiB、5000 万像素、边长 12000；每用户默认配额 1 GiB。`MAX_UPLOAD_BYTES` 使用字节。提高限制时需同时调整 `deploy/Caddyfile.cloudflare` 的 `max_size 12MB`（为 multipart 留余量），并确认 Cloudflare 套餐请求体上限、容器内存和临时目录空间。模板固定关闭开放注册，用户通过邀请码或管理员创建。

<a id="step-5"></a>
## 5. 配置 Cloudflare DNS 与证书

以下操作在自己的 Cloudflare 控制台完成，菜单名称可能随界面更新略有变化。

### 5.1 DNS 与 TLS

1. 确认域名已添加到 Cloudflare，注册商 nameserver 已切换且区域为 Active。
2. DNS 添加 `pic` 与 `img` 两条 A 记录，均指向自己的服务器公网 IPv4，**代理状态均为橙云 Proxied**。
3. 只有服务器 IPv6、防火墙和路由都可用时才添加 AAAA；移除指向旧服务器的记录。
4. SSL/TLS → Overview：选择 **Full (strict)**，不要选 Flexible。
5. SSL/TLS → Edge Certificates：确认边缘证书覆盖两个子域名并已激活；启用 **Always Use HTTPS**。

### 5.2 创建 Origin CA 证书

SSL/TLS → Origin Server → Create Certificate：

1. 让 Cloudflare 生成私钥和 CSR，可选 RSA。
2. Hostnames 包含 `pic.example.com`、`img.example.com`，或覆盖它们的 `*.example.com`。
3. 选择有效期，并记录更新日期。
4. 保存 Origin Certificate 与 Private Key 为 PEM。私钥通常只展示一次。

在服务器创建文件并粘贴各自的 PEM 内容：

```bash
install -d -m 0750 -o 65532 -g 65532 /etc/fujipic/origin
install -m 0600 -o 65532 -g 65532 /dev/null /etc/fujipic/origin/cert.pem
install -m 0600 -o 65532 -g 65532 /dev/null /etc/fujipic/origin/key.pem
nano /etc/fujipic/origin/cert.pem
nano /etc/fujipic/origin/key.pem
chown 65532:65532 /etc/fujipic/origin/*.pem
chmod 0600 /etc/fujipic/origin/*.pem
openssl x509 -in /etc/fujipic/origin/cert.pem -noout -subject -issuer -dates -ext subjectAltName
```

Caddy 以 UID/GID 65532 运行，必须能读取这两个文件。Origin CA 用于 Cloudflare → 源站链路，普通浏览器通常不信任它，所以不要通过关闭橙云来测试。它也不同于 Authenticated Origin Pulls 客户端证书；本教程使用来源 IP + 共享密钥认证，不要求启用 AOP。

### 5.3 创建缓存刷新 API Token

Cloudflare 个人资料 → API Tokens → Create Token → Custom Token：

- 权限：**Zone → Cache Purge → Purge**。
- Zone Resources：只包含图片域名所在的 zone。
- 保存 Token 到 `CDN_PURGE_TOKEN`；zone 概览页中的 **Zone ID** 填入 `CDN_ZONE_ID`，不是 Account ID。

这个 Token 用于运行时删除图片后刷新缓存，应持续有效。它与 Turnstile 管理 Token、Turnstile Secret Key 是三种不同凭据。

<a id="step-6"></a>
## 6. 配置 Cloudflare 源站认证与缓存

### 6.1 覆盖源站认证请求头

进入 Rules → Transform Rules → Modify Request Header（或 Rules 概览中创建相应规则）。

规则表达式：

```text
(http.host in {"pic.example.com" "img.example.com"})
```

操作：**Set static / 覆盖设置** `X-Origin-Secret`，值为服务器配置里的 `ORIGIN_SHARED_SECRET`。不是 Add，也不是响应头规则。

规则必须覆盖这两个域名的**所有路径**，包含 `/login`、`/static/*`、`/api/*`、`/i/*`。不要只在图片规则中添加。不要在 Caddy 中自行注入共享密钥，否则会削弱“请求确实经过你的 Cloudflare 配置”的校验。

### 6.2 图片请求移除认证信息

再建一条 Request Header Transform Rule：

```text
(http.host eq "img.example.com" and starts_with(http.request.uri.path, "/i/") and http.request.method in {"GET" "HEAD"})
```

操作：Remove `Cookie`；Remove `Authorization`。Caddy 也会移除这两个头，但在边缘移除有助于避免 Cookie/Bearer 请求影响缓存判断。只对图片读取执行，不要删除网站/API 的认证头。

### 6.3 图片缓存规则

Rules → Cache Rules → Create rule，匹配上面的图片 GET/HEAD 表达式：

- Cache eligibility：**Eligible for cache**。
- Edge TTL：选择**使用源站 Cache-Control；没有该头时绕过缓存**的选项。
- Browser TTL：Respect existing headers / 尊重源站头。
- Cache key：默认，保留 hostname、路径、查询参数；不要把不同域名/路径合并。
- 不覆盖源站的 `no-store`，不要强制给所有状态码设置一年 TTL。

成功图片由应用返回一年 `public, max-age=31536000, immutable`；错误响应不应缓存。删除后应用排队刷新边缘缓存，刷新存在延迟，已下载文件和浏览器本地缓存无法撤回。

### 6.4 网站静态文件与账号页面

网站保持 Cloudflare 默认缓存行为，或只为 `pic.example.com/static/*` 的 GET/HEAD 添加“Eligible + 尊重源站头”规则。应用静态 CSS/JS 返回 7 天可缓存头，模板引用带内容哈希的 `?v=...` URL。

**保留查询字符串作为缓存键的一部分**，不要开启 Ignore Query String。升级后 `?v=` 改变才能获取新内容。账号页面、上传、JSON API 使用 `Cache-Control: no-store`，不要配置整站 Cache Everything + 强制 Edge TTL。

不需要整站 Bypass；那会让 JS/CSS 一起失去缓存。如已有旧 Page Rules、Cache Rules、Origin Rules，检查所有匹配项及优先级，移除冲突的强制缓存、忽略查询字符串或 8443 端口覆盖。

<a id="step-7"></a>
## 7. 构建和启动容器

### 7.1 定义 Compose 命令

在服务器 root Bash 中定义函数，后文统一用 `dc`：

```bash
cd /opt/fujipic/src
dc() {
  docker compose --env-file /etc/fujipic/fujipic.env \
    -f /opt/fujipic/src/deploy/compose.cloudflare.yml "$@"
}
dc config --quiet
```

**每次重新 SSH 登录后重新定义函数**。`config --quiet` 检查 Compose 配置与必填插值；它不验证 S3 权限或 Cloudflare 规则。不要把不带 `--quiet` 的展开配置公开，它含密码和 Token。

确认 `172.30.84.0/29` 不与现有 Docker/VPN 网络冲突：

```bash
docker network ls
ip route
```

若冲突，要一致修改 Compose 子网、Caddy `.2`、应用 `.3`、`TRUSTED_PROXY_CIDRS`。应用只信任 Caddy 的 `/32` 地址，不应放宽为全部私网。

### 7.2 校验证书配置并启动

```bash
dc pull caddy
dc run --rm --no-deps caddy caddy validate --config /etc/caddy/Caddyfile --adapter caddyfile
dc build --pull app
dc up -d
dc ps
dc logs --tail=100 app caddy
```

预期两个容器处于 running；Caddy 发布 `443->8443/tcp`；app 没有宿主机端口。首次启动自动创建 SQLite 表并执行迁移，创建配置里的初始管理员。

数据卷默认为 `fujipic-cloudflare_fujipic-data`，来自固定项目名 `fujipic-cloudflare`。不要随意改变项目名或使用 `down -v`，否则可能切换到新卷或删除数据。默认 Docker named volume 会继承镜像 `/data` 的非 root 所有权；自定义 bind mount 时需自行设置 UID/GID 65532 可写。

模板 Caddy 的管理 API 被关闭，更新 Caddyfile/证书后使用 `dc restart caddy`；更新应用环境配置要用 `dc up -d --no-deps app` 重建容器，仅 restart 不会应用新环境变量。

<a id="step-8"></a>
## 8. 首次登录与后台管理

网站自动读取浏览器发送的 `Accept-Language`，按最高优先级（包括 `q` 权重）显示英语、简体中文或正体中文。其他语言（例如法语、德语、日语）和未提供语言时默认英语；可在浏览器语言偏好中调整首选语言后刷新。显式 `zh-Hans` / `zh-Hant` 优先于地区，`zh-TW` / `zh-HK` / `zh-MO` 使用正体，`zh-CN` / `zh-SG` / `zh` 使用简体。

无需新增环境变量。反向代理需保留 `Accept-Language`；页面和 API 错误响应带 `Content-Language` 与 `Vary: Accept-Language`，仍为 `no-store`。图片和静态 JS/CSS 不因语言改变缓存键或内容。

1. 浏览器打开 `https://pic.example.com/login`，使用初始管理员邮箱/密码登录。
2. 进入“设置”修改密码，妥善保存新密码。
3. 在 `/etc/fujipic/fujipic.env` 中同时清空 `BOOTSTRAP_ADMIN_EMAIL` 与 `BOOTSTRAP_ADMIN_PASSWORD`，然后执行 `dc up -d --no-deps app`。已创建的管理员保存在数据库，不会因清空变量被删除。
4. 进入 `/admin`，确认默认桶、用户配额，上传一张可丢弃图片检查。

### 日常管理

- **用户**：直接创建普通用户并设置初始密码，或创建邀请码供用户注册；可调整配额、禁用/启用账号。初始密码需通过合适渠道交给用户，当前没有强制首次登录改密流程。
- **邀请码**：默认一次，可设置次数/无限次及有效期；删除后不能再使用，已经注册的账号不受影响。
- **图片**：管理员图库 `/admin/images` 支持用户与存储桶多选筛选。同一类内匹配任一选中项，两类之间取交集；不勾选或勾选全部等同于不限。默认显示全部用户、全部桶，此时可逐张勾选并批量删除；应用有效筛选后，还可“全选并删除筛选结果”，范围包含其他分页。页面显示匹配数、已选数，提交时确认删除数量；修改筛选条件后须先应用。若确认前匹配图片发生变化，会要求核对最新结果，不删除任何图片。普通 API 即使使用管理员 Token 也只操作 Token 所属用户的图片。
- **批量删除**：配额释放、图片删除标记、审计记录和清理任务在同一事务内完成；对象删除和 CDN 缓存清理由后台任务异步处理。事务失败不会留下部分删除；成功提示表示任务已入队，不表示云端清理已全部结束。
- **存储桶池**：全局共享，由管理员添加，用户不能指定桶。新上传按优先级选择启用且容量足够的桶；已有图片仍从原桶读取。禁用桶只停止新分配，不移动图片。
- **容量**：配置字节值时区分 1 GB = 1,000,000,000 B 与 1 GiB = 1,073,741,824 B，按表单单位填写。桶的真实云账单/容量应另外监控。
- **凭据**：默认桶 `bkt_default` 使用环境变量；后台自定义桶凭据加密保存在 SQLite。删除桶有引用保护，不要通过改数据库绕过。
- **会话**：设置页可查看自己的有效登录会话、注销其他浏览器；最近活动时间最多约每小时更新。注销使对应浏览器登录状态失效；API Token 独立管理，需单独撤销。
- **审计**：管理操作保留审计日志；清空前按需要导出/备份。

### 管理员密码遗失

CLI 密码来自环境变量，不会自动弹出密码提示。下面通过 Bash 隐藏输入并把环境传给容器：

```bash
read -rsp '新管理员密码: ' FUJIPIC_ADMIN_PASSWORD
echo
export FUJIPIC_ADMIN_PASSWORD
dc exec -T -e FUJIPIC_ADMIN_PASSWORD app /usr/local/bin/fujipic admin reset-password --email admin@example.com
unset FUJIPIC_ADMIN_PASSWORD
```

邮箱替换为真实管理员。创建另一个管理员使用 `admin create --email ... --quota-mb 1024`，同样传入密码环境变量。无需进入应用容器 shell：运行镜像是 distroless，没有 `sh` 或 `curl`。

<a id="step-9"></a>
## 9. 可选：启用 Turnstile

完成基本上传验收后，可为登录和注册启用 Cloudflare Turnstile：

1. Cloudflare 账户级 Turnstile 页面 → Add widget。
2. 模式选 **Managed**，Hostname 填网站域名 `pic.example.com`（不带协议/路径）。图片域名不需要配置。
3. 保存组件的 Site Key 与 Secret Key 到自己的私有配置：

```dotenv
TURNSTILE_SITE_KEY=自己的组件SiteKey
TURNSTILE_SECRET=自己的组件SecretKey
TURNSTILE_HOSTNAMES=pic.example.com
```

4. 执行 `dc up -d --no-deps app`。
5. 保留当前登录，用无痕窗口打开登录/注册页，完成验证后实际登录。仅看到组件不足以证明服务端 Siteverify 成功。

三项要一起配置；全部留空表示禁用。多个合法网站 hostname 用逗号分隔。服务器必须能访问 `https://challenges.cloudflare.com/turnstile/v0/siteverify`，客户端也需能加载组件脚本。

Site Key 可公开展示，Secret Key 仅供服务端 Siteverify。通过控制台创建组件无需在服务器保存 Cloudflare Account ID 或 Turnstile 管理 API Token；如用管理 Token 创建完组件，可以撤销那个管理 Token，但不能删除正在使用的组件/Secret Key。缓存刷新 `CDN_PURGE_TOKEN` 仍需保留。

Turnstile 保护网页登录/注册，不代替 API Token 认证；Cloudflare 整页 WAF/机器人质询是独立功能，可能在请求到达应用前拦截自动化客户端。

<a id="step-10"></a>
## 10. 验收部署

### 10.1 公网与缓存

在能访问网站的终端执行（域名均替换）：

```bash
curl -sS -D - -o /dev/null https://pic.example.com/healthz
curl -sS -D - -o /dev/null https://pic.example.com/readyz
curl -sS -D - -o /dev/null https://pic.example.com/login
curl -sS -D - -o /dev/null https://pic.example.com/static/app.css
curl -sS -D - -o /dev/null https://pic.example.com/static/app.css
```

预期健康/就绪为 200；登录页 `Cache-Control: no-store`；静态资源可缓存，重复访问同一边缘节点通常由 MISS 变成 HIT。也检查浏览器页面实际引用的带 `?v=` URL。`/readyz` 不是完整 S3 写入验收，仍需上传测试。

在设置页创建短期测试 API Token，勾选 `upload,read,delete`，用新图片运行仓库验收脚本：

```bash
read -rsp '测试 API Token: ' TEST_TOKEN
echo
bash scripts/verify-cdn.sh https://pic.example.com https://img.example.com "$TEST_TOKEN" /path/to/disposable.png
unset TEST_TOKEN
```

脚本会真实上传、下载比对、检查 HEAD/Range/304、API no-store，并删除刚上传的图片。不要传入要保留的文件作为测试数据；若触发去重，脚本会中止并保留已有图片。最后在设置页撤销测试 Token。

**脚本输出的缓存状态和删除后状态仍需人工确认**：缓存 HIT 只展示，不是脚本退出码的硬性条件；删除后仍为 200 会给出 WARN。若有 WARN，查看刷新 Token、队列/日志，等待重试并再次请求直到返回 404。成功退出不等于所有缓存验收都已完成。

### 10.2 源站保护与图片隔离

- 直接通过源站 IP/`--resolve` 请求，应被云防火墙拒绝或由 Caddy 返回 403；Origin CA 的客户端不信任错误本身不能证明来源限制生效。
- 公网 `https://img.example.com/login` 应为 404，图片域名不能登录或上传。
- 成功图片响应不应含 `Set-Cookie`，应含图片 Content-Type、ETag、缓存头。
- 无痕窗口验证登录、注册邀请码、退出；启用 Turnstile 时还要真实完成人机验证。
- API 无 Token 请求应被应用拒绝；正常 API 不应返回 Cloudflare HTML 质询页。

若命令行返回 `cf-mitigated: challenge`，先在 Cloudflare 安全事件中定位规则；这是边缘质询，不是应用密码错误。为确有需要的 API 客户端/路径调整精确规则，保留应用认证。

<a id="step-11"></a>
## 11. API 使用

API 基址为网站 `https://pic.example.com`。推荐在“设置”创建最小 scope 的 API Token，通过 `Authorization: Bearer fp_...` 使用。Token 明文只在创建时展示。浏览器 Cookie 认证的写请求还需要 CSRF token。

### 接口表

以下表由自动化测试与服务器注册路由双向核对，`{id}` 是图片/Token ID，`{key}` 可包含 `/`。

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| POST | `/api/v1/upload` | `upload` scope；multipart 字段 `file` 或原始图片请求体 + `X-Filename` |
| GET | `/api/v1/images` | `read` scope；自己的图片，支持 `limit`、`cursor` 分页 |
| GET | `/api/v1/images/{id}` | `read` scope；自己的单张图片 |
| DELETE | `/api/v1/images/{id}` | `delete` scope；自己的图片，成功 204 |
| GET | `/api/v1/me` | `read` scope；当前用户与配额 |
| GET | `/api/v1/tokens` | Token 列表；只允许 Cookie 会话 |
| POST | `/api/v1/tokens` | 创建 Token；只允许 Cookie 会话 + CSRF |
| DELETE | `/api/v1/tokens/{id}` | 撤销 Token；只允许 Cookie 会话 + CSRF |
| GET | `/i/{key}` | 公开图片读取，支持 Range、条件请求 |
| HEAD | `/i/{key}` | 图片响应头 |
| GET | `/healthz` | 存活检查 |
| GET | `/readyz` | 就绪检查 |

示例（先把域名和文件路径替换）：

```bash
read -rsp 'API Token: ' TOKEN
echo
curl --fail-with-body https://pic.example.com/api/v1/me \
  -H "Authorization: Bearer $TOKEN"
curl --fail-with-body https://pic.example.com/api/v1/upload \
  -H "Authorization: Bearer $TOKEN" -F 'file=@/path/to/photo.png'
curl --fail-with-body 'https://pic.example.com/api/v1/images?limit=20' \
  -H "Authorization: Bearer $TOKEN"
unset TOKEN
```

新上传返回 201，同一用户相同字节去重返回 200 和 `deduplicated=true`；响应包含 `id`、`url` 等字段，配置图片别名时还可能有 `alternate_urls`。列表响应含 `items` 与 `next_cursor`，把后者原样 URL 编码后作为下一页 `cursor`。错误格式是 `{"error":{"code":"...","message":"..."}}`，按 HTTP 状态码和 `error.code` 处理，不能把 HTML 质询页当 JSON。

<a id="step-12"></a>
## 12. 备份与恢复

### 12.1 需要备份什么

完整恢复至少需要：SQLite、`/etc/fujipic`（环境/加密密钥/证书）、准确版本的源码/部署配置与镜像，以及**所有使用中桶的图片对象**。Cloudflare DNS/规则与桶策略也应单独记录。下面命令备份数据库和服务器配置，不复制云端图片。

在服务器 root Bash 重新定义好 `dc` 后执行。保留变量 `B`，升级步骤也会使用它：

```bash
set -e
umask 077
STAMP=$(date -u +%Y%m%dT%H%M%SZ)
B=/var/backups/fujipic/$STAMP
install -d -m 0700 "$B"
dc exec -T app /usr/local/bin/fujipic backup --out "/data/backup/$STAMP.db"
dc cp "app:/data/backup/$STAMP.db" "$B/fujipic.db"
chmod 0600 "$B/fujipic.db"
sqlite3 "$B/fujipic.db" 'PRAGMA integrity_check; PRAGMA foreign_key_check;'
cp -a /etc/fujipic "$B/config"
cp -a /opt/fujipic/src/deploy "$B/deploy"
git -C /opt/fujipic/src rev-parse HEAD > "$B/commit.txt"
git -C /opt/fujipic/src archive --format=tar.gz -o "$B/source.tar.gz" HEAD
IMAGE=$(docker inspect "$(dc ps -q app)" --format '{{.Image}}')
printf '%s\n' "$IMAGE" > "$B/image.txt"
docker image tag "$IMAGE" "fujipic:rollback-$STAMP"
docker image save -o "$B/app-image.tar" "fujipic:rollback-$STAMP"
sha256sum "$B/fujipic.db" "$B/source.tar.gz" "$B/app-image.tar" > "$B/SHA256SUMS"
```

数据库检查应只输出 `ok`，外键检查无记录。备份命令使用 SQLite 在线 `VACUUM INTO`，不会直接复制正在写入的数据库主文件。`git archive` 只含已提交源码；有本地源码修改时还需另外保存，并在 `git status` 中确认。

将备份加密传送到独立位置，并实际演练恢复。Docker 卷内 `/data/backup/` 仍有一份数据库副本，会逐渐占用磁盘；确认异地备份完整后可按保留策略清理选定旧文件。不要用 `docker volume prune` 代替备份管理。

### 12.2 恢复数据库

恢复会把账号、Token、配额、图片元数据回退到备份时间。恢复前先额外备份当前状态并停止写入；若旧元数据引用的对象已被删除，必须从对象备份恢复对应对象。仅恢复 SQLite 不能找回已删除的图片文件。

以下是在同一部署、同一数据卷上恢复已验证备份的示例。将 `B` 改为自己的绝对备份目录，确认配置/源码/镜像版本相匹配：

```bash
set -e
B=/var/backups/fujipic/替换为实际时间戳
sqlite3 "$B/fujipic.db" 'PRAGMA integrity_check; PRAGMA foreign_key_check;'
docker pull alpine:3.23
dc stop app
docker run --rm --user 0:0 \
  --mount type=volume,src=fujipic-cloudflare_fujipic-data,dst=/data \
  --mount "type=bind,src=$B,dst=/restore,readonly" \
  alpine:3.23 sh -ec '
    test -s /restore/fujipic.db
    rm -f /data/fujipic.db-wal /data/fujipic.db-shm
    cp /restore/fujipic.db /data/fujipic.db
    chown 65532:65532 /data/fujipic.db
    chmod 0600 /data/fujipic.db
  '
dc up -d --no-deps app
dc logs --tail=100 app
```

删除 WAL/SHM 仅在应用已停止、恢复备份时执行，绝不能对运行中的数据库这样操作。在新服务器恢复时先准备 Docker、源码、`/etc/fujipic` 和数据卷，再用相同方式填入数据库；确保 `/data/tmp` 存在且 UID 65532 可写，恢复桶访问、DNS 和防火墙后再开放访问。

<a id="step-13"></a>
## 13. 升级与回退

### 13.1 升级

先完成上一节备份，确认 `$B` 指向该次备份并保存其路径。检查本地修改，再获取新版本：

```bash
cd /opt/fujipic/src
git status --short
git fetch origin main
git log --oneline HEAD..origin/main
git diff HEAD..origin/main -- deploy migrations .env.example
```

阅读变更，合并自己的部署调整及新增环境变量后再继续。有本地修改导致合并失败时，先妥善保存/合并，不要 `reset --hard` 覆盖服务器配置。

```bash
git merge --ff-only origin/main
dc config --quiet
dc build --pull app
dc up -d --no-deps app
dc logs --tail=100 app
```

重新做健康检查、登录、上传、图片读取和缓存验收。镜像构建失败时旧容器仍运行；`up` 切换后才使用新镜像。迁移会在启动时自动执行，**旧二进制不一定兼容已迁移数据库**。

如更新 Caddy，另外执行 `dc pull caddy`、`dc run --rm --no-deps caddy caddy validate --config /etc/caddy/Caddyfile --adapter caddyfile`，再执行 `dc up -d --no-deps caddy`。若仅修改挂载的 Caddyfile/证书而镜像配置未变，用 `dc restart caddy` 使文件生效。

### 13.2 回退

1. 确认回退时间点及之后新增数据如何保留，停止应用写入。
2. 恢复备份记录的源码、部署配置、环境和证书；不要用旧模板覆盖自己的新秘密而没有备份。
3. 使用保留的旧镜像；若已被清理，`docker image load -i "$B/app-image.tar"`。
4. 只有确认数据库迁移兼容时才能只回退镜像。否则按上一节恢复升级前 SQLite，并同步处理对象备份差异。
5. 重新启动并验收。

同机仍保留旧 image ID 时，在恢复匹配配置/数据库后可以这样启动旧镜像：

```bash
docker image tag "$(cat "$B/image.txt")" fujipic:cloudflare
dc up -d --no-deps --no-build app
```

不要立即重建 app，否则相同标签会再次指向新源码。源码/配置回退可通过备份 `source.tar.gz` 与 `deploy/` 恢复到独立目录再核对，或检出 `commit.txt` 记录的提交；私有 `/etc/fujipic` 需要单独恢复。回退后再次检查容器实际 image ID、日志和图片内容，而不只是查看 Git 提交。

<a id="step-14"></a>
## 14. 扩展图片域名

首次部署只需一个图片域名。要添加第二个图片 origin，例如 `https://img2.example.com`：

1. 为它配置橙云 DNS、覆盖该 hostname 的源站/边缘证书。
2. 扩展共享密钥、图片认证头移除、图片缓存规则的 hostname 范围。
3. 在 `Caddyfile.cloudflare` 中复制主图片站点块，把站点地址改为 `https://img2.example.com:8443`，保留相同 IP gate、GET/HEAD `/i/*` 限制和代理设置。
4. 设置 `PUBLIC_BASE_ALIASES=https://img2.example.com`，多个 HTTPS origin 用逗号分隔；不能与网站/主图片 hostname 重复，不能带路径、查询或凭据。
5. 验证 Caddy 配置，重启 Caddy，重建 app，再对新旧域名验收。

别名读取相同对象，不复制图片；界面把它显示为“中国大陆优化”链接，但这个标签本身不提供网络优化，需要自行配置并实测网络服务。删除时所有配置域名均进入刷新队列。当前 Cloudflare 刷新适配器使用单个 `CDN_ZONE_ID`，最简单的是别名都处于同一 zone；跨 zone/Cloudflare for SaaS 接入还需专门设计证书、路由及各 zone 刷新，不能仅填写别名就认为完成。

<a id="step-15"></a>
## 15. 故障排查与维护

先看 `dc ps`、`dc logs --tail=100 app caddy` 和 Cloudflare 安全事件；分享日志前移除凭据、Cookie、Token 和个人信息。

| 现象 | 优先检查 |
| --- | --- |
| Caddy 提示端口被占用 | 宿主机已有服务占用 TCP 443；`ss -ltnp` |
| Caddy 读不到证书 | `ORIGIN_CERT_DIR` 是否绝对路径，文件名与 UID 65532 权限 |
| Cloudflare 521/522 | DNS 源站 IP、云安全组、防火墙、Caddy 是否运行，是否仍覆盖回源到 8443 |
| Cloudflare 525/526 | TLS 握手、Origin CA 有效期/hostname、证书与私钥是否匹配 |
| 返回 `Forbidden` | Caddy 实际 TCP 对端不是允许的 Cloudflare 网段；核对网络/NAT 和最新网段，不要仅放行伪造请求头 |
| 返回 `forbidden` | 应用共享密钥缺失/不匹配；检查覆盖全部路径的 Request Header Transform Rule |
| 登录/API 返回 HTML 或 `cf-mitigated: challenge` | Cloudflare 整页质询；查看安全事件，而非修改应用密码 |
| 图片一直 DYNAMIC/BYPASS | 回源端口 443、规则优先级、Cookie/Authorization 移除、源站缓存头、Set-Cookie、查询缓存键 |
| CSS/JS 更新后仍旧 | 页面是否带新 `?v=`，缓存键是否忽略查询字符串，浏览器缓存/旧页面 |
| 上传 413 | 应用 10 MiB、Caddy 12MB、Cloudflare 套餐上限是否一致 |
| 上传失败或新桶认证失败 | endpoint/区域/namespace、Customer Secret Key 类型、时间同步、桶读写删除权限；不是网站密码问题 |
| 桶解密失败 | 是否恢复了对应 `STORAGE_ENCRYPTION_KEY`；不要生成新密钥替换旧值 |
| 删除后仍能看到图片 | CDN 刷新权限/zone、worker 日志、浏览器本地缓存；删除是最终一致的 |
| 更新环境变量无效 | 是否执行 `up -d` 重建而不是仅 restart；手工 Compose 是否传入相应变量 |
| 重新部署后像是空站 | 项目名或数据卷是否改变；先查旧卷，不要继续创建/覆盖数据 |

定期检查磁盘、SQLite 备份可恢复性、桶容量、Cloudflare Token/Origin CA 到期、后台删除/刷新失败日志；模板只保留有限大小容器日志，不代替长期监控。更新 `deploy/cloudflare-ips.caddy` 时，对照官方 IPv4/IPv6 列表，并同步云安全组，验证后重启 Caddy。

### 官方参考

- [Docker Engine on Ubuntu](https://docs.docker.com/engine/install/ubuntu/)
- [Docker 与防火墙](https://docs.docker.com/engine/network/packet-filtering-firewalls/)
- [Cloudflare Origin CA](https://developers.cloudflare.com/ssl/origin-configuration/origin-ca/)
- [Cloudflare 支持端口](https://developers.cloudflare.com/fundamentals/reference/network-ports/)
- [Request Header Transform Rules](https://developers.cloudflare.com/rules/transform/request-header-modification/)
- [Cache Rules](https://developers.cloudflare.com/cache/how-to/cache-rules/)
- [Turnstile](https://developers.cloudflare.com/turnstile/)

仓库自动化测试覆盖应用行为与部署模板约束；实际 Docker 启动、S3 凭据、Cloudflare 规则、真实浏览器验证码和灾备恢复，需要部署者在自己的环境按本教程验收。
