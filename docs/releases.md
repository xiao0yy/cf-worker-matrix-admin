# 构建、发布、升级与现场接入

## 发布来源

源码与工具属于这个公开项目。代码从 Gitea 镜像到 GitHub，GitHub Actions 构建并发布 Release 附件。release.config.json 中的下载基址与恢复入口统一指向 GitHub；代码镜像不承担 Release 附件同步。更换发布来源需要审核项目配置，不由 R2 point 选择任意域名。

版本 tag 必须先保存在 Gitea 源仓库，再镜像到 GitHub。Gitea 的 push mirror 会强制同步远端引用；仅在 GitHub 创建的 tag 可能在下次同步时被删除，影响关联 Release 的发布与下载。GitHub 发布页面只选择已经镜像到位的 tag，不在该页面新建 tag。[Gitea 镜像说明](https://docs.gitea.com/1.25/usage/repository/repo-mirror/#pushing-to-a-remote-repository)

VERSION 是工具版本，point 的软件 descriptor 固定 manifest SHA。现有版本资产不可覆盖；源代码、运行包、镜像都应保留到最后一个引用它的恢复点被人工淘汰。

## 自动发布

| 操作 | Actions 行为 |
| --- | --- |
| 普通分支 push、单独推送 tag | 不触发 Actions |
| 创建或更新 PR | Checks：Node / Go 测试、源码与前端检查；不打包、不上传产物。仅 Markdown / docs / LICENSE 的变更跳过 |
| 网页 Publish release（含预发布） | Release：完整测试、运行包构建、恢复集成测试、上传附件 |
| Actions → Checks → Run workflow | 手动执行基础检查 |
| Actions → Release → Run workflow | 为已有 tag 执行完整发版，或重试补齐附件 |

正常发版步骤：

1. 更新 VERSION、package.json、package-lock.json 中的版本，三者一致后提交、push 到 Gitea。
2. 为这次提交创建 `v<VERSION>` tag 并推送到 Gitea（例如 `git tag v0.1.0 <完整提交SHA>`、`git push origin refs/tags/v0.1.0`，其中 origin 是 Gitea）。也可在 Gitea 网页创建指向同一提交的 tag。等待镜像后，确认 GitHub 上该 tag 指向的提交相同。
3. GitHub → Releases → Draft a new release，选择这个已有 tag。
4. 点击 **Publish release**。标题、说明可以留空，不需要上传附件或添加专用标记。
5. 等待 Release 工作流成功。Actions 上传固定的 7 个附件，自动填写标题和更新说明；已有标题、手写说明和预发布设置会保留。

只保存草稿不会触发 Actions。单独推送 tag 也不会触发发版；如采用命令行创建 tag，需要镜像 tag 到 GitHub 后，在 Actions → Release → Run workflow 中填写该 tag。手动入口不创建 tag；Release 尚不存在时会创建草稿，附件全部校验后再公开。

普通 push 和 tag push 均不触发工作流，避免同一提交同时跑 Checks 和 Release，或一次网页发布同时触发 tag 与 release 两次构建。PR 新提交取消该 PR 尚未完成的旧 Checks；同一 tag 的发版串行执行，不中断正在上传的任务。

工作流会依次：

1. 核对 tag = v<VERSION>、package 版本、提交，以及配置的下载仓库。
2. 在只读构建 job 中运行前端/API、发布故障场景、Go、恢复集成和真实 restic 测试，构建固定工具链的完整资产。成功产物保留 7 天。
3. 将同一次运行中的已测试产物交给发布 job，再核对完整文件集、SHA-256、构建提交和 GitHub tag。
4. 核验同名 Release 的 tag 和所有已有附件，再补齐缺失附件。已上传附件的大小和 SHA-256 必须一致；任何冲突在写入前停止。未知附件保留并报错。
5. 上传完成后再次核验完整文件集与 tag，写入自动更新说明。网页已发布的 Release 保持已发布；草稿则公开。脚本新建的 Release 根据版本中是否含 `-` 设置 pre-release；已有 Release 保留用户设置。

只使用 GitHub 自动提供的 GITHUB_TOKEN；构建 job 为 contents:read，仅发布 job 为 contents:write。无需添加个人 Token、B2/R2 密钥或 Cloudflare 凭据。Actions 不升级 VPS，也不部署 Worker。

官方 Actions 固定到已核对的完整提交 SHA，并采用 Node.js 24 运行时；项目构建所用的 Node.js 和 Go 版本分别由 setup-node / setup-go 指定，两者不等同于 Action 自身的运行时。升级 Actions 时同时核对上游 `action.yml`、输入和产物兼容性，不通过强制运行旧 Action 隐藏弃用告警。

失败时在 Actions 选择 **Re-run failed jobs**。发布 job 使用成功构建 job 记录的产物名，不因重跑次数变化而找不到附件。上传响应丢失后会回读已有资产；草稿和网页已发布的可变 Release 均可补齐。仅已知文件名、零字节、无摘要的 `starter` 失败上传占位会删除重试；任何已上传文件都不覆盖。构建产物超过保留期时重跑整个工作流。

网页先 Publish release 时，附件会在 Actions 完成后才齐全；恢复工具只应使用工作流成功的版本。发布器将 software.json 放在上传顺序最后，恢复工具仍独立验证 manifest 和文件哈希。若仓库开启了 GitHub Immutable releases，已发布 Release 不能再补附件，应使用已有 tag 的手动入口，让脚本先准备草稿再发布；已发布且不完整的不可变版本必须换新版本处理。

工作流和脚本固定在该 tag 的提交上。修复发布代码后，重跑旧 tag 仍会使用旧脚本；应提交修复并使用新版本，不能移动已经发出的版本 tag。网页创建的空 Release 是受支持的发布目标，不需要删除重建。

如果已发布的版本重新显示为 Draft，先检查 Gitea 和 GitHub 的 tag 是否都存在且指向原发布提交，再检查镜像同步记录。不要反复点击 Publish，让缺失 tag 自动指向当前 main；这会使新构建与原有附件冲突。若确认是 tag 丢失，应根据原成功工作流及已校验的 manifest 找回原提交，在 Gitea 恢复该 tag，镜像到 GitHub 后再发布原草稿。保留原附件，不用覆盖附件来消除哈希冲突；已有 tag 若指向其他提交，应先调查，不做强制覆盖。

参考：[GitHub release 事件](https://docs.github.com/en/actions/reference/workflows-and-actions/events-that-trigger-workflows#release)、[Release asset API](https://docs.github.com/en/rest/releases/assets)、[不可变 Release](https://docs.github.com/en/code-security/concepts/supply-chain-security/immutable-releases)。

## 自动更新说明

不需要手写 Release 描述，也不需要手动点击 Generate release notes。发布器从完整 Git 历史生成变更列表，适用于直接提交后从 Gitea 镜像的仓库，不依赖 PR、额外 Token 或 AI 服务。

- 基线是当前提交祖先中、低于当前版本的最高版本 tag，支持 annotated tag 和预发布版本；忽略无关分支、更高版本和非版本 tag。没有基线时展示首次版本的提交历史。
- 描述列出最多 50 条提交标题与链接，并提供完整差异、源码提交、部署恢复文档和升级文档链接。链接固定到 commit SHA。标题来自实际 Git 提交，不做 AI 改写。
- 空标题自动填写为 `Matrix Admin v<VERSION>`；已有标题保持原样。自动说明放在专属段落中，段落前后的手写内容保留。重跑更新该段落，不重复追加。
- 附件验证完成后才写入说明；重跑可以单独补上说明而不重传附件。更新响应丢失时，下一次运行读取实际结果继续。

自动段落的 HTML 注释仅用于划定更新范围，不用于判断 Release 的来源或允许覆盖附件。更新说明前会重新读取 Release，尽量保留上传期间用户新增的文字；避免同时在网页与工作流里编辑同一个自动段落。

## 构建

开发环境：Node.js 22+、Go 1.23+、git、curl、jq、tar、gzip、bzip2。Go 二进制使用 CGO_ENABLED=0、GOAMD64=v1，无 AVX 要求。restic 的版本与二进制校验值固定在 dependencies.lock.json。

```sh
npm ci
npm test && npm run build && npm run check:source
npm run test:go
RESTIC_TEST_BINARY=/path/to/restic-0.19.1 npm run test:node
# 提交审核后的完整源码；正式构建拒绝脏工作区
RESTIC_BINARY=/path/to/restic-0.19.1 npm run release:build
```

未设置 RESTIC_BINARY 时构建器从锁定的上游地址下载，解压后校验二进制。设置该变量也不能绕过校验。开发构建 `npm run release:build -- --development` 标记不可发布，用于本地验证。

正式输出 dist/v<VERSION>/：

- matrix-admin-linux-amd64.tar.gz：显式清单构建的运行树。
- matrix-admin-linux-amd64.manifest.json：文件/大小/模式/哈希、协议、commit、archive SHA。
- matrix-release-linux-amd64：首次下载使用的独立校验器。
- software.json、SHA256SUMS：固定版本描述与引导校验。
- source.tar.gz、BUILD.json：已提交源码与构建记录。

本地构建用于开发和验证，不会上传。正式附件由 Release 工作流自动放在配置的 `/v<VERSION>/` 下；Checks 构建不会发布。不要上传早期本地构建结果替换 Actions 的正式附件。

所有运行包路径经过 allowlist，私密 .env、身份、journal、数据库、凭据和现场日志不能入包。manifest 与 archive 使用固定排序、时间戳、owner；同一源码/工具链/依赖可重复打包。编译器版本也是构建环境的一部分。

## 镜像是独立依赖

point 记录 registry digest 及对应 image ID，恢复拉取 digest 并核对 ID/平台。应用容器必须按含 registry 域名的固定 digest 创建（例如 `ghcr.io/owner/image@sha256:...`），备份记录容器实际配置的引用并核对 image ID。不能从 `RepoDigests` 中随便取第一项：Docker 的本地 tag 也可能生成看似有效、却无法远程下载的 digest 别名。桥使用本地自定义镜像时，应先发布到可恢复获取的镜像仓库、再按 digest 创建容器；仅有本地 tag 不足以完成 v2 接入。不能把镜像重新塞进 B2 代替这一步。

工具包升级不升级应用镜像。应用升级须另外备份、验证数据库迁移和桥双向消息，再生成绑定新镜像的 point。

## 两种更新入口

**匹配某个数据点**：交接目标发现 UPDATE_REQUIRED 时，运行该固定点的
`recover.sh --update-foundation --point <完整ID> --point-sha256 <hash>`。
这只读取少量配置和更新工具，不下载业务数据库，也不取得运行资格。

**主动升级工具**：先取得并验证目标 Release，再运行：

```sh
bash /verified-release/backup/upgrade.sh --release /verified-release
bash /verified-release/backup/upgrade.sh --release /verified-release --apply
```

第一条仅检查。可用 --target 指定已登记的基础目录。更新覆盖基础目录与活动数据代的公开工具，保留身份、journal、密钥和数据库；下一次备份记录新版本。

ONLINE 状态只允许不改变控制器二进制/单元的代码更新。控制器变更时，先核对备份并受控 fence，再更新；核对 owner/epoch、版本和健康后明确恢复原实例。不要直接 restart matrix-control：它的停止钩子会关闭业务。

失败后保留 agent/pending-update.json，重试相同版本。若 controller 暂时不可用，更新器只接受匹配 pending 身份/版本且本地仍阻断、未可能写入的状态。不能删 pending 来换另一个版本。

## 现有机器首次接入验收

源码整理完成不等于生产已经切换。接入前依次完成：

1. 发布正式公开资产，用独立下载目录运行 bootstrap 并验证；禁止使用 development 包充当正式发行版。
2. 确认两个应用镜像都有可获取的 immutable registry digest；保持 image ID 与正在运行的数据版本一致。
3. 确认当前备份成功，无正在交接/备份/未完成 checkpoint。保留当前代码和已验证恢复点，准备维护窗口。
4. 通过控制器阻断本实例，记录原身份/owner/epoch；用公开升级入口安装工具，不恢复数据库、不重建身份。
5. Agent 将当前控制器 R2 连接与 local.json 的 cluster_id 写入唯一用户输入 `/opt/matrix/config/s3.env`（先私密保留旧文件），核对后同步到 deploy/control/s3.env。内部 backup/restic.env 继续使用原 B2 repository/key；配置与索引改为 R2 `cluster/<id>/recovery/v2/`。保留原媒体配置。只调整输入，不复制或重新初始化数据库。
6. 核验身份/journal 未改变，明确恢复本实例；执行 FULL_VERIFY=1 的完整备份发布（在内部备份配置中设置，不依赖可能被配置覆盖的环境变量），确认 R2 point/configuration/repository 记录读回一致、B2 新快照仅含两个数据库、公开工具与镜像可下载。
7. 在独立目标/临时环境执行固定点隔离恢复，再在第二 VPS 准备 standby，验证无业务启动、实例报告和管理指令通路正常。
8. 按计划交接完成真实双 VPS 验收，确认目标 ONLINE、owner/epoch、应用健康和桥双向消息；保留旧数据。

当前读写器只实现 v2，不自动解析旧格式，也不批量清理旧对象。正式切换前保留旧安装与恢复工具，避免尚未发布的新工具使既有备份失去入口。
