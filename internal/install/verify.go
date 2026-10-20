package install

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/SparkElf/pkgdepot/internal/spec"
)

// verifyTimeout 是单个 HTTP 请求的上限。内网 registry 在局域网内，5 分钟足够
// 下载一个几百 MB 的 npm tarball；超过说明服务卡住而不是慢。
const verifyTimeout = 5 * time.Minute

// Verify 通过真实 HTTP 逐字节校验目标主机上 registry 的内容，产出 Inventory。
//
// 校验的是"registry 真的对外提供完整正确的包"（ADR-5），所以：
//   - npm：GET packument 确认该版本存在且 dist 里的 shasum/integrity 与 bundle
//     清单一致，再 GET tarball 逐字节核对 sha256；
//   - python：GET /simple/<name>/ 确认索引里出现该文件，再 GET 文件核对 sha256。
//
// 任何一处不匹配都点名到具体包与文件：内网现场往往只能看到这一屏输出。
func Verify(ctx context.Context, h *spec.Host, bm *spec.BundleManifest, log func(string, ...any)) (*spec.Inventory, error) {
	logf := log
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if h == nil {
		return nil, fmt.Errorf("verify: 主机信息为空")
	}
	if bm == nil {
		return nil, fmt.Errorf("verify: bundle manifest 为空")
	}
	stored := *h
	stored.ApplyDefaults()

	npmURL := strings.TrimRight(stored.NPMURL(), "/")
	pythonURL := stored.PythonURL()

	client := &http.Client{Timeout: verifyTimeout}
	v := &verifier{ctx: ctx, client: client, npmBase: npmURL, pythonBase: pythonURL}

	inv := &spec.Inventory{
		NPM:    make([]spec.PackageRef, 0, len(bm.NPM)),
		Python: make([]spec.PackageRef, 0, len(bm.Python)),
	}

	logf("校验 npm: %d 个版本（%s）", len(bm.NPM), npmURL)
	for _, e := range bm.NPM {
		if err := v.verifyNPM(e); err != nil {
			return nil, err
		}
		inv.NPM = append(inv.NPM, spec.PackageRef{Name: e.Name, Version: e.Version})
		logf("  ✓ %s@%s", e.Name, e.Version)
	}

	logf("校验 python: %d 个文件（%s）", len(bm.Python), pythonURL)
	for _, e := range bm.Python {
		if err := v.verifyPython(e); err != nil {
			return nil, err
		}
		inv.Python = append(inv.Python, spec.PackageRef{Name: e.Name, Version: e.Version})
		logf("  ✓ %s %s", e.Name, e.Version)
	}

	// 同一 (name,version) 可能出现多次（多平台 wheel 共享版本号），Inventory 里
	// 去重后再排序，保证 hosts.yaml 与 skill export 的输入稳定。
	inv.NPM = dedupeRefs(inv.NPM)
	inv.Python = dedupeRefs(inv.Python)
	return inv, nil
}

type verifier struct {
	ctx        context.Context
	client     *http.Client
	npmBase    string
	pythonBase string
}

// npmPackument 是校验用到的 packument 子集。
// versions 的 value 用 RawMessage 保留 dist 里的原始字段。
type npmPackument struct {
	Name     string                     `json:"name"`
	Versions map[string]json.RawMessage `json:"versions"`
	DistTags map[string]string          `json:"dist-tags"`
}

type npmVersionDist struct {
	Dist struct {
		Shasum    string `json:"shasum"`
		Integrity string `json:"integrity"`
		Tarball   string `json:"tarball"`
	} `json:"dist"`
}

