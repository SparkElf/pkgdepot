package bundle

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/SparkElf/pkgdepot/internal/spec"
)

// pipPlanReport 是 `pip install --dry-run --report` 的输出。
//
// 为什么用 pip 自己的解析结果而不是手写 requirements 解析：requirements 里
// 可能有环境标记（python_version < "3.11"）、extras、constraints、hash 等，
// 手写解析几乎必然漏。pip 的 report 给出的是「这台构建机按给定 platform /
// python-version / implementation 真正会装的那一组文件」，正是我们要复制的闭包。
type pipPlanReport struct {
	Install []pipPlanItem `json:"install"`
}

type pipPlanItem struct {
	Metadata struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"metadata"`
	DownloadInfo struct {
		URL string `json:"url"`
	} `json:"download_info"`
}

// pipDownloadItem 是一个待落盘的 python 分发文件。
type pipDownloadItem struct {
	Name    string // 规范化的项目名（PyPI 规范：小写、- 连接）
	Version string
	File    string // 文件名，如 idna-3.10-py3-none-any.whl
	URL     string // 上游下载地址（可能为空，表示用包名+版本重新下载）
}

// pythonConstraintArgs 把 manifest 的平台/解释器约束翻成 pip 参数。
//
// 关键点：--platform/--python-version/--implementation 三者一旦出现，
// pip 就要求同时给出 --only-binary=:all:（否则直接报错退出）。所以这里把
// 「有约束」作为一个整体处理，而不是逐条判断。
func pythonConstraintArgs(py spec.PythonInput) []string {
	var args []string
	platforms := dedupeStrings(py.Platforms)
	args = append(args, "--python-version", defaultString(py.PythonVersion, "3.12"))
	args = append(args, "--implementation", defaultString(py.Implementation, "cp"))
	for _, p := range platforms {
		args = append(args, "--platform", p)
	}
	// 有平台约束时必须只收 wheel；纯解释器约束（无 platform）也统一走
	// only-binary，因为 bundle 要的是可直接安装的产物，sdist 在离线侧
	// 还需要编译工具链——目标机明确只有 sh+docker+tar，编译是不可能的。
	args = append(args, "--only-binary", ":all:")
	return args
}

func dedupeStrings(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// collectPythonClosure 解析 python 侧闭包。
//
// needDownload=false（plan）时只跑 dry-run 报告拿清单；否则直接 `pip download`
// 到临时目录，返回的条目里 File 仍是文件名，URL 字段暂存本地路径。
//
// 返回的 cleanup 负责删除下载暂存目录：调用方必须在把文件拷进 bundle 之后
// 才调用它。暂存目录刻意建在系统临时目录而不是 bundle 暂存树内——后者会被
// 整棵树打进 tar.gz，导致交付物里多出一份重复 wheel。
func collectPythonClosure(ctx context.Context, m *spec.Manifest, manifestDir string, needDownload bool, log logf) ([]pipDownloadItem, func(), error) {
	noop := func() {}
	py := m.Python
	if len(py.Requirements) == 0 && len(py.Packages) == 0 {
		return nil, noop, nil
	}

	var inputArgs []string
	// requirements 文件路径相对清单文件所在目录；先校验存在，好让报错直接
	// 指向用户写错的路径，而不是 pip 那句 "Could not open requirements file"。
	for _, rel := range py.Requirements {
		if strings.TrimSpace(rel) == "" {
			continue
		}
		path := rel
		if !filepath.IsAbs(path) {
			path = filepath.Join(manifestDir, path)
		}
		if _, err := os.Stat(path); err != nil {
			return nil, noop, fmt.Errorf("python requirements 文件 %s: %w", path, err)
		}
		inputArgs = append(inputArgs, "-r", path)
	}
	for _, pkg := range py.Packages {
		pkg = strings.TrimSpace(pkg)
		if pkg == "" {
			continue
		}
		// requirements 与 packages 互斥（spec 已校验），所以这里直接把包规格
		// 当作命令行参数交给 pip，语义与 requirements 里写一行完全一致。
		inputArgs = append(inputArgs, pkg)
	}
	if len(inputArgs) == 0 {
		return nil, noop, fmt.Errorf("python 未声明任何包")
	}

	constraints := pythonConstraintArgs(py)
	if !needDownload {
		items, err := planPython(ctx, manifestDir, inputArgs, constraints, log)
		return items, noop, err
	}
	return downloadPython(ctx, manifestDir, inputArgs, constraints, log)
}

// planPython 只解析、不下载。pip download 没有 --dry-run，所以走
// `pip install --dry-run --report`：它同样执行完整解析，只是不落盘。
func planPython(ctx context.Context, dir string, inputArgs, constraints []string, log logf) ([]pipDownloadItem, error) {
	tmp, err := os.MkdirTemp("", "pkgdepot-pip-plan-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	report := filepath.Join(tmp, "report.json")

	args := append([]string{
		"install", "--dry-run", "--ignore-installed",
		"--report", report,
		"--no-input", "--disable-pip-version-check",
	}, inputArgs...)
	args = append(args, constraints...)
	log.printf("解析 python 依赖闭包: %s", strings.Join(compactArgs(inputArgs), " "))
	if _, err := runPip(ctx, dir, args...); err != nil {
		return nil, fmt.Errorf("解析 python 依赖闭包: %w", err)
	}
	return readPipReport(report)
}

func readPipReport(path string) ([]pipDownloadItem, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取 pip report: %w", err)
	}
	var rep pipPlanReport
	if err := json.Unmarshal(raw, &rep); err != nil {
		return nil, fmt.Errorf("解析 pip report: %w", err)
	}
	seen := map[string]bool{}
	var out []pipDownloadItem
	for _, item := range rep.Install {
		file := urlBaseName(item.DownloadInfo.URL)
		if file == "" {
			continue
		}
		key := item.Metadata.Name + "\x00" + item.Metadata.Version + "\x00" + file
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, pipDownloadItem{
			Name:    canonicalPyName(item.Metadata.Name),
			Version: item.Metadata.Version,
			File:    file,
			URL:     item.DownloadInfo.URL,
		})
	}
	sortPythonItems(out)
	return out, nil
}

func downloadPython(ctx context.Context, dir string, inputArgs, constraints []string, log logf) ([]pipDownloadItem, func(), error) {
	// 暂存目录建在系统临时区，且把 cleanup 交给调用方：文件要被复制进
	// bundle 的 python/packages 之后才能删除。
	dl, err := os.MkdirTemp("", "pkgdepot-pip-download-*")
	if err != nil {
		return nil, func() {}, fmt.Errorf("创建 pip 下载目录: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(dl) }
	fail := func(err error) ([]pipDownloadItem, func(), error) {
		cleanup()
		return nil, func() {}, err
	}
	args := append([]string{
		"download", "-d", dl,
		"--no-input", "--disable-pip-version-check",
	}, inputArgs...)
	args = append(args, constraints...)
	log.printf("下载 python 依赖: %s", strings.Join(compactArgs(inputArgs), " "))
	if _, err := runPip(ctx, dir, args...); err != nil {
		return fail(fmt.Errorf("下载 python 依赖: %w", err))
	}

	entries, err := os.ReadDir(dl)
	if err != nil {
		return fail(err)
	}
	var out []pipDownloadItem
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !isPythonDistFile(name) {
			continue // 例如 pip 生成的 .whl.metadata 之类
		}
		item, err := pythonItemFromFile(name)
		if err != nil {
			// 文件名解析失败不致命：仍然把文件带进 bundle（pypiserver 按目录
			// 枚举），只是 plan 里无法给出 name/version。日志留证据即可。
			log.printf("警告: 无法从文件名解析 name/version: %s（%v）", name, err)
			item = pipDownloadItem{File: name}
		}
		item.URL = filepath.Join(dl, name)
		out = append(out, item)
	}
	sortPythonItems(out)
	return out, cleanup, nil
}

func compactArgs(args []string) []string {
	out := make([]string, 0, len(args))
	for _, a := range args {
		if a == "-r" {
			out = append(out, "requirements")
			continue
		}
		if len(a) > 120 {
			out = append(out, a[:117]+"...")
			continue
		}
		out = append(out, a)
	}
	return out
}

func sortPythonItems(items []pipDownloadItem) {
	sort.Slice(items, func(i, j int) bool {
		if items[i].File != items[j].File {
			return items[i].File < items[j].File
		}
		return items[i].Version < items[j].Version
	})
}

func isPythonDistFile(name string) bool {
	lower := strings.ToLower(name)
	return strings.HasSuffix(lower, ".whl") || strings.HasSuffix(lower, ".tar.gz") ||
		strings.HasSuffix(lower, ".zip")
}

// pythonItemFromFile 从分发文件名解析项目名与版本。
//
// wheel 名格式（PEP 427）：
//
//	{distribution}-{version}(-{build tag})?-{python tag}-{abi tag}-{platform tag}.whl
//
// 注意 distribution 里的 '_' 按 PEP 503 规范等价于 '-'，所以
// charset_normalizer-3.5.2-… 与 charset-normalizer 是同一个包。
// 版本段之前不能简单按第一个 '-' 切：distribution 本身可能不含 '-'，
// 但 wheel 规范要求 distribution 段内的 '-' 必须换成 '_'，因此第一个 '-'
// 之前的整段就是发行名，可以安全切分。
func pythonItemFromFile(file string) (pipDownloadItem, error) {
	lower := strings.ToLower(file)
	switch {
	case strings.HasSuffix(lower, ".whl"):
		stem := file[:len(file)-len(".whl")]
		parts := strings.Split(stem, "-")
		if len(parts) < 5 {
			return pipDownloadItem{}, fmt.Errorf("wheel 名段数不足: %q", file)
		}
		// 段数可能 >5（带 build tag），但发行名与版本始终是最前两段。
		return pipDownloadItem{
			Name:    canonicalPyName(parts[0]),
			Version: parts[1],
			File:    file,
		}, nil
	case strings.HasSuffix(lower, ".tar.gz"):
		stem := file[:len(file)-len(".tar.gz")]
		name, ver, ok := splitSDistName(stem)
		if !ok {
			return pipDownloadItem{}, fmt.Errorf("sdist 名无法解析: %q", file)
		}
		return pipDownloadItem{Name: canonicalPyName(name), Version: ver, File: file}, nil
	case strings.HasSuffix(lower, ".zip"):
		stem := file[:len(file)-len(".zip")]
		name, ver, ok := splitSDistName(stem)
		if !ok {
			return pipDownloadItem{}, fmt.Errorf("zip sdist 名无法解析: %q", file)
		}
		return pipDownloadItem{Name: canonicalPyName(name), Version: ver, File: file}, nil
	}
	return pipDownloadItem{}, fmt.Errorf("不是 python 分发文件: %q", file)
}

// splitSDistName 切分 sdist 的 name-version。
//
// sdist 没有 wheel 那种强约束，发行名里可能出现 '-'（如 my-package-1.0.0），
// 所以从右往左找第一个看起来像版本号的段：以数字开头。这样
// "charset-normalizer-3.5.2" → (charset-normalizer, 3.5.2)。
func splitSDistName(stem string) (name, version string, ok bool) {
	for i := len(stem) - 1; i > 0; i-- {
		if stem[i] != '-' {
			continue
		}
		candidate := stem[i+1:]
		if candidate == "" {
			continue
		}
		if candidate[0] >= '0' && candidate[0] <= '9' {
			return stem[:i], candidate, true
		}
	}
	return "", "", false
}

// canonicalPyName 按 PEP 503 规范化项目名：小写，连续的 -_. 统一成单个 '-'。
// 这样 plan 输出、manifest、verify 与 PyPI 的 /simple/<name>/ 路径全部一致。
func canonicalPyName(name string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(name) {
		switch r {
		case '-', '_', '.':
			if !prevDash {
				b.WriteByte('-')
				prevDash = true
			}
		default:
			b.WriteRune(r)
			prevDash = false
		}
	}
	return strings.Trim(b.String(), "-")
}
