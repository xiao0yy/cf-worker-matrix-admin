# 构建与发版

唯一版本来源是 Git tag `vX.Y.Z`，构建时注入程序及 manifest。package.json
不维护服务器版本，恢复点的 `recovery_version` 是数据协议，不是程序版本。

将已审阅代码推到主分支，再用 `git tag vX.Y.Z`、`git push origin vX.Y.Z`
创建并推送 tag。**Release 由工作流自动创建。** 页面显示 `on: push` 是指
版本标签的 push，普通分支 push 不触发发版。tag push 触发 Release workflow：

1. 检查 tag 与测试提交相同、工作树干净。
2. Worker 测试、Go 测试/vet、源码审计；构建固定版本运行包。
3. 使用包内 restic 做真实临时仓库恢复测试。
4. 创建草稿，上传并下载比对附件；全部通过后自动发布。

Checks workflow 运行于 PR 与手动触发；普通分支 push 不重复运行 Checks。
Release 自带完整检查。需要重试发布时重跑失败 job，或手动选择已有 tag。
同 tag 已有附件必须逐字节一致，否则失败；不覆盖附件，不把公开 Release
改回草稿。也支持在 GitHub 网页上创建 tag 并发布 Release：工作流会校验已有
附件，只上传缺失附件，全部验证后生成标题和说明。中断后可重跑失败的 publish
job，无需删除 Release 或重建 tag。若附件内容冲突，修复应使用新 tag。

公开附件只有三个：

| 附件 | 内容 |
| --- | --- |
| `matrix-admin-linux-amd64.tar.gz` | matrix-admin、restic、manifest.json、AGENT.md、LICENSE、THIRD-PARTY.md |
| `SHA256SUMS` | 压缩包与 AGENT.md 的 SHA256 |
| `AGENT.md` | 服务器 Agent 的同版本部署/恢复说明 |

GitHub 自动生成的源码下载链接不属于服务器运行依赖。Release 描述由 tag 间
提交记录自动生成，直接提交也会列出，不需要用户写发布说明。

开发者可用 `go run ./tools/release build --tag vX.Y.Z` 本地构建已打 tag 的
干净源码。`--restic /path/to/restic` 可使用本地依赖，但仍验证 lock 中的哈希。
`--dev` 仅用于未提交源码的隔离验证，产物不得发布。

升级读取新包的 manifest 和当前实例身份，原子安装到 `releases/X.Y.Z`。
先 fence，再用新包执行 `upgrade --from <新包> --apply`；完成后检查并 resume。
运行代码只安装一份，数据 generations 不包含程序。详见 [AGENT.md](AGENT.md)。

仓库中的 `tools/release` 和 `scripts/publish-release.sh` 是开发/CI 工具，
不会进入运行包。VPS 不安装 Go、Node、Git 或执行发布脚本。