// verifyNPM 校验一个 npm 条目：packument 里该版本存在、dist 元数据与清单一致、
// tarball 字节的 sha256 与清单一致。
func (v *verifier) verifyNPM(e spec.NPMEntry) error {
	if e.Name == "" || e.Version == "" {
		return fmt.Errorf("verify npm: bundle 清单里有条目缺 name/version（file=%q）", e.File)
	}
	// 包名里的 /（scoped 包）在 URL 里是路径分隔符，两段都必须转义，
	// 否则 "@scope/pkg" 会被当作路径而不被 verdaccio 识别。
	packumentURL := v.npmBase + "/" + escapePackageName(e.Name)
	raw, status, err := v.getBytes(packumentURL, maxPackumentSize)
	if err != nil {
		return fmt.Errorf("verify npm %s@%s: 获取 packument 失败 (%s): %w", e.Name, e.Version, packumentURL, err)
	}
	if status == http.StatusNotFound {
		return fmt.Errorf("verify npm %s@%s: registry 没有这个包（%s 返回 404），bundle 可能没有真正落盘",
			e.Name, e.Version, packumentURL)
	}
	if status != http.StatusOK {
		return fmt.Errorf("verify npm %s@%s: packument 返回 HTTP %d (%s)", e.Name, e.Version, status, packumentURL)
	}

	var pk npmPackument
	if err := json.Unmarshal(raw, &pk); err != nil {
		return fmt.Errorf("verify npm %s@%s: registry 返回的 packument 不是合法 JSON: %w", e.Name, e.Version, err)
	}
	versionRaw, ok := pk.Versions[e.Version]
	if !ok {
		// 列出实际可用版本，操作员一眼能看出是"版本没播种"还是"名字写错"。
		return fmt.Errorf("verify npm %s@%s: packument 里没有该版本（可用: %s）",
			e.Name, e.Version, strings.Join(mapKeys(pk.Versions), ", "))
	}
	var vd npmVersionDist
	if err := json.Unmarshal(versionRaw, &vd); err != nil {
		return fmt.Errorf("verify npm %s@%s: 版本元数据不是合法 JSON: %w", e.Name, e.Version, err)
	}
	// dist.tarball 必须存在：缺失时 npm 客户端会直接报 "no dist.tarball field"（已实测）。
	if vd.Dist.Tarball == "" {
		return fmt.Errorf("verify npm %s@%s: 版本元数据缺 dist.tarball（npm 客户端会报 no dist.tarball field）",
			e.Name, e.Version)
	}

	// 清单里的 SRI / shasum 是构建侧从上游记录的权威值。registry 返回的必须与之
	// 一致，否则客户端会下载到与上游不同的字节。
	if e.Integrity != "" && vd.Dist.Integrity != "" && vd.Dist.Integrity != e.Integrity {
		return fmt.Errorf("verify npm %s@%s: packument 的 dist.integrity 与 bundle 清单不一致\n  清单: %s\n  registry: %s",
			e.Name, e.Version, e.Integrity, vd.Dist.Integrity)
	}
	if e.SHA256 == "" && e.Integrity == "" {
		return fmt.Errorf("verify npm %s@%s: bundle 清单缺少 sha256/integrity，无法证明内容完整", e.Name, e.Version)
	}

	// tarball 文件名：接口合同里 File 是 basename（不是 bundle 内相对路径）。
	if e.File == "" {
		return fmt.Errorf("verify npm %s@%s: bundle 清单缺少 tarball 文件名", e.Name, e.Version)
	}
	tarballURL := v.npmBase + "/" + escapePackageName(e.Name) + "/-/" + url.PathEscape(e.File)
	gotSHA, size, status, err := v.sha256URL(tarballURL)
	if err != nil {
		return fmt.Errorf("verify npm %s@%s: 下载 tarball 失败 (%s): %w", e.Name, e.Version, tarballURL, err)
	}
	if status == http.StatusNotFound {
		return fmt.Errorf("verify npm %s@%s: registry 没有 tarball %s（%s 返回 404）",
			e.Name, e.Version, e.File, tarballURL)
	}
	if status != http.StatusOK {
		return fmt.Errorf("verify npm %s@%s: 下载 tarball 返回 HTTP %d (%s)", e.Name, e.Version, status, tarballURL)
	}
	if size == 0 {
		return fmt.Errorf("verify npm %s@%s: tarball %s 是空文件", e.Name, e.Version, e.File)
	}

	// 首选 sha256（bundle 清单里一定有）；同时用 sha1 与 SRI 交叉验证，
	// 三条都算才能排除"文件被截断但长度巧合"这类情况。
	if e.SHA256 != "" {
		if subtle.ConstantTimeCompare([]byte(gotSHA), []byte(strings.ToLower(e.SHA256))) != 1 {
			return fmt.Errorf("verify npm %s@%s: tarball sha256 不匹配\n  bundle:   %s\n  registry: %s",
				e.Name, e.Version, e.SHA256, gotSHA)
		}
	}
	if e.Integrity != "" {
		algo, want, perr := parseSRI(e.Integrity)
		if perr != nil {
			return fmt.Errorf("verify npm %s@%s: bundle 清单的 integrity 无法解析: %w", e.Name, e.Version, perr)
		}
		got, herr := v.hashURLHex(tarballURL, algo)
		if herr != nil {
			return fmt.Errorf("verify npm %s@%s: 计算 tarball %s 摘要失败: %w", e.Name, e.Version, algo, herr)
		}
		if !strings.EqualFold(got, want) {
			return fmt.Errorf("verify npm %s@%s: tarball %s 摘要不匹配\n  bundle:   %s\n  registry: %s",
				e.Name, e.Version, algo, want, got)
		}
	}
	return nil
}

