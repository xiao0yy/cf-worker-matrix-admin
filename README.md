# Matrix Admin

为个人 Matrix 实例提供数据库备份、恢复和受控迁移。宁可暂停服务，也不允许
两个实例同时写入。服务器端是一个 Go 程序 `matrix-admin`；Cloudflare Worker
提供通过 Access 保护的管理界面，两者通过 R2 通信，无需 SSH 管理通道。

- Tuwunel：在线 RocksDB checkpoint，restic 去重上传 B2。
- Telegram 桥：SQLite backup API，包含已提交 WAL、登录状态和加密状态。
- R2：配置、恢复索引、实例状态与交接；媒体保留在原生 S3，不重复备份。
- 切换：停止写者、最终备份、完整回读和隔离启动校验，然后授权新实例上线。
- 失败：保存操作日志与数据，不以超时判定旧实例死亡，不自动覆盖已开始写入的数据。

服务器部署直接使用 [Release](https://github.com/xiao0yy/cf-worker-matrix-admin/releases)
附件，按 [Agent 操作说明](docs/AGENT.md) 执行。用户只填写
`/opt/matrix/config/s3.env` 的 R2 连接和已有 CLUSTER_ID；系统依赖由 Agent 检查安装。
准备 standby 不下载数据库，也不启动 Matrix/Telegram。

```text
matrix-admin doctor
matrix-admin prepare --preflight
matrix-admin prepare
matrix-admin status
```

运行包同时包含程序、固定版本 restic 和校验清单。版本由 Git tag 注入，
没有源码部署路线，没有额外的 matrix-control/matrix-release 二进制，
也没有随快照复制的恢复脚本。现有 recovery v2 数据由新版直接读取。

| 文档 | 内容 |
| --- | --- |
| [AGENT.md](docs/AGENT.md) | 新机器准备、操作、升级与故障处理 |
| [架构](docs/architecture.md) | 程序边界、持久化与状态流转 |
| [协议](PROTOCOL.md) | R2 控制和命令协议 |
| [Worker 部署](docs/worker.md) | Cloudflare Git 构建、R2 绑定、Access |
| [发版](docs/releases.md) | 单一版本来源、GitHub Actions 与附件 |
| [Telegram 镜像](docs/telegram-image.md) | 上游镜像镜像分发 |

当前支持 Linux amd64、systemd、Docker Compose 插件、nginx auth_request、
SQLite CLI。强制接管和自动删除旧实例数据尚未开放。

开发环境使用 Go 与 Node（仅 Worker）：

```sh
npm ci
npm test
npm run build
npm run check:source
go test ./...
go vet ./...
```

真实 SQLite 测试需要 `sqlite3`；设置 `RESTIC_TEST_BINARY` 为固定版本 restic
路径可运行真实仓库恢复测试。CI 会强制执行这些测试。测试使用临时文件、
本地仓库和模拟控制存储，不访问生产账号。
