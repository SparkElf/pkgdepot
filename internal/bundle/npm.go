package bundle

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Masterminds/semver/v3"

	"github.com/SparkElf/pkgdepot/internal/spec"
)

// npmRegistry 是构建侧访问上游 registry 的极简客户端。
//
// 为什么不逐条调用 `npm view`：npm CLI 的 `--json` 输出形状会随字段组合变化
// （有时返回数组、有时返回整份 packument），要在上面做稳定解析反而更脆。
// 直接走 registry 的公开 HTTP 契约更确定，而 npm CLI 只保留在它真正擅长的
// 地方——用 package-lock 解析依赖图（见 collectNPMLockfiles）。
type npmRegistry struct {
	base string
	http *http.Client

	mu         sync.Mutex
	packuments map[string]*npmPackument
}

// npmPackument 只保留解析闭包需要的字段。Accept 用 install-v1 的精简格式，
// 体积比完整 packument 小一个量级（完整版每个版本都带 readme）。
type npmPackument struct {
	Name     string                     `json:"name"`
	DistTags map[string]string          `json:"dist-tags"`
	Versions map[string]json.RawMessage `json:"versions"`
}

// npmDist 对应 registry 的 dist 对象。integrity 是 SRI（sha512-base64），
// shasum 是老式 sha1；两者都保留，因为不同客户端/离线环境校验方式不同。
type npmDist struct {
	Tarball   string `json:"tarball"`
	Integrity string `json:"integrity"`
	Shasum    string `json:"shasum"`
}

// npmVersion 是闭包里的一个 (name, version)。
//
// Raw 保存上游 version 对象的完整内容，而不是解析成几个已知字段：写 verdaccio
// packument 时如果丢掉 bin/os/cpu/engines/hasInstallScript 等字段，离线侧
// npm 会按错误的平台/入口处理包，这属于「看起来成功、装出来的东西不对」的
// 静默故障，比直接失败更难查。
type npmVersion struct {
	Name                 string
	Version              string
	Dist                 npmDist
	Dependencies         map[string]string
	OptionalDependencies map[string]string
	Raw                  map[string]any

	// lockResolved/lockIntegrity 来自 package-lock.json 的 pinned 记录。
	// 它们指向 lock 生成时的确切字节，比重新按 registry 推导更可信。
	lockResolved  string
	lockIntegrity string
	// optionalEntry 标记该条目来自 optionalDependencies：下载失败时按 npm
	// 语义跳过而不是中断整个构建。
	optionalEntry bool
	// hydrated 记录是否成功取到上游完整 metadata（用于日志与降级提示）。
	hydrated bool
}

func (v *npmVersion) key() string { return v.Name + "@" + v.Version }

// tarballFileName 决定 bundle 内 tarball 的文件名。
//
// npm 约定是「取包名最后一段 + 版本」，例如 @types/node → node-22.10.2.tgz。
// 直接用上游 URL 的 basename 最稳（它天然符合约定），装进本地 packument 后
// verify 会把 <name>/-/<file> 原样拼出来，所以这里必须和 dist.tarball 的
// basename 一致。
func (v *npmVersion) tarballFileName() string {
	for _, u := range []string{v.lockResolved, v.Dist.Tarball} {
		if u == "" {
			continue
		}
		if base := urlBaseName(u); isNMPSafeTarballName(base) {
			return base
		}
	}
	lastSegment := v.Name
	if i := strings.LastIndex(lastSegment, "/"); i >= 0 {
		lastSegment = lastSegment[i+1:]
	}
	return fmt.Sprintf("%s-%s.tgz", lastSegment, v.Version)
}

// tarballURLs 给出按可信度排序的下载地址：lockfile 的 pinned 地址优先，
// 其次是 registry 元数据里的地址（通常是同一个，但可能指向另一个镜像）。
// 任一地址失败时换下一个，这是「构建机有多个镜像可用」时的现实需要。
func (v *npmVersion) tarballURLs() []string {
	var out []string
	seen := map[string]bool{}
	for _, u := range []string{v.lockResolved, v.Dist.Tarball} {
		if u == "" || seen[u] {
			continue
		}
		seen[u] = true
		out = append(out, u)
	}
	return out
}

func urlBaseName(raw string) string {
	if i := strings.IndexAny(raw, "?#"); i >= 0 {
		raw = raw[:i]
	}
	raw = strings.TrimRight(raw, "/")
	if i := strings.LastIndex(raw, "/"); i >= 0 {
		return raw[i+1:]
	}
	return raw
}

