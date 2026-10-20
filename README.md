# pkgdepot

一个 Go 静态二进制 CLI：把「私有 npm / PyPI 仓库」从联网侧一路带到完全离线的内网主机——外网侧把依赖打成自包含 bundle，内网侧一条 `apply` 通过 SSH 建起可用的私有仓库并逐字节验证。

```
外网构建机                             内网目标主机（有 docker，无外网）
依赖清单 → plan → bundle ──拷入──▶ apply → verify → env → skill export
             │                        │
             ├ 包文件 + sha256         ├ verdaccio:4873  (npm)
             ├ verdaccio 镜像          └ pypiserver:8080 (PyPI)
             └ pypiserver 镜像
```

目标主机完全离线，拉不到任何镜像，所以两个 registry 镜像由 bundle 携带，`docker load` 是唯一可用路径。目标机上只假设有 `sh`、`docker`、`tar`。

## 安装

从源码构建（推荐，产物是单个静态二进制）。在仓库根目录执行：

```bash
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o pkgdepot ./cmd/pkgdepot
sudo install -m 0755 pkgdepot /usr/local/bin/pkgdepot
pkgdepot version
```

构建机还需要 `docker`（保存两个 registry 镜像），以及清单里实际用到的 `npm`（解析 npm 闭包）、`pip`（下载 wheel）。内网目标机只需要 `sh`、`docker`、`tar`；`npm` / `pip` 只在消费包的机器上需要。

内网完全不联网时，把编译好的二进制直接拷进去即可：`CGO_ENABLED=0` 的静态二进制没有运行时依赖；`-trimpath` 让二进制里不出现构建机路径。

交叉编译到内网架构（在构建机上执行，产物直接拷入）：

```bash
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -o pkgdepot-linux-amd64 ./cmd/pkgdepot
```

## 30 秒快速开始

在**外网构建机**上：

```bash
# 1. 写清单
cat > pkgdepot.yaml <<'EOF'
apiVersion: pkgdepot/v1
name: intra-repo
npm:
  packages:
    - lodash@^4.17.0
python:
  packages:
    - requests==2.32.3
  platforms:
    - manylinux2014_x86_64
  pythonVersion: "3.12"
EOF

# 2. 解析闭包 → 产出 bundle
pkgdepot plan   -f pkgdepot.yaml
pkgdepot bundle -f pkgdepot.yaml -o intra-repo.tar.gz
```

把 `intra-repo.tar.gz` 和 `pkgdepot` 二进制一起拷进内网（走介质或单向摆渡；建议同时带上 `sha256sum intra-repo.tar.gz` 的结果）。

在**内网运维机**上：

```bash
# 3. 登记目标主机（SSH 免密登录已配好）
pkgdepot host add intra-01 --address 10.20.30.40 --user ops --key ~/.ssh/id_ed25519

# 4. 部署：上传 → 解包 → docker load → 起 registry → 逐字节验证
pkgdepot apply --host intra-01 --bundle intra-repo.tar.gz
```

在**内网使用侧**：

```bash
# 5. 拿到可直接 source 的客户端配置
eval "$(pkgdepot env --host intra-01)"
npm install lodash
pip install requests

# 6. 为内网 AI 导出取包技能（含该主机真实可用的包清单）
pkgdepot skill export --host intra-01 -o ./skills/intra-01-pkgdepot
```

## 命令总表

| 命令 | 作用 |
|---|---|
| `pkgdepot plan -f pkgdepot.yaml` | 解析依赖清单，打印闭包摘要，不产出 bundle 文件 |
| `pkgdepot bundle -f pkgdepot.yaml -o <file>` | 下载包与镜像、生成 verdaccio 存储、打出 bundle tar.gz |
| `pkgdepot host add <name> --address <ip> --user <u> [--port 22] [--key <file>] [--base-dir <dir>] [--npm-port <p>] [--python-port <p>]` | 登记目标主机（覆盖同名） |
| `pkgdepot host list` | 列出已登记主机与上次部署结果 |
| `pkgdepot host remove <name>` | 移除主机条目（只删本地配置，不动远端数据） |
| `pkgdepot apply --host <name> --bundle <file> [--skip-verify]` | 上传并部署，默认随后逐字节验证 |
| `pkgdepot status --host <name>` | 查看容器状态与仓库地址 |
| `pkgdepot verify --host <name> --bundle <file>` | 只做 HTTP 逐字节校验，产出包清单 |
| `pkgdepot env --host <name> [--npm] [--python]` | 输出 `export` 语句，可 `eval` |
| `pkgdepot skill export --host <name> -o <dir>` | 生成内网 AI 的取包技能目录（`<dir>/SKILL.md` + `<dir>/references/`） |
| `pkgdepot uninstall --host <name>` | 停止并移除容器，数据目录保留 |
| `pkgdepot version` | 打印版本 |

