# Matrix R2 Admin

通过 Cloudflare Worker + 现有 R2 控制桶管理个人 Matrix 集群。前端是静态 HTML/JS，可直接在自己电脑上打开，或由 Worker 一起托管。电脑上无需 Python、SSH 或后台服务。VPS 实例通过自己的轮询器领取指令，调用本机现有控制器，再把结果写回 R2。

**本项目当前仅交付源码和测试，没有创建 Worker、改 DNS、安装轮询器或向生产 R2 提交管理指令。** 当前机器原有控制器和切换 worker 继续运行；新的 R2 管理指令通路需要在每个实例安装后才可用。

```text
本地浏览器 ──HTTPS + 管理令牌──> Worker ──binding──> 现有 R2 控制桶
                                                     ↑       ↓
                                              实例报告 / 指令回执
                                                     ↑       ↓
                                             各 VPS 的指令轮询器
                                                     ↓
                                             本机 Matrix 控制器
                                                     ↓
                                          停写 / 最终备份 / 恢复 / 上线
```

Worker 不改 owner、epoch、control.json 或实例数据，不执行备份/恢复。运行资格继续由现有控制器以 ETag 条件更新、停写证明和 epoch 检查控制。没有自动抢占或绕过验证的 force。

## 放入 Git，连接 Cloudflare 部署

将**这个目录的内容**作为仓库根目录，包含 `.gitignore`、`package-lock.json`、`worker/`、`web/`、`poller/` 等。没有初始化远端仓库，也没有代你提交或推送。

1. 编辑 `wrangler.jsonc`：
   - `name` 是 Worker 名称，默认 `matrix-admin`，与 Cloudflare 创建/连接的 Worker 名称一致。
   - `r2_buckets[0].bucket_name` 已填当前控制桶名称；确认是**已经存放 control.json 的那个 R2 bucket**，不要新建一个空桶。
   - `vars.CLUSTER_ID` 保持现有 `matrix-personal`。
2. 在 Cloudflare 的 Workers Git 集成中连接你的仓库。若仓库根目录就是本项目，构建根目录选 `/`；若保留为其他仓库的子目录，就选择该子目录。
3. 使用 Node.js 22+，自动依赖安装使用 `npm ci`，构建命令使用 `npm run build`，部署命令使用 `npm run deploy`。可把构建命令改为 `npm test && npm run build`，先跑 Worker 测试。
4. 在 Worker 的 **Settings → Variables and Secrets** 设置 Secret：
   - `ADMIN_TOKEN`：至少 32 字符的随机令牌。例如自己在可信环境运行 `openssl rand -hex 32` 生成。只放 Cloudflare Secret 和你电脑上的连接配置，不放 Git。
   - Worker 无需 R2 access key / secret key，使用 `CONTROL` binding。
5. 管理 Worker 使用独立的 workers.dev 地址或独立域名，不绑定到 Matrix 的业务 fallback；VPS 离线也能访问管理入口。首次访问 Worker 地址，填管理令牌，刷新状态。未配置令牌时 API 返回 503；没有/错误令牌时返回 401，不读取 R2 状态。

配置由仓库中的 Wrangler 文件管理，Git 部署会带上 `CONTROL` binding。Git 集成所需的平台部署权限由你在 Cloudflare 配置；本地界面不需要 Cloudflare 账号 API Token。