// isNMPSafeTarballName 过滤掉明显不是 tarball 的 basename（例如 URL 指向了
// 目录），同时拒绝带路径分隔符的名字，避免写文件时逃出存储目录。
func isNMPSafeTarballName(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	if strings.ContainsAny(name, "/\\") {
		return false
	}
	return strings.HasSuffix(name, ".tgz") || strings.HasSuffix(name, ".tar.gz")
}

func newNPMRegistry(ctx context.Context) (*npmRegistry, error) {
	out, err := runNPM(ctx, "", "config", "get", "registry")
	if err != nil {
		return nil, fmt.Errorf("读取 npm registry 配置: %w", err)
	}
	base := strings.TrimSpace(out)
	if base == "" || base == "undefined" || base == "null" {
		base = "https://registry.npmjs.org/"
	}
	base = strings.TrimRight(base, "/")
	if _, err := url.Parse(base); err != nil {
		return nil, fmt.Errorf("npm registry 地址 %q 无效: %w", base, err)
	}
	return &npmRegistry{
		base:       base,
		http:       &http.Client{Timeout: 90 * time.Second},
		packuments: map[string]*npmPackument{},
	}, nil
}

func (r *npmRegistry) url(parts ...string) string {
	segs := make([]string, 0, len(parts))
	for _, p := range parts {
		// scoped 包名里的 "/" 必须编码，否则会被当作路径层级；
		// 版本号里的 "+"（build metadata）在路径中保持字面量即可。
		segs = append(segs, strings.ReplaceAll(url.PathEscape(p), "%40", "@"))
	}
	return r.base + "/" + strings.Join(segs, "/")
}

