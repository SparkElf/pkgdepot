package install

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/SparkElf/pkgdepot/internal/remote"
	"github.com/SparkElf/pkgdepot/internal/spec"
)

// ApplyOptions 是 apply 的输入（冻结签名）。
type ApplyOptions struct {
	Host       *spec.Host
	BundlePath string
	Log        func(format string, args ...any)
	SkipVerify bool
}

// healthTimeout 是等待两个 registry 就绪的上限。install.sh 内部也等待 180s；
// 这里再等一轮是因为端口可达不等于服务已经能正确返回包内容（verdaccio 在
// 加载 storage 时可能短暂 503）。两段时间不叠加冲突：脚本里成功就直接返回。
const healthTimeout = 180 * time.Second

// applyState 记录一次 apply 过程中需要跨阶段共享的信息。
type applyState struct {
	host        *spec.Host
	bm          *spec.BundleManifest
	rawManifest []byte
	files       *deployFiles
	digest      string
}

// Apply 完成一次部署：上传 bundle → 写部署资产 → 执行 install.sh → 等健康 →
// （可选）verify → 返回 Inventory。
//
// 不写 hosts.yaml：主机的 LastApply 由 CLI 在拿到 Inventory 后回写，避免部署层
// 与配置层各写一次导致状态撕裂（并发 apply 时尤其明显）。
func Apply(ctx context.Context, opts ApplyOptions) (*spec.Inventory, error) {
	logf := opts.Log
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if opts.Host == nil {
		return nil, fmt.Errorf("apply: 主机信息为空")
	}
	if strings.TrimSpace(opts.BundlePath) == "" {
		return nil, fmt.Errorf("apply: bundle 路径为空")
	}

	// 先做本地校验再去连远端：bundle 损坏/不存在是最常见的失败，不该占用一次
	// SSH 会话，也不该在内网机器上留下半截文件。
	rawManifest, bm, err := readBundle(opts.BundlePath)
	if err != nil {
		return nil, err
	}
	stored := *opts.Host
	stored.ApplyDefaults()
	files, err := buildDeployFiles(&stored, bm, rawManifest)
	if err != nil {
		return nil, fmt.Errorf("apply: %w", err)
	}
	_, size, err := fileDigest(opts.BundlePath)
	if err != nil {
		return nil, err
	}

	logf("连接 %s@%s:%d", stored.User, stored.Address, stored.Port)
	client, err := remote.Dial(&stored)
	if err != nil {
		return nil, fmt.Errorf("apply: %w", err)
	}
	defer client.Close()

	// docker 不可用要在这里就失败：离线主机上这几乎总是"daemon 没起"或
	// "用户不在 docker 组"，继续走下去只会在 compose up 阶段报得更难懂。
	logf("检查目标机 docker")
	if err := client.DockerAvailable(ctx); err != nil {
		return nil, fmt.Errorf("apply: %w", err)
	}

	logf("创建远端目录 %s", stored.BaseDir)
	if err := client.MkdirAll(ctx, stored.BaseDir); err != nil {
		return nil, fmt.Errorf("apply: %w", err)
	}
	// 布局里的子目录先建好：install.sh 会往这些目录里合并内容，提前存在能给出
	// 更干净的错误（例如 baseDir 只读时会在这里失败，而不是在 tar 解包中途）。
	for _, dir := range []string{
		remotePath(stored.BaseDir, spec.RemoteNPMConfDir),
		remotePath(stored.BaseDir, spec.RemoteNPMStorage),
		remotePath(stored.BaseDir, spec.RemotePythonPkgs),
		remotePath(stored.BaseDir, spec.RemoteImagesDir),
		remotePath(stored.BaseDir, remoteHTTPClientDNC),
	} {
		if err := client.MkdirAll(ctx, dir); err != nil {
			return nil, fmt.Errorf("apply: %w", err)
		}
	}

	// 先传资产再传 bundle：资产是小文件，先落地可以让后面大文件的上传进度
	// 日志更有意义；且 install.sh 依赖同目录的 apply.env。
	logf("上传部署资产到 %s", stored.BaseDir)
	if err := uploadAssets(ctx, client, &stored, files); err != nil {
		return nil, fmt.Errorf("apply: %w", err)
	}

	logf("上传 bundle %s (%s) 到 %s", opts.BundlePath, humanBytes(size), remotePath(stored.BaseDir, remoteBundleFile))
	if err := client.UploadFile(ctx, opts.BundlePath, remotePath(stored.BaseDir, remoteBundleFile)); err != nil {
		return nil, fmt.Errorf("apply: %w", err)
	}

	logf("执行 install.sh（docker load + compose up）")
	out, err := client.Run(ctx, "sh "+shellSingle(remotePath(stored.BaseDir, spec.RemoteInstallFile)))
	if err != nil {
		return nil, fmt.Errorf("apply: 目标机安装脚本失败: %w", err)
	}
	if strings.TrimSpace(out) != "" {
		for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
			logf("  %s", line)
		}
	}

	logf("等待 registry 就绪")
	if err := waitHealthy(ctx, &stored, logf); err != nil {
		// 失败时把容器状态带回来：内网排障时操作员通常只能再看一次输出。
		if status, serr := containerStatus(ctx, client, &stored); serr == nil && status != "" {
			logf("容器状态:\n%s", status)
		}
		return nil, fmt.Errorf("apply: %w", err)
	}

	if opts.SkipVerify {
		logf("跳过校验（--skip-verify）：无法确认包内容完整")
		// 未校验也必须给 CLI 一个可回写的清单，否则 hosts.yaml 里的 inventory
		// 会与目标机实际内容脱节。这里用 bundle manifest 的"应该存在"集合。
		return inventoryFromManifest(bm), nil
	}

	logf("校验 registry 内容（逐字节）")
	inv, err := Verify(ctx, &stored, bm, logf)
	if err != nil {
		return nil, fmt.Errorf("apply: %w", err)
	}
	return inv, nil
}

