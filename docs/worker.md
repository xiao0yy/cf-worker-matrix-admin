# Cloudflare Worker 管理控制台

浏览器 → HTTPS + Cloudflare Access → Worker → R2 指令/报告 → VPS mailbox → 本机控制器。
电脑不需要 SSH、Python 或后台进程。Worker 不读 B2 私密数据、不执行恢复、不直接修改 owner/epoch。

## 使用界面

- **总览**：先看当前服务实例、最近备份的数据时间与校验范围，再查看机器和最近指令。实例名称可点击展开详情侧栏，稳定 ID、完整报告和名称/IP 编辑放在侧栏中。
- **切换实例**：选择备用机器，核对源与目标，调整 Cloudflare 回源 IP，再输入确认。目标报告过期、已有未结束指令、源实例未就绪时不能提交；确认过程中归属或目标 IP 变化，需要返回重新核对。
- **运维工具**：先选择实例，再检查通路、继续/取消原切换或恢复/停用服务。仅展示已实现的能力；DNS 未配置时提示手动调整，已被认领的指令不能按过期清除。

顶部导航在各页面间保持位置，手机端分两行显示。读操作在详情侧栏完成，服务干预使用独立确认窗口；检查指令通路可直接发起只读探测。指令反馈固定在视口内，重试、关闭过期指令及 DNS 修改都使用统一的确认交互。设计规范与组件令牌见 `DESIGN.md`。

自动刷新不会触发操作。状态读取失败时保留上次观测，并明确标记待确认、暂停新操作。提交结果未知时可以重发同一条原指令，不能静默生成新编号。`APPLIED` 只代表指令处理完成；切换成功还需匹配完成记录、归属、数据代数和新实例的有效服务报告。

### 交接状态与动效

切换开始后，页面固定展示「源实例 → 远端备份 → 目标实例」。归属标记单独移动，表示哪个实例持有服务归属；数据流只表示当前处于备份或恢复阶段，不表示字节数、百分比或预计完成时间。页面不会按时间自动推进真实交接。

| 界面阶段 | 实际状态 | 可视反馈 |
| --- | --- | --- |
| 请求交接 | `REQUESTED` | 源实例等待确认，目标等待接管 |
| 停止写入 | `QUIESCING` | 源实例停写，目标保持等待 |
| 最终备份 | `QUIESCED`、`SNAPSHOTTING` | 数据冻结；开始备份后显示源实例到存储的数据流 |
| 释放归属 | `QUIESCED` + operation `READY`、`RELEASED` | 备份就绪；只有 `RELEASED` 才将归属标记移到无持有者位置 |
| 恢复与校验 | `RESTORING`、`VALIDATING` | 归属移至目标，存储到目标显示恢复数据流，入口仍关闭 |
| 启动服务 | `STARTING` | 停止传输动效，目标等待健康检查 |
| 完成切换 | `ONLINE` + 有效目标服务报告 | 显示完成标记；仅有归属记录时显示等待上线确认 |

当前后端的 `VALIDATING` 包含下载、恢复和隔离校验，不细分为已完成的恢复步骤。错误或关键实例报告过期时，保留最后阶段并停止流动；不依据旧实例失联认定其已停止。释放后只要求目标的有效报告，旧实例离线不会遮住目标恢复进度。

「预览切换过程」使用明确标记的示例数据，可播放完整流程或选择失败、报告过期等状态，不提交任何管理指令。真实视图保留 DOM，轮询不重启动画；隐藏页面、离开视口或手动暂停会停止持续动效，并支持系统减少动态效果设置。旧实例数据的清理不属于完成动画，不会由前端触发。

备份摘要是实例上报的最近完成记录，不是整个仓库的历史或实时备份任务监控。数据库文件校验、完整恢复校验和启动校验分别展示，不把文件校验当成恢复演练。媒体是否包含以记录为准，未打包的媒体继续使用原 S3 存储。

前端使用原生 HTML/CSS/JavaScript，无第三方 CDN 请求或运行时 UI 依赖。标题使用同源托管的 Noto Sans SC 字形子集，许可、来源与校验值随仓库保存，正文沿用系统字体。`npm test` 使用实际页面 DOM 验证向导、确认、重试和过期状态，测试数据使用文档保留 IP；这些测试不连接生产 API。

## Git 部署

仓库的 `wrangler.jsonc` 只保存通用配置和 Worker 名称。实例 IP、主机名、回源域名、Access 标识和存储桶名称均不进 Git。

1. 先在 Cloudflare 创建/确认目标 Worker，在 **Bindings** 中把已有控制桶绑定为 `CONTROL`。已有实例必须使用同一个桶，不要另建空桶。首次连接 Git 前需要已有 Worker 版本及此绑定。
2. 在 Worker 的运行时 **Settings → Variables and Secrets** 中设置下表三项。Access 应用及仅允许本人身份的策略也在 Cloudflare 管理。无需构建变量、私有 Wrangler 文件或配置生成脚本。
3. 连接 Git：根目录 `/`、Node.js 22+、构建命令 `npm test && npm run build`、部署命令 **`npx wrangler deploy`**。关闭预览构建；开发/预览使用另一 Worker，不能向生产 Worker 上传未部署的版本。
4. 开启整个 Worker 的 Access 保护（包含 `/api/*`）。Allow 只放自己的完整邮箱/身份，不使用 Everyone 或 Bypass。部署后检查绑定、Access 登录及 `/api/status`。