func (r *npmRegistry) getJSON(ctx context.Context, accept, rawurl string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawurl, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("User-Agent", "pkgdepot")
	resp, err := r.http.Do(req)
	if err != nil {
		return fmt.Errorf("GET %s: %w", rawurl, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 128<<20))
	if err != nil {
		return fmt.Errorf("读取 %s 响应: %w", rawurl, err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s 返回 HTTP %d: %s", rawurl, resp.StatusCode, outputTail(string(body), 300))
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("解析 %s 响应: %w", rawurl, err)
	}
	return nil
}

func (r *npmRegistry) packument(ctx context.Context, name string) (*npmPackument, error) {
	r.mu.Lock()
	if p, ok := r.packuments[name]; ok {
		r.mu.Unlock()
		return p, nil
	}
	r.mu.Unlock()

	var p npmPackument
	err := r.getJSON(ctx, "application/vnd.npm.install-v1+json", r.url(name), &p)
	if err != nil {
		// 部分私有 registry 不支持精简 Accept 头，退回完整格式再试一次。
		err = r.getJSON(ctx, "application/json", r.url(name), &p)
	}
	if err != nil {
		return nil, fmt.Errorf("获取 %s 的 packument: %w", name, err)
	}
	r.mu.Lock()
	r.packuments[name] = &p
	r.mu.Unlock()
	return &p, nil
}

// resolveVersion 把 npm 版本范围解析成具体版本。
//
// 顺序遵循 npm 自身语义：先看 dist-tags（latest/next 这类别名不是 semver 范围，
// 直接按 semver 解析会失败），再试精确版本，最后才用 semver 约束选最高版本。
func (r *npmRegistry) resolveVersion(ctx context.Context, name, rng string) (string, error) {
	p, err := r.packument(ctx, name)
	if err != nil {
		return "", err
	}
	rng = strings.TrimSpace(rng)
	if rng == "" {
		rng = "latest"
	}
	if v, ok := p.DistTags[rng]; ok {
		if _, exists := p.Versions[v]; exists {
			return v, nil
		}
	}
	if _, ok := p.Versions[rng]; ok {
		return rng, nil
	}
	c, err := semver.NewConstraint(rng)
	if err != nil {
		return "", fmt.Errorf("%s 的版本范围 %q 无法解析: %w", name, rng, err)
	}
	var best *semver.Version
	for raw := range p.Versions {
		v, err := semver.NewVersion(raw)
		if err != nil {
			continue
		}
		if !c.Check(v) {
			continue
		}
		if best == nil || best.LessThan(v) {
			best = v
		}
	}
	if best == nil {
		return "", fmt.Errorf("%s 没有匹配 %q 的版本（registry: %s）", name, rng, r.base)
	}
	return best.Original(), nil
}

// versionManifest 取单个版本的完整 metadata。
//
// 优先用 registry 的单版本端点（只有几 KB），失败时退回 packument 里的版本对象：
// 个别私有 registry 不实现单版本端点，但 packument 一定有。
func (r *npmRegistry) versionManifest(ctx context.Context, name, version string) (*npmVersion, error) {
	var obj map[string]any
	err := r.getJSON(ctx, "application/json", r.url(name, version), &obj)
	if err != nil {
		p, perr := r.packument(ctx, name)
		if perr != nil {
			return nil, err
		}
		raw, ok := p.Versions[version]
		if !ok {
			return nil, fmt.Errorf("%s@%s 不存在", name, version)
		}
		var fallback map[string]any
		if uerr := json.Unmarshal(raw, &fallback); uerr != nil {
			return nil, fmt.Errorf("解析 %s@%s metadata: %w", name, version, uerr)
		}
		obj = fallback
	}
	v, err := newNPMVersionFromRaw(obj)
	if err != nil {
		return nil, fmt.Errorf("%s@%s: %w", name, version, err)
	}
	v.hydrated = true
	return v, nil
}

func newNPMVersionFromRaw(obj map[string]any) (*npmVersion, error) {
	v := &npmVersion{Raw: obj}
	v.Name, _ = obj["name"].(string)
	v.Version, _ = obj["version"].(string)
	if v.Name == "" || v.Version == "" {
		return nil, fmt.Errorf("metadata 缺少 name/version")
	}
	v.Dependencies = stringMapOf(obj["dependencies"])
	v.OptionalDependencies = stringMapOf(obj["optionalDependencies"])
	if rawDist, ok := obj["dist"].(map[string]any); ok {
		v.Dist.Tarball, _ = rawDist["tarball"].(string)
		v.Dist.Integrity, _ = rawDist["integrity"].(string)
		v.Dist.Shasum, _ = rawDist["shasum"].(string)
	}
	return v, nil
}

// stringMapOf 把 JSON 解出来的 map 转成 map[string]string，忽略非字符串值
// （registry 元数据里偶尔出现 null 值，例如 dependencies 被显式置空）。
func stringMapOf(raw any) map[string]string {
	m, ok := raw.(map[string]any)
	if !ok || len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// resolveNPMClosure 处理 npm.packages 形式：从 "name@range" 根出发，按
// dependencies + optionalDependencies 递归展开完整闭包。
//
// 去重键是 (name, version)，不是 name：同一包的 1.x 与 2.x 可能被不同分支
// 同时需要，只留一个会让另一条分支在离线侧解析失败。但同一 (name, range)
// 只解析一次，避免依赖环或重复路径导致指数级重复请求。
func resolveNPMClosure(ctx context.Context, reg *npmRegistry, specs []string, log logf) ([]*npmVersion, error) {
	type depRequest struct {
		name     string
		rng      string
		optional bool
		via      string
	}
	var queue []depRequest
	for _, s := range specs {
		name, rng, err := spec.SplitNPMSpec(s)
		if err != nil {
			return nil, err
		}
		queue = append(queue, depRequest{name: name, rng: rng, via: "manifest"})
	}

	seenRequest := map[string]bool{}
	chosen := map[string]*npmVersion{}
	var order []string

	for len(queue) > 0 {
		req := queue[0]
		queue = queue[1:]
		reqKey := req.name + "\x00" + req.rng
		if seenRequest[reqKey] {
			continue
		}
		seenRequest[reqKey] = true

		if err := ctx.Err(); err != nil {
			return nil, err
		}

		version, err := reg.resolveVersion(ctx, req.name, req.rng)
		if err != nil {
			if req.optional {
				log.printf("警告: 跳过可选依赖 %s@%s: %v", req.name, req.rng, err)
				continue
			}
			return nil, fmt.Errorf("解析 %s@%s（由 %s 引入）: %w", req.name, req.rng, req.via, err)
		}
		key := req.name + "@" + version
		if _, ok := chosen[key]; ok {
			continue
		}
		meta, err := reg.versionManifest(ctx, req.name, version)
		if err != nil {
			if req.optional {
				log.printf("警告: 跳过可选依赖 %s@%s: %v", req.name, version, err)
				continue
			}
			return nil, fmt.Errorf("获取 %s@%s metadata（由 %s 引入）: %w", req.name, version, req.via, err)
		}
		meta.optionalEntry = req.optional
		chosen[key] = meta
		order = append(order, key)
		via := req.name + "@" + version

		for dep, depRange := range meta.Dependencies {
			queue = append(queue, depRequest{name: dep, rng: depRange, via: via})
		}
		// optionalDependencies 也要展开：不展开的话离线侧 npm 解析可选依赖时
		// 会 404，而 npm 对 optional 的处理是「解析不到就跳过」，那就等于
		// 该功能在离线环境永远拿不到——所以尽力带上，实在拿不到再跳过。
		for dep, depRange := range meta.OptionalDependencies {
			queue = append(queue, depRequest{name: dep, rng: depRange, optional: true, via: via})
		}
	}

	out := make([]*npmVersion, 0, len(order))
	for _, k := range order {
		out = append(out, chosen[k])
	}
	sortNPMVersions(out)
	return out, nil
}

func sortNPMVersions(list []*npmVersion) {
	sort.Slice(list, func(i, j int) bool {
		if list[i].Name != list[j].Name {
			return list[i].Name < list[j].Name
		}
		return list[i].Version < list[j].Version
	})
}

// ---- package-lock 解析 -------------------------------------------------

// lockFile 同时覆盖 v1（dependencies 树）与 v2/v3（packages 扁平表）。
type lockFile struct {
	Name            string                    `json:"name"`
	Version         string                    `json:"version"`
	LockfileVersion int                       `json:"lockfileVersion"`
	Packages        map[string]lockPackage    `json:"packages"`
	Dependencies    map[string]lockDependency `json:"dependencies"`
}

type lockPackage struct {
	Name                 string            `json:"name"`
	Version              string            `json:"version"`
	Resolved             string            `json:"resolved"`
	Integrity            string            `json:"integrity"`
	Dev                  bool              `json:"dev"`
	DevOptional          bool              `json:"devOptional"`
	Optional             bool              `json:"optional"`
	Link                 bool              `json:"link"`
	Dependencies         map[string]string `json:"dependencies"`
	OptionalDependencies map[string]string `json:"optionalDependencies"`
}

type lockDependency struct {
	Version      string                    `json:"version"`
	Resolved     string                    `json:"resolved"`
	Integrity    string                    `json:"integrity"`
	Dev          bool                      `json:"dev"`
	Optional     bool                      `json:"optional"`
	Dependencies map[string]lockDependency `json:"dependencies"`
}

const lockfileCacheFile = "package-lock.json"

// collectNPMLockfiles 解析 manifest.npm.lockfiles。
//
// 两种输入都要接受：直接给 package-lock.json（解析即可，绝不重新解析依赖，
// 因为 lock 是用户已经验证过的确定性证据），或给 package.json（让 npm 生成
// lock——这是 npm 自己的解析器，比我们手写更接近真实行为）。
// package.json 会被复制到临时目录再跑 npm，避免污染用户仓库。
func collectNPMLockfiles(ctx context.Context, m *spec.Manifest, manifestDir string, log logf) ([]*npmVersion, error) {
	seen := map[string]bool{}
	var out []*npmVersion
	for _, rel := range m.NPM.Lockfiles {
		if strings.TrimSpace(rel) == "" {
			continue
		}
		path := rel
		if !filepath.IsAbs(path) {
			path = filepath.Join(manifestDir, path)
		}
		items, err := lockfilePackages(ctx, path, m.NPM.IncludeDev, log)
		if err != nil {
			return nil, err
		}
		for _, item := range items {
			if seen[item.key()] {
				continue
			}
			seen[item.key()] = true
			out = append(out, item)
		}
	}
	sortNPMVersions(out)
	return out, nil
}

func lockfilePackages(ctx context.Context, path string, includeDev bool, log logf) ([]*npmVersion, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取 npm 依赖文件 %s: %w", path, err)
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("解析 %s: %w", path, err)
	}
	if _, isLock := probe["lockfileVersion"]; !isLock {
		// 这是 package.json，交给 npm 生成 lock 后再解析。
		lockPath, cleanup, err := npmGenerateLockfile(ctx, path, log)
		if err != nil {
			return nil, err
		}
		defer cleanup()
		raw, err = os.ReadFile(lockPath)
		if err != nil {
			return nil, fmt.Errorf("读取生成的 lockfile: %w", err)
		}
	}
	var lf lockFile
	if err := json.Unmarshal(raw, &lf); err != nil {
		return nil, fmt.Errorf("解析 lockfile %s: %w", path, err)
	}
	return flattenLockfile(&lf, includeDev, path)
}

// npmGenerateLockfile 在临时目录里让 npm 从 package.json 生成 lockfile。
// 复制而不是原地执行：构建过程不应该在用户的仓库里留下任何文件。
func npmGenerateLockfile(ctx context.Context, packageJSON string, log logf) (string, func(), error) {
	tmp, err := os.MkdirTemp("", "pkgdepot-npm-lock-*")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = os.RemoveAll(tmp) }
	src, err := os.ReadFile(packageJSON)
	if err != nil {
		cleanup()
		return "", nil, fmt.Errorf("读取 %s: %w", packageJSON, err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "package.json"), src, 0o644); err != nil {
		cleanup()
		return "", nil, err
	}
	log.printf("解析 npm 依赖图: %s（npm install --package-lock-only）", packageJSON)
	if _, err := runNPM(ctx, tmp, "install", "--package-lock-only", "--no-audit", "--no-fund", "--ignore-scripts"); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("从 %s 生成 lockfile: %w", packageJSON, err)
	}
	lock := filepath.Join(tmp, lockfileCacheFile)
	if _, err := os.Stat(lock); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("npm 未生成 %s（可能没有可解析的依赖）: %w", lockfileCacheFile, err)
	}
	return lock, cleanup, nil
}

