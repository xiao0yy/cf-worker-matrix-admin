# Matrix Admin：实例部署与管理

这是服务器 Agent 的执行说明。使用与本文件同一 Release 的 Linux amd64
运行包，不克隆源码，不执行仓库中的脚本。服务器端所有工作由
`matrix-admin` 完成；同目录 `restic` 是校验过的备份依赖。

## 先检查，再安装

1. 默认根目录 `/opt/matrix`。先查看 `agent/runtime.json`、
   `deploy/control/local.json`、`deploy/control/state/journal.json`。
   有实例记录就保留其身份、epoch、数据和未完成操作；不得重新初始化。
2. Agent 自行检查 Linux amd64、systemd、Docker Engine 与 Compose 插件、
   nginx（含 auth_request 模块）、SQLite CLI，以及足够的磁盘空间。
   缺什么安装什么；不要因为找不到默认路径而重复安装。
   程序不安装系统包，也不需要 Go、Node、Git、Python、jq 或运行时 Shell 脚本。
3. 从固定的 GitHub Release 下载 `matrix-admin-linux-amd64.tar.gz` 和
   `SHA256SUMS`。首次可先解析 latest 得到固定 tag，随后全部从该 tag 下载。
   用系统 SHA256 工具校验附件，解压至私密临时目录，执行
   `./matrix-admin verify --from <解压目录>` 和 `./matrix-admin version`。
   后续已有程序可用 `matrix-admin download --version X.Y.Z --output <新目录>`。
   必须保留整个运行包；不能只复制程序而丢失 manifest 或 restic。
4. 唯一用户输入是 `/opt/matrix/config/s3.env`，权限 600。由
   `./matrix-admin config-example` 获取模板，用户填写 R2 endpoint、region
   (`auto`)、bucket、访问凭据和已有 `CLUSTER_ID`。不要新造 cluster ID，
   不索取 B2 凭据或 RESTIC_PASSWORD，它们从私密 R2 配置记录取得。

## 本机路径记录

默认 nginx 主配置 `/etc/nginx/nginx.conf`、HTTP include 目录
`/etc/nginx/conf.d`、systemd unit 目录 `/etc/systemd/system`。
如本机不同，Agent 先写 `/opt/matrix/agent/host.json`（权限 600），例如：

```json
{
  "schema": 1,
  "root": "/opt/matrix",
  "docker": "/usr/bin/docker",
  "sqlite": "/usr/bin/sqlite3",
  "nginx": "/usr/sbin/nginx",
  "systemctl": "/usr/bin/systemctl",
  "nginx_config": "/etc/nginx/nginx.conf",
  "nginx_directory": "/etc/nginx/conf.d",
  "nginx_service": "nginx",
  "systemd_directory": "/etc/systemd/system",
  "runtime_directory": "/run/matrix-control"
}
```

可以只写需要覆盖的字段；root 必须与 `--root` 相同。Controller runtime
固定为 `/run/matrix-control`。nginx_directory 必须已被主配置在 http 层引用。
程序用 argv 调用依赖，配置值不能写命令或 Shell 表达式。

## 新实例只准备 standby

在解压目录，以 root 运行：

```text
./matrix-admin doctor
./matrix-admin prepare --preflight
./matrix-admin prepare
./matrix-admin status
```

默认 `--root /opt/matrix`。prepare 只读取 R2 控制信息与配置胶囊，安装本版本
到 `releases/X.Y.Z`，保存随机实例 ID，准备 nginx 守卫、controller 和轮询任务。
**不会下载 B2 数据、启动业务容器或开启备份任务。** 不需要用户提供 SSH。

把实例 ID、主机显示名与管理界面中的实例对应起来。常规切换由用户在管理界面
显式发起；旧实例冻结写入并完成最终备份后，新实例才恢复、验证、接管。
`matrix-admin handover` 与 systemd 定时器使用相同逻辑。

## 日常操作

所有命令来自 `agent/runtime.json` 指向的同一个版本目录：

| 命令 | 行为 |
| --- | --- |
| `status` / `doctor` | 查询实例状态 / 检查依赖 |
| `backup` / `backup --full` | 当前授权 owner 备份；后者完整回读校验 |
| `restore` | 恢复最新已提交恢复点到隔离 generation，不上线 |
| `restore --point <完整快照ID> --sha256 <恢复点哈希>` | 恢复明确指定的不可变恢复点 |
| `fence --instance <ID>` | 关闭入口并停止写者 |
| `resume --instance <ID> --epoch <N>` | 在当前授权仍有效时显式恢复并开启备份 |
| `source-resume --operation <ID> --instance <ID> --epoch <N>` | 交接未释放前显式撤销冻结，恢复旧 owner |
| `upgrade --from <新运行包目录>` | 检查代码升级条件 |
| `upgrade --from <新运行包目录> --apply` | 更新本机运行程序与 units，保留身份、日志与数据 |

CLI 的 `control`、`mailbox` 是同一个程序的内部 systemd 入口。
不要从源码目录执行替代脚本。故障日志在 `agent/logs` 和
`deploy/control/state/{final,handover}`，可能包含敏感信息，只摘录必要错误。

## 代码升级

先验证完整新运行包。显式 fence，确认 blocked、不再 serving，且无进行中的
交接/最终备份，再用**新包里的程序**执行 upgrade --apply。
成功后读取 status，确认原实例 ID、epoch 与目录保留，再显式 resume。
中断时保留 `agent/pending-update.json`，用同一新版本重试；不要重建身份、
手改 journal 或回滚已经可能写入的数据。旧运行目录保留供诊断，units 只引用
一个生效版本。0.1.x 的旧脚本不再被新 units 调用。

## 数据与安全边界

- R2：控制状态、实例报告、配置胶囊、恢复点索引、B2 仓库凭据与密码。
- B2：restic 数据库快照；媒体仍由 Tuwunel 原生 S3 保存，不重复备份。
- 新程序直接读取明确支持的 recovery v2，不下载/执行快照生产者的旧程序。
- 日常是两个数据库各自一致的在线快照；切换必须先停止全部写者，最终备份
  完整回读并用隔离网络的容器验证数据库启动。
- STARTING 后的数据可能已写入，失败时只修复当前 generation，不能再次覆盖恢复。
- 当前实现只支持常规交接；强制接管和自动清理旧机数据未开放。旧机失联不等于
  已停止写入，不能手改 R2 owner 绕过隔离。保留旧数据供人工核验。
- 读 R2 私密命名空间即有恢复数据的能力，凭据不能写入日志或公开仓库。

若失败，请报告命令、退出码、操作 ID、阶段和脱敏错误；原样保留实例、
恢复点及工作目录，先定位原因再继续。不要以“重新跑全部恢复”代替故障修复。
