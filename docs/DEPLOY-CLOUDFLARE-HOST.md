# 宿主机 Caddy + Docker 应用（Cloudflare）

目标：Cloudflare → 宿主机 Caddy TCP 443 → `127.0.0.1:18080` → app:8080。
Docker 不再运行 Caddy。网站和图片仍使用两个域名，图片缓存，网站绕过缓存。
2026-11-11 已在线验证登录、上传、443 图片路径 HIT，以及真实 OCI 桶的后台添加探针。
自动化本地测试仍使用替身，不替代每次部署的 Docker、OCI 和 Cloudflare 验收。

日常操作见本文“日常备份与更新”；已经迁移的服务器无需重复执行迁移步骤。

## 文件与安全边界

- `deploy/compose.cloudflare-host.yml` 是独立替换文件，不和旧 Compose 合并。
- 项目名仍为 `fujipic-cloudflare`，卷仍为 `fujipic-data`，网络名和应用 IP 不变。
- 应用只发布 `127.0.0.1:18080:8080/tcp`，不要改成所有地址。
- 宿主机代理通过 bridge 网关连接应用，应用只信任 `172.30.84.1/32`，不信任整个私网。
- `deploy/Caddyfile.cloudflare-host` 只有站点和 snippet，不覆盖宿主机全局配置或其他站点。
- 两域名都验证实际 TCP 来源属于 Cloudflare；透传 Cloudflare 提供的共享密钥，不在源站注入。
- 图片仅允许 GET/HEAD `/i/*`，去掉 Cookie/Authorization；网站保留认证头。

## 新部署准备

