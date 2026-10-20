---
name: pkgdepot
description: 在联网的构建机上把 npm / PyPI 依赖清单打成离线 bundle，供完全离线的内网主机建立私有包仓库。当用户提到内网私有 npm/PyPI 仓库、air-gap 部署包仓库、离线安装依赖、把依赖打包带进内网、离线 bundle、为内网 AI 生成取包技能时使用。
---

# pkgdepot

`pkgdepot` 把「私有 npm / PyPI 仓库」从联网侧一路带到完全离线的内网主机。本技能只覆盖构建侧：拿到依赖清单，产出可离线携带的 bundle。部署侧（`pkgdepot apply`）与内网取包由运维机和导出后的 `<host>-pkgdepot` 技能负责。

## 何时用这个技能

- 用户要在没有外网的内网主机上建 npm / PyPI 私有仓库。
- 用户要把一组依赖打成离线包带进内网。
- 用户手里的内网机器 `npm install` / `pip install` 失败，需要提前把包补齐。
- 用户要为内网 AI 生成「仓库里有什么、缺包怎么办」的技能。

## 前置检查

```bash
pkgdepot version     # 有输出即可，不需要联网
docker version       # bundle 里的两个 registry 镜像要 docker save
npm --version        # 解析 npm 依赖闭包
pip --version        # 解析 python 依赖闭包
```

`pkgdepot version` 报 command not found 时先安装（见仓库 README）。清单里配了哪个生态，就要有哪个生态的工具：缺 `npm` 时 npm 闭包解析不了，缺 `pip` 时 wheel 下载不了。

构建机需要能访问 npm registry 与 PyPI。内网侧不需要，bundle 自带所有包文件与镜像。

## 写 pkgdepot.yaml

最小可用清单（同时带 npm 与 python 两个仓库）：

```yaml
apiVersion: pkgdepot/v1
name: intra-repo

npm:
  # 二选一：用已有 lockfile 解析完整闭包，或直接列 "name@range"
  lockfiles:
    - ./app/package-lock.json
  includeDev: false

python:
  # 二选一：用 requirements 文件，或直接列包名
  requirements:
    - ./python/requirements.txt
  # 目标运行时的平台标签，必须与内网消费者一致
  platforms:
    - manylinux2014_x86_64
  pythonVersion: "3.12"
  implementation: cp
```

字段约束（`internal/spec` 在加载时校验）：

| 字段 | 约束 |
|---|---|
| `apiVersion` | 必须 `pkgdepot/v1` |
| `name` | 必填，不能含空白或路径分隔符；决定默认 bundle 文件名 |
| `npm.lockfiles` / `npm.packages` | 二选一，不能同时出现 |
| `python.requirements` / `python.packages` | 二选一，不能同时出现 |
| 至少一个包来源 | npm 与 python 至少要有一边配了 |

几处容易写错的地方：

- `npm.packages` 的每一项是 `name@range`（如 `lodash@^4.17.0`），range 可省略；scoped 包写成 `@scope/pkg@1.2.3`。
- `python.platforms` 用 pip 的 platform tag，不是 `linux/amd64`。Alpine/musl 用 `musllinux_1_2_x86_64`，常规 glibc 镜像用 `manylinux2014_x86_64`；`platforms` 可以写多条，一次下载多个平台的 wheel。
- `python.pythonVersion` 只写 `3.12` 这样的大版本；wheel 的 `cp312` 标签由它和 `implementation` 共同决定。
- `images` 可覆盖两个 registry 镜像（默认 `verdaccio/verdaccio:6`、`pypiserver/pypiserver:latest`），内网有固定镜像仓库时用：

```yaml
images:
  verdaccio:  registry.local/verdaccio:6
  pypiserver: registry.local/pypiserver:latest
```

- `lockfiles` / `requirements` 的相对路径按**清单文件所在目录**解析，与在哪个目录执行 `pkgdepot` 无关。

## 构建

先 plan 看闭包，再 bundle 出产物：

```bash
pkgdepot plan -f pkgdepot.yaml
pkgdepot bundle -f pkgdepot.yaml -o intra-repo-20260101.tar.gz
```

`plan` 解析依赖清单并打印闭包摘要（两个镜像名、npm 包及其解析版本、python 文件清单），不产出 bundle 文件。解析需要访问 npm registry 与 PyPI。