// flattenLockfile 把两种 lockfile 结构压平成 (name, version) 列表并去重。
//
// 去重很关键：v1 的嵌套 dependencies 树里同一个包会大量重复（每个父级一份），
// 不去重会写出重复 tarball、manifest 里出现重复条目，apply 后 verify 也会
// 重复校验同一份字节。
func flattenLockfile(lf *lockFile, includeDev bool, source string) ([]*npmVersion, error) {
	seen := map[string]*npmVersion{}
	var out []*npmVersion
	add := func(v *npmVersion) {
		if v == nil || v.Name == "" || v.Version == "" {
			return
		}
		if existing, ok := seen[v.key()]; ok {
			// 同一 (name,version) 只保留一份；但若另一份带 resolved/integrity
			// 而先到的没有，补上——两条路径给的信息可能互补。
			if existing.lockResolved == "" && v.lockResolved != "" {
				existing.lockResolved = v.lockResolved
			}
			if existing.lockIntegrity == "" && v.lockIntegrity != "" {
				existing.lockIntegrity = v.lockIntegrity
			}
			return
		}
		seen[v.key()] = v
		out = append(out, v)
	}

	if len(lf.Packages) > 0 {
		// v2/v3：key 形如 "node_modules/pkg" 或 "node_modules/a/node_modules/@s/b"。
		for key, entry := range lf.Packages {
			if key == "" || entry.Link {
				continue // 根项目或 workspace 软链
			}
			if entry.Dev && !includeDev {
				continue
			}
			name := entry.Name
			if name == "" {
				name = nameFromLockPath(key)
			}
			if name == "" || entry.Version == "" {
				continue
			}
			add(&npmVersion{
				Name:          name,
				Version:       entry.Version,
				Dependencies:  entry.Dependencies,
				lockResolved:  entry.Resolved,
				lockIntegrity: entry.Integrity,
				optionalEntry: entry.Optional || entry.DevOptional,
			})
		}
		sortNPMVersions(out)
		return out, nil
	}

	// v1：dependencies 是嵌套树，version 就在节点里。
	var walk func(deps map[string]lockDependency, parentOptional bool)
	walk = func(deps map[string]lockDependency, parentOptional bool) {
		for name, dep := range deps {
			if dep.Version == "" {
				continue
			}
			if dep.Dev && !includeDev {
				continue
			}
			add(&npmVersion{
				Name:          name,
				Version:       dep.Version,
				lockResolved:  dep.Resolved,
				lockIntegrity: dep.Integrity,
				optionalEntry: dep.Optional || parentOptional,
			})
			walk(dep.Dependencies, dep.Optional || parentOptional)
		}
	}
	walk(lf.Dependencies, false)
	sortNPMVersions(out)
	return out, nil
}

