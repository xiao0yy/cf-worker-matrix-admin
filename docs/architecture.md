# 架构与不变量

本项目提供单写者的计划交接，不是多主数据库复制。可以停机，优先保留数据和排除并发写入。

## 三类状态

1. **公开软件**：Git 提交对应一个不可变 Release。所有实例使用同一包布局，包含备份生产者、恢复适配器、控制器、mailbox、服务模板。不是把某台机器的 deploy 目录打包。
2. **私密数据**：B2 的 restic 数据库快照，R2 的配置包和 point。point 固定公开工具的版本/manifest SHA、容器 registry digest/image ID、服务身份、一致性和 source epoch。
3. **运行权威**：R2 control.json 的 owner/epoch/revision/phase，使用条件更新。本地 journal 持久化最高观察值、绑定 epoch、阻断和是否可能写入。UI 报告、DNS、备份和 Agent state.json 都不是运行许可。

用户在恢复机上只填写 R2 连接与 cluster ID。R2 point 固定配置包与数据库仓库记录的哈希；后者包含 B2 连接和 restic 密码。standby 不读取该仓库记录、不访问 B2。实例 R2 凭据来自本机输入，永远不被历史配置覆盖。B2/媒体凭据轮换后重新发布配置包。媒体继续使用原生 S3，恢复不下载媒体正文。

## 计划交接

| 阶段 | 执行动作 | 失败后的约束 |
| --- | --- | --- |
| ONLINE | owner 服务，定期 checkpoint/SQLite 备份 | 非 owner 保持 fenced |
| REQUESTED | 目标提交 intent，旧 owner 仍拥有当前 epoch | 超时不授权目标 |
| QUIESCING / QUIESCED | 关闭业务入口，停止相关服务并核验干净停写 | 禁止靠 DNS 推断停写完成 |
| SNAPSHOTTING | 最终备份、完整恢复校验、隔离数据库启动检查 | 失败保留源数据并保持冻结 |
| RELEASED | CAS 释放并固定最终 snapshot/hash | 目标先检查工具版本，再领取 next epoch |
| RESTORING / VALIDATING | 新数据代、固定版本恢复、隔离校验、绑定 canonical mounts | 没有写入前可按同一 operation 重试 |
| STARTING | 持久化可能写入标记，再启动固定容器 | 禁止覆盖恢复、重建数据代或回退旧数据 |
| ONLINE | 验证健康并提交目标 owner/next epoch | 旧源保持 RETIRED，保留其数据 |

上线和数据代绑定仍由控制器完成。脚本处理数据，Worker 提交有限指令，不直接写控制归属。管理命令 APPLIED 表示本机调用完成，不代表整个交接结束。

源端最终备份必须完整验证，日常在线备份允许两个数据库分别一致，并明确记录这种差异。restic 对固定 SST/文件块进行去重上传；它不是逐条事务复制，也不是零数据丢失的异步数据库副本。

## 版本与恢复闭环

```text
Git commit → 公共 Release → 实例工具版本
                                  ↓
                  checkpoint + SQLite + 私密配置
                                  ↓
            R2 point（配置包/hash + B2 快照 + 软件/hash + 镜像 digest）
                                  ↓
新 VPS：公开入口 → 固定工具版本 → 少量配置 → standby
                                  ↓
R2 交接许可 → R2 固定配置 + B2 固定数据 → 隔离验证 → 新数据代上线
                                  ↓
               同一个公开版本继续生产下一份备份
```

公开工具保存在基础 deploy 和活动 generation/deploy；更新必须覆盖两处，否则迁移后生产者会退回旧版本。版本不匹配不能靠修改 S3 标签绕过。

主动升级不依赖“先有一份新版本备份”：先验证新 Release，再执行代码更新，下一次备份引用该 Release。恢复按旧 point 仍使用旧工具，不自动升级数据库。

## 更新失败

更新锁定 recovery/handover/mailbox/producer，先写 pending-update，再替换公开清单中的文件，最后验证并记录 software.json。身份、凭据、journal、数据库不属于更新清单。

基础控制器本体或其服务单元发生变化时，只有已 fenced 的安全阶段可更新。在线代码更新不重启控制器；其 ExecStopPost 会停业务，因此不能把重启当作普通文件刷新。

更新中断不宣称自动回滚。只重试 pending 记录的同一版本和身份，修复未完成的代码替换。STARTING 或可能写入的阶段拒绝更新/恢复覆盖。新 standby 安装中断则保留目录，Agent 依据本地状态修复，不重建身份。

## 明确边界

支持一个 Tuwunel、一个 mautrix-telegram SQLite、现有 nginx 配置与固定应用网络的 Linux amd64 布局；不是通用 Synapse/任意桥安装器。

没有自动抢占、强制灾难接管、R2 权威重建、自动删除旧数据或自动 prune。公开 Release、镜像和私密恢复点必须一起保留。双 VPS 真实切换需要独立验收，替身测试不能证明公网网络和真实应用登录正常。

## 恢复点提交

1. 同一暂存目录记录数据库一致性、配置和完整 SHA-256 清单。
2. restic 仅上传 RocksDB 文件和 Telegram SQLite 主库。配置包包含清单及配置字节，不含数据库正文。
3. 数据库按完整恢复或日常文件树检查验证；交接必须完整恢复并隔离启动验证。
4. 将仓库记录、配置包和 point 条件写入 R2（If-None-Match），读回比较。已有同名对象只有内容完全一致才能复用。
5. 日常发布重新核对实例/epoch，并以发布开始时读取的 ETag 更新 latest；竞争失败不会覆盖另一个发布者。备份索引不授予运行权，跨 control/latest 对象没有事务。
6. 最终交接不修改 latest；以 R2 operation 引用的 point/hash 为准。源未释放前目标不能恢复上线。

失败的暂存数据保留；部分上传不会成为有效的已提交恢复点。本地 last-success 仅在 R2 提交确认后推进。所有仍被 point 引用的配置、仓库记录、restic 快照、Release 和镜像都要保留，不自动清理。
