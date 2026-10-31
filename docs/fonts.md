# 标题字体

标题和品牌名使用同源托管的 `Matrix Admin Display`：Noto Sans SC 的固定 600 字重子集，覆盖当前界面字符与拉丁字母。正文、机器名称等动态内容继续使用系统字体；超出子集的标题字符也可回退。

字体采用 SIL OFL 1.1，完整许可在 `web/fonts/OFL.txt`。固定上游提交、源文件和产物 SHA-256、生成工具版本及覆盖码位在 `web/fonts/SOURCE.json`。WOFF2 约 67 KiB，随源码提交；常规构建与部署不需要字体工具，也不访问字体 CDN。

仅在需要补充字形时重新生成。先按 `SOURCE.json` 的固定提交，从 Google Fonts 的 `ofl/notosanssc/NotoSansSC[wght].ttf` 下载源文件，校验 `source_sha256`；在临时虚拟环境安装该文件记录的 `fonttools[woff]` 与 `brotli` 版本。以下代码在仓库根目录运行，参数为下载的 TTF 路径：

```python
from pathlib import Path
from fontTools.ttLib import TTFont
from fontTools.varLib.instancer import instantiateVariableFont
from fontTools import subset
import hashlib, json, sys

base = Path("web/fonts")
manifest = json.loads((base / "SOURCE.json").read_text())
source = Path(sys.argv[1])
assert hashlib.sha256(source.read_bytes()).hexdigest() == manifest["source_sha256"]
font = instantiateVariableFont(TTFont(source), {"wght": 600}, inplace=True)
chars = set("".join((Path("web") / p).read_text()
                    for p in ["index.html", "app.js", "handover.js"]))
chars.update(chr(cp) for cp in range(32, 383))
unicodes = sorted({ord(c) for c in chars} & font.getBestCmap().keys())
options = subset.Options()
options.flavor = "woff2"
options.name_IDs = ["*"]
options.name_legacy = True
options.name_languages = ["*"]
builder = subset.Subsetter(options)
builder.populate(unicodes=unicodes)
builder.subset(font)
for record in font["name"].names:
    if record.nameID in [1, 4, 6, 16]:
        name = "MatrixAdminDisplay-Semibold" if record.nameID == 6 else "Matrix Admin Display"
        record.string = name.encode(record.getEncoding())
font.flavor = "woff2"
destination = base / manifest["file"]
font.save(destination)
manifest.update(unicodes=unicodes, bytes=destination.stat().st_size,
                sha256=hashlib.sha256(destination.read_bytes()).hexdigest())
(base / "SOURCE.json").write_text(json.dumps(manifest, ensure_ascii=False, indent=2) + "\n")
```

生成后检查 `npm run check:source`，并在字体加载完成后核对桌面和手机端标题。审计仅对这个有明确来源、固定摘要且不超过 256 KiB 的 WOFF2 开放二进制例外，其余私密文件和未知二进制仍被拒绝。