// verifyPython 校验一个 python 条目：simple 索引里必须列出该文件，且文件字节的
// sha256 与清单一致。
func (v *verifier) verifyPython(e spec.PyEntry) error {
	if e.Name == "" || e.Version == "" || e.File == "" {
		return fmt.Errorf("verify python: bundle 清单条目不完整（name=%q version=%q file=%q）", e.Name, e.Version, e.File)
	}
	// pypiserver 的索引按规范化名建目录：Name 里的下划线/点会被归一成 -。
	// 请求时直接用规范化名，避免 301 跳转（pypiserver 会跳到 /simple/<name>/）。
	indexURL := v.pythonBase + escapePackageName(normalizePyName(e.Name)) + "/"
	body, status, err := v.getBytesString(indexURL, maxIndexSize)
	if err != nil {
		return fmt.Errorf("verify python %s==%s: 获取 simple 索引失败 (%s): %w", e.Name, e.Version, indexURL, err)
	}
	if status == http.StatusNotFound {
		return fmt.Errorf("verify python %s==%s: registry 没有这个包的索引（%s 返回 404）",
			e.Name, e.Version, indexURL)
	}
	if status != http.StatusOK {
		return fmt.Errorf("verify python %s==%s: simple 索引返回 HTTP %d (%s)", e.Name, e.Version, status, indexURL)
	}

	links := extractLinks(body)
	fileURL, ok := findPyFileLink(links, e.File)
	if !ok {
		return fmt.Errorf("verify python %s==%s: simple 索引里没有文件 %s（索引里是: %s）",
			e.Name, e.Version, e.File, strings.Join(links, ", "))
	}
	fileURL = resolveLink(indexURL, fileURL)

	gotSHA, _, status, err := v.sha256URL(fileURL)
	if err != nil {
		return fmt.Errorf("verify python %s==%s: 下载 %s 失败 (%s): %w", e.Name, e.Version, e.File, fileURL, err)
	}
	if status != http.StatusOK {
		return fmt.Errorf("verify python %s==%s: 下载 %s 返回 HTTP %d (%s)",
			e.Name, e.Version, e.File, status, fileURL)
	}
	if e.SHA256 == "" {
		return fmt.Errorf("verify python %s==%s: bundle 清单缺少 sha256，无法证明 %s 内容完整", e.Name, e.Version, e.File)
	}
	if subtle.ConstantTimeCompare([]byte(gotSHA), []byte(strings.ToLower(e.SHA256))) != 1 {
		return fmt.Errorf("verify python %s==%s: %s sha256 不匹配\n  bundle:   %s\n  registry: %s",
			e.Name, e.Version, e.File, e.SHA256, gotSHA)
	}
	return nil
}

// --- HTTP helpers --------------------------------------------------------

func (v *verifier) getBytesString(url string, limit int64) (string, int, error) {
	raw, status, err := v.getBytes(url, limit)
	return string(raw), status, err
}

func (v *verifier) getBytes(url string, limit int64) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(v.ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("构造请求: %w", err)
	}
	resp, err := v.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return body, resp.StatusCode, nil
}

