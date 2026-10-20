package install

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/SparkElf/pkgdepot/internal/remote"
	"github.com/SparkElf/pkgdepot/internal/spec"
)

// Status 返回目标主机的可读状态文本。
//
// 尽量在目标机上取真实状态（容器是否在跑、镜像是否已加载、数据目录有多大），
// 而不是只读本地 hosts.yaml：状态命令的价值就在于回答"那台机器现在到底怎么样"。
// 单个检查失败不终止整体：例如数据目录读不到时仍然应该输出容器状态。
func Status(ctx context.Context, h *spec.Host) (string, error) {
	if h == nil {
		return "", fmt.Errorf("status: 主机信息为空")
	}
	stored := *h
	stored.ApplyDefaults()

	client, err := remote.Dial(&stored)
	if err != nil {
		return "", fmt.Errorf("status: %w", err)
	}
	defer client.Close()

	var b strings.Builder
	fmt.Fprintf(&b, "主机 %s@%s:%d\n", stored.User, stored.Address, stored.Port)
	fmt.Fprintf(&b, "baseDir  %s\n", stored.BaseDir)
	fmt.Fprintf(&b, "npm      %s\n", stored.NPMURL())
	fmt.Fprintf(&b, "python   %s\n", stored.PythonURL())

	// docker 不可用时后面的检查都没有意义，但仍输出已收集的地址信息。
	if err := client.DockerAvailable(ctx); err != nil {
		fmt.Fprintf(&b, "\ndocker   ✗ %v\n", err)
		return b.String(), nil
	}
	version, verr := client.Run(ctx, "docker version --format '{{.Server.Version}}'")
	if verr == nil {
		fmt.Fprintf(&b, "docker   %s\n", strings.TrimSpace(version))
	}

	// 部署目录是否存在决定"这台机器是否 apply 过"。
	deployed := false
	info, statErr := client.Stat(ctx, remotePath(stored.BaseDir, spec.RemoteManifestFile))
	switch {
	case statErr != nil:
		fmt.Fprintf(&b, "\n部署状态 未部署（%s 下没有 %s）\n", stored.BaseDir, spec.RemoteManifestFile)
	case info != nil:
		deployed = true
		fmt.Fprintf(&b, "\n部署状态 已部署（manifest.json %s 字节）\n", humanBytes(info.Size()))
	}

	// 最近一次 apply 的 bundle 名与时间来自本地记录（CLI 回写到 hosts.yaml）。
	if stored.LastApply != nil {
		fmt.Fprintf(&b, "上次部署 %s（%s）\n",
			stored.LastApply.BundleName, stored.LastApply.AppliedAt.Format("2006-01-02 15:04:05"))
		fmt.Fprintf(&b, "已校验包 npm %d 个版本 / python %d 个文件\n",
			len(stored.LastApply.Inventory.NPM), len(stored.LastApply.Inventory.Python))
	}

	b.WriteString("\n容器:\n")
	if !deployed {
		b.WriteString("  （未部署，跳过）\n")
	} else if statusText, cerr := containerStatus(ctx, client, &stored); cerr != nil {
		fmt.Fprintf(&b, "  （无法获取：%v）\n", cerr)
	} else {
		for _, line := range strings.Split(strings.TrimRight(statusText, "\n"), "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			fmt.Fprintf(&b, "  %s\n", line)
		}
	}

	// 从 CLI 侧探一下端口：容器 running 不等于服务可达（例如 pypiserver 索引 500）。
	// 未部署的主机不做探测：目标机上根本没有这两个端口，探测只会在网络层等超时，
	// 让 `status` 在"机器还没准备好"这个最常见场景下白等十几秒。
	if deployed {
		b.WriteString("\n可达性:\n")
		probeClient := &http.Client{Timeout: 3 * time.Second}
		for _, t := range []struct{ name, url string }{
			{"npm   ", strings.TrimRight(stored.NPMURL(), "/") + "/-/ping"},
			{"python", stored.PythonURL()},
		} {
			if err := probe(ctx, probeClient, t.url); err != nil {
				fmt.Fprintf(&b, "  %s ✗ %s（%v）\n", t.name, t.url, err)
			} else {
				fmt.Fprintf(&b, "  %s ✓ %s\n", t.name, t.url)
			}
		}
	}

	// 数据目录用量：增量 apply 多了以后内网机器磁盘可能被撑满，顺手给出。
	if sizeText, err := remoteDu(ctx, client, &stored); err == nil && sizeText != "" {
		fmt.Fprintf(&b, "\n数据目录 %s\n", sizeText)
	}
	return b.String(), nil
}

