# 包间接口合同（并行开发唯一准绳）

所有包只依赖 `internal/spec` 的类型。以下签名是已冻结的集成面，实现方不得修改签名；
确需变更先改本文件并通知 lead。

## internal/bundle（owner: builder）

```go
// Options 描述一次 bundle 构建。
type Options struct {
    Manifest     *spec.Manifest
    ManifestPath string   // 相对它解析 requirements/lockfile
    OutFile      string   // 输出 tar.gz 绝对路径
    ToolVersion  string
    Log          func(format string, args ...any)
}

// Build 解析闭包、下载包与镜像、写出 bundle，返回已写盘的 manifest。
func Build(ctx context.Context, opts Options) (*spec.BundleManifest, error)

// PlanOptions 只解析依赖闭包，不下载、不写盘，供 `pkgdepot plan` 使用。
type PlanOptions struct {
    Manifest     *spec.Manifest
    ManifestPath string   // requirements/lockfile 相对它解析
    Log          func(format string, args ...any)
}

// Plan 返回将要进入 bundle 的完整包清单；字段结构与 BundleManifest 的
// NPM/Python 一致，但 File/SHA256 允许为空（尚未下载）。
func Plan(ctx context.Context, opts PlanOptions) (*spec.BundleManifest, error)

// ReadManifest 从 bundle tar.gz 里读出 manifest.json，不落盘。
func ReadManifest(bundlePath string) (*spec.BundleManifest, string, error)
```

> `Build`/`Plan` 的 `Options`/`PlanOptions` 都带 `ManifestPath`：requirements 与
> lockfile 的路径按清单文件所在目录解析，而不是按进程工作目录。

bundle 目录结构（tar.gz 解包后）：

```
manifest.json
images/verdaccio.tar
images/pypiserver.tar
npm/storage/<pkg>/package.json
npm/storage/<pkg>/<pkg>-<ver>.tgz
npm/storage/.verdaccio-db.json
python/packages/*.whl
python/packages/*.tar.gz
client/npmrc
client/pip.conf
```

- `npm/storage` 必须符合 verdaccio 落盘格式：packument 的 `dist.tarball` 写
  `http://localhost:<port>/<name>/-/<file>`（verdaccio 会按请求 Host 重写）；
  `versions[v].dependencies/optionalDependencies` 必须原样保留。
- `client/npmrc` / `client/pip.conf` 用占位符，apply 时替换为真实值：
  - `{{NPM_URL}}`   → npm registry 完整 URL（含端口、结尾 `/`）
  - `{{PYTHON_URL}}`→ pip index 完整 URL（含端口、`/simple/`）
  - `{{PYTHON_HOST}}`→ 纯主机名/IP（不含 scheme 与端口），用于 `trusted-host`
- `NPMEntry.File` / `PyEntry.File` 是**文件 basename**，不是 bundle 内相对路径：
  npm 用 `<name>-<version>.tgz`（scoped 包与上游约定一致，如 `node-22.10.2.tgz`），
  python 用 wheel/sdist 文件名。verify 用它拼 `<npmURL><name>/-/<file>` 与 simple 索引文件名。

## internal/remote（owner: deployer）

```go
type Client struct{ /* ... */ }
func Dial(h *spec.Host) (*Client, error)
func (c *Client) Close() error
// Run 在目标机用 /bin/sh -c 执行脚本，返回合并输出；非零退出返回 error（含输出）。
func (c *Client) Run(ctx context.Context, script string) (string, error)
func (c *Client) RunScript(ctx context.Context, script string) (string, error) // 同上，便于要求 stdin 形态
func (c *Client) UploadFile(ctx context.Context, localPath, remotePath string) error
func (c *Client) WriteFile(ctx context.Context, remotePath string, data []byte, mode fs.FileMode) error
func (c *Client) ReadFile(ctx context.Context, remotePath string) ([]byte, error)
func (c *Client) MkdirAll(ctx context.Context, remotePath string) error
func (c *Client) Stat(ctx context.Context, remotePath string) (os.FileInfo, error)
func (c *Client) DockerAvailable(ctx context.Context) error
```

## internal/install（owner: deployer）

```go
type ApplyOptions struct {
    Host       *spec.Host
    BundlePath string
    Log        func(format string, args ...any)
    SkipVerify bool
}

// Apply 上传 bundle、上传内嵌部署资产、执行 install.sh、等待健康、验证并返回 Inventory。
func Apply(ctx context.Context, opts ApplyOptions) (*spec.Inventory, error)

// Verify 通过真实 HTTP 逐字节校验目标主机上的包，产出 Inventory。
func Verify(ctx context.Context, h *spec.Host, bm *spec.BundleManifest, log func(string, ...any)) (*spec.Inventory, error)

// Status 返回目标主机的可读状态文本。
func Status(ctx context.Context, h *spec.Host) (string, error)

// Uninstall 停止并移除容器，保留数据目录。
func Uninstall(ctx context.Context, h *spec.Host) error
```

## internal/skillexport（owner: docs）

```go
type Options struct {
    Host      *spec.Host
    HostName  string
    OutDir    string
    Inventory *spec.Inventory
    ToolVersion string
}
// Write 生成内网 AI 技能目录（SKILL.md + references/），返回写入的文件列表。
func Write(ctx context.Context, opts Options) ([]string, error)
```

## internal/hosts（owner: deployer）

```go
func Path() (string, error)                          // os.UserConfigDir()/pkgdepot/hosts.yaml
func Load() (*spec.HostsFile, error)                 // 文件缺失 = 空清单，不是错误
func Save(hf *spec.HostsFile) error                  // 0600
func Add(name string, h *spec.Host) error            // 覆盖同名；内部调用 h.ApplyDefaults()
func Remove(name string) error
func Get(name string) (*spec.Host, error)            // 内部调用 h.ApplyDefaults()
func Update(name string, h *spec.Host) error         // 已存在才更新；apply/verify 后回写 LastApply
```

## CLI 命令面（owner: lead，实现于 internal/cli）

```
pkgdepot plan    -f pkgdepot.yaml
pkgdepot bundle  -f pkgdepot.yaml -o <file>
pkgdepot host add <name> --address --user [--port --key --base-dir --npm-port --python-port]
pkgdepot host list
pkgdepot host remove <name>
pkgdepot apply   --host <name> --bundle <file> [--skip-verify]
pkgdepot status  --host <name>
pkgdepot verify  --host <name> --bundle <file>
pkgdepot env     --host <name> [--npm] [--python]
pkgdepot skill export --host <name> -o <dir>
pkgdepot uninstall --host <name>
pkgdepot version
```
