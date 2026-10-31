# 管理指令协议 1

原 `cluster/<cluster>/control.json` 保持原结构与原写入者。新管理协议从旁路提交意图，不创建第二份运行授权。

| R2 key | 写入者 | 用途 |
|---|---|---|
| `cluster/C/control.json` | 原控制器 | 唯一权威 owner/epoch/phase，原 ETag CAS 协议 |
| `cluster/C/instances/I.json` | 原控制器 | 运行观测，不是授权 |
| `cluster/C/instance-info/I.json` | 授权 Worker | 名称和人工登记公网 IPv4，ETag CAS；仅用于识别，不是运行授权 |
| `cluster/C/agents/I.json` | 实例 mailbox poller | 能力、轮询时间、备份与 worker 错误摘要 |
| `cluster/C/commands/I/requests/ID.json` | 授权 Worker | 不可变指令归档，条件创建；同 ID 不同内容拒绝 |
| `cluster/C/commands/I/pending.json` | 授权 Worker | 每实例一个待处理槽，ETag CAS；已有未完成指令不能覆盖 |
| `cluster/C/commands/I/results/ID.json` | poller；或过期未认领关闭入口 | 先条件创建 CLAIMED，再由 poller 条件更新最终回执 |
| `cluster/C/audit/*` | 授权 Worker | DNS 尝试和过期未认领关闭操作的审计 |

## 指令

schema/id/cluster_id/target/action/created_at/expires_at/expected/bound_epoch/operation_id/source_epoch 为固定字段。expected 固定 revision/epoch/owner/phase，必须与读取的权威状态相符。TTL 最大 5 分钟，未来时钟容差 30 秒。

允许 probe、handover-request、handover-resume、handover-cancel、fence-local、resume-local、source-resume。没有 shell、文件路径、URL、控制器内部 claim/release/bind 等参数入口，不接受任意 S3 key 或 JSON 覆盖。

正常交接 operation_id 与单次管理 command_id 分开。操作中的所有管理命令必须沿用相同 operation_id/source_epoch；新的确认指令使用新的 command_id。丢失 HTTP 回应则重发完整相同指令，不修改其中时间和状态。

## 状态流转

```text
Worker 验证 Access JWT（签名、团队 issuer、应用 AUD、有效期和用户身份）、实例身份、新鲜观测和精确权威状态
  → requests 不可变归档
  → pending CAS 提交
  → 实例读取、验证固定字段/身份/时间/期望状态/绑定 epoch/操作 ID
      ├─ 前置条件不符 → REJECTED
      └─ 本地 fsync CLAIMED → R2 条件创建 CLAIMED
          → 再读权威状态、再检查有效期
          → 调用固定的本机管理命令（原控制器继续做 ETag 和停写验证）
          → 本地 fsync APPLIED / FAILED → R2 条件更新回执
```

本地 CLAIMED 后崩溃，无论是否已经实际执行，都不自动重放，写 UNKNOWN。R2 已 CLAIMED 而本地结果丢失也写 UNKNOWN。本地最终回执存在则补发/核对远端，不再调用本机命令。R2 已终结的不同状态不能覆盖。回执包含原始指令字节 SHA256，避免同 ID 被换内容。

APPLIED 表示管理命令已返回，不表示业务恢复结束。服务切换的最终状态依然来自 control.json 及对应实例报告。FAILED 不等于没有副作用；UNKNOWN 不自动回滚。工具不宣称 exactly-once，只保证不因自动重试重复派发存在歧义的指令。

仅缺少执行回执且指令已过期，才允许人工关闭。Worker 首先以 If-None-Match 条件创建该回执为 REJECTED。与 poller 的 CLAIMED 竞争同一键，只能有一个成功；poller 未得到 CLAIMED 就不能调用本机命令。TTL 过期本身不会解除 pending 占用。

R2 不承担过程调度：源和目标原 handover worker 独立工作。本机 mailbox timer 与 handover timer 分开，所以长时间备份/恢复不会阻止领取人工停用指令。source-resume 包装器可能运行数分钟，其间该实例 mailbox 观测可暂时过期。

## 故障边界

- 无法访问 R2：不领取/执行新指令，原控制器按原新鲜度窗口关入口和停用服务。
- R2 接受指令但 Worker 响应丢失：按原 ID 查询/重发，归档与 pending 回读幂等。
- 写了归档，pending CAS 失败：没有提交成功；后续重试会实际争取槽位，不因归档存在误报成功。
- 两个浏览器并发提交：pending 条件写入只有一个成功。孤立归档不被 poller 执行。
- 动作进行中权威状态变化：执行前再次读取；原控制器再校验自身条件，不允许由消息队列直接改运行授权。
- Worker 返回成功后无人领取：保留 pending，可观察 agent reported_at；不能解释为实例已经停止。
- 回执上传失败：本地持久结果在下次轮询补发；不重复执行。
- 本地磁盘丢失：远端 CLAIMED 阻止重复执行，并标 UNKNOWN；新的实例仍须正常恢复身份，不能复用旧身份取得归属。
- Git 升级：协议字段/schema 不兼容时拒绝指令；poller capability 版本必须匹配 Worker 支持版本。静态资源不含 token。

当前协议不实现强制接管、自动清理、跨 R2/DNS 事务、集群软件自动升级或基于心跳的选主。
