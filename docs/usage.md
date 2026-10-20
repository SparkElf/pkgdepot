# 使用指南

pkgdepot 有三条使用路径，分别对应三类人。每条路径都可以独立照做，命令与 flag 与 [interfaces.md](interfaces.md) 的命令面一致。

| 路径 | 在哪执行 | 需要什么 | 产出 |
|---|---|---|---|
| 构建者（外网） | 联网构建机 | `pkgdepot`、`docker`、`npm`、`pip` | 一个 bundle tar.gz |
| 运维（内网部署） | 内网运维机 + 目标主机 | `pkgdepot`、SSH 到目标机、目标机有 docker | 两个可用 registry + 已验证 inventory |
| AI Agent（内网取包） | 内网开发机 | 导出的技能目录 + 客户端工具链 | 从私有仓库装包；缺包时知道去哪补 |

---

## 路径一：构建者（外网）

目标：把一份依赖清单变成自包含的离线 bundle。完整产物既是「哪些包」的清单，也是「怎么验证」的凭据。

### 1. 准备清单

在项目里放一个 `pkgdepot.yaml`。两种 npm 输入选一种，两种 python 输入选一种：

```yaml
apiVersion: pkgdepot/v1
name: intra-repo

npm:
  lockfiles:
    - ./app/package-lock.json
  includeDev: false

python:
  requirements:
    - ./python/requirements.txt
  platforms:
    - manylinux2014_x86_64
  pythonVersion: "3.12"
  implementation: cp
```

如果不想用 lockfile，直接把要的包列出来即可（闭包仍会展开到传递依赖）：

```yaml
npm:
  packages:
    - lodash@^4.17.0
    - "@scope/pkg@1.2.3"
```

`lockfiles` / `requirements` 里的相对路径按**清单文件所在目录**解析。字段细节与平台标签选择见 [skills/pkgdepot/references/manifest.md](../skills/pkgdepot/references/manifest.md)。

只有 npm 或只有 python 也可以，另一边留空：

```yaml
apiVersion: pkgdepot/v1
name: npm-only
npm:
  packages:
    - lodash@^4.17.0
```

### 2. 先 plan 再 bundle

```bash
pkgdepot plan -f pkgdepot.yaml
```

输出示例：

```
清单: intra-repo (/srv/config/pkgdepot.yaml)
镜像: verdaccio/verdaccio:6, pypiserver/pypiserver:latest
npm:  42 个包
  lodash@4.17.21
  ...
python: 7 个文件
  requests-2.32.3-py3-none-any.whl (2.32.3)
  ...
```

`plan` 只解析、不产出 bundle 文件，用来确认闭包范围符合预期——包数量明显不对时先修清单，别急着跑几百 MB 的构建。

确认无误后构建：

```bash
pkgdepot bundle -f pkgdepot.yaml -o intra-repo.tar.gz
```

输出：

```
bundle: /srv/build/intra-repo.tar.gz (312577894 bytes)
  images: 2
  npm:    42
  python: 7

下一步: 把该文件拷入内网，执行 pkgdepot apply --host <name> --bundle intra-repo.tar.gz
```

### 3. 校验产物

bundle 是标准 tar.gz，`manifest.json` 是权威清单：

```bash
tar -tzf intra-repo.tar.gz | head
mkdir -p /tmp/b && tar -xzf intra-repo.tar.gz -C /tmp/b
python3 -m json.tool /tmp/b/manifest.json | head -30
```

`manifest.json` 的结构：

```json
{
  "apiVersion": "pkgdepot.bundle/v1",
  "name": "intra-repo",
  "createdAt": "...",
  "toolVersion": "...",
  "images": [{ "ref": "verdaccio/verdaccio:6", "file": "images/verdaccio.tar", "size": 232284160, "sha256": "..." }],
  "npm":    [{ "name": "lodash", "version": "4.18.1", "file": "lodash-4.18.1.tgz", "sha256": "...", "integrity": "sha512-...", "metadataJson": "{\"dist\":{\"tarball\":\"http://localhost:4873/lodash/-/lodash-4.18.1.tgz\",...},...}" }],
  "python": [{ "name": "requests", "version": "2.32.3", "file": "requests-2.32.3-py3-none-any.whl", "sha256": "..." }]
}
```

