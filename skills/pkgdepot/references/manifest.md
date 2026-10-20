# pkgdepot.yaml 字段参考

清单是构建侧唯一的输入，`internal/spec` 在加载时校验；校验失败时 `pkgdepot plan` 与 `pkgdepot bundle` 都在开始工作前就退出。

## 完整形态

```yaml
apiVersion: pkgdepot/v1
kind: pkgdepot                 # 可选，纯说明字段
name: intra-repo               # 必填

npm:
  lockfiles:                   # 与 packages 二选一
    - ./app/package-lock.json
  packages:                    # 与 lockfiles 二选一
    - lodash@^4.17.0
    - "@scope/pkg@1.2.3"
  includeDev: false            # 是否包含 devDependencies

python:
  requirements:                # 与 packages 二选一
    - ./python/requirements.txt
  packages:                    # 与 requirements 二选一
    - requests==2.32.3
  platforms:                   # pip 的 platform tag，可多条
    - manylinux2014_x86_64
  pythonVersion: "3.12"
  implementation: cp

images:                        # 可选，覆盖两个 registry 镜像
  verdaccio: verdaccio/verdaccio:6
  pypiserver: pypiserver/pypiserver:latest
```

## 顶层

| 字段 | 必填 | 说明 |
|---|---|---|
| `apiVersion` | 是 | 必须为 `pkgdepot/v1` |
| `kind` | 否 | 说明字段，不参与行为 |
| `name` | 是 | 清单名，不能含空白或 `/`、`\`；默认 bundle 文件名用它 |
| `images` | 否 | 覆盖镜像引用；缺省为 `verdaccio/verdaccio:6`、`pypiserver/pypiserver:latest` |

## npm

| 字段 | 说明 |
|---|---|
| `lockfiles` | 路径列表，指向 `package-lock.json` 或 `package.json`；用 lockfile 得到上游已锁定的完整闭包 |
| `packages` | `name@range` 列表；range 省略时按 registry 的默认标签解析，每个包再展开传递依赖 |
| `includeDev` | 是否把 devDependencies 也纳入闭包；默认 `false` |

约束：

- `lockfiles` 与 `packages` 互斥，同时出现直接报错。
- `lockfiles` 里的路径相对**清单文件所在目录**；`package.json` 会先解析成 lockfile 再取闭包。
- 每个 `(name, version)` 只处理一次，闭包内版本去重。
- 写入 registry 存储时保留 `versions[x].dependencies` 与 `optionalDependencies`，并补 `dist.tarball`；这两项缺失会让内网 `npm install` 失败（见 troubleshooting）。

## python

| 字段 | 说明 |
|---|---|
| `requirements` | 路径列表，相对**清单文件所在目录** |
| `packages` | 直接列包名（可带 `==` 版本） |
| `platforms` | `pip download --platform` 的值，可多条；不写则不限定平台 |
| `pythonVersion` | 默认 `3.12`；对应 `pip download --python-version` |
| `implementation` | 默认 `cp`；对应 `pip download --implementation` |

约束：

- `requirements` 与 `packages` 互斥。
- `platforms` 是 pip 的平台标签（`manylinux2014_x86_64`、`musllinux_1_2_x86_64`、`win_amd64` 等），不是 `linux/amd64` 这种容器平台写法。
- 下载到的 wheel 文件名决定 inventory 里的包名与版本，所以 `platforms` 必须写成目标机真的装得上的标签；pip 找不到匹配 wheel 时这一步会失败。

## 平台标签怎么选

内网消费者的 libc 与 Python 版本决定标签：

| 内网消费者 | platforms | pythonVersion |
|---|---|---|
| Debian/Ubuntu 系、python:3.12-slim | `manylinux2014_x86_64`（或 `manylinux_2_17_x86_64`） | `3.12` |
| Alpine、python:3.12-alpine | `musllinux_1_2_x86_64` | `3.12` |
| 只有 3.11 运行时 | 同上，对应 manylinux/musl | `3.11` |

不同 Python 小版本的 wheel 不通用（`cp312` 装不进 3.11），manylinux 与 musllinux 也不通用。`platforms` 写多条只解决同一个 `pythonVersion` 下的多种 libc 平台；内网同时有 3.11 与 3.12 消费者时，按版本各建一个 bundle（清单里换 `pythonVersion`）。

## 路径解析

`lockfiles`、`requirements` 里的相对路径按**清单文件所在目录**解析，不是进程的当前工作目录。所以下面这条命令在哪个目录执行都一样，都会去 `/srv/pkgdepot-config/app/` 找 lockfile：

```bash
pkgdepot bundle -f /srv/pkgdepot-config/pkgdepot.yaml -o ./intra-repo.tar.gz
```

把所有清单与 lockfile/requirements 放在同一个可版本化的目录里，bundle 的输入才能复现。
