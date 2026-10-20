// Package bundle 负责构建侧的全部工作：把一份 pkgdepot.yaml 变成自包含的
// bundle tar.gz。它只依赖 internal/spec 的类型，不关心传输与部署。
package bundle

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/SparkElf/pkgdepot/internal/spec"
)

// 客户端配置里的占位符。apply 侧会替换成目标机的真实地址，
// 所以 bundle 内永远是模板形态——同一份 bundle 可以被部署到任意主机。
const (
	PlaceholderNPMURL     = "{{NPM_URL}}"
	PlaceholderPythonURL  = "{{PYTHON_URL}}"
	PlaceholderPythonHost = "{{PYTHON_HOST}}"
)

// bundle 内部布局。与 docs/interfaces.md 的目录结构逐字对应；install.sh 与
// CLI verify 都按这些相对路径取文件，属线上契约，改动需同步文档。
const (
	bundleManifestName = "manifest.json"
	bundleImagesDir    = "images"
	bundleNPMPrefix    = "npm"
	bundleNPMStorage   = "npm/storage"
	bundlePythonDir    = "python"
	bundlePythonPkgs   = "python/packages"
	bundleClientDir    = "client"
	bundleNPMRCFile    = "client/npmrc"
	bundlePipConfFile  = "client/pip.conf"
)

// bundleTopLevel 是 bundle 根允许存在的全部顶层路径。
//
// 这是一条必须被机器检查的合同：bundle 是交付物，多一个目录不只是「多占几
// MB」——它会跟着被拷进内网、被 install.sh 忽略、被用户当成有效内容。曾经的
// 实际故障就是 pip 暂存目录建在打包树内，导致 bundle 里多出一份重复 wheel。
var bundleTopLevel = map[string]bool{
	bundleManifestName: true,
	bundleImagesDir:    true,
	bundleNPMPrefix:    true,
	bundlePythonDir:    true,
	bundleClientDir:    true,
}

// checkBundleLayout 在打包前把暂存树与合同对一遍。
//
// 放在写 tar 之前而不是之后：产物一旦写出，调用方可能已经把它拷走，
// 此时才发现多目录就晚了。
func checkBundleLayout(root string) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return fmt.Errorf("检查 bundle 布局: %w", err)
	}
	var unexpected []string
	for _, e := range entries {
		if !bundleTopLevel[e.Name()] {
			unexpected = append(unexpected, e.Name())
		}
	}
	if len(unexpected) > 0 {
		sort.Strings(unexpected)
		return fmt.Errorf("bundle 暂存目录里存在合同之外的顶层条目 %v；"+
			"bundle 只允许 %v（见 docs/interfaces.md）",
			unexpected, []string{bundleManifestName, bundleImagesDir, bundleNPMPrefix, bundlePythonDir, bundleClientDir})
	}
	return nil
}

// logf 是可选日志回调。nil 时静默，调用方不必到处判空。
type logf func(format string, args ...any)

// printf 是 nil-safe 的日志入口：Options.Log 允许为 nil，
// 每个调用点都判空会把代码弄脏，集中在这里判断一次。
func (l logf) printf(format string, args ...any) {
	if l != nil {
		l(format, args...)
	}
}

// Options 描述一次 bundle 构建。
type Options struct {
	Manifest     *spec.Manifest
	ManifestPath string // 相对它解析 requirements/lockfile
	OutFile      string // 输出 tar.gz 绝对路径
	ToolVersion  string
	Log          func(format string, args ...any)
}

// PlanOptions 只解析依赖闭包，不下载、不写盘，供 `pkgdepot plan` 使用。
type PlanOptions struct {
	Manifest     *spec.Manifest
	ManifestPath string // requirements/lockfile 相对它解析
	Log          func(format string, args ...any)
}