// uploadAssets 按远端布局写入全部部署资产。
//
// permissions 有讲究：install.sh 必须可执行（即使 apply 用 sh 显式调用，
// 操作员手工 ./install.sh 也要能用）；verdaccio 以 root 运行，config 可读即可；
// htpasswd 归 root 且空内容。
func uploadAssets(ctx context.Context, c *remote.Client, h *spec.Host, files *deployFiles) error {
	type entry struct {
		rel  string
		data []byte
		mode os.FileMode
	}
	entries := []entry{
		{spec.RemoteInstallFile, files.InstallSH, modeExecutable},
		{remoteComposeFile, files.Compose, modeReadable},
		{spec.RemoteNPMConfig, files.VerdaccioConfig, modeReadable},
		{remotePath(spec.RemoteNPMConfDir, "htpasswd"), files.Htpasswd, modeReadable},
		{spec.RemoteManifestFile, files.BundleManifest, modeReadable},
		{remotePath(remoteHTTPClientDNC, "npmrc"), files.ClientNPMRC, modeReadable},
		{remotePath(remoteHTTPClientDNC, "pip.conf"), files.ClientPipConf, modeReadable},
		{remoteApplyEnv, files.ApplyEnv, modeReadable},
	}
	for _, e := range entries {
		remote := remotePath(h.BaseDir, e.rel)
		if err := c.WriteFile(ctx, remote, e.data, e.mode); err != nil {
			return fmt.Errorf("写入远端资产 %s: %w", remote, err)
		}
	}
	return nil
}

