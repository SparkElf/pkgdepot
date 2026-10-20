# 故障排查

按「哪一步出错」组织。每条都给出可观察的症状、判定方法和处理动作。先看下面这张表定位，再读对应小节。

| 症状 | 出错位置 | 跳到 |
|---|---|---|
| `pkgdepot apply` 一开始就失败，提到 docker | 目标机 docker | [目标机 docker 不可用](#目标机-docker-不可用) |
| 容器起不来，verdaccio 日志报权限 | 挂载目录权限 | [verdaccio 权限](#verdaccio-挂载目录权限) |
| `/simple/` 返回 500，容器却是 running | pypiserver 目录权限 | [pypiserver /simple/ 500](#pypiserver-simple-返回-500) |
| `pip install` 报 `No matching distribution found` | 平台标签 | [pip 平台标签不匹配](#pip-平台标签不匹配) |
| `npm install` 报 `no dist.tarball field` | bundle 播种数据 | [no dist.tarball field](#npm-报-no-disttarball-field) |
| 主包装上了，依赖的包找不到 | bundle 播种数据 | [传递依赖缺失](#传递依赖缺失) |
| apply 卡在健康检查或端口被占用 | 端口冲突 | [端口冲突](#端口冲突) |
| 逐字节校验失败，点名某个包 | 数据损坏或传输损坏 | [verify 校验失败](#verify-校验失败) |

---

## 目标机 docker 不可用

**症状**：`pkgdepot apply --host <name> --bundle <file>` 连上 SSH 后立刻返回错误，提示 docker 不可用；仓库根本没开始部署。

**判定**：在运维机上用同一套 SSH 身份执行：

```bash
ssh -i ~/.ssh/id_ed25519 ops@10.20.30.40 'docker version --format "{{.Server.Version}}"'
```

三种结果对应三种原因：

| 输出 | 原因 | 处理 |
|---|---|---|
| `permission denied while trying to connect to the Docker daemon socket` | SSH 用户不在 docker 组 | 目标机上 `sudo usermod -aG docker ops`，重新登录使组生效 |
| `Cannot connect to the Docker daemon at unix:///var/run/docker.sock` | daemon 没运行 | `sudo systemctl start docker && sudo systemctl enable docker` |
| `docker: command not found` | 没装 docker | 目标机是离线环境，需从内网镜像源安装或预先装好；pkgdepot 不会替你装 docker |

**为什么不能绕过**：pkgdepot 的 registry 以容器形式运行，`docker load` + `docker compose up` 是唯一路径（ADR-3/ADR-4）。没有 docker 就没有别的实现方式。

修好后重新执行原来的 `apply` 命令，不需要先清理。

---

## verdaccio 挂载目录权限

**症状**：容器启动后立即退出，`pkgdepot apply` 在健康等待阶段超时。目标机上 `docker compose logs` 能看到 verdaccio 写 storage 目录失败。

**原因**：verdaccio 镜像默认以 uid `10001` 运行，bind mount 的宿主机目录通常不属于该 uid，写不进去。

**pkgdepot 的处理**：apply 上传的 `compose.yaml` 让 verdaccio 以 root 运行（`user: "0:0"`），因此标准流程不需要在目标机上做任何 `chown`。这是有意的设计取舍（ADR-6）：内网隔离环境下用 root 规避 bind mount 的 uid 问题，`install.sh` 也不需要 sudo。

**什么情况下会踩到**：

- 手工改过目标机上的 `compose.yaml`，把 `user: "0:0"` 删了；
- 用 `chown -R` 改过数据目录的属主，把挂载点改成了容器内 root 也写不进去的状态；
- 用 `--base-dir` 指向了 NFS 一类对 root squash 的挂载。

**处理**：让 apply 重新上传并覆盖部署资产（再执行一次 `pkgdepot apply --host <name> --bundle <file>`），确认 `--base-dir` 在本地文件系统上。

---

## pypiserver `/simple/` 返回 500

**症状**：`curl http://<host>:8080/simple/` 返回 HTTP 500，但容器状态是 running；`pkgdepot apply` 的验证阶段报错，点名某个 python 包取不到。

**原因**：pypiserver 默认以非 root 用户运行，`python/packages` 目录对它不可读。容器活着，但索引无法枚举。

**判定**：

```bash
ssh -i ~/.ssh/id_ed25519 ops@10.20.30.40 \
  'docker compose -f /opt/pkgdepot/compose.yaml logs --tail=20'
```

日志里出现 permission denied 即可确认。

**处理**：恢复 `python/packages` 目录的可读权限，重新 apply。不要用 `docker restart` 掩盖：权限问题在下次重建容器时仍然复现，而且 `verify` 不会通过。

另一个相关坑：pypiserver 必须带 `-P .` 参数才能在没有 `.htpasswd` 的情况下启动；缺了它容器直接起不来。pkgdepot 生成的 `compose.yaml` 已包含该参数，手工改过启动命令的话补回来。

---

## pip 平台标签不匹配

**症状**：内网消费者执行 `pip install --index-url http://<host>:8080/simple/ --trusted-host <host> <包名>` 报：

```
ERROR: Could not find a version that satisfies the requirement <包名>
ERROR: No matching distribution found for <包名>
```

**这不是网络问题，也不是 registry 问题**：索引里没有匹配当前解释器与平台的 wheel。

**两个高频原因**：

1. **Python 版本不一致**：消费者是 3.12（`cp312`），bundle 里只有 3.11（`cp311`）的 wheel。
2. **libc 不一致**：消费者是 Alpine（musl），bundle 里只有 manylinux（glibc）的 wheel；或反过来。

**判定**：先在消费者机器上确认解释器与平台：

```bash
python3 -VV
python3 -c 'import sysconfig; print(sysconfig.get_platform())'
```

`python3 -VV` 输出的版本号决定 `pythonVersion`；容器基础镜像决定 manylinux 还是 musllinux。逐字节校验只证明「bundle 里的包都在」——它证明不了「这个包在消费者平台上装得上」，所以平台标签只能靠这一节核对。

**处理**：回外网构建机，把清单里的平台字段改成与消费者一致，重新构建：

```yaml
python:
  packages:
    - requests==2.32.3
  platforms:
    - musllinux_1_2_x86_64     # Alpine 消费者；glibc 消费者用 manylinux2014_x86_64
  pythonVersion: "3.12"
  implementation: cp
```

```bash
pkgdepot bundle -f pkgdepot.yaml -o intra-repo-2.tar.gz
```

回到内网：

```bash
pkgdepot apply --host intra-01 --bundle intra-repo-2.tar.gz
```

apply 幂等，已有包保留，新的平台 wheel 叠加进去。

**一台主机要同时服务两种 libc 怎么办**：`platforms` 写多条（manylinux + musllinux），一次 bundle 同时带两套 wheel。Python 小版本不同则无法用同一份 bundle 解决：按 3.11 与 3.12 各建一个 bundle，或把内网版本统一。

---

## npm 报 `no dist.tarball field`

**症状**：内网容器里 `npm install` 报：

```
npm error code ENOENT
npm error ... no dist.tarball field ...
```

**原因**：消费者拿到的 packument（`http://<host>:4873/<包名>`）里，对应版本的 `dist` 对象缺少 `tarball`。verdaccio 播种的元数据不完整时会出现。

**判定**：

```bash
curl -fsS http://<host>:4873/<包名> | python3 -m json.tool | grep -A5 '"dist"'
```

对应版本里没有 `tarball` 字段即可确认。

**这属于构建侧缺陷**，不是客户端问题，也不是 verdaccio 配置问题：正常的播种 packument 必须带 `dist.tarball`（verdaccio 会按请求方的 Host 重写它，因此播种时写 `localhost` 也能工作）。

**处理**：在外网重建 bundle，然后重新 apply。如果手上有 bundle 但是想先确认播种数据，解包后看：

```bash
tar -xzf intra-repo.tar.gz -C /tmp/b
python3 -c 'import json;print(json.load(open("/tmp/b/npm/storage/<包名>/package.json"))["versions"])'
```

---

## 传递依赖缺失

**症状**：

- 主包能装上，但它 `dependencies` 里的包报 404；
- 或 `npm install` 只装了顶层包，`node_modules` 里少了依赖包的依赖；
- 或 `npm ls` 报 `missing`。

**原因**：播种的 packument 里 `versions[x]` 保留了 `name`/`version`/`dist` 却丢了 `dependencies` / `optionalDependencies`。npm 解析依赖树时只信 packument 里的这两个字段，看不到就不会去装，且不会主动报「元数据缺字段」，表现为静默少包。

**判定**：

```bash
curl -fsS http://<host>:4873/<包名> | python3 -c '
import json,sys
p=json.load(sys.stdin)
v=p["versions"][p["dist-tags"]["latest"]]
print("dependencies:", v.get("dependencies"))
'
```

真实发布过的包应该有非空 `dependencies`（除非它确实没有依赖）；输出 `None` 而包本身有依赖即可确认。

**处理**：同 `no dist.tarball field`——构建侧缺陷，重建 bundle 后重新 apply。修复后 `pkgdepot verify` 会重新跑通逐字节校验。

---

## 端口冲突

**症状**：`pkgdepot apply` 失败，或卡在健康等待直到超时。目标机上 `docker compose up` 报端口已被占用。

**判定**：先 `pkgdepot host list` 看该主机登记的两个端口，再在目标机上确认谁占用了它们（下面的 `4873` / `8080` 替换成实际端口）：

```bash
ss -lntp | grep -E ':(4873|8080)\b'
```

**处理**：换端口重新登记主机（同名覆盖），再 apply：

```bash
pkgdepot host add intra-01 --address 10.20.30.40 --user ops \
  --npm-port 14873 --python-port 18080
pkgdepot apply --host intra-01 --bundle intra-repo.tar.gz
```

`host add` 对同名条目是覆盖语义，不需要先 `host remove`。改完端口后客户端配置也会跟着变：重新 `eval "$(pkgdepot env --host intra-01)"`，并重新 `pkgdepot skill export --host intra-01 -o <dir>`，否则内网 AI 手上的技能还指向旧端口。

端口变更后必须重新 apply：旧的 compose 配置仍在目标机上，只改 hosts.yaml 不会自动迁移已部署的容器。

---

## verify 校验失败

**症状**：`pkgdepot verify` 或 apply 末尾的验证阶段失败，错误里点名了具体的包与文件，例如某个 tarball 的 sha256 与 `manifest.json` 不符。

**判定顺序**：

1. **bundle 本身损坏**：在外网构建机上重新解包算 sha256（见 [skills/pkgdepot/references/troubleshooting.md](../skills/pkgdepot/references/troubleshooting.md) 的脚本）。构建机上就坏，说明下载或写盘坏了，重建 bundle。
2. **传输损坏**：内网侧 `sha256sum -c intra-repo.tar.gz.sha256` 不通过，重新摆渡。
3. **目标机上的数据被改过**：bundle 与传输都没问题，但远端存储与清单不一致。重新 apply 会用 bundle 覆盖远端数据。

**不要**为了“让命令过”而加 `--skip-verify`：它跳过的是发现问题的步骤，包本身该是错的还是错的，问题只会在内网消费者那里以更难查的形式出现。

---

## 找不到某台主机

**症状**：`pkgdepot apply --host intra-01 ...` 报 `unknown host "intra-01"`，并列出已知主机名。

**原因**：`hosts.yaml` 里没有这个条目。常见于换了用户或换了 `HOME`：配置文件在 `~/.config/pkgdepot/hosts.yaml`，`sudo` 之后读的是 root 的配置。

**处理**：用当前用户重新 `pkgdepot host add`；或 `pkgdepot host list` 确认当前账号能看到什么。

---

## 仍然无法定位

收集这三样再排查：

```bash
pkgdepot status --host <name>
pkgdepot verify --host <name> --bundle <file>
ssh -i <key> <user>@<host> 'docker compose -f <base-dir>/compose.yaml ps; docker compose -f <base-dir>/compose.yaml logs --tail=50'
```

`status` 告诉你服务在不在，`verify` 告诉你数据对不对，compose 日志告诉你容器内部发生了什么。三者能区分「服务没起来」「数据不对」「客户端配置错」三类问题。
