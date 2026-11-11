# 新增 OCI 桶认证失败排查

## 先检查实际提交参数

- OCI 使用 **Customer Secret Key** 的 Access/Secret，不是 API Signing Key 或 Auth Token。
- 端点格式为 `https://<namespace>.compat.objectstorage.<region>.oraclecloud.com`，
  不附加桶名或路径；端点区域、表单 Region、桶所在区域应一致。
- 命名空间与凭据属于目标租户。不同租户可使用不同凭据，默认桶配置不会替代新增桶参数。
- 自定义桶表单需勾选 path-style；默认桶的 `S3_FORCE_PATH_STYLE` 不控制自定义桶。
- 保留失败时间、请求方法、HTTP 状态、错误代码和 OCI Request ID；不要记录表单原文或认证头。

识别出的 OCI 端点会在请求前校验区域一致性。path-style 未勾选可能触发虚拟主机名的
TLS 证书错误；不要关闭证书校验，也不要把它与 OCI 返回的签名错误混为一谈。

## 加密密钥与 S3 密钥是两回事

`STORAGE_ENCRYPTION_KEY` 仅用于 AES-256-GCM 加密数据库里的自定义桶 Secret。
新增流程是校验输入 → 生成密文 → 用表单中的原 Secret 创建 S3 客户端 → PUT/GET/DELETE 探针 → 保存数据库。
探针不解密，不把本地加密密钥发送到 OCI。启动后加载已有桶才需要解密。

- 空的加密密钥：添加功能被禁用。
- 格式错误：创建 registry/启动失败。
- 换成另一把有效密钥：旧桶解密在本地失败，不会带着乱码 Secret 请求 OCI；新桶探针仍用表单 Secret。
- 因此不要为排查新增桶返回的 `SignatureDoesNotMatch` 而更换该密钥。

## 已覆盖的本地验证

- 表单保留含 `+ / = &` 的 Secret。
- 不同区域、租户的客户端与数据库重载不串用参数。
- 独立重算 SigV4 HMAC，验证 PUT/GET/HEAD/DELETE 的凭据作用域和签名。
- 实际本地 TLS HTTP/1.1 与 HTTP/2 传输后验证 PUT/DELETE 签名。
- OCI 端点与 Region 不匹配时，在请求前给出明确错误。

这些本地验证本身不代表真实 OCI 通过。默认桶使用 SDK HTTP 客户端，新增桶使用
`bucket.PublicHTTPClient()`，后者绕过代理、逐次解析并检查公网 IP，保留 HTTPS 主机名校验。
两者都使用同一 `storage.NewS3` 签名逻辑。未发现其必然导致线上 403 的证据。

## 不使用 FujiPic/SDK/本地加密密钥的对照

在发生问题的 Ubuntu 服务器运行：

```bash
python3 scripts/diagnose-oci.py
```

交互输入完全相同的端点、区域、桶名、Customer Secret Key 的 Access/Secret。
凭据隐藏输入，不写入文件、命令行或日志。脚本验证 TLS、不跟随重定向、不使用代理，
仅 GET 随机不存在的对象，不上传、不删除、不列举对象。

- `404 NoSuchKey`：签名与该次读取路径通过，但不能证明写/删权限正常。
- `403 SignatureDoesNotMatch`：脱离 FujiPic 和加密密钥后仍能复现，需要继续查凭据、命名空间、区域及 OCI 返回。
- `AccessDenied` / `NoSuchBucket`：不能当作认证全部通过的结论，继续核对桶与权限。
- 若独立脚本通过而 FujiPic 仍失败：保留两次请求的时间与 Request ID，继续比较请求方法、SDK payload/checksum、HTTP 协议及环境差异。

只分享 HTTP 状态、错误代码和 Request ID，不分享密钥或 Authorization 请求头。

## 2026-11-11 生产主机对照结果

在生产 Ubuntu 主机上，使用操作者当次提供的 Johannesburg 参数：

- 独立 Python GET 随机不存在对象：`404 NoSuchKey`。
- 实际 `storage.NewS3`，分别使用默认 HTTP 客户端和 `bucket.PublicHTTPClient()`：读取随机不存在对象均返回 `storage.ErrNotFound`。
- 使用新增桶 HTTP 客户端、启用 path-style：7 字节临时对象的 PUT、GET、内容核对、DELETE 全部成功。
- 禁用 path-style：GET 返回虚拟主机名的 TLS 证书不匹配，和先前的 OCI `SignatureDoesNotMatch` 不同。

这些对照先在主机临时诊断进程运行，随后补做了真实生产 Web 提交，结果如下。

随后，经操作者批准，通过生产管理页面原生表单提交同一组参数，明确启用 path-style，
页面返回“存储桶已添加并通过读写检查”。只读数据库核对确认桶已启用、优先级 100、
容量为 20,000,000,000 字节（20 GB），并存在加密 Secret。该次操作没有部署新代码、
重启服务或更换 `STORAGE_ENCRYPTION_KEY`。

因此本次真实 Web 添加已成功；此前失败请求的完整参数未留存，原先
`SignatureDoesNotMatch` 的具体成因仍无法追溯，不能将成功归因于某项未证实的修复。
OCI 侧暂时性状态或此前提交参数差异均未被证实，不应将此案例标记为已确认的 OCI 故障。
