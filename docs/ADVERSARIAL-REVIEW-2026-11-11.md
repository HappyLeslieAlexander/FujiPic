# 对抗性审查与修复（2026-11-11）

## 范围

本轮阅读了 `cmd/`、`internal/` 的应用代码及测试、SQL 迁移、嵌入代码、
前端 JS/CSS/模板、仓库脚本、部署与 CI 配置。以当前工作区为基础审查，
保留进入任务时已有的默认桶优先级修改及 `0006` 迁移。
不包含第三方依赖的完整源码审计。

## 已修复问题

| 问题 | 触发与影响 | 修复与回归测试 |
| --- | --- | --- |
| PUT 失败后遗留对象 | 存储端已提交，但响应丢失或客户端取消；原实现只归还配额 | 写入失败后先持久化清理任务，再尝试删除；`TestAmbiguousPutIsCleanedOrQueued` 覆盖取消、成功清理与删除失败 |
| 删除与 CDN 刷新不原子 | 元数据删除后进程退出，或刷新队列写入失败；缓存失效任务可能永久丢失 | `SoftDeleteImageWithPurge` 在同一 SQLite 事务中提交删除和刷新任务；`TestDeletePurgeQueueFailureRollsBack` 注入队列失败并验证回滚 |
| 自定义 pragma 跳过必要设置 | DSN 包含任意 `_pragma` 时，WAL、busy timeout、外键初始化被整体跳过；迁移后的新连接可关闭外键 | 每个连接都应用必要 pragma；`TestCustomPragmaPreservesRequiredPragmas` 强制替换连接验证 |
| SQL 迁移分割损坏合法语法 | 方括号标识符丢失 `[`；`BEGIN/*注释*/` 未正确结束关键词扫描 | 保留括号并在注释前结束关键词；`TestMigrationBracketIdentifiersAndAdjacentComments` 实际执行含触发器的迁移脚本 |
| 失败的活动写入污染节流状态 | 已取消请求的 session/token 写入失败，却记录“已更新”，阻止立即重试；短会话可能提前失效 | 成功写入后才记录节流时间；`TestFailedActivityWriteCanRetryImmediately` |
| Range 与缓存条件顺序错误 | 匹配 ETag/日期的越界 Range 返回 416；普通 Range 的 304 携带部分内容的 Content-Range | 对完整对象先判断条件，再处理 Range；`TestImageConditionsPrecedeRange` 覆盖两种验证器及两种范围 |
| 数值下溢导致大整数资源消耗 | `ParseFloat` 可将极大负指数静默解析为 0，后续 `big.Rat` 仍按巨大指数分配 | 在大整数解析前限制输入长度和指数；`TestCapacityRejectsExcessivePrecisionAndUnderflowExponents` |
| 桶容量修改存在检查/写入竞态 | 管理员读取用量后上传又预留空间，旧快照仍允许保存过小容量 | SQL UPDATE 原子检查当前 used_bytes；`TestBucketCapacityUpdateRejectsStaleUsage` 模拟确定性交错 |
| 不限容量桶的计数溢出 | `max_bytes=0` 跳过上限比较，MaxInt64 后继续预留使 SQLite 转成 REAL，Go 无法读取 | 即使业务容量不限，也限制计数的 int64 表示范围；`TestUnlimitedBucketCounterCannotOverflow` |
| 桶探针误报可用 | GET 只检查打开/关闭，不读取内容；删除错误被忽略，坏数据或无删除权限的桶仍被接纳 | 有界读取并核对探针内容，传播删除错误；`TestProbeVerifiesContentsAndDeletePermission` |
| 无 JS 上传回退不可操作 | HTML 文件输入永久 hidden，禁用 JS 后无法选择文件 | HTML 默认显示输入，仅由 JS 增强时隐藏；`TestDashboardHasUsableNoScriptPicker`，并运行现有 Chromium 回归 |

首批六个测试函数及后续探针/容量两个边界测试组均在修改对应实现前执行，
观察到预期失败；修复后全部通过。数值输入和无 JS 回退补充了回归检查。

## 本轮验证

通过：

```text
go build ./...
go test ./...
go test -race ./...
go vet ./...
go mod verify
python3 scripts/test-browser.py
```

普通测试与竞态测试覆盖所有 Go 包；Chromium 回归验证真实 `app.js` 的中文/emoji
文件名、空 MIME、上传队列和去重，并检查 JS 增强隐藏文件输入。
存储异常使用本地故障替身，未执行真实 S3/OCI、CDN 或生产 Docker/Compose 验收。
测试通过不表示不存在其他缺陷，也不等同于所有进程崩溃时序均已验证。