1. 按 [基础准备](archive/DEPLOY-CLOUDFLARE-8443.md#2-从-docker-官方-apt-源安装)的
   第 2–5 节安装 Docker、获取代码、准备环境文件和 Origin CA 证书；这些是共用的准备步骤。
   **不要启动旧容器 Caddy，也不要创建 8443 回源规则。** Docker Engine 要求 28+。
2. 安装并启动宿主机 Caddy（建议 2.11.9+）。已有 Caddy 时沿用其配置和 systemd 服务。
3. Cloudflare 开启两域名橙云与 Full (strict)，回源使用 TCP 443。
   按 [CDN 指南](CDN.md)配置共享密钥、头清理和缓存。两个域名的证书必须已生效。
4. 云防火墙、宿主机规则和 Caddy 来源检查配合保护 FujiPic 站点；18080 不对公网开放。
   若 443 同时服务其他网站，按其需求保留访问，不要全局收紧而中断其他业务。
5. 为宿主机 Caddy 准备证书与 Cloudflare 地址列表：

```bash
sudo install -d -o root -g caddy -m 0750 /etc/caddy/fujipic-origin
sudo install -o root -g caddy -m 0640 /etc/fujipic/origin/cert.pem /etc/caddy/fujipic-origin/cert.pem
sudo install -o root -g caddy -m 0640 /etc/fujipic/origin/key.pem /etc/caddy/fujipic-origin/key.pem
sudo install -o root -g caddy -m 0640 deploy/cloudflare-ips.caddy /etc/caddy/fujipic-cloudflare-ips.caddy
```

这些路径是模板约定；已有文件先备份并核对，不直接覆盖。更新 IP 列表时核对 Cloudflare 官方列表。
然后按下文第 2 节准备站点，定义第 3 节 `dch` 后先运行 `dch build app`，
再启动 app、验证 Caddy 并重载，按第 4 节验收。全新部署无需第 5 节移除旧代理。

## 从旧 Compose 迁移

在维护窗口操作。已有 Origin CA 证书应覆盖两个域名，宿主机 Caddy 正常运行。
使用 Docker Engine 28+（旧版本存在 loopback 发布端口的同二层网络访问问题）。
不改变 Docker daemon 路由选项，不向公网开放 18080。
不要执行旧 Compose 的 `down`（两套文件共享项目名），也不要执行 `down -v`、
`volume rm` 或 prune；不要重新生成存储加密密钥。

### 1. 保存回退资料和备份

在 `/opt/fujipic/src` 使用旧 `dc` 函数，按
[旧指南第 10 节](archive/DEPLOY-CLOUDFLARE-8443.md#10-在线备份更新与恢复边界)在线备份数据库。
保存 `/etc/caddy/Caddyfile`、旧图片站点文件、旧 Compose 和可用的容器 Caddy 配置。
确认网关一致、18080 空闲：

```bash
sudo docker network inspect fujipic-cloudflare_origin --format '{{json .IPAM.Config}}'
sudo ss -lntp '( sport = :18080 )'
sudo docker version --format '{{.Server.Version}}'
```

若网关不是 `172.30.84.1` 或端口被占用，停止并调整，不强制删除网络。

### 2. 准备宿主机站点

沿用 `/etc/caddy/fujipic-origin/cert.pem`、`key.pem` 和
`/etc/caddy/fujipic-cloudflare-ips.caddy`。确保 caddy 用户可读、私钥不对所有用户可读。
这些是独立副本，证书续期和 Cloudflare IP 更新要同步。

把模板替换为实际域名（宿主机 systemd 不自动读取 Compose 环境）：

```bash
sed -e 's/{$APP_HOST}/pic.lesliealexander.eu/g' \
    -e 's/{$IMAGE_HOST}/img.lesliealexander.eu/g' \
    deploy/Caddyfile.cloudflare-host | sudo tee /etc/caddy/fujipic-sites.caddy >/dev/null
```

主 Caddyfile 中，将旧的 `import /etc/caddy/fujipic-image.caddy` 替换为
`import /etc/caddy/fujipic-sites.caddy`，不要重复导入同名站点。
全新部署则在主 Caddyfile 站点块之外添加这条 import。
保留全局配置和其他业务，先验证、不 reload：

```bash
sudo -u caddy /usr/bin/caddy validate --config /etc/caddy/Caddyfile --adapter caddyfile
```

### 3. 重建应用，暂不删除旧代理

```bash
dch() { sudo docker compose --env-file /etc/fujipic/fujipic.env -f /opt/fujipic/src/deploy/compose.cloudflare-host.yml "$@"; }
dch config --quiet
dch up -d --no-deps app
curl --noproxy '*' --connect-timeout 3 --max-time 10 -fsS http://127.0.0.1:18080/healthz
curl --noproxy '*' --connect-timeout 3 --max-time 10 -sS -o /dev/null -w '%{http_code}\n' http://127.0.0.1:18080/login
```

预期健康响应和 403（无共享密钥），否则停止。不要将密钥放进测试命令。
重建有短暂中断。旧代理为 orphan，暂不使用 `--remove-orphans`。

### 4. 重载与回源切换

```bash
sudo systemctl reload caddy
sudo systemctl is-active caddy
```

在 Cloudflare 禁用仅针对 FujiPic 的 8443 回源改写，两域名都使用 HTTPS 443。
保留 Full (strict)、共享密钥注入、图片 Cookie/Authorization 移除、缓存规则和上传 WAF 例外。
不要修改其他业务的 Origin Rules。

验收登录、上传、图片 GET MISS/HIT、图片域名 `/login` 404、删除后原 URL 404，
并检查原博客/API。查看 app 访问日志确认真实客户端 IP，不应全部是 Docker 网关。
不要公开完整日志或认证信息。

### 5. 移除旧代理

全部验收通过后，用旧文件仅停止并删除其 caddy 服务：

```bash
sudo docker compose --env-file /etc/fujipic/fujipic.env -f deploy/compose.cloudflare.yml stop caddy
sudo docker compose --env-file /etc/fujipic/fujipic.env -f deploy/compose.cloudflare.yml rm -f caddy
dch ps
```

不停止宿主机 Caddy。确认其他业务不依赖 8443 后撤销该端口放行。
把原 dc 函数、更新/备份脚本改用新文件，避免恢复旧可信代理配置或重新创建容器 Caddy。

## 回退

不恢复旧数据库、不删除数据卷。恢复保存的宿主机配置与旧 Compose/Caddy 文件，
验证宿主机配置后，用旧 Compose `up -d` 恢复 app 环境与容器代理，再 reload 宿主机 Caddy。
恢复原来仅网站的 8443 回源规则；若原图片已走宿主机 443，则继续使用其旧站点。
必要时恢复仅 Cloudflare IP 的 8443 放行。

## 日常备份与更新

始终使用上面的 `dch`（绝对路径），先以 `dch config --quiet` 验证配置，不输出展开后的密钥。
应用镜像是 distroless，没有 `sh`、`bash` 或 `curl`；管理命令直接执行二进制。

### 在线数据库备份

每次使用唯一文件名；备份命令拒绝覆盖已有文件：

```bash
BACKUP_DATE=$(date -u +%Y%m%dT%H%M%SZ)
dch exec -T app /usr/local/bin/fujipic backup --out "/data/backup/fujipic-${BACKUP_DATE}.db"
sudo install -d -o root -g root -m 0700 /var/backups/fujipic
dch cp "app:/data/backup/fujipic-${BACKUP_DATE}.db" "/var/backups/fujipic/fujipic-${BACKUP_DATE}.db"
sudo chown root:root "/var/backups/fujipic/fujipic-${BACKUP_DATE}.db"
sudo chmod 0600 "/var/backups/fujipic/fujipic-${BACKUP_DATE}.db"
```

若任一步失败，先处理失败，不继续升级。该备份使用 SQLite `VACUUM INTO`，
不要直接复制活跃数据库主文件来替代。另行加密、离机保存环境文件（尤其
`STORAGE_ENCRYPTION_KEY`）、Caddy 配置、证书及私钥。数据库不包含 OCI 图片字节。
本方案没有自动备份任务；需自行安排周期、保留策略、离机同步和恢复演练。

### 更新应用

先完成备份并记录当前提交与镜像 ID。检查服务器工作区，特别是此前手工创建的
`deploy/compose.cloudflare-host.yml`：新版本开始跟踪这个文件，未跟踪的同名文件可能阻止拉取。
先在仓库外保存副本并比较差异；仅在确认可用仓库版本接替后移走旧副本，保留回退资料。
不要使用 `reset --hard` 或 `git clean` 解决配置冲突。

以下 Git 命令由仓库所有者执行（仓库归 root 所有时使用 `sudo git -C /opt/fujipic/src ...`）：

```bash
git -C /opt/fujipic/src status --short
git -C /opt/fujipic/src rev-parse HEAD
dch images
git -C /opt/fujipic/src pull --ff-only
dch config --quiet
dch build app
dch up -d --no-deps app
dch ps
curl --noproxy '*' --connect-timeout 3 --max-time 10 -fsS http://127.0.0.1:18080/healthz
```

重建应用会有短暂中断。完成后验证公网登录、普通用户上传、已有图片读取，以及新增功能；
检查真实 GET 的缓存 HIT 和原有宿主机业务。仅更新应用无需重载 Caddy。
不要使用 `--remove-orphans` 自动清理其他容器。

### 配置维护和恢复

- Caddy 配置/证书更新：先备份，再以 caddy 用户运行 `caddy validate`，成功后才 reload。
  实际部署可拆分为 `fujipic-web.caddy`、`fujipic-image.caddy`；它们与本模板二选一导入，避免重复站点。
- Cloudflare 地址段更新需同步宿主机 allowlist 和相关网络规则，保留其他站点配置。
- 数据库恢复需要停 app、保留当前卷内容，并一致处理 DB/WAL/SHM；恢复文件归属为 `65532:65532`。
  使用与备份匹配的存储加密密钥。代码回退不会撤销迁移或恢复已删除的云对象。
- 数据卷是 `fujipic-cloudflare_fujipic-data`，标准 Docker 路径通常为
  `/var/lib/docker/volumes/fujipic-cloudflare_fujipic-data/_data`；以实际挂载为准，图片存储在 OCI。

## 本地检查

```bash
go test ./internal/server
python3 scripts/test-cloudflare-proxy.py --host
```

原生测试验证来源拒绝、双域名路由、图片头隔离、共享密钥透传；不验证 Linux Docker NAT、
systemd 权限、真实证书、Cloudflare 缓存或云桶。