// Plan 返回将要进入 bundle 的完整包清单。
//
// 它的价值在于「先看清楚再花时间下载 200MB 镜像」：用户改完 pkgdepot.yaml
// 想知道闭包长什么样，不需要等完整构建。因此这里只解析版本，不取 tarball、
// 不导镜像。返回值仍是 BundleManifest，方便 CLI 直接用同一套结构打印。
func Plan(ctx context.Context, opts PlanOptions) (*spec.BundleManifest, error) {
	if opts.Manifest == nil {
		return nil, fmt.Errorf("Plan: Manifest 不能为空")
	}
	log := logf(opts.Log)
	dir := manifestDirOf(opts.ManifestPath)

	npmItems, err := collectNPM(ctx, opts.Manifest, dir, false, log)
	if err != nil {
		return nil, err
	}
	npmEntries := make([]spec.NPMEntry, 0, len(npmItems))
	for _, item := range npmItems {
		npmEntries = append(npmEntries, spec.NPMEntry{
			Name:    item.Name,
			Version: item.Version,
			// File 在 plan 阶段也给出：它由 name+version 决定，不需要下载。
			File: item.tarballFileName(),
		})
	}

	pyItems, cleanupPy, err := collectPythonClosure(ctx, opts.Manifest, dir, false, log)
	defer cleanupPy()
	if err != nil {
		return nil, err
	}
	pyEntries := make([]spec.PyEntry, 0, len(pyItems))
	for _, item := range pyItems {
		pyEntries = append(pyEntries, spec.PyEntry{
			Name:    item.Name,
			Version: item.Version,
			File:    item.File,
		})
	}

	images := []spec.ImageEntry{
		{Ref: defaultString(opts.Manifest.Images.Verdaccio, spec.DefaultVerdaccioImage), File: "images/verdaccio.tar"},
		{Ref: defaultString(opts.Manifest.Images.Pypiserver, spec.DefaultPypiserverImage), File: "images/pypiserver.tar"},
	}
	return &spec.BundleManifest{
		APIVersion: spec.BundleAPIVersion,
		Name:       opts.Manifest.Name,
		CreatedAt:  time.Now().UTC(),
		Images:     images,
		NPM:        npmEntries,
		Python:     pyEntries,
	}, nil
}

// Build 解析闭包、下载包与镜像、写出 bundle，返回已写盘的 manifest。
//
// 顺序不是随意的：先解析闭包再下载镜像——闭包可能因为用户写错包名而失败，
// 那时不该已经花掉几分钟做 docker save。所有产物都先落在 workDir 的暂存树里，
// 最后一次性打 tar.gz；中途失败不会留下半个 bundle 文件。
func Build(ctx context.Context, opts Options) (*spec.BundleManifest, error) {
	if opts.Manifest == nil {
		return nil, fmt.Errorf("Build: Manifest 不能为空")
	}
	if strings.TrimSpace(opts.OutFile) == "" {
		return nil, fmt.Errorf("Build: OutFile 不能为空")
	}
	log := logf(opts.Log)
	m := opts.Manifest
	dir := manifestDirOf(opts.ManifestPath)

	// 构建中间态（tarball、镜像 tar）体积可能到 GB 级，必须落在可清理的临时
	// 目录里；Options 不暴露 WorkDir，所以由 Build 自己拥有生命周期。
	work, err := os.MkdirTemp("", "pkgdepot-build-*")
	if err != nil {
		return nil, fmt.Errorf("创建临时目录: %w", err)
	}
	defer os.RemoveAll(work)
	if err := os.MkdirAll(work, 0o755); err != nil {
		return nil, fmt.Errorf("准备临时目录 %s: %w", work, err)
	}

	// --- 1. npm 闭包 + 下载 + verdaccio 存储 ---
	npmItems, err := collectNPM(ctx, m, dir, true, log)
	if err != nil {
		return nil, err
	}
	storageDir := filepath.Join(work, bundleNPMStorage)
	if err := os.MkdirAll(storageDir, 0o755); err != nil {
		return nil, err
	}
	var npmSeeds []npmSeedEntry
	if len(npmItems) > 0 {
		seeds, err := downloadNPMPackages(ctx, storageDir, npmItems, log)
		if err != nil {
			return nil, err
		}
		npmSeeds = seeds
		if err := writeNPMStorage(storageDir, seeds, log); err != nil {
			return nil, err
		}
	}

	// --- 2. python 闭包 + 下载 ---
	pyItems, cleanupPy, err := collectPythonClosure(ctx, m, dir, true, log)
	if err != nil {
		cleanupPy()
		return nil, err
	}
	// 下载暂存目录在 python/packages 拷完之后才能删：stagePythonPackages
	// 仍要从那里面读文件。
	defer cleanupPy()
	pyPkgDir := filepath.Join(work, bundlePythonPkgs)
	if err := os.MkdirAll(pyPkgDir, 0o755); err != nil {
		return nil, err
	}
	pyEntries, err := stagePythonPackages(pyItems, pyPkgDir, log)
	if err != nil {
		return nil, err
	}

	// --- 3. 镜像 ---
	imagesDir := filepath.Join(work, bundleImagesDir)
	imageEntries, err := exportImages(ctx, imagesDir, m, log)
	if err != nil {
		return nil, err
	}

	// --- 4. 客户端模板 ---
	if err := writeClientConfigs(work); err != nil {
		return nil, err
	}

	// --- 5. manifest.json ---
	bm := &spec.BundleManifest{
		APIVersion:  spec.BundleAPIVersion,
		Name:        m.Name,
		CreatedAt:   time.Now().UTC(),
		ToolVersion: opts.ToolVersion,
		Images:      imageEntries,
		NPM:         npmSeedToManifest(npmSeeds),
		Python:      pyEntries,
	}
	if err := writeBundleManifest(work, bm); err != nil {
		return nil, err
	}

	// --- 6. 打 tar.gz ---
	abs, err := filepath.Abs(opts.OutFile)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return nil, fmt.Errorf("创建输出目录: %w", err)
	}
	if err := checkBundleLayout(work); err != nil {
		return nil, err
	}
	log.printf("打包 bundle: %s", abs)
	if err := writeTarGz(work, abs); err != nil {
		return nil, err
	}
	if fi, err := os.Stat(abs); err == nil {
		log.printf("bundle 完成: %s（%s）", abs, humanBytes(fi.Size()))
	}
	return bm, nil
}