| 运行变量 | 内容 |
| --- | --- |
| `CLUSTER_ID` | 已有集群 ID，与实例配置一致 |
| `TEAM_DOMAIN` | Access 团队 URL，格式 `https://YOUR-TEAM.cloudflareaccess.com` |
| `POLICY_AUD` | 保护此 Worker 的 Access 应用 AUD |

公开 `wrangler.jsonc` 只声明 `CONTROL` 的 `type: inherit` 绑定，Cloudflare 上传 API 从 Worker 最新已上传版本继承实际绑定；没有这个绑定时部署失败，不自动创建空桶。`keep_vars: true` 保留控制台管理的运行变量，Secret 也会保留。因此 bucket 名、团队域名和 AUD 不进入源码或构建输入。`inherit` 属于 Wrangler 的 `unsafe.bindings` API 透传能力，本项目固定 Wrangler 版本；升级时必须复查部署 dry-run 和绑定继承契约。继承目标是**最新上传版本**，不一定是当前生产版本，所以生产 Worker 必须采用单一部署通路。

本地开发使用 `wrangler.dev.jsonc` 的独立本地 R2 存储；复制 `.dev.vars.example` 到 `.dev.vars`、权限 600 后填写开发用身份配置，运行 `npm run dev`。它不会访问生产 R2。生产部署可以直接 `npx wrangler deploy`；`npm run deploy` 只是相同命令的别名。部署日志可能出现现有绑定标识，不要公开原始日志。

依据：[Workers API 绑定](https://developers.cloudflare.com/api/resources/workers/subresources/scripts/methods/update/)、[Wrangler 配置](https://developers.cloudflare.com/workers/wrangler/configuration/)、[Workers Builds 配置](https://developers.cloudflare.com/workers/ci-cd/builds/configuration/)。Builds 不执行 Wrangler 的自定义 build 命令，检查必须放在 Builds 的构建命令中。

不使用 ADMIN_TOKEN。API 校验 Access JWT 签名、固定团队 issuer、AUD、有效期和用户身份；不信任单独邮箱头。缺配置返回 503，无身份返回 401。前端与 API 同源，不支持 file:// 跨域管理。

## 实例端

控制器、handover 和 mailbox 已统一纳入公开运行包，由 standby 准备和代码更新流程安装。没有额外的旧脚本插入/补丁安装步骤。实例只向 R2 轮询，不新增公网管理端口。

界面显示名称、公网 IPv4 与稳定实例 ID。人工名称/IP 保存在 instance-info，条件写入；也上报 hostname 和活动网卡地址。网卡 IP 可能是内网地址，不作为公网 IP 或运行许可。更名不更换实例身份。

## 切换与人工处理

先检查指令通路，再手动切换实际回源 IP，然后选择目标实例提交正常交接。若业务域名通过 SaaS fallback 回源，应修改实际 origin 记录。DNS 不代替源端停写。

- APPLIED：管理命令调用完成，不等于交接已上线。以权威 ONLINE、目标 owner/epoch 和 serving 为准。
- REJECTED：过期或期望状态不符。刷新后明确提交新 command_id。
- FAILED：调用错误，可能已有部分效果。先核对状态。
- UNKNOWN：中断后不能确定是否执行过，不自动重放。检查后人工决定。
- 响应丢失：查询/重发原 command_id 和原完整内容，不生成另一个相同动作。
- 已过期且未领取：可关闭未认领指令；通过 CAS 与 poller 领取竞争，不覆盖已 CLAIMED 指令。
- 释放之前可明确恢复源实例；源端 worker 活跃或已释放会拒绝。
- STARTING 后只能按原 operation 继续目标，不覆盖已可能写入的数据。
- 没有强制灾难接管按钮，不根据心跳过期自动抢 owner。

诊断导出只含集群状态、实例报告、错误摘要与最近指令回执，不读取消息、数据库、配置密钥、Cookie 或原始服务日志。

## 可选 DNS 操作

默认手动调整。若启用 Worker DNS 操作，在 Worker 的运行时 **Variables and Secrets** 中另设 CF_DNS_TOKEN（Secret，对应 zone 的 DNS Read/Edit）、CF_ZONE_ID、CF_RECORD_ID、CF_RECORD_NAME；不要把它们写入源码。`keep_vars` 保留这些控制台设置，不需要把这些值提供给构建环境。Worker 验证 A 记录名称后只改 content，保留其他配置。失败先重新读 DNS，因为 DNS/R2 不是原子事务。

Worker 构建与测试不会部署或修改账号。真实 Access、R2 通路、DNS 和双 VPS 交接仍需现场验证。
