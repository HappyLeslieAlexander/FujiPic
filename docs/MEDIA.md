# 扩展格式与隔离图片处理

## 功能范围

JPEG、PNG、GIF、WebP 保留现有处理方式。启用 `MEDIA_SOCKET` 后支持静态 AVIF、BMP、TIFF/TIF、受限 SVG、LibRaw 可识别的 RAW、NEF、DNG、CR2。

- 原始字节保留，按用户 SHA-256 去重；所有新增格式的原图通过 `application/octet-stream`、`attachment`、`nosniff`、`CSP sandbox` 下载。
- 后台生成最长边 2048px、最大 16 MiB 的 PNG 网页预览，透明度保留。复制图片链接/Markdown 使用预览，不嵌入原始 SVG 或 RAW。
- TIFF 只渲染第一页，不枚举或转换其他页；完整多页数据仍在原文件中。
- RAW 使用 LibRaw 完整解码，按相机白平衡、无自动增亮、sRGB 生成 8-bit 预览。效果不承诺等同相机直出/Lightroom。`.raw` 是路由提示，不接受任意字节；NEF/DNG/CR2 额外验证容器标记。
- 静态 AVIF 以 Pillow/底层解码库支持范围为准；动画 AVIF 拒绝。HDR/高位深的网页预览不保证保留原始动态范围，原图不变。
- SVG 使用 XML 元素/属性允许列表；拒绝 DTD/实体、脚本、事件、foreignObject、style、外部或 data/file URL、href/use 引用、动画和不支持的特性。上限 4 MiB、10000 元素、32 层。无静默“清理后看起来一样”的保证；当前仅发布位图预览，不发布可执行原始 SVG 或宣称提供完整安全矢量编辑器。
- 预览去除 EXIF/GPS/文本；有效 ICC 转换至 sRGB，处理不了的文件保留原图并标记预览失败。原图仍可能包含 GPS。

## 架构与安全边界

Go 主服务不加载新增的原生解码器。`converter/` 是无网络、无凭据、无数据库和对象存储挂载的独立容器。仅共享 Unix socket，主服务发送文件字节，不传 URL 或可读取的宿主路径。

每次解析/转换启动新的受限子进程：解码单并发、最多 4 个连接处理线程、60 秒 CPU、75 秒墙钟、64 个文件描述符、16 MiB 输出限制；容器 1 CPU、1536 MiB 内存、32 PID、384 MiB 临时磁盘、只读根文件系统、非 root、移除 capabilities。Go 客户端将检查与预览转换按 FIFO 共用单槽，最多 8 个等待者，等待上限 85 秒且可随请求取消；槽位持有至响应体处理结束。队列满或等待超时返回 busy，busy 不消耗后台解码重试次数。转换器自身仍在忙碌时立即返回 503，作为第二层保护。检查也会执行原生解析，不能假定只是廉价读取文件头。每用户最多 2 个上传，全局最多 8 个。默认容器 seccomp 生效。解析器不是形式化安全沙箱；及时更新锁定的依赖和基础镜像仍然必要。

主服务独立校验输出必须是尺寸/大小受限 PNG。`media_jobs` 在插入原图时通过数据库触发器创建；任务有租约、最多三次尝试、退避和崩溃恢复。预览对象使用随机、不可复用的版本 URL；仅 `ready` 状态对外发布。转换失败不删除原图。

预览上传前事务性预留用户及同桶容量；失败/过期租约释放预留并排队清理。软删除与预览清理、配额释放、主域名和别名 CDN purge 同事务完成。转换与删除竞争时，旧租约不能发布；PUT 返回后再次排队处理可能出现的孤立对象。

## 启用

现有数据库升级到 migration 0009；新增表不改写旧图片文件或 URL。先备份数据库与当前镜像。

```bash
docker compose --env-file /etc/fujipic/fujipic.env \
  -f deploy/compose.cloudflare-host.yml -f deploy/compose.media.yml up -d --build
```

后续每次启动/更新均包含 `compose.media.yml`，否则扩展格式处理服务不会启用。其他生产 Compose 也可叠加此文件。纯 Go 本地模式继续运行，未配置 socket 时扩展格式上传拒绝，不静默存入未经验证的文件。

默认上传上限仍为 10 MiB。RAW 往往超过此值；调整 `MAX_UPLOAD_BYTES` 时同时检查 Caddy 请求体上限、Cloudflare 上传限制、临时磁盘与容器内存。转换器配置与主服务使用相同文件/像素/边长限制；不能只修改前端 accept。

现有磁盘准入检查会在 `TMP_DIR` 与系统临时目录各要求至少 `64 MiB + 16 × MAX_UPLOAD_BYTES` 可用空间。例如上限 20 MiB 时至少约 384 MiB，上限 100 MiB 时至少约 1664 MiB；必须同步扩大 Compose `/tmp` 限额或使用容量受控的磁盘临时卷，否则上传会返回 `temporary_storage_full`。

## API 与 CDN

为兼容已有 API，`url` 仍为原图对象 URL，新增：

- `original_url`：原图地址。
- `preview_url`：预览完成时才出现；旧格式等于原图地址。
- `processing_state`：`pending` / `processing` / `ready` / `failed`。
- 扩展格式未生成预览时上传响应 `markdown` / `html` 为空。

网页最多轮询十分钟，每十秒一次列表请求，仅更新完成的卡片，不自动刷新筛选表单。超过范围或时间可手动刷新图库。API 客户端可查询单图状态；原图已保存不代表预览已完成。

原图及预览均经 `/i/` 返回可缓存、无 Cookie 的响应；失败和未发布地址为 `no-store`。Cloudflare Cache Rules 应匹配公开图片域名的 `/i/` 路径，而非只依赖默认扩展名；RAW/NEF/DNG/CR2 不在默认缓存扩展名列表内。部署规则需实际验证 `CF-Cache-Status: HIT`，不能从源站响应头推断边缘已经命中。大文件需遵守套餐单对象缓存限制。

## 回归验证

```bash
go test ./...
go test -race ./...
go vet ./...
docker build -f converter/Dockerfile -t fujipic-converter:test .
docker run --rm --network none --read-only --cap-drop ALL \
  --security-opt no-new-privileges --memory 1536m --cpus 1 --pids-limit 32 \
  --tmpfs /tmp:rw,noexec,nosuid,nodev,size=384m \
  -v "$PWD/converter:/tests:ro" fujipic-converter:test python -I /tests/test_converter.py
docker build -f deploy/Dockerfile -t fujipic-media-app:test .
docker compose -f deploy/compose.media-test.yml up -d
# 用 make_fixtures.py 生成小型 AVIF/BMP/TIFF/SVG/DNG/RAW 样本，
# 加入经过授权的真实 NEF、CR2 样本后运行：
python3 scripts/test-media.py /path/to/fixtures
docker compose -f deploy/compose.media-test.yml down -v
```

测试使用独立 Compose 项目、内存对象存储和临时数据库。小型 DNG/RAW 为项目生成的 CFA 测试图；真实相机回归样本取自 rawpy 测试目录（`iss030e122639.NEF`、`RAW_CANON_40D_SRAW_V103.CR2`），不打包进入仓库或镜像。支持实际机型/压缩模式仍应以对应样张测试为准。
