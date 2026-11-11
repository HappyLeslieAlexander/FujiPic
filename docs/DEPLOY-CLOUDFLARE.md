# Ubuntu ARM64：Docker + Cloudflare + 多租户 OCI 部署

适用：Ubuntu 22.04/24.04/26.04 LTS、ARM64（aarch64）、1 核 / 6 GB 内存；
Cloudflare 从第一天启用，使用 Origin CA 源站证书；多个 OCI 租户/区域的私有桶。
使用独立的 `deploy/compose.cloudflare.yml`，**不要与 `compose.prod.yml` 合并**。

文中的两个域名是示例，可以替换，但必须在配置、证书和 Cloudflare 规则中同步修改：

| 地址 | 用途 |
| --- | --- |
| `https://pic.lesliealexander.eu` | 网站、登录、后台、上传、API |
| `https://img.lesliealexander.eu/i/...` | 图片访问；该域名只允许 GET/HEAD `/i/*` |

```text
访客 HTTPS :443 → Cloudflare（边缘证书 / CDN）
                         │ Full (strict)，Origin Rule 端口覆盖
                         ▼ HTTPS :8443
                     Caddy（Origin CA 证书 / 来源 IP 校验）
                         │ Docker 网络，HTTP :8080
                         ▼
                      FujiPic ── 私有 OCI S3 桶（多个租户/区域）
                         └── 数据卷：SQLite / 临时上传文件
```

**8443 是源站端口，不是访客 URL 的端口。** Cloudflare 支持代理访客的 8443，
但普通套餐对该访客端口默认禁用缓存；本方案让访客使用标准 443，再覆盖回源端口。
源站不需要开放 80/443，也不依赖 ACME 申请证书。

## 1. 部署前检查与网络边界

在服务器执行（以下命令以具有 sudo 权限的普通用户执行）：

```bash
cat /etc/os-release
uname -m
dpkg --print-architecture
free -h
df -h /
sudo ss -lntp
```

架构预期为 `aarch64` / `arm64`。确保 8443 未被占用，磁盘有构建镜像、数据库和备份空间。
宿主机不需要安装 Go，Docker 在 ARM64 服务器上原生构建 ARM64 应用。

云安全组/OCI NSG 或 Security List 的入站规则：