注意 `npm[].file` 与 `python[].file` 是**文件名**（不含目录），实际位置由布局决定：npm tarball 在 `npm/storage/<包名>/` 下，python 文件在 `python/packages/` 下。`images[].file` 则带 `images/` 前缀。

抽查字节（可在离开构建机前发现传输/下载损坏）：

```bash
cd /tmp/b && sha256sum images/*.tar python/packages/*.whl | head
```

### 4. 带进内网

```bash
sha256sum intra-repo.tar.gz > intra-repo.tar.gz.sha256
```

把三样东西带进去：bundle、`pkgdepot` 二进制、校验和文件。拷入后再算一次 `sha256sum -c intra-repo.tar.gz.sha256`，不一致就重新摆渡，不要继续部署。

构建机的 `~/.config/pkgdepot/hosts.yaml` 不要带——它描述的是构建机视角的主机，内网侧重新 `host add`。

---

## 路径二：运维（内网部署）

目标：在一台有 docker 的内网主机上建起两个私有仓库，并拿到经过逐字节校验的包清单。

### 1. 登记主机

```bash
pkgdepot host add intra-01 \
  --address 10.20.30.40 \
  --user ops \
  --key ~/.ssh/id_ed25519
```

全部可选参数与默认值：

| flag | 默认 | 说明 |
|---|---|---|
| `--port` | `22` | SSH 端口 |
| `--key` | 无 | SSH 私钥路径；不写则按本机 SSH 的默认身份尝试连接 |
| `--base-dir` | `/opt/pkgdepot` | 远端部署目录 |
| `--npm-port` | `4873` | verdaccio 对外端口 |
| `--python-port` | `8080` | pypiserver 对外端口 |

主机配置写入 `~/.config/pkgdepot/hosts.yaml`（`0600`）。同名 host 会被覆盖，所以改端口就是重新 `host add` 一次。

确认身份与连通性：

```bash
pkgdepot host list
ssh -i ~/.ssh/id_ed25519 -p 22 ops@10.20.30.40 'docker version'
```

`docker version` 走不通时先解决 docker 权限（用户需能免 sudo 执行 docker），否则 apply 会在第一步失败。

### 2. apply

```bash
pkgdepot apply --host intra-01 --bundle intra-repo.tar.gz
```

apply 依次做：

1. 检查目标机 docker 可用。
2. 在 `--base-dir` 下创建目录结构。
3. 上传 bundle 与 CLI 内嵌的部署资产（`install.sh`、`compose.yaml`、registry 配置、实际地址的客户端配置）。
4. 执行 `install.sh`：解包 → `docker load` 两个镜像 → `docker compose up -d` → 等待健康。
5. 轮询两个 registry 直到可访问。
6. 逐字节验证每个包，失败即报错并点名具体包。

成功输出：

```
主机 "intra-01" 已就绪
  npm      http://10.20.30.40:4873/
  python   http://10.20.30.40:8080/simple/
  npm 包   42 个
  python 包 7 个

在目标网段内配置客户端:  eval "$(pkgdepot env --host intra-01)"
为内网 AI 生成取包技能:  pkgdepot skill export --host intra-01 -o ./skills/intra-01-pkgdepot
```

`--skip-verify` 跳过第 6 步（仅在你已单独验证过、或包特别多想先让服务起来时使用；之后可以补跑 `pkgdepot verify`）。

### 3. 验证与状态

只验证、不重新部署：

```bash
pkgdepot verify --host intra-01 --bundle intra-repo.tar.gz
```

输出 `校验通过: npm N 个版本, python M 个文件`，同时把 inventory 写回主机条目。这是部署后最可信的「仓库里确实有什么」的依据。

```bash
pkgdepot status --host intra-01
```

确认两个 registry 地址、容器状态与包数量。

### 4. 客户端配置

```bash
eval "$(pkgdepot env --host intra-01)"
npm install lodash
pip install requests
```

`env` 输出的是可以直接 source 的 `export` 语句：`NPM_CONFIG_REGISTRY`、`PIP_INDEX_URL`、`PIP_TRUSTED_HOST`。只想要其中一个生态时加 `--npm` 或 `--python`。

等价的持久配置（不依赖 `eval`）：

```ini
# ~/.npmrc
registry=http://10.20.30.40:4873/
```

