# Matrix Admin：实例 Agent 操作契约

用户只填写 `/opt/matrix/config/s3.env`，格式见同目录 `s3.env.example`，权限 600。不要索要 RESTIC_PASSWORD，不打印凭据，不通过 SSH 从旧服务器复制东西。

代码来自公开项目 Release，数据库来自 B2，配置、恢复索引与运行资格来自 R2。三者不可互相替代。下载的数据不能携带或覆盖工具代码。

## 1. 识别本机

默认基础目录 `/opt/matrix`，本机也是普通实例。

| 路径 | 含义 |
| --- | --- |
| config/s3.env | 用户唯一私密输入：R2 连接和 CLUSTER_ID |
| agent/registry.json | 本机路径登记，与实际文件交叉核对 |
| agent/state.json | 依赖/服务探测结果，仅观察值 |
| agent/local.env | Agent 配置的本机路径覆盖，600 |
| agent/downloads/、work/、logs/ | 公开下载、恢复工作目录、私密日志 |
| deploy/release.json、release-manifest.json | 工具来源、版本及完整校验清单 |
| deploy/control/local.json、state/journal.json | 稳定身份、本地防回滚与运行约束 |
| deploy/control/host.env | 已确认的 Docker/nginx/systemd 路径 |
| deploy/control/s3.env | 用户 R2 输入的本机副本，不从源端配置包恢复 |
| generations/ | 隔离恢复与后续业务数据代 |
| agent/pending-update.json | 未完成代码更新，只能重试固定版本 |

已有 host.env 优先于重新猜测路径。先运行同版本 `backup/inspect-host.sh`，读取登记、实时控制状态和依赖结果。已安装但故障的服务应修复，不能当成未安装。已有身份/日志禁止删除重建。不同 VPS 使用不同身份。

## 2. 取得公开工具

从用户指定的可信项目仓库取得源码，审核对应版本 VERSION、release.config.json 和本文。不要从私有数据桶下载脚本执行。

在源码目录执行：

```sh
bash scripts/bootstrap.sh --output /opt/matrix/agent/downloads/runtime
```

输出目录必须不存在。已有下载先验证，不反复覆盖。地址由项目配置决定，可使用项目镜像；R2 point 不能提供任意代码下载 URL。

bootstrap 经 HTTPS 下载发布方的校验清单、描述和独立校验器；核对后下载完整包，校验大小、权限、文件清单和哈希，再原子提交目录。校验值与资产属于同一发布信任域，不代替对发布方的信任。

## 3. 依赖与输入

检查并按需准备 Linux amd64、systemd、Docker/Compose、nginx、curl（支持 aws-sigv4）、jq、sqlite3、Bash、GNU coreutils/findutils、util-linux/flock、gzip。运行包不要求 Go/Node/Python。

由 Agent 决定依赖安装。自定义可执行文件或 nginx 位置，按 local.env.example 写入 agent/local.env，600。先检查已有 vhost 和网络，不覆盖无关业务。准备 agent/ 和空的 generations/。安装成功不是健康证明。

R2 输入填写 endpoint、region=auto、bucket、CLUSTER_ID、access key ID、secret access key。仅使用认证的 R2 S3 endpoint。B2 连接及 restic 密码从该集群的 R2 repository record 获取；用户不用填写。媒体配置来自同一恢复点的 R2 配置包。R2 凭据始终取本机当前输入，不能从历史配置还原；更换后同步本机 deploy/control/s3.env，并核对两份输入。B2 或媒体凭据轮换后发布并验证新的恢复点。

## 4. 准备 standby

```sh
bash /opt/matrix/agent/downloads/runtime/backup/inspect-host.sh
bash /opt/matrix/agent/downloads/runtime/backup/recover.sh --prepare-standby --preflight
bash /opt/matrix/agent/downloads/runtime/backup/recover.sh --prepare-standby
```

工具版本不同时，先下载并验证恢复点固定的公开版本，由该版本继续。只从 R2 获取固定哈希的配置包；**不访问 B2、不下载数据库、媒体或应用镜像**。配置包不可包含可执行工具或旧实例的 R2 凭据。

核对新 instance_id、已有 cluster、blocked=true、serving=false、bound_epoch=0；控制器、handover/mailbox 轮询正常；没有启动业务或常规备份 timer。不要生成运行许可或修改 DNS。在管理页登记名称/IP，或通过上报的 hostname 识别实例。

重复执行返回 already_prepared 并保留身份。断电留下不完整基础目录时脚本停止；按目录约定与日志修复缺失步骤，禁止删目录从头恢复来规避状态检查。

## 5. 切换

用户切换 Cloudflare 回源并提交交接后，源端停写、干净停止 Tuwunel 和桥，完成最终备份、完整恢复及隔离数据库启动验证，再通过 R2 释放。目标领取下一 epoch，恢复指定 snapshot/hash，验证后上线。基础 guard 固定，数据代不等于实例身份。

软件不同返回 UPDATE_REQUIRED：在取得运行资格前，用同一固定点执行：
`recover.sh --update-foundation --point <完整ID> --point-sha256 <已核对哈希>`。
本机路径来自 registry/host.env。

STARTING 表示数据代可能已接受写入，此后禁止覆盖恢复、重建数据代、改 owner 或回退数据库。保留现场，读控制器和操作日志。退出的旧实例保留旧数据，不自动清理。

## 6. 升级

主动代码升级使用目标公开 Release 的 `backup/upgrade.sh --release <已验证包>`，先检查，再显式 --apply。见项目 docs/releases.md。只更新代码，不恢复数据库、不替换身份/凭据；下一次备份记录新版本。

控制器本体变更需要先受控停机并 fence，脚本拒绝在线替换。中断后读取 pending-update.json，只重试相同版本；不要删 pending 或换恢复点掩盖问题。

不要在准备、恢复或代码更新中自动升级 Tuwunel/Telegram 数据库版本。应用升级单独备份、锁定镜像 digest、验证数据迁移，再发布新恢复点。

## 7. 故障处理

认证、下载、校验、协议失败：停止并保留私密日志，原数据不动。依赖对象发布失败不推进 latest；latest 使用 ETag 条件更新，冲突时保留原成功记录并停止。最终备份失败后源端保持冻结，人工选择继续同一操作或明确恢复源端。未知版本拒绝执行，先读新版本文档。

报告实例 ID、owner/epoch/phase、操作 ID、步骤、错误摘要、snapshot/hash 和日志路径；不贴密钥、Telegram session、Matrix token 或完整配置。

目前没有强制灾难接管或 R2 权威重建。旧机失联时不能根据 DNS 或历史备份自行取得运行资格，向用户说明这一限制。