// nameFromLockPath 从 "node_modules/…" 路径还原包名。
// scoped 包在路径里是两段（@scope/pkg），所以只去掉最后一个 node_modules/。
func nameFromLockPath(key string) string {
	const marker = "node_modules/"
	idx := strings.LastIndex(key, marker)
	if idx < 0 {
		return ""
	}
	name := key[idx+len(marker):]
	if i := strings.Index(name, "/node_modules/"); i >= 0 {
		name = name[i+len("/node_modules/"):]
		if j := strings.Index(name, "/"); j >= 0 {
			name = name[:j]
		}
	}
	if strings.HasPrefix(name, "@") {
		// scoped：@scope/pkg，保留两段；若带子路径（@s/p/sub）只取两段。
		parts := strings.SplitN(name, "/", 3)
		if len(parts) >= 2 {
			return parts[0] + "/" + parts[1]
		}
		return name
	}
	if i := strings.Index(name, "/"); i >= 0 {
		return name[:i]
	}
	return name
}

// hydrateNPMMetadata 为 lockfile 条目补全上游 metadata。
//
// lockfile 里虽然已经有 dependencies/bin/os 等字段，但 v1 lock 与部分 npm 版本
// 写得不全；缺 os/cpu 会让离线侧 npm 把平台专属包当成通用包安装，结果是
// 装上了却跑不起来。因此默认逐版本从 registry 取完整版；取不到时退回 lock
// 自带字段并记一条警告，而不是让整个构建失败（离线构建机偶尔会撞上 registry
// 限流，此时 lock 的数据仍然可用）。
func hydrateNPMMetadata(ctx context.Context, reg *npmRegistry, items []*npmVersion, log logf) error {
	var need []*npmVersion
	for _, item := range items {
		if !item.hydrated {
			need = append(need, item)
		}
	}
	if len(need) == 0 {
		return nil
	}
	log.printf("补全 %d 个 npm 包的上游 metadata", len(need))
	return forEachParallel(len(need), 8, func(i int) error {
		v := need[i]
		meta, err := reg.versionManifest(ctx, v.Name, v.Version)
		if err != nil {
			if v.lockResolved == "" {
				if v.optionalEntry {
					log.printf("警告: 跳过可选依赖 %s（%v）", v.key(), err)
					return nil
				}
				return fmt.Errorf("获取 %s metadata: %w", v.key(), err)
			}
			log.printf("警告: %s 取不到上游 metadata，回退到 lockfile 记录: %v", v.key(), err)
			v.Raw = v.fallbackRaw()
			return nil
		}
		// 保留 lock 的 pinned dist 地址与 integrity：它们对应实际锁定字节。
		meta.lockResolved = firstNonEmpty(v.lockResolved, meta.lockResolved)
		meta.lockIntegrity = firstNonEmpty(v.lockIntegrity, meta.lockIntegrity)
		meta.optionalEntry = v.optionalEntry
		*v = *meta
		return nil
	})
}

