# pkgdepot 端到端验收

本目录是 pkgdepot 的系统测试。它是本仓库唯一的功能验收层级。

## 为什么不是 Playwright

本仓库是纯 CLI，没有 Web 前端，`product-design` / `test-governance` 里的
Playwright 系统测试口径不适用于「无 UI 的离线部署工具」。这里保留的是同一套
精神：**从用户的真实入口走完整闭环，断言用户可观察的真实结果**，而不是断言
内部函数、mock 调用或「进程还活着」。

## 真实环境（不是模拟）

- 真 `pkgdepot` 二进制（`go build` 出来，不是内存里调函数）
- 真 SSH：在 `127.0.0.1:2222` 起一个独立 sshd，用独立 host key 和授权密钥
- 真 Docker：目标机就是本机，通过 SSH 操作
- 真离线：消费者容器跑在 `--internal` 网络里，公网不可达；只能访问私有仓库
- 真包：npm 包与 PyPI wheel 都是真实下载的

## 用例（每个 case 对应一个用户目标）

| case | 用户目标 | 断言（可观察结果） |
|---|---|---|
| `TestApplyCreatesWorkingRegistries` | 运维把 bundle 部署到内网主机，两个私有仓库可用 | 离线 npm 容器成功 `npm install` 清单里的包；离线 python 容器成功 `pip install`；`verify` 逐字节通过 |
| `TestVerifyDetectsTamperedBundle` | 包被改动/缺失时能发现问题 | 校验失败且错误点名具体包 |
| `TestSkillExportGuidesMissingDeps` | 内网 AI 拿到技能后能判断包在不在、缺了怎么办 | 导出的 SKILL.md 含真实 registry URL、含 inventory 里的包名、含「回外网补包」的指引 |
| `TestReapplyIsIdempotent` | 重新部署同一/新 bundle 不破坏已有仓库 | 二次 apply 成功，之前能装的包仍能装，新包也可装 |
| `TestEnvAndStatus` | 运维能一键拿到客户端配置与运行状态 | `env` 输出可直接 source 的 registry 配置；`status` 反映真实容器与包数量 |

## 运行

```bash
cd pkgdepot
go test ./test/e2e/ -v -timeout 30m
```

前置：本机有 docker、sshd、node、python3；已 pull verdaccio 与 pypiserver 镜像。
harness 会自己起 sshd、建 docker 网络、清理容器，不依赖外部准备。

需要外网（构建 bundle 要下载真实包）。内网主机上的行为不依赖外网。
