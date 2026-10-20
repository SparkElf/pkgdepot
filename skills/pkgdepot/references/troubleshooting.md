# 构建侧排错

本文件只覆盖「在外网构建机把依赖清单打成 bundle」这一段。内网侧的报错（registry 连不上、`No matching distribution`、端口冲突等）见 `docs/troubleshooting.md` 与导出后的 `<host>-pkgdepot` 技能。

## 先确认失败发生在哪一步

```bash
pkgdepot plan   -f pkgdepot.yaml    # 只解析闭包，不写产物
pkgdepot bundle -f pkgdepot.yaml -o x.tar.gz
```

`plan` 失败是清单或依赖树的问题；`plan` 通过而 `bundle` 失败，问题在下载、镜像或写盘。这个区分能省掉大部分猜测。

## 清单校验失败

报错以 `pkgdepot: invalid manifest <path>: ...` 开头时，按冒号后的内容处理：

| 报错片段 | 处理 |
|---|---|
| `apiVersion must be "pkgdepot/v1"` | 补上或改对 `apiVersion` |
| `name is required` | 填 `name` |
| `name "..." must not contain whitespace or path separators` | 名字里去掉空格与 `/` |
| `declare at least one npm or python package source` | npm 与 python 至少配一边 |
| `npm.lockfiles and npm.packages are mutually exclusive` | 两者只留一个 |
| `python.requirements and python.packages are mutually exclusive` | 两者只留一个 |
| `invalid npm package spec "..."` | 检查 `name@range` 写法，scoped 包是 `@scope/pkg@1.2.3` |

## npm 闭包解析失败

症状：`plan` 阶段报 `No matching version found`、`ERESOLVE`、或 `package-lock.json` 读取失败。

- **range 没有匹配版本**：`npm view <pkg> versions --json` 看实际发布过哪些版本，改 range 或写精确版本。
- **`ERESOLVE` 依赖树冲突**：这是 `package.json` 自身的 peer dependency 冲突，先在该项目里修好依赖版本；或直接改用项目已有的 `package-lock.json`，lockfile 自带完整解析结果，不再重新求解。
- **lockfile 格式过旧**：`lockfileVersion` 为 1 的 lockfile 只有 `dependencies` 字段，可被解析；但同一目录里的 `package.json` 若与 lockfile 不同步，先 `npm install --package-lock-only` 重新生成。
- **scoped 包名**：`@scope/pkg` 直接写，不要加引号以外的转义；YAML 里以 `@` 开头建议加引号。

## pip 平台标签不匹配

症状：`bundle` 阶段 `pip download` 报 `No matching distribution found for <pkg>`，或下到的 wheel 在内网装不上。

```bash
pip download --only-binary=:all: --platform manylinux2014_x86_64 --python-version 3.12 --implementation cp -d /tmp/t requests
```

- 先用上面这条命令单独验证一个包在目标平台有没有 wheel。
- 报错里出现 `--platform` 相关提示时，说明该包没有这个平台的 wheel；换平台标签或找替代包。
- Alpine 容器要 `musllinux_1_2_x86_64`，不是 `manylinux`；两者二进制不兼容。
- 消费者是 3.11 时 `pythonVersion` 必须写 `3.11`，`cp312` 的 wheel 装不上。

## docker save 失败

- `permission denied while trying to connect to the Docker daemon socket`：当前用户不在 docker 组。用有权限的账号执行，或 `sudo usermod -aG docker $USER` 后重新登录（组变更对新会话生效）。
- `No such image`：本机没有该镜像。`docker pull verdaccio/verdaccio:6` 与 `docker pull pypiserver/pypiserver:latest`，或在清单 `images` 里换成可访问的地址。
- 磁盘不足：两个镜像约 221MB + 92MB，加上全部包，bundle 在几百 MB 量级；`df -h` 确认临时目录与输出目录在同一可用分区。

## 产物校验

`manifest.json` 是权威清单，`sha256` 是每个文件的校验和。解包后抽查：

```bash
tar -xzf x.tar.gz -C /tmp/b
python3 - <<'EOF'
import json, hashlib, pathlib
root = pathlib.Path("/tmp/b")
m = json.loads((root / "manifest.json").read_text())
for e in m["images"] + m["npm"] + m["python"]:
    p = root / e["file"].lstrip("/")
    h = hashlib.sha256(p.read_bytes()).hexdigest()
    print("OK " if h == e["sha256"] else "BAD", e["file"])
EOF
```

这段脚本与部署侧 `pkgdepot verify` 的逐字节口径一致，能在离开构建机前发现损坏文件。

## 常见误解

- `plan` 只解析闭包、不产出 bundle；下载包文件与 `docker save` 发生在 `bundle` 阶段。
- `bundle` 不修改 registry 数据目录，也不连接任何内网主机；它只产出一个文件。
- npm 的 tarball 与 python 的 wheel 都是上游原样字节，pkgdepot 不做二次打包，sha256 因此可以与上游比对。
- 构建机与内网主机的 CPU 架构不需要一致，但 python 平台标签必须与内网消费者一致。