// stagePythonPackages 把 pip download 的产物拷进 bundle 的 python/packages。
//
// 这里不保留 pip 的目录结构（pip 会把 sdist 解包成目录、生成 *.whl.metadata
// 等中间产物）：pypiserver 是按目录扁平枚举的，只有把 .whl/.tar.gz 直接放在
// 根目录才能被 /simple/ 列出。
func stagePythonPackages(items []pipDownloadItem, dstDir string, log logf) ([]spec.PyEntry, error) {
	entries := make([]spec.PyEntry, 0, len(items))
	for _, item := range items {
		src := item.URL // downloadPython 把本地路径放在 URL 字段
		if src == "" {
			continue
		}
		dst := filepath.Join(dstDir, item.File)
		digest, err := copyFileAndHash(src, dst)
		if err != nil {
			return nil, fmt.Errorf("复制 python 包 %s: %w", item.File, err)
		}
		entries = append(entries, spec.PyEntry{
			Name:    item.Name,
			Version: item.Version,
			File:    item.File,
			SHA256:  digest.SHA256Hex,
		})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].File < entries[j].File })
	log.printf("python 包: %d 个文件", len(entries))
	return entries, nil
}

// writeClientConfigs 写 client/npmrc 与 client/pip.conf。
//
// 用占位符而不是具体地址，是因为同一份 bundle 可能被部署到不同主机/端口，
// 替换发生在 apply 时（目标地址由 host 配置决定）。写死地址会让 bundle
// 变成「只能装在某一台机器上」。
func writeClientConfigs(workDir string) error {
	dir := filepath.Join(workDir, bundleClientDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	npmrc := fmt.Sprintf("registry=%s\n", PlaceholderNPMURL)
	if err := os.WriteFile(filepath.Join(workDir, bundleNPMRCFile), []byte(npmrc), 0o644); err != nil {
		return err
	}
	// trusted-host 需要纯主机名（不带 scheme/端口）：pip 只按主机名匹配
	// --trusted-host，写成 URL 会不生效并导致 HTTPS 校验失败。
	pipConf := strings.Join([]string{
		"[global]",
		"index-url = " + PlaceholderPythonURL,
		"trusted-host = " + PlaceholderPythonHost,
		"",
	}, "\n")
	return os.WriteFile(filepath.Join(workDir, bundlePipConfFile), []byte(pipConf), 0o644)
}

func writeBundleManifest(workDir string, bm *spec.BundleManifest) error {
	raw, err := json.MarshalIndent(bm, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化 bundle manifest: %w", err)
	}
	raw = append(raw, '\n')
	return os.WriteFile(filepath.Join(workDir, bundleManifestName), raw, 0o644)
}

// writeTarGz 把目录打成 tar.gz，路径相对于 bundle 根。
//
// 目录条目也写进去并带显式权限：解包侧（install.sh）用 `tar -xzf` 时
// 依赖 tar 记录的模式位，缺目录条目在某些 tar 实现下会因父目录不存在失败。
func writeTarGz(root, out string) error {
	tmp := out + ".part"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	gz := gzip.NewWriter(f)
	gz.Name = path.Base(out)
	tw := tar.NewWriter(gz)

	var files []string
	err = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if p == root {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		tw.Close()
		gz.Close()
		f.Close()
		os.Remove(tmp)
		return err
	}
	sort.Strings(files)

	for _, rel := range files {
		full := filepath.Join(root, filepath.FromSlash(rel))
		info, err := os.Lstat(full)
		if err != nil {
			tw.Close()
			gz.Close()
			f.Close()
			os.Remove(tmp)
			return err
		}
		link := ""
		if info.Mode()&os.ModeSymlink != 0 {
			link, _ = os.Readlink(full)
		}
		hdr, err := tar.FileInfoHeader(info, link)
		if err != nil {
			tw.Close()
			gz.Close()
			f.Close()
			os.Remove(tmp)
			return err
		}
		hdr.Name = rel
		if info.IsDir() {
			hdr.Name = rel + "/"
		}
		hdr.Uid, hdr.Gid = 0, 0
		hdr.Uname, hdr.Gname = "", ""
		hdr.ModTime = time.Unix(0, 0) // 固定时间戳，让同样的输入产出可复现的条目时间
		hdr.AccessTime, hdr.ChangeTime = time.Time{}, time.Time{}
		if err := tw.WriteHeader(hdr); err != nil {
			tw.Close()
			gz.Close()
			f.Close()
			os.Remove(tmp)
			return err
		}
		if info.IsDir() || link != "" {
			continue
		}
		in, err := os.Open(full)
		if err != nil {
			tw.Close()
			gz.Close()
			f.Close()
			os.Remove(tmp)
			return err
		}
		_, cerr := io.Copy(tw, in)
		in.Close()
		if cerr != nil {
			tw.Close()
			gz.Close()
			f.Close()
			os.Remove(tmp)
			return cerr
		}
	}
	if err := tw.Close(); err != nil {
		gz.Close()
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := gz.Close(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, out)
}

// ReadManifest 从 bundle tar.gz 里读出 manifest.json，不落盘。
//
// 只读流不做全量解包：`pkgdepot verify --bundle x.tar.gz` 要在目标网段里
// 立刻跑起来，为了一个 300KB 的 JSON 把 300MB 的 tar.gz 解到磁盘没有意义。
// 第二个返回值保留给「调用方需要解包」的场景（当前实现不落盘，返回空串）。
func ReadManifest(bundlePath string) (*spec.BundleManifest, string, error) {
	f, err := os.Open(bundlePath)
	if err != nil {
		return nil, "", fmt.Errorf("打开 bundle %s: %w", bundlePath, err)
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, "", fmt.Errorf("%s 不是合法的 tar.gz: %w", bundlePath, err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, "", fmt.Errorf("读取 %s 失败: %w", bundlePath, err)
		}
		name := strings.TrimPrefix(strings.TrimPrefix(hdr.Name, "./"), "/")
		if name != bundleManifestName {
			continue
		}
		raw, err := io.ReadAll(io.LimitReader(tr, 64<<20))
		if err != nil {
			return nil, "", fmt.Errorf("读取 manifest.json: %w", err)
		}
		var bm spec.BundleManifest
		if err := json.Unmarshal(raw, &bm); err != nil {
			return nil, "", fmt.Errorf("解析 manifest.json: %w", err)
		}
		if bm.APIVersion != spec.BundleAPIVersion {
			return nil, "", fmt.Errorf("bundle manifest apiVersion 必须为 %q，实际 %q",
				spec.BundleAPIVersion, bm.APIVersion)
		}
		if strings.TrimSpace(bm.Name) == "" {
			return nil, "", fmt.Errorf("bundle manifest 缺少 name")
		}
		return &bm, "", nil
	}
	return nil, "", fmt.Errorf("bundle %s 里没有 %s", bundlePath, bundleManifestName)
}

// ReadClientConfigs 从 bundle 里读出 client 模板（npmrc / pip.conf）。
// deploy 侧要用它做占位符替换；单独提供而不是让调用方自己解包。
func ReadClientConfigs(bundlePath string) (npmrc, pipConf []byte, err error) {
	f, err := os.Open(bundlePath)
	if err != nil {
		return nil, nil, fmt.Errorf("打开 bundle %s: %w", bundlePath, err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, nil, fmt.Errorf("%s 不是合法的 tar.gz: %w", bundlePath, err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, err
		}
		name := strings.TrimPrefix(strings.TrimPrefix(hdr.Name, "./"), "/")
		switch name {
		case bundleNPMRCFile:
			npmrc, err = io.ReadAll(io.LimitReader(tr, 1<<20))
			if err != nil {
				return nil, nil, err
			}
		case bundlePipConfFile:
			pipConf, err = io.ReadAll(io.LimitReader(tr, 1<<20))
			if err != nil {
				return nil, nil, err
			}
		}
	}
	if npmrc == nil && pipConf == nil {
		return nil, nil, fmt.Errorf("bundle %s 里没有 client 配置", bundlePath)
	}
	return npmrc, pipConf, nil
}
