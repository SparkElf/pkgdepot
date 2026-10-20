# pkgdepot 任务清单与并行计划

前置：`docs/architecture.md`（方案）、`docs/interfaces.md`（冻结签名）、`internal/spec`（唯一权威类型）。
任何人不得修改 `internal/spec` 或 `docs/interfaces.md`；需要变更先找 lead。

## 阶段 A：构建侧（bundle）

| # | 任务 | owner | 文件范围 | 完成条件 |
|---|---|---|---|---|
| A1 | npm 闭包解析 | builder | `internal/bundle/npm.go` | 从 `package.json`/`package-lock.json`/`name@range` 得到完整版本闭包（含 transitive deps），版本唯一、可复现 |
| A2 | npm 包下载与归档 | builder | `internal/bundle/npmseed.go` | 下载 tarball + 生成 verdaccio 存储布局（packument 含 `dist.tarball`、`dependencies/optionalDependencies` 保留、`_attachments`、`.verdaccio-db.json`） |
| A3 | python 闭包下载 | builder | `internal/bundle/python.go` | 按 `platforms/pythonVersion/implementation` 用 `pip download` 拉取全部 wheel，落 `python/packages` |
| A4 | 镜像导出 | builder | `internal/bundle/images.go` | `docker save` 出 `images/*.tar`，记录 sha256 |
| A5 | bundle 打包 | builder | `internal/bundle/bundle.go` | 组装目录、写 `manifest.json`、生成 `client/*`、打 tar.gz；重复构建产物清单稳定 |
| A6 | 构建侧单测 | builder | `internal/bundle/*_test.go` | 覆盖 packument 生成、闭包去重、manifest 往返 |

## 阶段 B：部署侧（apply / verify / skill）

| # | 任务 | owner | 文件范围 | 完成条件 |
|---|---|---|---|---|
| B1 | SSH 传输层 | deployer | `internal/remote/*.go` | key 认证连接、`Run` 执行、`UploadFile`/`WriteFile`/`ReadFile`/`MkdirAll`/`Stat` |
| B2 | 部署资产（compose/install.sh/config） | deployer | `internal/install/assets/*` | 目标机只用 `sh`+`docker`+`tar` 即可起服务；verdaccio root 运行；pypiserver 可读 |
| B3 | apply 流程 | deployer | `internal/install/apply.go` | 上传→解包→docker load→compose up→健康等待→（可选）verify |
| B4 | verify（CLI 侧 HTTP 逐字节校验） | deployer | `internal/install/verify.go` | 逐个 packument/tarball/wheel 校验 sha256 与 SRI，产出 Inventory |
| B5 | status/uninstall/env | deployer | `internal/install/status.go`、`internal/hosts/*.go` | 状态可读、卸载保留数据、env 输出可直接 source |
| B6 | 部署侧单测 | deployer | `internal/install/*_test.go`、`internal/remote/*_test.go`、`internal/hosts/*_test.go` | 覆盖环境文件生成、inventory 解析、hosts 往返 |

## 阶段 C：CLI + 技能 + 文档

| # | 任务 | owner | 文件范围 | 完成条件 |
|---|---|---|---|---|
| C1 | CLI 子命令装配 | lead | `internal/cli/*.go`、`cmd/pkgdepot/main.go` | 全部命令可用、退出码正确、错误可读 |
| C2 | 构建侧 DSH 技能 | docs | `skills/pkgdepot/SKILL.md` + `references/` | 指导 AI 从依赖清单产出 bundle；含真实命令与排错 |
| C3 | 技能导出模板 | docs | `internal/skillexport/*.go` + 模板 | 按主机+inventory 生成内网取包技能 |
| C4 | README + 快速开始 | docs | `README.md`、`docs/usage.md` | 构建者/运维/AI 三条路径各自可照做 |

## 阶段 D：系统测试（唯一门禁，全部代码完成后集中执行）

| # | 任务 | owner | 文件范围 | 完成条件 |
|---|---|---|---|---|
| D1 | e2e harness | lead | `test/e2e/*.go` | 起真 sshd（独立端口）、真 docker；跑完整链路 |
| D2 | 离线消费者断言 | lead | `test/e2e/*.go` | 在 `--internal` docker 网络内用 node/python 容器从私有仓库真实安装成功 |

## 集成顺序

1. lead 冻结 spec（已完成）
2. builder / deployer / docs 并行
3. lead 写 CLI 装配（依赖已冻结签名）
4. 全部完成后 lead 集中跑 e2e；失败按共同根因整批退回对应 owner