```ini
# ~/.config/pip/pip.conf
[global]
index-url = http://10.20.30.40:8080/simple/
trusted-host = 10.20.30.40
```

`trusted-host` 是必需的：registry 是内网 HTTP，没有 TLS。

### 5. 增量更新

新增包时不要卸载重装：在外网重建 bundle，然后对同一主机再 apply 一次。

```bash
pkgdepot apply --host intra-01 --bundle intra-repo-2.tar.gz
```

apply 是幂等的：新包叠加到已有存储上，之前能装的包继续能装。数据目录（`--base-dir`）在 apply 与 uninstall 之间始终保留。

### 6. 停止服务

```bash
pkgdepot uninstall --host intra-01
```

停止并移除容器，保留数据目录。重新 `apply` 会直接复用已有存储。

---

## 路径三：AI Agent（内网取包）

目标：让内网的 AI 知道「这台主机的私有仓库里有哪些包、怎么装、缺了怎么办」，并且给出的命令真的能执行。

### 1. 导出技能

在运维机上（能读到 `hosts.yaml` 的那台）：

```bash
pkgdepot skill export --host intra-01 -o ./skills/intra-01-pkgdepot
```

生成的目录就是一个完整的技能目录（`-o` 指向它，`SKILL.md` 落在该目录根上）：

```
skills/
└── intra-01-pkgdepot/
    ├── SKILL.md
    └── references/
        ├── packages.md
        └── troubleshooting.md
```

技能名由主机名推导：小写、非法字符换成 `-`，固定以 `-pkgdepot` 结尾（`intra-01` → `intra-01-pkgdepot`）。`-o` 省略时用它作为相对目录名。

导出内容的两个决定因素：

- registry 地址来自主机配置（`pkgdepot env` 同源）；
- 包清单来自该主机最近一次 `apply` / `verify` 的 inventory。

因此**先 apply 或 verify，再 export**：没有 inventory 时 `SKILL.md` 会明确写「尚无校验过的清单」，而不是编造包名；inventory 为空时也会单独说明。

把整个目录交给内网 AI 的技能加载目录，或以项目内技能的形式随代码仓库分发。

### 2. 技能内容

`SKILL.md`：

- 两个 registry 地址（npm / python）
- `.npmrc` 与 `pip.conf` 配置片段
- 真实的 npm（`name@version`）与 Python（`name==version`）清单
- 依赖缺失时的固定排查顺序，终点是「回外网侧补包、重建 bundle、重新 apply」

`references/packages.md`：逐包表格，含每个包的完整安装命令和一次性安装全部包的命令行。

`references/troubleshooting.md`：registry 连不上、`No matching distribution`、端口冲突等内网侧故障的定位方法。

### 3. 补包的闭环

内网 AI 判断缺包后，动作固定为四步，全部在能连线两侧的人手上完成：

```bash
# 外网构建机：把包加进 pkgdepot.yaml 后重建
pkgdepot bundle -f pkgdepot.yaml -o intra-repo-2.tar.gz

# 内网运维机：增量部署
pkgdepot apply --host intra-01 --bundle intra-repo-2.tar.gz

# 重新导出，让技能里的清单与新 inventory 对齐
pkgdepot skill export --host intra-01 -o ./skills/intra-01-pkgdepot
```

apply 完成后旧的包仍然可用，不需要重装已有依赖。

### 4. 技能的时效性

技能内容是某一次 inventory 的快照。以下情况需要重新 `skill export`：

- 主机重新 apply 或 verify 过；
- 主机端口、地址变了（`host add` 覆盖后）；
- 技能里的包清单与 `pkgdepot verify` 结果不一致。

建议把导出的技能目录纳入版本管理，bundle 重建后一起提交，这样「技能说的」与「仓库有的」始终同步。

---

## 排错

三类故障的完整清单见 [troubleshooting.md](troubleshooting.md)。按发生位置分：

- 构建侧（plan/bundle 失败、平台标签不匹配、docker save 权限）：[skills/pkgdepot/references/troubleshooting.md](../skills/pkgdepot/references/troubleshooting.md)
- 部署侧（docker 不可用、端口冲突、verdaccio/pypiserver 权限）：[troubleshooting.md](troubleshooting.md)
- 内网使用侧（连不上、装不上、缺包）：`pkgdepot skill export` 生成的 `references/troubleshooting.md`
