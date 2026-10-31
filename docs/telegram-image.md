# 同步官方 Telegram 镜像到 GHCR

当 VPS 不能从 `dock.mau.dev` 拉取镜像时，可以让 GitHub Actions 复制官方镜像到本项目账号的 GHCR。源地址固定为 `dock.mau.dev/mautrix/telegram`；目标为 `ghcr.io/<仓库所有者>/matrix-telegram`。不构建桥程序、不修改官方镜像，也不读取 VPS 的镜像、配置或登录数据。

这项操作与 Matrix Admin 工具发版独立。普通 push、PR、发布 Release 都不会自动触发镜像同步。

## 首次运行

1. 将代码 push 到 Gitea，等待镜像同步到 GitHub 的默认分支。
2. GitHub → Actions → **Mirror Telegram image** → **Run workflow**。
3. `version` 默认 `v0.2610.0`，是官方 Git tag。`expected_digest` 首次发现可以留空；已知道上游 digest 时填写完整 `sha256:...`，不匹配就停止。镜像版本以官方实际发布的 tag 为准，脚本不会猜其他 tag 或退到 latest。
4. 运行结果显示官方来源 digest、GHCR 固定下载地址、匿名访问验证结果。完整记录也保存在 `telegram-mirror-<run>-<attempt>` artifact 的 `mirror.json` 中。

工作流使用 GitHub 自带的 `GITHUB_TOKEN`，权限为 `contents: read`、`packages: write`。不需要创建 PAT，也不需要在 VPS 放置发布凭据。

GHCR 新 package 默认可能是 Private。若摘要提示匿名访问未验证，进入账号的 Packages → matrix-telegram → Package settings → Change visibility，设为 **Public**；然后使用报告中的 digest 重跑工作流。只有匿名验证通过，才满足新 VPS 不填写镜像仓库凭据的要求。源码仓库的公开状态不代表 package 已公开。

## 固定内容与重试

工作流先读取版本 manifest，计算 SHA-256 并核对可选的预期摘要，检查 Linux amd64 配置，然后只使用这个固定 digest 复制。使用 `skopeo copy --all --preserve-digests` 保留所有源平台及原始 manifest；不转换格式或修改标签元数据。

目标 tag 包含版本和完整 SHA-256，例如 `v0.2610.0-sha256-<64位摘要>`，不写入可移动的 latest 或短版本 tag。同版本上游内容变化时会得到另一个目标 tag；旧副本保留。部署与恢复使用报告里的 `ghcr.io/...@sha256:...`，不按版本 tag 猜测。

复制完成后分别检查认证访问的 manifest、匿名访问的 manifest 和 Linux amd64 config。匿名检查不完整下载镜像层；首次 VPS 接入仍需真实拉取并验证镜像运行。源站权限/地区限制、网络错误、摘要不符均会停止，不会改用本地自制镜像作为隐式回退。

GitHub 执行机器能否访问源站需要实际运行确认。若源站认证入口也返回 HTTP 451，报告任务失败，保留日志；不把 workflow 代码检查通过当作镜像发布成功。

## 接入生产

此工作流不会升级 VPS。若当前桥使用自制镜像，官方镜像的启动入口、运行用户、权限和二进制构建方式可能不同。必须先拉取固定 digest、使用隔离副本验证数据库和媒体转换，再通过既有控制器受控切换。不能只改恢复点里的 image ID 声称已经兼容。

参考：[官方 Docker 部署](https://docs.mau.fi/bridges/general/docker-setup.html?bridge=telegram)、[GHCR 发布与权限](https://docs.github.com/en/packages/working-with-a-github-packages-registry/working-with-the-container-registry)、[Skopeo copy](https://github.com/containers/skopeo/blob/main/docs/skopeo-copy.1.md)。