// fallbackRaw 用 lockfile 自带字段拼一个最小可用的 version 对象。
func (v *npmVersion) fallbackRaw() map[string]any {
	obj := map[string]any{
		"name":    v.Name,
		"version": v.Version,
	}
	if len(v.Dependencies) > 0 {
		obj["dependencies"] = v.Dependencies
	}
	obj["dist"] = map[string]any{
		"tarball":   firstNonEmpty(v.lockResolved, v.Dist.Tarball),
		"integrity": firstNonEmpty(v.lockIntegrity, v.Dist.Integrity),
	}
	return obj
}

// collectNPM 是 npm 侧的入口：按 manifest 选择 lockfile 或 packages 形态，
// needMetadata=false 时（plan）只解析版本、不取完整 metadata。
func collectNPM(ctx context.Context, m *spec.Manifest, manifestDir string, needMetadata bool, log logf) ([]*npmVersion, error) {
	if len(m.NPM.Lockfiles) == 0 && len(m.NPM.Packages) == 0 {
		return nil, nil
	}
	reg, err := newNPMRegistry(ctx)
	if err != nil {
		return nil, err
	}
	if len(m.NPM.Lockfiles) > 0 {
		items, err := collectNPMLockfiles(ctx, m, manifestDir, log)
		if err != nil {
			return nil, err
		}
		if len(items) == 0 {
			return nil, fmt.Errorf("npm lockfile 未解析出任何包（检查 lockfile 是否为空或只含 devDependencies）")
		}
		if needMetadata {
			if err := hydrateNPMMetadata(ctx, reg, items, log); err != nil {
				return nil, err
			}
		}
		log.printf("npm 闭包: %d 个版本", len(items))
		return items, nil
	}

	items, err := resolveNPMClosure(ctx, reg, m.NPM.Packages, log)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("npm packages 未解析出任何包")
	}
	log.printf("npm 闭包: %d 个版本", len(items))
	return items, nil
}

func manifestDirOf(manifestPath string) string {
	if manifestPath == "" {
		return "."
	}
	abs, err := filepath.Abs(manifestPath)
	if err != nil {
		return filepath.Dir(manifestPath)
	}
	return filepath.Dir(abs)
}