默认值：SSH 端口 `22`、远端目录 `/opt/pkgdepot`、npm 端口 `4873`、python 端口 `8080`。主机配置保存在 `~/.config/pkgdepot/hosts.yaml`（`0600`）。

## 它怎么工作

1. **解析**：从 lockfile 或 `name@range` 推导 npm 完整依赖闭包；python 侧按 `platforms` / `pythonVersion` 解析出要带的分发文件清单。`plan` 把这份清单打印出来供确认。
2. **打包**：`bundle` 把包文件按 verdaccio 落盘格式写好（packument 带 `dist.tarball`、保留 `dependencies`），`docker save` 出两个镜像，附客户端配置模板与 `manifest.json`（含每个文件的 sha256）。
3. **部署**：`apply` 通过 SSH 上传 bundle 与 CLI 内嵌的部署资产（`install.sh`、`compose.yaml`、registry 配置），再执行 `install.sh`：解包 → `docker load` → `docker compose up` → 等待健康。
4. **验证**：CLI 侧直接发 HTTP，对每个 packument、tarball、wheel 做逐字节 sha256 校验，结果写成 inventory 并回存到主机条目。
5. **导出**：`skill export` 用 inventory 生成内网技能，写明 registry 地址、真实可用的包清单与缺包处理流程。

## 目录结构

```
cmd/pkgdepot/          二进制入口
internal/spec/         数据合同（Manifest / BundleManifest / HostsFile / Inventory）
internal/bundle/       依赖闭包解析、包下载、镜像导出、bundle 打包
internal/remote/       SSH 连接、脚本执行、文件上传
internal/install/      apply / verify / status / uninstall、内嵌部署资产
internal/hosts/        hosts.yaml 读写
internal/skillexport/  内网取包技能生成（模板 + Inventory 驱动）
internal/cli/          子命令装配、输出、退出码
skills/pkgdepot/       构建侧 DSH 技能
docs/                  技术方案、接口合同、使用与排错
test/e2e/              真 sshd + 真 docker 的端到端验收
```

## 支持矩阵

| 项 | 支持情况 |
|---|---|
| 构建机 | 能访问外网、装得动 `docker` / `npm` / `pip` 的机器；`pkgdepot` 产物是静态二进制 |
| 目标机 | Linux，需要 `sh`、`docker`、`tar`，SSH 可达 |
| 目标机网络 | 可完全离线；镜像与包全部由 bundle 携带 |
| npm registry | verdaccio（bundle 默认携带 `verdaccio/verdaccio:6`） |
| PyPI registry | pypiserver（bundle 默认携带 `pypiserver/pypiserver:latest`） |
| Python 目标平台 | 由清单 `platforms` / `pythonVersion` / `implementation` 决定；同一 Python 版本下 `platforms` 可写多条 |
| 传输 | SSH 私钥（`--key`），sftp 上传 |

## 已知限制

- **无 TLS、无鉴权**：registry 是明文 HTTP，只适用于隔离网段；不要把端口暴露到其他网段。
- **Linux 目标**：目标机按 Linux 处理（`sh` + docker），安装脚本与容器编排都是 Linux 假设；Windows / macOS 目标不在支持范围内。
- **目标机必须能跑 docker**：registry 以容器形式运行，没有其他实现路径。
- **包版本固定**：bundle 里的版本就是内网可用的版本；新增包要回外网重建 bundle 后重新 apply（apply 幂等，已有包不受影响）。
- **没有发布命令**：pkgdepot 只做「把包带进内网并对外提供」，命令面里没有 `publish` 一类子命令。自有包与第三方包一样，在构建机上从可访问的 registry 按清单解析、随 bundle 进入内网。内网 registry 的写入能力不是 pkgdepot 的工作流入口，需要长期维护自有包时按你所在环境的包发布流程处理后，再把版本收进清单。

## 文档

- [docs/architecture.md](docs/architecture.md)：技术方案、ADR、实测结论
- [docs/interfaces.md](docs/interfaces.md)：包间接口合同与 CLI 命令面
- [docs/usage.md](docs/usage.md)：构建者 / 运维 / AI Agent 三条路径的完整操作
- [docs/troubleshooting.md](docs/troubleshooting.md)：真实故障与定位方法
- [skills/pkgdepot/SKILL.md](skills/pkgdepot/SKILL.md)：构建侧 DSH 技能
- [test/e2e/README.md](test/e2e/README.md)：端到端验收的用例与运行方式