// Uninstall 停止并移除容器，但保留数据目录与镜像。
//
// 保留数据是刻意的：内网机器上重新 apply 需要重新传 bundle，而 registry 里
// 已经验证过的包是宝贵资产；幂等的 install.sh 会把这些包合并回去。
func Uninstall(ctx context.Context, h *spec.Host) error {
	if h == nil {
		return fmt.Errorf("uninstall: 主机信息为空")
	}
	stored := *h
	stored.ApplyDefaults()

	client, err := remote.Dial(&stored)
	if err != nil {
		return fmt.Errorf("uninstall: %w", err)
	}
	defer client.Close()

	// compose 文件缺失意味着从未部署成功或目录被清理，此时"停止容器"无事可做。
	if _, statErr := client.Stat(ctx, remotePath(stored.BaseDir, spec.RemoteComposeFile)); statErr != nil {
		return fmt.Errorf("uninstall: 目标机 %s 下没有 %s，看起来从未部署过",
			stored.BaseDir, spec.RemoteComposeFile)
	}

	compose, err := composeCommand(ctx, client)
	if err != nil {
		return fmt.Errorf("uninstall: %w", err)
	}
	// down 会移除容器与网络，不动 bind mount 里的数据。
	script := fmt.Sprintf("cd %s && %s -f %s -p %s down --remove-orphans 2>&1",
		shellSingle(stored.BaseDir), compose, spec.RemoteComposeFile, shellSingle(projectName(stored.BaseDir)))
	if out, err := client.Run(ctx, script); err != nil {
		return fmt.Errorf("uninstall: 停止容器失败: %w", err)
	} else if strings.TrimSpace(out) != "" {
		// down 的输出对操作员有信息量（移除了哪些容器），但成功时不作为错误。
		_ = out
	}
	return nil
}

// remoteDu 汇总目标机部署目录的磁盘占用（总大小 + 各子目录）。
func remoteDu(ctx context.Context, c *remote.Client, h *spec.Host) (string, error) {
	// du -sk 用 KB 输出，避免不同 busybox/coreutils 的格式差异。
	script := fmt.Sprintf(
		"cd %s 2>/dev/null || exit 0; for d in npm/storage python/packages images; do if [ -d \"$d\" ]; then echo \"$d $(du -sk \"$d\" 2>/dev/null | cut -f1)\"; fi; done; echo \"total $(du -sk . 2>/dev/null | cut -f1)\"",
		shellSingle(h.BaseDir))
	out, err := c.Run(ctx, script)
	if err != nil {
		return "", err
	}
	return formatDu(out), nil
}

// formatDu 把 "dir KB" 行格式化成可读文本，顺序固定（与脚本里的顺序一致）。
func formatDu(out string) string {
	var lines []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		var kb int64
		if _, err := fmt.Sscanf(fields[1], "%d", &kb); err != nil {
			continue
		}
		lines = append(lines, fmt.Sprintf("  %-16s %s", fields[0], humanBytes(kb*1024)))
	}
	if len(lines) == 0 {
		return ""
	}
	sort.SliceStable(lines, func(i, j int) bool { return false }) // 保持脚本给出的顺序
	return strings.Join(lines, "\n")
}

// containerJSON 是 `docker compose ps --format json` 的单行结构（只取用到的字段）。
// compose v2 每行输出一个 JSON 对象；用 JSON 而不是表格，是为了让状态判断
// 不依赖列宽或人类可读文本的变化。
type containerJSON struct {
	Name    string `json:"Name"`
	Service string `json:"Service"`
	State   string `json:"State"`
	Health  string `json:"Health"`
	Status  string `json:"Status"`
	Image   string `json:"Image"`
}

// parseComposePS 解析 compose ps 的 JSON 输出（每行一个对象），返回容器列表。
// 解析失败的行被跳过：这里只用于展示与状态判断，不构成失败条件。
func parseComposePS(out string) []containerJSON {
	var list []containerJSON
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.HasPrefix(line, "{") {
			continue
		}
		var c containerJSON
		if err := json.Unmarshal([]byte(line), &c); err != nil {
			continue
		}
		list = append(list, c)
	}
	return list
}
