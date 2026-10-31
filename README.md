# Matrix Admin

个人 Matrix 服务的备份、恢复、实例切换和管理控制台。适配 **Tuwunel / RocksDB + mautrix-telegram / SQLite + 原生 S3 媒体**，以保留数据和单写者为优先，允许切换期间停机。

仓库统一维护服务器端工具与 Cloudflare Worker。工具通过版本化 Release 分发，私密数据留在对象存储，浏览器通过 Access 登录管理页面。管理过程不需要 SSH。

普通 push 只同步源码，不触发 Actions。版本 tag 先推到 Gitea，镜像到 GitHub 后，在 GitHub 网页选择该 tag 发布 Release（如 `v0.1.0`）；Actions 才执行完整测试、构建、上传附件并自动填写更新说明。PR 执行基础检查。标题和描述可以留空，无需手写更新日志或上传运行包。见[自动发布说明](docs/releases.md#自动发布)。

| 位置 | 内容 |
| --- | --- |
| Git | 源代码、协议、测试、模板、Agent 操作说明 |
| 项目 Release | 固定版本的工具包、预编译二进制、文件清单和 SHA-256 |
| 容器镜像仓库 | Tuwunel、Telegram 镜像，以 digest 固定 |
| 私有 B2 | restic 数据库快照（RocksDB 与桥 SQLite），不存部署配置或恢复索引 |
| 私有 R2 | owner / epoch、交接状态、报告、指令；恢复点索引、版本化配置包、B2 连接和仓库密码 |
| 原生媒体 S3 | Tuwunel 直接存储的媒体，继续原样使用，不重复备份 |

**B2 不保存工具代码、可执行文件或容器镜像。媒体不重复备份。** 恢复点固定工具版本和镜像摘要；缺失或校验不符时停止，不替换成最新版。

## 从这里开始

- [新机器 Agent 操作说明](node/backup/AGENT.md)：用户只编辑 `/opt/matrix/config/s3.env`。
- [一条恢复提示词](RECOVER-PROMPT.txt)：部署细节在项目文档中维护。
- [架构与状态流转](docs/architecture.md)：控制权、数据、代码的职责。
- [发布和升级](docs/releases.md)：构建 Release、更新实例、现场接入验收。
- [Telegram 官方镜像同步](docs/telegram-image.md)：手动运行 Actions，原样复制到 GHCR，无需个人 Token。
- [Worker 部署](docs/worker.md)：Git 接入 Cloudflare、Access、R2 绑定。
- [备份格式](node/backup/S3-RECOVERY.md)、[R2 管理协议](PROTOCOL.md)、[安全边界](SECURITY.md)。

## 新机器准备

目标环境：Linux amd64、systemd、Bash、Docker Compose、nginx、curl、jq、SQLite CLI、GNU coreutils/util-linux。依赖由 Agent 检查已有安装和路径后准备，脚本不运行包管理器。目标运行包不需要 Go、Node.js 或 Python。

从可信项目版本取得源码目录，Agent 执行：

```sh
bash scripts/bootstrap.sh --output /opt/matrix/agent/downloads/runtime
bash /opt/matrix/agent/downloads/runtime/backup/inspect-host.sh
bash /opt/matrix/agent/downloads/runtime/backup/recover.sh --prepare-standby
```

用户输入只有 R2 endpoint、bucket、cluster ID 和 R2 访问凭据；不填写 B2 或 restic 密码。

bootstrap 只下载并验证公开工具。`--prepare-standby` 只从 R2 读取恢复点及配置，建立新实例身份、控制器和轮询任务；不恢复业务数据库、不拉取应用镜像、不取得运行资格。现有实例重复调用会保留身份。

正常切换先调整 Cloudflare 回源，再从管理页面提交。旧实例关闭入口并停止相关服务，最终备份验证完成后释放归属；新实例取得下一 epoch，恢复指定数据、隔离校验、上线。DNS 改动本身不构成停写屏障，Telegram 桥也必须停止。

## 开发与验证

```sh
npm ci
npm test
npm run build
npm run check:source
npm run test:go
# 隔离 Linux 测试环境，root；仅替身服务和临时数据：
RESTIC_TEST_BINARY=/path/to/restic-0.19.1 npm run test:node
# 提交已审核源码后构建正式包；不会上传：
RESTIC_BINARY=/path/to/restic-0.19.1 npm run release:build
```

Go 模块位于 `node/control`、`node/release`、`poller`，全部使用标准库。Node.js 22+ 用于 Worker 开发、打包和测试，Go 1.23+ 用于构建。正式构建要求 Git 工作区干净；发布包使用显式清单，不打包现场部署目录。

测试覆盖校验失败、数据/代码隔离、单写者控制、SQLite WAL、真实 restic 往返、standby 准备、跨版本更新、更新中断重试和管理协议。脚本集成测试的 Docker/systemd/S3 是替身，不能代替真实双 VPS 交接验收。

当前支持计划切换；**强制灾难接管、R2 权威状态重建、自动清理旧实例数据尚未实现**。不要通过编辑 owner/epoch 或手动启动容器模拟这些功能。首次现场切换按发布文档验收，保留旧数据。

采用 [MIT License](LICENSE)。第三方程序保留各自许可证，见运行包 `backup/THIRD-PARTY.md`。
