# 私有数据协议 v2

## 存储布局

```text
私有 R2：cluster/<cluster-id>/
  control.json                         # 唯一运行权威，CAS
  instances/、agents/、commands/…       # 状态和管理通路
  recovery/v2/
    repositories/<id>-<sha>.json       # B2 连接、凭据、restic 密码
    configurations/<sha>.json          # 有界配置包、metadata、完整文件清单
    points/<snapshot-id>.json          # 固定以上两个对象及 SHA-256
    latest.json                        # 完整发布的日常 point/key/hash，CAS

私有 B2：<restic repository>/           # 仅数据库快照及 restic 自身索引
原生媒体 S3：<existing prefix>/        # Tuwunel 原生媒体，不重复备份
```

对象存储不分发工具代码或容器镜像。公开工具来自版本化 Release，镜像使用可下载的 registry digest。恢复点含 `protocol=2`、`point_schema=2`、`catalog_schema=1`、`data_layout=database-only-v1`。不提供旧布局自动降级。

## 数据与配置的对应关系

本地暂存 bundle 的 metadata 固定服务身份、一致性、公开工具 descriptor、镜像引用/image ID 和 source epoch；manifest.sha256 覆盖完整文件集合。只上传以下数据到 restic：

- `bundle/payload/database/`：Tuwunel checkpoint 或停写副本。
- `bundle/payload/telegram/data/telegram.db`：SQLite 一致性副本，含 session/crypto 状态。

其余允许路径进入 R2 配置包：Tuwunel compose、注册信息、appservices、媒体连接配置；Telegram compose、config.yaml、registration.yaml 和验证元数据；两个 nginx 文件；`control/source-local.json` 作为源实例参考。配置包包含完整 metadata/manifest，但不包含数据库正文、控制器 R2 凭据或可执行工具。

不包含 journal、managed 许可、Agent registry、运行日志、缓存或媒体正文。`matrix-release config-pack/config-unpack` 校验路径、文件集合、哈希、大小及权限，拒绝链接、路径穿越、未知字段和超限输入。配置正文上限 16 MiB、单文件 8 MiB、外层 JSON 32 MiB；超限停机检查，不静默截断。

恢复先取得 R2 point 固定的配置包，再按需从仓库记录连接 B2。`data-merge` 只接受清单中的数据库文件，在任何合并前验证全部文件哈希及集合；B2 中的配置/工具注入会被拒绝。合并后的完整 bundle 再做全量校验、SQLite 检查和隔离应用启动。standby 只取 R2 配置，不连接 B2。

## 提交与一致性

依赖对象使用条件创建并读回校验；同名不同内容会报错。日常 latest 以先前 ETag 条件更新，再读回确认；冲突或网络失败不更新本地 last-success。只有数据验证、R2 配置、仓库记录、point 和 latest 全部成功，日常恢复点才算完成。

最终交接要求 globally-quiesced、完整恢复、两库隔离启动和停写证明。它只写不可变 point 及本地回执，由 R2 operation 固定 snapshot/hash，不覆盖日常 latest。控制器验证 point key 属于该集群命名空间。在线点为 independent-online-snapshots，两个数据库分别一致，不能宣称跨服务事务一致。

## 版本、凭据与保留

point 的 `software` 包含 project/version/manifest_sha256；`images` 含 registry digest/image ID。下载来源只取自可信项目 release.json，不接受 R2 提供任意代码 URL，不自动换用最新版。

用户只填写 R2 输入。R2 仓库记录中的密码和 B2 凭据用于恢复数据库，因此 R2 读权限足以取得备份明文，不是独立的第二重加密因素。所有桶均应私有；Worker API 不返回配置包或仓库记录。新实例保留本机 R2 输入，历史配置不会回滚 R2 凭据。

轮换 B2/媒体凭据后重新发布并验证恢复点；旧恢复点所引用的旧凭据失效会使其不可恢复。仍被引用的配置包、仓库记录、数据快照、Release 和镜像都须保留。本工具不自动 prune。源身份仅作参考，不得用备份重建或抢占 R2 运行权。
