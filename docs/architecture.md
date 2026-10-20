# pkgdepot 技术方案

`pkgdepot` 是一个 Go 静态二进制 CLI，把「私有 npm / PyPI 仓库」从联网侧一路带到完全离线的内网主机：
联网侧按依赖清单解析完整依赖闭包 → 打成自包含 bundle（包含包文件、registry 镜像、compose 与安装脚本）
→ 带进内网 → `pkgdepot apply --host <name> --bundle <file>` 通过 SSH 在目标主机上建起可用的私有仓库
→ 从 CLI 侧做逐字节完整性验证 → 导出「内网 AI 取缺失依赖」的 DSH 技能。

## 1. 用户与场景

- **构建者**：有外网的研发机，写依赖清单、产出 bundle。
- **内网运维/研发**：内网工作站，携带 bundle 与 CLI 二进制，指定一台有 Docker 的主机，一键建立私有仓库。
- **内网 AI Agent**：在私有仓库环境中开发，遇到缺失依赖时需要知道「仓库里有什么、怎么取、缺了怎么办」。

主机运行前提（已与用户确认）：目标主机有 Docker，但完全离线、拉不到任何镜像，因此镜像必须由 bundle 携带（`docker save` / `docker load`）。

## 2. 关键链路

```
pkgdepot.yaml ──plan/bundle──▶ <name>-<date>.tar.gz
                                 ├── manifest.json          # 权威清单与校验和
                                 ├── images/*.tar            # verdaccio + pypiserver
                                 ├── npm/storage/            # 已按 verdaccio 存储格式落盘
                                 ├── python/packages/*.whl
                                 ├── compose.yaml + install.sh + verify.md
                                 └── client/{npmrc,pip.conf}
        ──scp/sftp 上传──▶ /opt/pkgdepot（目标主机）
        ──install.sh（sh + docker only）──▶ docker load / compose up / 健康等待
        ──CLI 侧 HTTP 验证──▶ 逐个 packument + tarball + wheel 校验 sha256
        ──▶ inventory.json 写回主机配置 ──▶ skill export
```

## 3. 数据合同（internal/spec，唯一权威）

- **Manifest**（`pkgdepot.yaml`，`apiVersion: pkgdepot/v1`）：`name`、`npm.packages|lockfile`、`python.packages|requirements|platforms|pythonVersion|implementation`、`images`。
- **BundleManifest**（bundle 内 `manifest.json`）：`images[]`、`npm[]`、`python[]`（名称、版本、文件名、sha256、npm SRI）、创建时间与工具版本。
- **HostsFile**（`~/.config/pkgdepot/hosts.yaml`，0600）：主机名、SSH 连接信息、远端 baseDir、两个 registry 端口、发布账号、上次 apply 记录与 inventory。
- **Inventory**：两个 registry 实际可用的包版本列表，由 `verify` 产出，供 `skill export` 与 `status` 使用。

远端布局固定为 `<baseDir>/manifest.json`、`npm/{conf,storage}`、`python/packages`、`images`、`compose.yaml`、`install.sh`。

## 4. 已实测确认的机制（不是推测）

| 机制 | 实测结论 |
|---|---|
| verdaccio 直接写 storage 播种 | 可行：`storage/<pkg>/package.json` + tarball + `.verdaccio-db.json`，服务端会把 `dist.tarball` 重写为请求方 Host，离线容器 `npm install` 成功 |
| verdaccio 版本 | `verdaccio/verdaccio:6`（6.10.6） |
| pypiserver | `pypiserver/pypiserver:latest`，把 wheel 放进 packages 目录即可 `/simple/` 可枚举并下载 |
| 离线消费者 | `docker --network internal` 容器内 `npm install` / `pip install` 均成功 |
| 镜像体积 | verdaccio 221MB、pypiserver 92MB，`docker save` node:22-alpine 约 3s/163MB |
| 关键坑 | ① pypiserver 默认跑非 root，挂载目录需可读；② verdaccio 默认 uid 10001 需可写；③ 播种的 packument 必须带 `dist.tarball`，否则 npm 报 `no dist.tarball field`；④ 缺失 `versions[x].dependencies` 会导致传递依赖无法解析 |

## 5. 模块与归属

| 模块 | 目录 | 责任 | owner |
|---|---|---|---|
| 合同 | `internal/spec` | Manifest/Bundle/Host/Inventory 类型、加载、校验、远端布局常量 | lead |
| 构建 | `internal/bundle` | npm 闭包（lockfile 解析 + packument 裁剪 + tarball 下载）、python 闭包（`pip download`）、镜像 `docker save`、打包 tar.gz | builder |
| 传输 | `internal/remote` | SSH 连接（key/agent）、执行脚本、上传文件 | deployer |
| 安装 | `internal/install` | 生成并上传 apply.env、解包、执行 install.sh、状态查询 | deployer |
| 验证 | `internal/install`（verify 部分） | CLI 侧 HTTP 逐字节校验并产出 Inventory | deployer |
| 主机配置 | `internal/hosts` | hosts.yaml 读写、默认值、密码生成 | deployer |
| 技能导出 | `internal/skillexport` | 生成内网技能目录 | docs |
| CLI | `internal/cli`、`cmd` | 子命令装配、输出、退出码 | lead |
| 技能/文档 | `skills/pkgdepot`、`README.md`、`docs/` | 构建侧技能与使用文档 | docs |
| 系统测试 | `test/e2e` | 真 sshd + 真 docker 的全链路验收 | lead |

## 6. ADR

**ADR-1 用 Go 静态二进制**：内网侧不能装运行时；单文件可拷入，`CGO_ENABLED=0` 静态编译。

**ADR-2 直接写 registry 存储播种，而不是 HTTP publish**：离线侧无网、无 token 流程；文件落盘是 verdaccio/pypiserver 的公开契约，且天然支持增量重放（重复 apply 幂等）。

**ADR-3 镜像随 bundle 走**：目标主机完全离线，`docker load` 是唯一可用路径。

**ADR-4 目标主机上只假设 `sh` + `docker` + `tar`**：不在目标机安装任何运行时；所有编排收敛进 bundle 内的 `install.sh`。

**ADR-5 验证在 CLI 侧走真实 HTTP 逐字节校验**：证明 registry 确实对外提供完整正确的包，不依赖目标机上的工具链。

**ADR-6 verdaccio 以 root 运行**：规避 bind mount 的 uid 10001 权限问题，内网隔离环境下可接受；`install.sh` 无需 sudo/chown。

**ADR-7 不内置 TLS/鉴权**：内网隔离网段，v1 只做 HTTP；鉴权后面按需再加。

## 7. 大阶段与验收

只有两个大阶段：

- **阶段 A（构建侧）**：manifest → plan → bundle。完成条件：从真实清单产出 bundle tar.gz，内容与 `manifest.json` 一致。
- **阶段 B（部署侧）**：apply → verify → env → status → skill export。完成条件：真 sshd + 真 docker 目标机上全链路跑通，离线网络命名空间内的 node/python 容器能从私有仓库安装。

系统测试只有一层：`test/e2e` 用真实 `pkgdepot` 二进制、真实 sshd、真实 docker 跑完整流程（本仓无前端，Playwright 不适用）。禁止 smoke：断言必须是「离线容器真的装上了包」「校验和真的匹配」「技能文件真的含可用命令」。

## 8. 反范围

v1 不做：TLS、多租户/权限模型、Windows 目标、registry 高可用、包淘汰、`npm login` token 流程（发布自有 npm 包走 `pkgdepot publish` 之外的方式在文档中说明）。