- SSH：仅允许你的管理 IP；保留当前连接，并测试第二个 SSH 连接后再收紧。
- TCP 8443：仅允许 [Cloudflare IPv4](https://www.cloudflare.com/ips-v4) 来源。
  若真的启用 IPv6 回源，再添加 [Cloudflare IPv6](https://www.cloudflare.com/ips-v6) 来源。
- 不给此服务开放 TCP 8080、80、443，也无需开放 UDP 8443。
- 检查关联的所有 NSG/Security List：它们的放行规则会叠加，不要留一个允许全网 8443 的规则。
- 保留 DNS、HTTPS 等必要出站访问（镜像下载、OCI S3、Cloudflare 刷新 API）。

**不能只依赖 UFW**：Docker 发布端口可能绕过其常规规则。云端白名单是第一道边界，
Caddy 对真实 TCP 来源做第二次校验，应用共享密钥再约束为你的站点。
Origin CA 只证明服务器身份，不会自动限制客户端，也不等于双向 TLS。
不要为了让 Docker 工作而清空服务器 iptables 或关闭所有防火墙。

## 2. 从 Docker 官方 APT 源安装

先检查已有 `docker.io`、`podman-docker`、`containerd` 等包；若存在其他容器工作负载，
不要直接卸载或覆盖，先按官方文档处理冲突。全新 Ubuntu 可执行：

```bash
sudo apt-get update
sudo apt-get install -y ca-certificates curl git openssl python3
sudo install -m 0755 -d /etc/apt/keyrings
sudo curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc
sudo chmod a+r /etc/apt/keyrings/docker.asc

sudo tee /etc/apt/sources.list.d/docker.sources >/dev/null <<EOF
Types: deb
URIs: https://download.docker.com/linux/ubuntu
Suites: $(. /etc/os-release && echo "${UBUNTU_CODENAME:-$VERSION_CODENAME}")
Components: stable
Architectures: $(dpkg --print-architecture)
Signed-By: /etc/apt/keyrings/docker.asc
EOF

sudo apt-get update
sudo apt-get install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
sudo systemctl enable --now docker
sudo docker run --rm hello-world
sudo docker compose version
```

保留 `sudo docker`，不必把普通用户加入 `docker` 组；该组具有近似 root 的权限。
生产环境不使用未经审查的 `curl | sh` 一键安装脚本。

## 3. 获取代码，创建配置目录

```bash
sudo install -d -o "$USER" -g "$(id -gn)" -m 0750 /opt/fujipic
git clone https://github.com/HappyLeslieAlexander/FujiPic.git /opt/fujipic/src
cd /opt/fujipic/src
git rev-parse HEAD

sudo install -d -o root -g root -m 0700 /etc/fujipic
sudo install -d -o root -g 65532 -m 0750 /etc/fujipic/origin
sudo install -o root -g root -m 0600 deploy/cloudflare.env.example /etc/fujipic/fujipic.env
```

必须使用包含本文及新部署文件的已审核提交；不要假定远程 main 已包含本地未推送的更改。
记录提交号便于升级和回滚。不要将生产密钥保存在仓库中。

## 4. 安装 Cloudflare Origin CA 证书

Cloudflare → `lesliealexander.eu` → SSL/TLS → Origin Server → Create Certificate：

1. 选择 PEM 格式，证书覆盖 `pic.lesliealexander.eu`、`img.lesliealexander.eu`。
   也可用覆盖这两个一级子域名的 `*.lesliealexander.eu`，但专用证书的授权范围更小。
2. 安全保存证书与私钥，**不要发给聊天助手、不要提交 Git**。
3. 在服务器编辑以下文件，将完整 PEM 内容分别放入：

```bash
sudo nano /etc/fujipic/origin/cert.pem
sudo nano /etc/fujipic/origin/key.pem
sudo chown root:65532 /etc/fujipic/origin/cert.pem /etc/fujipic/origin/key.pem
sudo chmod 0640 /etc/fujipic/origin/cert.pem /etc/fujipic/origin/key.pem
sudo openssl x509 -in /etc/fujipic/origin/cert.pem -noout -subject -dates -ext subjectAltName
```

Caddy 以 UID/GID `65532:65532` 运行，必须能读取目录和私钥；不是把私钥改成全员可读。
证书目录整体只读挂载，便于原子替换文件。自行设置到期提醒，Origin CA 不会自动续期。
浏览器通常不信任 Origin CA；不要因此关闭 TLS 校验，也不要将 DNS 切成灰云作为正式访问方式。

## 5. 配置应用与第一个 OCI 桶

所有桶保持私有，使用 Standard 层；只授予对应桶的对象读写、删除权限。
OCI 凭据是 **Customer Secret Key 的 Access Key / Secret Key**，不是 OCI API Signing Key。
确认每个租户、区域和存储层的免费资格，并将其他项目占用计入同一租户预算。

分别执行以下命令，生成三个不同的秘密，并保存至密码管理器：

```bash
openssl rand -base64 32
openssl rand -hex 32
openssl rand -hex 24
sudo nano /etc/fujipic/fujipic.env
```

依次用于 `STORAGE_ENCRYPTION_KEY`、`ORIGIN_SHARED_SECRET`、`BOOTSTRAP_ADMIN_PASSWORD`。
填好模板中的所有必填项：

- `APP_HOST=pic.lesliealexander.eu`、`IMAGE_HOST=img.lesliealexander.eu`，不加协议、路径或端口。
- `S3_ENDPOINT=https://<namespace>.compat.objectstorage.<region>.oraclecloud.com`。
- `S3_REGION`、`S3_BUCKET`、`S3_ACCESS_KEY_ID`、`S3_SECRET_ACCESS_KEY`：第一个桶的信息。
- `BOOTSTRAP_ADMIN_EMAIL`：你的管理员邮箱；密码长度要求 10–200 字符。
- `CDN_ZONE_ID`：`lesliealexander.eu` 的 Zone ID，不是 Account ID。
- `CDN_PURGE_TOKEN`：创建 Cloudflare API Token，权限仅 `Zone → Cache Purge → Purge`，
  资源仅 `lesliealexander.eu`。不要使用 Global API Key。

这是供 Compose 读取的 dotenv 文件，**不要 `source` 它**；若值包含 `$`，用单引号包裹避免插值。
不要运行或分享未脱敏的 `docker inspect` 或 `docker compose config`，它们可能显示环境密钥。
环境变量并非对宿主机 root/Docker 管理者保密。本方案没有声称使用 Docker Secrets。

`STORAGE_ENCRYPTION_KEY` 之后不要随意更换，否则数据库中其他桶的凭据不能解密。
应用的 `/data` 会自动创建为 nonroot 可写的命名卷，不要预先建 root 所有的数据库文件。

## 6. Cloudflare DNS、TLS、回源和缓存

### DNS / SSL

为 `pic` 和 `img` 各添加一个 A 记录，指向服务器公网 IPv4，**两者开启橙云代理**。
未验证 IPv6 回源前不要添加 AAAA。确认两个域名的边缘证书均已生效。

SSL/TLS 使用 **Full (strict)**，不要用 Flexible。若同区域还有其他站点，
先确认全局切换不会破坏它们，必要时仅对这两个主机名使用配置规则。
开启这两个域名的 HTTP → HTTPS 重定向；可用主机名限定的重定向规则，
或在不会影响其他站点时使用区域级 Always Use HTTPS。

### Origin Rule：443 → 8443

规则匹配：

```text
http.host in {"pic.lesliealexander.eu" "img.lesliealexander.eu"}
```

设置 **Destination port = 8443**。不要覆盖 Host/SNI，不需要另建灰云 origin 域名。
Free 套餐也支持目的端口覆盖。访客及 `APP_BASE_URL`/`PUBLIC_BASE_URL` 均不加 `:8443`。

### Request Header Transform Rule：回源鉴权

对同样的两个主机名、**所有路径**，使用 **Set static（覆盖，不是追加）**：

```text
X-Origin-Secret: <与 ORIGIN_SHARED_SECRET 完全一致的随机值>
```

必须覆盖登录、API、静态资源和 `/i/*`。不能只给图片路径加，也不能放在浏览器 JS 中。
不要用响应头规则（Response Header Rule），避免把密钥发回客户端。
Caddy 不会替你注入这个秘密；否则来自其他 Cloudflare 站点的请求也可能被源站自动授权。

### 图片请求头清理

另一条 Request Header Transform Rule 匹配：

```text
http.host eq "img.lesliealexander.eu" and starts_with(http.request.uri.path, "/i/")
```

删除请求头 `Cookie` 和 `Authorization`。只作用于图片域名；**不要删除网站/API 的这两个头**。
Caddy 也做了同样清理，Cloudflare 处的清理有助于图片缓存不被凭据请求影响。
不要移除 `CF-Connecting-IP`，不要启用将 IPv6 访客替换为伪 IPv4 的模式。

### Cache Rules

使用两条互不重叠的规则，避免整个网站被“Cache Everything”：

1. 匹配 `http.host eq "pic.lesliealexander.eu"`：**Bypass cache**。
   首次部署网站域名的静态资源也先绕过，避免引入第三套缓存逻辑。
2. 匹配图片请求头清理使用的同一表达式：**Eligible for cache**。
   Edge TTL 选择“Use cache-control header if present, bypass cache if not”；
   Browser TTL 选择 Respect origin。不要强制给所有状态码一个长期 TTL。

不要给图片缓存规则加仅 `GET` 的方法条件，Cloudflare 文档提示这会影响按 URL 刷新。
保持默认 URL 缓存键，不添加 Cookie/Authorization/自定义头，首次也不配置忽略查询参数。
应用生成的图片链接不带查询参数，验收时使用原始 URL。
不要启用图片转码/优化/裁剪，不强制缓存错误页、带 Set-Cookie 或 no-store 的响应。

Cloudflare 上的 WAF/限流不要对 API 上传施加浏览器挑战；不要为此关闭全站防护。
图片域名不提供网页登录，`/login`、`/admin` 和非 GET/HEAD 图片请求由 Caddy 返回 404。

## 7. 校验并启动

在仓库根目录定义一个当前 shell 可用的快捷函数，后续 `dc` 都指这套配置：

```bash
cd /opt/fujipic/src
dc() { sudo docker compose --env-file /etc/fujipic/fujipic.env -f deploy/compose.cloudflare.yml "$@"; }
dc config --quiet
dc pull caddy
dc build app
dc run --rm --no-deps caddy caddy validate --config /etc/caddy/Caddyfile --adapter caddyfile
dc up -d
dc ps
dc logs --tail=100 app caddy
```

`config --quiet` 只验证，不打印秘密。1 核机器初次构建可能较慢；在 ARM64 主机原生构建，
不强制 amd64 模拟。Caddy 已固定版本，应用应配合已审核 Git 提交构建；升级镜像需主动测试。

若 Docker 报网络重叠，先检查 `docker network ls/inspect` 及云网络路由。
本配置使用 `172.30.84.0/29`；如需调整，**同步修改 subnet、两个静态地址和应用
`TRUSTED_PROXY_CIDRS`**，不要扩大为信任所有私网。

安全配置包括非 root、只读根文件系统、丢弃 capabilities、禁止提权、内存/PID 上限、日志轮转。
Go 内存软目标不是硬保证；2 GiB 应用硬限制和 256 MiB Caddy 限制是起点，需要观察实际负载。
没有配置假的 `curl` 容器健康检查：distroless 应用镜像没有 shell/curl。
`depends_on` 仅保证启动顺序，不保证应用已就绪，必须进行下面的 HTTP 验收。

## 8. 首次登录与多桶限额

```bash
curl -fsS https://pic.lesliealexander.eu/healthz
curl -fsS https://pic.lesliealexander.eu/readyz
curl -fsS -o /dev/null -w 'login HTTP %{http_code}\n' https://pic.lesliealexander.eu/login
curl -sS -o /dev/null -w 'image login HTTP %{http_code}\n' https://img.lesliealexander.eu/login
```

预期依次成功、成功、200、404。**readyz 不验证真实桶的全部读写权限**，还需实际上传。
若登录 403 而健康检查成功，首先检查 Cloudflare 是否对所有路径覆盖注入共享密钥。
若所有路径都是 403，检查 Caddy 来源白名单与实际连接来源，不要改为信任客户端头。

管理员登录 `/admin` 后，在上传业务图片前：

1. **先给默认桶设置容量**，它初始为 `0`（不限），优先级为 `0`。
2. 其他租户/区域逐个添加：各自 endpoint、region、bucket、Customer Secret Key，
   勾选 path-style，优先级例如 `10、20、30`。保存会执行 Put/Get/Delete 探针。
3. 每个桶目标为十进制 **19 GB** 时，后台最大容量填 **`17.69` GiB**
   （约 18,994,492,866 字节）。**不要填 19 GiB**，那约等于 20.40 GB。
4. 如果同一租户有多个桶，把该租户可用预算分摊到这些桶。不同区域并不自动拥有独立免费额度；
   逐租户检查区域资格、存储层、其他项目占用、API 请求和流量费用。
5. 不要给同一个真实桶注册多个条目，不要跨多个 FujiPic 实例共享同一批对象并各自独立计数。

应用按优先级选择第一个有足够容量的桶，并发上传会原子预留容量；全满时返回 507。
这是**容量溢出**，不是“网络故障自动切换”：不要假定一个未满但不可用的桶会自动跳过。

这些限额不是云端收费硬上限：只统计本应用的活跃图片。未成功删除的对象、版本历史、
外部写入、备份和 API 请求均可能额外占用资源。应用删除时先释放配额，实际对象删除失败会重试，
因此反复上传/删除可能使真实占用高于界面计数。建议专用空桶、关闭不需要的版本/复制/自动分层，
设置 OCI 用量监控及预算告警（预算告警本身也不是停止计费的硬开关）。

确认管理员存在且能登录后，编辑配置把 **两个** `BOOTSTRAP_ADMIN_*` 都清空，然后执行：

```bash
dc up -d --force-recreate app
```

现有管理员不会被删除，移除初始化密码可减少长期驻留的明文秘密。

## 9. CDN 与源站保护验收

使用专门的测试账号生成短期 Token，权限 upload/read/delete。验收脚本会删除它上传或去重命中的图片，
因此不要给生产图片用这个脚本。不要把 Token 字面量写进命令历史：

```bash
read -rsp 'Test API token: ' FP_TOKEN
echo
bash scripts/verify-cdn.sh https://pic.lesliealexander.eu https://img.lesliealexander.eu "$FP_TOKEN"
unset FP_TOKEN
```

本地进程列表在脚本运行期间仍可能看到参数，仅在可信管理环境运行，并在验收后撤销 Token。
脚本仅显示缓存头，**不会以第二次 HIT 作为严格断言**；必须人工确认以下检查：

- 用真实 GET 连续请求同一张新图，应从 MISS 变为 HIT，字节一致、没有 Set-Cookie。
- HEAD/Range/ETag 行为正常；网站登录和 API 不能是 HIT，返回 no-store。
- 删除后等待后台刷新（worker 约每 30 秒运行），再次请求原 URL 应返回 404。
  未立刻 404 不代表刷新一定失败，但不能一直停留在 HIT；检查应用日志和 Cloudflare Token。
- 浏览器端长期缓存或已下载的图片不能通过 CDN 刷新收回；图片 URL 公开，不是敏感资料访问控制。
- `dc restart` 后重新登录、查看图片、实际读取，验证数据库与对象持久化。
- 从非 Cloudflare 网络直连服务器 8443，预期云防火墙拒绝连接；若请求抵达 Caddy，则返回 403。
  仅对这一项隔离检查，可使用 `curl -k --resolve pic.lesliealexander.eu:8443:服务器IP
  https://pic.lesliealexander.eu:8443/login`（写成一行）。`-k` 只为绕过浏览器不信任 Origin CA，
  不能证明证书正确，也不能用于正常 HTTPS 验收。
- 检查 `dc ps` 和 `docker ps`：应用没有宿主机端口，Caddy 只发布 TCP 8443。

## 10. 在线备份、更新与恢复边界

使用应用自带 `VACUUM INTO` 在线备份，不要只复制活跃数据库主文件（WAL 中可能还有数据）：

```bash
dc exec -T app /usr/local/bin/fujipic backup --out /data/backup/fujipic.db
sudo install -d -o root -g root -m 0700 /var/backups/fujipic
BACKUP_DATE=$(date -u +%Y%m%dT%H%M%SZ)
dc cp app:/data/backup/fujipic.db "/var/backups/fujipic/fujipic-${BACKUP_DATE}.db"
sudo chown root:root "/var/backups/fujipic/fujipic-${BACKUP_DATE}.db"
sudo chmod 0600 "/var/backups/fujipic/fujipic-${BACKUP_DATE}.db"
```

另行加密并离机备份 `/etc/fujipic/fujipic.env`、Origin CA 私钥和证书。
数据库含用户/Token 哈希及自定义桶加密凭据；没有 `STORAGE_ENCRYPTION_KEY` 不能解密。
数据库备份不含图片字节，需要独立的图片恢复策略，其占用也要纳入各租户容量预算。
制定自动备份周期、保留数量及离机同步；只留同一服务器不构成可靠灾备。

更新前先备份、记录提交和镜像 ID，确认迁移兼容性，再执行：

```bash
git pull --ff-only
dc build app
dc up -d
curl -fsS https://pic.lesliealexander.eu/readyz
curl -fsS -o /dev/null -w '%{http_code}\n' https://pic.lesliealexander.eu/login
```

恢复时先停止 app，保留当前数据，再将已验证备份恢复到原数据卷，所有权 `65532:65532`；
数据库主文件与 WAL/SHM 必须作为一致状态处理，不能把旧备份覆盖到仍运行的服务上。
在隔离环境做一次恢复演练后，才能认为备份可用。代码回滚不会撤销数据库迁移或云端对象删除。
**不要执行 `docker compose down -v` 或卷清理来“修复部署”**，它会删除数据库卷。

更换证书、IP 白名单后先 `dc run --rm --no-deps caddy caddy validate --config
/etc/caddy/Caddyfile --adapter caddyfile`（一行），再 `dc restart caddy`。
此配置关闭 Caddy 管理 API，不使用 `caddy reload`。定期检查官方 Cloudflare IP 段是否变化，
同步更新 `deploy/cloudflare-ips.caddy` 和云端白名单。主动测试镜像与系统安全更新。

## 参考与验证范围

- [Docker Ubuntu 安装与防火墙注意事项](https://docs.docker.com/engine/install/ubuntu/)
- [Cloudflare 代理端口与缓存限制](https://developers.cloudflare.com/fundamentals/reference/network-ports/)
- [Origin Rules 套餐支持](https://developers.cloudflare.com/rules/origin-rules/)
- [Origin CA / Full (strict)](https://developers.cloudflare.com/ssl/origin-configuration/origin-ca/)
- [Cache Rules 设置](https://developers.cloudflare.com/cache/how-to/cache-rules/settings/)
- [OCI Always Free 说明](https://docs.oracle.com/en-us/iaas/Content/FreeTier/freetier_topic-Always_Free_Resources.htm)

仓库测试和本机 Caddy 配置检查不能代替 Ubuntu Docker / Cloudflare / 真实 OCI 桶验收。
上线前按第 8–9 节逐项验证；不要仅凭 healthz/readyz 判断部署安全或桶可用。

本机可用 Caddy 2.11.9+、Python 3 和 OpenSSL 运行代理回归：

```bash
CADDY_BIN=/path/to/caddy python3 scripts/test-cloudflare-proxy.py
```

该测试用临时自签名证书和本地替身验证双域名路由、来源拒绝及认证头隔离，不使用生产证书或密钥。