// waitHealthy 轮询两个 registry，直到都能正确响应或超时。
//
// 从 CLI 侧探测（而不是只在远端 curl）：这正是 verify 要走的网络路径，
// 能提前发现"目标机本机通、跨机不通"的防火墙问题。
func waitHealthy(ctx context.Context, h *spec.Host, logf func(string, ...any)) error {
	npmPing := strings.TrimRight(h.NPMURL(), "/") + "/-/ping"
	pythonSimple := h.PythonURL()
	targets := []struct {
		name string
		url  string
	}{
		{"verdaccio", npmPing},
		{"pypiserver", pythonSimple},
	}

	deadline := time.Now().Add(healthTimeout)
	pending := make([]bool, len(targets))
	for i := range pending {
		pending[i] = true
	}
	lastErr := make([]error, len(targets))

	httpClient := &http.Client{Timeout: 5 * time.Second}
	for {
		allUp := true
		for i, t := range targets {
			if !pending[i] {
				continue
			}
			err := probe(ctx, httpClient, t.url)
			if err == nil {
				pending[i] = false
				logf("%s 就绪: %s", t.name, t.url)
				continue
			}
			lastErr[i] = err
			allUp = false
		}
		if allUp {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if time.Now().After(deadline) {
			var b strings.Builder
			for i, t := range targets {
				if pending[i] {
					fmt.Fprintf(&b, "\n  %s (%s): %v", t.name, t.url, lastErr[i])
				}
			}
			return fmt.Errorf("registry 在 %s 内未就绪（从 CLI 侧探测失败，可能是端口未开放或防火墙）:%s",
				healthTimeout, b.String())
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// probe 判断一个健康检查 URL 是否可用。任何 2xx/3xx 都算就绪：
// verdaccio 的 /-/ping 返回 200 {}，pypiserver 的 /simple/ 返回 200 HTML。
func probe(ctx context.Context, c *http.Client, url string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("构造请求: %w", err)
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

// inventoryFromManifest 把 bundle 清单折算成 Inventory（跳过校验时使用）。
// 顺序按名字+版本排序，保证同样的 bundle 产出同样的 hosts.yaml。
func inventoryFromManifest(bm *spec.BundleManifest) *spec.Inventory {
	inv := &spec.Inventory{
		NPM:    make([]spec.PackageRef, 0, len(bm.NPM)),
		Python: make([]spec.PackageRef, 0, len(bm.Python)),
	}
	for _, e := range bm.NPM {
		inv.NPM = append(inv.NPM, spec.PackageRef{Name: e.Name, Version: e.Version})
	}
	for _, e := range bm.Python {
		inv.Python = append(inv.Python, spec.PackageRef{Name: e.Name, Version: e.Version})
	}
	sortRefs(inv.NPM)
	sortRefs(inv.Python)
	return inv
}

// containerStatus 取回目标机上的容器状态文本，用于 apply/status 的诊断输出。
func containerStatus(ctx context.Context, c *remote.Client, h *spec.Host) (string, error) {
	compose, err := composeCommand(ctx, c)
	if err != nil {
		return "", err
	}
	script := fmt.Sprintf("cd %s && %s -f %s -p %s ps 2>&1",
		shellSingle(h.BaseDir), compose, spec.RemoteComposeFile, shellSingle(projectName(h.BaseDir)))
	return c.Run(ctx, script)
}

// composeCommand 探测目标机上的 compose 调用形式。
// 与 install.sh 保持同一套优先级：优先 compose v2 插件，回退独立 docker-compose。
func composeCommand(ctx context.Context, c *remote.Client) (string, error) {
	out, err := c.Run(ctx, "docker compose version >/dev/null 2>&1 && echo plugin || (command -v docker-compose >/dev/null 2>&1 && echo standalone || echo none)")
	if err != nil {
		return "", fmt.Errorf("探测 docker compose: %w", err)
	}
	switch strings.TrimSpace(out) {
	case "plugin":
		return "docker compose", nil
	case "standalone":
		return "docker-compose", nil
	default:
		return "", fmt.Errorf("目标机没有 docker compose 插件，也没有 docker-compose")
	}
}

// fileDigest 计算 bundle 的 sha256 与大小。
func fileDigest(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, fmt.Errorf("打开 bundle %s: %w", path, err)
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, fmt.Errorf("读取 bundle %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// randomSecret 生成 32 字符的 storage secret。verdaccio 6 在 Node 22+ 上要求
// 恰好 32 字符，否则拒绝启动；本工具目前用 bundle 里的 db 文件，这里保留
// 生成能力给需要自建 db 的路径（例如目标机上没有任何 bundle 数据时的兜底）。
func randomSecret() (string, error) {
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("生成随机 secret: %w", err)
	}
	for i, b := range buf {
		buf[i] = alphabet[int(b)%len(alphabet)]
	}
	return string(buf), nil
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// errNotReady 让调用方可以区分"还没就绪"与"配置错误"。
var errNotReady = errors.New("尚未就绪")