参考：[Workers Builds](https://developers.cloudflare.com/workers/ci-cd/builds/)、[R2 Workers API](https://developers.cloudflare.com/r2/api/workers/workers-api-reference/)、[Worker Secrets](https://developers.cloudflare.com/workers/configuration/secrets/)。

## 在自己的电脑使用

两种方式使用相同界面：

- 浏览器打开 Worker 地址。
- 下载仓库中的 `web/` 整个文件夹，双击 `web/index.html`，填 Worker URL 和令牌。无需 `npm install` 或本地 HTTP 服务。

也可在电脑上复制 `web/connection.example.json` 为自己的 `connection.json`：

```json
{
  "worker_url": "https://matrix-admin.YOUR-SUBDOMAIN.workers.dev",
  "admin_token": "你设置的随机管理令牌"
}
```

页面点击“导入本地连接配置”即可。配置只在浏览器解析，不上传文件；令牌只保存在当前页面内存中，不写 localStorage、不进入 URL 或诊断报告。`connection.json` 被 Git 忽略，不能放入 `web/` 后随静态资源发布；放在仓库以外的个人私密目录。

`ALLOWED_ORIGINS: "null"` 允许 `file://` 页面。Worker 自身来源也允许。若自行用 HTTPS 站点托管前端，可将其精确 origin 加到该变量（逗号分隔）。CORS 不授予权限，所有管理 API 仍要求 Bearer 管理令牌，不使用 Cookie。

状态自动刷新不触发任何管理动作，关闭页面后服务器继续执行。浏览器可能限制 file:// 下的 localStorage；此时原指令保留在当前页面内存和 R2 中，后续可从 R2 查询。

## 每个 VPS 安装指令轮询器

这一步是部署实例端代码，由 VPS 上的 Agent 完成；与电脑工具不需要 SSH 没有冲突。管理工具从不连接 VPS，VPS 不新增公网管理端口。

前提是 Agent 已按备份桶 README 准备好统一实例：`deploy/control/local.json`、控制器、host.env、现有 handover timer 均存在。这个项目不安装 Docker、nginx、Go 等系统依赖，由 Agent 根据本机情况准备。

在有 Go 1.23+ 的构建机器运行：

```sh
bash scripts/build-poller.sh
```

产物是 `bin/matrix-mailbox-linux-amd64` 和 `bin/SHA256SUMS`，Go 运行时静态链接，目标 VPS 无需安装 Go。可以把仓库及本次构建产物交给目标 VPS 的 Agent；也可以由 Agent 在 VPS 本地构建。当前二进制仅针对已有部署使用的 linux/amd64、GOAMD64=v1，无 AVX 要求。

在实例上先检查，再明确安装：

```sh
bash deploy/install-instance.sh --check
bash deploy/install-instance.sh --install
```

自定义基础路径使用 `MATRIX_FOUNDATION=/你的基础路径`。其他位置由已有 local.json、host.env 和当前 deployment_root 读取，不复用源实例身份。安装需要 root，默认只检查。脚本不安装系统软件，不访问 Cloudflare API，不修改控制 owner/epoch，不重启现有业务和控制器。

安装器会：

- 核验二进制 SHA256、既有控制器和备份脚本布局。
- 检查备份生产者、handover worker、旧 mailbox poller 的文件锁；忙时拒绝安装。
- 安装独立 `matrix-mailbox.timer` / `.service`，15 秒轮询。只使用既有 R2 私密配置，不新增用户凭据。
- 在**受支持的现有备份脚本布局**中插入两个明确标记的补充块：把轮询器和服务模板纳入备份；让新 standby 从已验证的 payload 安装相同轮询任务。
- 保留原控制器、原切换 worker 和所有实例身份；不下载数据库，不启动业务。

已处于切换中的实例不要升级。脚本只支持本仓库开发时的备份脚本结构，不匹配时停止，让 Agent 审阅集成，不能猜测修改。安装不是多文件事务，断电后需由 Agent重新检查；重复安装不会重建身份。不要在安装期间手动启动其他备份/切换操作。

安装后需等待一次正常备份完成发布，再让另一台 VPS 从该新备份准备。新备份才包含本轮询器及其安装钩子；较早的恢复点没有这条指令通路，界面会显示 capability 缺失并拒绝提交。

## 第一次验收和切换

1. 只安装当前实例轮询器，页面刷新应看到实例控制报告和 mailbox 报告均新鲜。
2. 先选择“检查指令通路”，输入“确认”提交；应依次看到 pending → CLAIMED → APPLIED。这个操作不改变服务，用于验证整条通路。
3. 第二台 VPS 由 Agent 从 S3 准备成 standby；安装/恢复相同轮询器后，会通过 R2 报告自动出现在列表中，不需要在电脑登记 IP、SSH 或实例清单。
4. 在 Cloudflare 手动将真实回源 A 记录 `ecs-5035.ermian.top` 指向目标 VPS。`matrix.wangjipeng.cn` 使用 SaaS fallback，不当作普通 A 记录改。
5. 选择目标实例、“正常切换”，填写唯一 operation_id，确认入口已切换后提交。
6. 观察源实例停写并完成最终备份，再释放归属。目标领取下一 epoch、恢复精确快照、校验和上线。
7. 完成判据是权威 `ONLINE`、owner=目标、epoch+1，目标 serving=true。APPLIED 仅表示 handover-request 已被控制器接受，不代表整个切换结束。旧机保持 RETIRED，不自动删除旧数据。

正常切换最终备份与日常在线备份不同。媒体仍在原生 S3，不增加媒体备份。R2 中的备份信息是实例同步的摘要，包含 snapshot、data_time 和实际验证等级；摘要过期不能解释为备份成功或失败，更不能替代 B2 精确恢复点。

## 人工干预与诊断

- **网络失败/HTTP 响应丢失**：先刷新，查询原 command_id。使用“重发上次原指令”时原样提交，包括 id、时间和期望状态，不生成新指令。
- **REJECTED**：未通过指令前置校验，例如过期、revision 已变化、数据 epoch 不符。修正后刷新、明确提交新 command_id。交接 operation_id 仍沿用原操作。
- **FAILED**：本机管理命令返回错误，不保证业务状态没变化，必须检查权威状态、实例状态和错误后再决定下一步。
- **UNKNOWN**：崩溃/中断后无法确定是否调用过本机命令。不自动重放，也不宣称回滚；查看状态后由人确认新指令。
- **CLAIMED**：正在/曾经执行。不能因为超时就覆盖或清空。轮询器下次检查本地持久记录后会将中断结果标为 UNKNOWN，或补发已保存的结果。
- **过期且从未认领**：界面可“关闭过期未认领指令”。Worker 以条件创建方式先抢占回执键，写入 REJECTED；如果 poller 已抢到 CLAIMED 则拒绝，避免清空与执行竞态。
- **恢复源实例**：释放之前可执行“中止最终备份并恢复源实例”。它调用原 final-backup.sh --resume 包装器，完成健康检查后恢复原备份定时器；worker 活跃或已释放时原控制器会拒绝。随后按实际状态取消目标请求、恢复入口。
- **继续目标实例**：STARTING/本代 ONLINE 后被停用，调用 handover-resume，保留源 operation_id/source_epoch，不覆盖已可能写入的数据。
- **旧实例故障**：强制接管未实现，按钮也没有提供。不会用心跳过期来自动抢 owner。需要后续实现可靠隔离后的灾难接管。

“导出诊断”只读取该集群 R2 状态、实例报告、poller 的 worker error.json 摘要及每实例最近按键排序的 10 条命令回执。不会读取密钥对象、消息、数据库、原始服务日志。报告保留实例 ID、阶段、时间、operation_id/command_id，便于交给维护 Agent；不包含 ADMIN_TOKEN 或配置凭据。

## 可选 Cloudflare DNS 操作

默认手动改 IP。启用时在 Cloudflare 中额外设置 Secrets：`CF_DNS_TOKEN`（仅所需 zone 的 DNS Read/Edit）、`CF_ZONE_ID`、`CF_RECORD_ID`、`CF_RECORD_NAME`。当前记录名应是 `ecs-5035.ermian.top`，以实际回源记录为准。

Worker 先核验配置记录确为 A 且名称匹配，再根据你填写的新 IPv4 修改 `content`，保留橙云、SSL 等其余配置。操作前写 R2 审计，API 错误不带出 Token。DNS 与 R2 不是原子事务，错误后先重新读取 DNS，不能假定未生效。界面仍需你确认入口切换，DNS API 不代替停写屏障。

## 开发验证

```sh
npm ci
npm test
npm run build
(cd poller && go test -v ./... && go vet ./...)
bash tests/integration.sh
```

Worker 单元测试使用内存 R2 模型，覆盖鉴权、CAS、并发提交、幂等、指令过期、取消与领取的竞态等；Go 测试覆盖落盘、回执丢失、未知结果不重放、epoch 和身份检查。安装集成测试只在临时目录渲染/校验脚本，不安装服务或写生产 R2。

尚未验收：真实 Cloudflare Workers runtime、浏览器端渲染/本地 file:// 兼容性、真实 R2 指令往返、两台 VPS 完整切换。构建检查和 mock 测试不能替代这些实测。代码不包含系统自动安装器、完整用户管理或强制灾难接管。