`bundle` 在 `plan` 的基础上下载 npm tarball、`pip download` 全部 wheel、`docker save` 两个镜像，再打成 tar.gz。产物是一个自包含文件，把它和 `pkgdepot` 二进制一起拷进内网即可。

`bundle` 会跑很久，这是正常的：两个镜像加全部包文件就是几百 MB 量级。

## 校验产物

```bash
tar -tzf intra-repo-20260101.tar.gz | head -40
mkdir -p /tmp/verify-bundle && tar -xzf intra-repo-20260101.tar.gz -C /tmp/verify-bundle
python3 -m json.tool /tmp/verify-bundle/manifest.json | head -40
```

bundle 解包后的关键结构（`manifest.json` 里 `file` 字段指向的相对路径都在这棵树里）：

```
manifest.json              # 权威清单：镜像、npm 包、python 文件，含 sha256
images/verdaccio.tar
images/pypiserver.tar
npm/storage/<包名>/package.json + <包名>-<版本>.tgz
npm/storage/.verdaccio-db.json
python/packages/*.whl（或 *.tar.gz）
client/npmrc
client/pip.conf
```

`manifest.json` 是唯一权威：`images[]`、`npm[]`、`python[]` 每项都带 `sha256`。抽查一个文件的字节是否与清单一致：

```bash
cd /tmp/verify-bundle
sha256sum images/*.tar python/packages/*.whl | head
```

把输出与 `manifest.json` 里对应条目的 `sha256` 逐个比对。逐字节校验的正式入口是部署侧 `pkgdepot verify`，构建侧这一步只是提前发现损坏的包或镜像。

## 拷进内网

- 走介质或单向摆渡：bundle 是单个 tar.gz，附 `pkgdepot` 二进制（静态编译，无运行时依赖）。
- 校验和一起带走：在本机算 `sha256sum intra-repo-20260101.tar.gz`，内网侧拷入后再算一次，不一致说明传输损坏，不要继续部署。
- 不要拷构建机的 `~/.config/pkgdepot/hosts.yaml`：它只描述构建机自己的目标主机，内网侧用 `pkgdepot host add` 重新登记。
- bundle 里已含两个 registry 镜像，内网目标机不需要、也拉不到任何镜像；但目标机必须有可用的 docker。

## 常见失败

| 现象 | 原因 | 处理 |
|---|---|---|
| `pkgdepot: invalid manifest ...: npm.lockfiles and npm.packages are mutually exclusive` | 两种 npm 来源同时写了 | 删掉其中一种 |
| `pkgdepot: missing required flag -f <pkgdepot.yaml>` | 没传 `-f` | 补上 `-f pkgdepot.yaml` |
| `read manifest ...: no such file or directory` | 清单路径错 | 在清单同目录执行，或写绝对路径 |
| npm 解析报 `No matching version found for <pkg>@<range>` | range 在 registry 里没有匹配版本，或包名拼错 | 修正 range/包名，或改用 lockfile 指定精确版本 |
| `npm install --package-lock-only` 报 `ERESOLVE` | 依赖树本身冲突 | 修 `package.json` 的依赖版本，或直接用已存在的 lockfile |
| pip 报 `No matching distribution found for <pkg>` | `platforms` / `pythonVersion` 与目标不匹配，或包里没有该平台的 wheel | 按内网消费者 `python3 -VV` 的版本改 `platforms`/`pythonVersion` 后重建；无 wheel 的包需要目标平台能编译 |
| `docker save` 报 `permission denied`（`/var/run/docker.sock`） | 当前用户无 docker 访问权限 | 用有权限的用户执行，或加入 docker 组后重新登录 |
| `docker save` 报 `No such image` | 本地没有该镜像 | `docker pull verdaccio/verdaccio:6` 与 `pypiserver/pypiserver:latest`，或改 `images` 指向内网可拉取的镜像 |

更细的字段与报错解释见 `references/manifest.md` 与 `references/troubleshooting.md`。

## 交接给部署侧

构建完成后把两样东西交给内网运维：bundle 文件、`pkgdepot` 二进制。部署侧命令是 `pkgdepot apply --host <name> --bundle <file>`，之后由 `pkgdepot skill export --host <name> -o <dir>` 生成内网 AI 的取包技能。本技能不负责也不需要执行这两步。