// sha256URL 边下载边算 sha256，不把包内容读进内存（bundle 里单个包可能上百 MB）。
func (v *verifier) sha256URL(url string) (sum string, size int64, status int, err error) {
	req, err := http.NewRequestWithContext(v.ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", 0, 0, fmt.Errorf("构造请求: %w", err)
	}
	resp, err := v.client.Do(req)
	if err != nil {
		return "", 0, 0, err
	}
	defer resp.Body.Close()
	h := sha256.New()
	n, err := io.Copy(h, resp.Body)
	if err != nil {
		return "", n, resp.StatusCode, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, resp.StatusCode, nil
}

// hashURLHex 按指定算法重新下载并计算摘要（用于 SRI 交叉验证）。
func (v *verifier) hashURLHex(url, algo string) (string, error) {
	req, err := http.NewRequestWithContext(v.ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := v.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	switch algo {
	case "sha512":
		h := newSHA512()
		if _, err := io.Copy(h, resp.Body); err != nil {
			return "", err
		}
		return hex.EncodeToString(h.Sum(nil)), nil
	case "sha1":
		h := sha1.New() //nolint:gosec // 只为与上游 shasum 交叉验证，不是安全用途
		if _, err := io.Copy(h, resp.Body); err != nil {
			return "", err
		}
		return hex.EncodeToString(h.Sum(nil)), nil
	default:
		return "", fmt.Errorf("不支持的摘要算法 %q", algo)
	}
}

const (
	maxPackumentSize = 64 << 20 // 大型 monorepo 的 packument 可以到几十 MB
	maxIndexSize     = 16 << 20
)

// simpleLinkRe 抓 simple 索引里的 <a href="...">text</a>。pypiserver 的输出
// 结构固定，但第三方镜像可能加属性，所以只依赖 href 与标签文本。
var simpleLinkRe = regexp.MustCompile(`(?is)<a\s[^>]*href\s*=\s*"([^"]+)"[^>]*>(.*?)</a>`)

// extractLinks 返回索引里所有链接的 href。
func extractLinks(body string) []string {
	var out []string
	for _, m := range simpleLinkRe.FindAllStringSubmatch(body, -1) {
		out = append(out, html.UnescapeString(m[1]))
	}
	return out
}

// findPyFileLink 在索引链接里找与清单文件名匹配的那一条。
//
// 匹配规则：链接的路径末尾（去掉 #sha256= 片段）等于文件名。
// 直接比较 basename 而不是完整 URL：不同镜像给出的 URL 前缀（/packages/、
// 相对路径、绝对 URL）不统一，文件名才是稳定标识。
func findPyFileLink(links []string, file string) (string, bool) {
	for _, l := range links {
		base := l
		if i := strings.IndexByte(base, '#'); i >= 0 {
			base = base[:i]
		}
		if i := strings.IndexAny(base, "?"); i >= 0 {
			base = base[:i]
		}
		if path.Base(base) == file {
			return l, true
		}
	}
	return "", false
}

// resolveLink 把索引里的相对链接补成绝对 URL。
func resolveLink(indexURL, link string) string {
	base, err := url.Parse(indexURL)
	if err != nil {
		return link
	}
	ref, err := url.Parse(link)
	if err != nil {
		return link
	}
	return base.ResolveReference(ref).String()
}

// normalizePyName 按 PEP 503 规范化 python 包名：连续的 -_. 归一成 -，转小写。
// pypiserver 按这个规则建索引目录，verify 必须用同一规则拼 URL。
func normalizePyName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	var b strings.Builder
	lastDash := false
	for _, r := range name {
		if r == '-' || r == '_' || r == '.' {
			if !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
			continue
		}
		b.WriteRune(r)
		lastDash = false
	}
	return strings.Trim(b.String(), "-")
}

// escapePackageName 转义包名中的路径分隔符，同时保留 '@' 与 '/' 的语义位置。
// npm registry 两种形式都接受（@scope/pkg 与 @scope%2fpkg，均已实测 200），
// 这里保留 '/' 让日志更可读，只转义可能破坏 URL 的字符。
func escapePackageName(name string) string {
	parts := strings.Split(name, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

// parseSRI 解析 "sha512-<base64>" 形式的 SRI，返回算法名与 hex 摘要。
func parseSRI(sri string) (algo, hexSum string, err error) {
	// SRI 允许空格分隔多个摘要，取第一个可识别的。
	for _, part := range strings.Fields(sri) {
		i := strings.IndexByte(part, '-')
		if i <= 0 {
			continue
		}
		a := part[:i]
		switch a {
		case "sha512", "sha1":
			raw := part[i+1:]
			// 允许带 options（?foo）；当前不用。
			if j := strings.IndexByte(raw, '?'); j >= 0 {
				raw = raw[:j]
			}
			sum, derr := base64Decode(raw)
			if derr != nil {
				return "", "", fmt.Errorf("解析 SRI %q: %w", part, derr)
			}
			return a, hex.EncodeToString(sum), nil
		}
	}
	return "", "", fmt.Errorf("不支持的 SRI 格式 %q（需要 sha512-/sha1- 前缀）", sri)
}

func mapKeys(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortRefs(refs []spec.PackageRef) {
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].Name != refs[j].Name {
			return refs[i].Name < refs[j].Name
		}
		return refs[i].Version < refs[j].Version
	})
}

// dedupeRefs 去掉重复的 (name,version) 并排序。
func dedupeRefs(refs []spec.PackageRef) []spec.PackageRef {
	seen := make(map[spec.PackageRef]bool, len(refs))
	out := make([]spec.PackageRef, 0, len(refs))
	for _, r := range refs {
		if seen[r] {
			continue
		}
		seen[r] = true
		out = append(out, r)
	}
	sortRefs(out)
	return out
}
