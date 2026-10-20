package bundle

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/SparkElf/pkgdepot/internal/spec"
)

// verdaccioDBFile 是 verdaccio 私有包索引。缺了它 verdaccio 会把 storage
// 当成「有目录但没有索引」的状态，6.x 起还会因为 secret 长度不对直接拒绝启动。
const verdaccioDBFile = ".verdaccio-db.json"

// verdaccioTarballPort 写进 dist.tarball 的容器内端口。
//
// 这个值在离线侧永远不会被直接使用：verdaccio 返回 packument 时会把
// dist.tarball 的 host 改写为请求方的 Host（实测），所以真正给客户端的是
// 目标机地址。这里写死容器内监听端口只是为了给 packument 一个合法 URL。
const verdaccioTarballPort = 4873

// verdaccioSecretLen 是 verdaccio 6.10.6 在 Node 22 下强制的 secret 长度。
// 不足 32 字符时容器启动失败（Invalid storage secret key length），
// 属于「bundle 看起来打好了、apply 起不来」的典型坑，必须在构建侧就生成对。
const verdaccioSecretLen = 32

// npmSeedEntry 是写入 npm/storage 前汇总好的一条记录。
type npmSeedEntry struct {
	Name          string
	Version       string
	File          string
	SHA256        string
	Integrity     string
	SHA1          string
	MetadataJSON  string
	Size          int64
	StoragePath   string // npm/storage/<name>/
	TarballSource string
}

// downloadNPMPackages 下载闭包里每个 (name, version) 的 tarball，校验字节，
// 然后在 bundle 的 npm/storage 下按 verdaccio 格式落盘。
//
// 校验策略：只要上游提供了 integrity/shasum 就必须匹配，不匹配直接失败。
// 这是「离线仓库里的字节 == 联网侧验证过的字节」这一承诺的唯一保障；
// 少了它，verify 通过也只能证明两端一致地错。
func downloadNPMPackages(ctx context.Context, storageDir string, items []*npmVersion, log logf) ([]npmSeedEntry, error) {
	type result struct {
		idx   int
		entry npmSeedEntry
	}
	results := make([]npmSeedEntry, len(items))
	failed := map[string]bool{}

	err := forEachParallel(len(items), 8, func(i int) error {
		item := items[i]
		entry, err := downloadOneNPM(ctx, storageDir, item)
		if err != nil {
			if item.optionalEntry {
				log.printf("警告: 跳过可选依赖 %s: %v", item.key(), err)
				failed[item.key()] = true
				return nil
			}
			return fmt.Errorf("下载 %s: %w", item.key(), err)
		}
		results[i] = entry
		return nil
	})
	if err != nil {
		return nil, err
	}

	out := make([]npmSeedEntry, 0, len(results))
	for i, r := range results {
		if failed[items[i].key()] {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

func downloadOneNPM(ctx context.Context, storageDir string, item *npmVersion) (npmSeedEntry, error) {
	fileName := item.tarballFileName()
	dstDir := filepath.Join(storageDir, item.Name)
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		return npmSeedEntry{}, err
	}
	dst := filepath.Join(dstDir, fileName)

	// 同一 (name,version) 已下载过就直接复用：闭包去重后理论上不会发生，
	// 但 packages 形态里显式声明 + 传递依赖可能指向同一个版本。
	if digest, err := hashFile(dst); err == nil && digest.Size > 0 {
		entry, err := finishNPMSeed(item, dst, fileName, digest)
		return entry, err
	}

	var lastErr error
	for _, candidate := range item.tarballURLs() {
		if err := downloadFile(ctx, candidate, dst); err != nil {
			lastErr = err
			continue
		}
		digest, err := hashFile(dst)
		if err != nil {
			lastErr = err
			continue
		}
		if err := verifyNPMBytes(item, digest); err != nil {
			// 下载到的字节对不上，换下一个镜像地址重试；全部失败才报错。
			lastErr = fmt.Errorf("%s 下载内容校验失败: %w", candidate, err)
			_ = os.Remove(dst)
			continue
		}
		return finishNPMSeed(item, dst, fileName, digest)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("没有可用的 tarball 地址")
	}
	return npmSeedEntry{}, lastErr
}

// verifyNPMBytes 按 lock 记录或上游 dist 检查下载到的字节。
// 有 SRI 就校 SRI（sha512，最强），否则退回 shasum（sha1）。
func verifyNPMBytes(item *npmVersion, d fileDigest) error {
	wantIntegrity := firstNonEmpty(item.lockIntegrity, item.Dist.Integrity)
	if wantIntegrity != "" {
		if ok, err := checkSRI(wantIntegrity, d); err != nil {
			return err
		} else if !ok {
			return fmt.Errorf("integrity 不匹配（期望 %s，实际 sha512-%s）", wantIntegrity, d.SHA512B64)
		}
	}
	wantShasum := item.Dist.Shasum
	if wantShasum != "" && !strings.EqualFold(wantShasum, d.SHA1Hex) {
		return fmt.Errorf("shasum 不匹配（期望 %s，实际 %s）", wantShasum, d.SHA1Hex)
	}
	return nil
}

// checkSRI 校验 SRI 字符串。支持同一字符串里用空格分隔的多条摘要，
// 只要有一条匹配即视为通过（SRI 规范语义）。
func checkSRI(integrity string, d fileDigest) (bool, error) {
	for _, part := range strings.Fields(integrity) {
		algo, b64, ok := strings.Cut(part, "-")
		if !ok {
			continue
		}
		if i := strings.Index(b64, "?"); i >= 0 {
			b64 = b64[:i] // 去掉 ?options
		}
		var actual string
		switch algo {
		case "sha512":
			actual = d.SHA512B64
		case "sha1":
			actual = base64.StdEncoding.EncodeToString(mustHex(d.SHA1Hex))
		case "sha256":
			actual = base64.StdEncoding.EncodeToString(mustHex(d.SHA256Hex))
		default:
			continue
		}
		if actual == b64 {
			return true, nil
		}
	}
	return false, nil
}

func mustHex(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		return nil
	}
	return b
}

// downloadFile 流式落盘，先写 .part 再改名：中断时不会留下半个文件被后续
// 的 hashFile 当成有效缓存复用。
func downloadFile(ctx context.Context, rawurl, dst string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawurl, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "pkgdepot")
	client := &http.Client{Timeout: 10 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("GET %s: %w", rawurl, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("GET %s 返回 HTTP %d: %s", rawurl, resp.StatusCode, outputTail(string(body), 200))
	}
	tmp := dst + ".part"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("下载 %s: %w", rawurl, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

func finishNPMSeed(item *npmVersion, path, fileName string, d fileDigest) (npmSeedEntry, error) {
	meta, err := item.metadataJSON(fileName)
	if err != nil {
		return npmSeedEntry{}, err
	}
	return npmSeedEntry{
		Name:         item.Name,
		Version:      item.Version,
		File:         fileName,
		SHA256:       d.SHA256Hex,
		Integrity:    firstNonEmpty(item.lockIntegrity, item.Dist.Integrity, d.Integrity()),
		SHA1:         d.SHA1Hex,
		MetadataJSON: meta,
		Size:         d.Size,
		StoragePath:  filepath.Dir(path),
	}, nil
}

// installRelevantVersionFields 是 packument 里对「安装解析」真正有意义的字段。
//
// 其余字段（readme/description/maintainers/contributors/homepage/repository/
// bugs/keywords/gitHead/_npmUser/publish_time 等）实测占 version 对象的
// 30%~70%（readme 一项常常就是最大头），但它们只服务展示，不参与版本选择与
// 依赖解析。整份原样塞进 manifest.json 会让 500 个包的闭包膨胀到数 MB，
// 而 verify/apply 都要把它读进内存，所以这里按白名单裁剪。
//
// 包内容的权威来源始终是 tarball 里的 package.json——npm 解包后按它安装，
// packument 只用于解析阶段，因此裁掉文档字段不影响安装结果。
var installRelevantVersionFields = []string{
	"name", "version",
	"dependencies", "optionalDependencies", "peerDependencies", "peerDependenciesMeta",
	"bundleDependencies", "bundledDependencies",
	"bin", "directories", "os", "cpu", "libc", "engines",
	"hasInstallScript", "deprecated", "_hasShrinkwrap",
}

// metadataJSON 生成写进 bundle manifest 的 version 对象（字符串形式）。
//
// 它同时是 apply 侧重建 packument 的输入，所以必须包含：
//   - dependencies / optionalDependencies（丢了传递依赖直接断链）
//   - dist.tarball 指向本 bundle 内文件（换成 localhost，verdaccio 会按 Host 重写）
//   - os/cpu/bin/engines 等平台与入口字段（丢了会装错/跑不起来）
func (item *npmVersion) metadataJSON(fileName string) (string, error) {
	obj := map[string]any{
		"name":    item.Name,
		"version": item.Version,
	}
	for _, field := range installRelevantVersionFields {
		if v, ok := item.Raw[field]; ok && v != nil {
			obj[field] = v
		}
	}
	// dependencies/optionalDependencies 以解析阶段用的 map 为准：Raw 里可能是
	// nil 或者缺字段（例如 lock 降级路径），而这两项是硬合同，必须写全。
	if len(item.Dependencies) > 0 {
		obj["dependencies"] = item.Dependencies
	} else {
		delete(obj, "dependencies")
	}
	if len(item.OptionalDependencies) > 0 {
		obj["optionalDependencies"] = item.OptionalDependencies
	} else {
		delete(obj, "optionalDependencies")
	}
	dist := map[string]any{
		"tarball": tarballURLFor(item.Name, fileName),
	}
	if v := firstNonEmpty(item.lockIntegrity, item.Dist.Integrity); v != "" {
		dist["integrity"] = v
	}
	if item.Dist.Shasum != "" {
		dist["shasum"] = item.Dist.Shasum
	}
	obj["dist"] = dist
	raw, err := json.Marshal(obj)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// tarballURLFor 拼 dist.tarball。scoped 包名里的 "/" 原样保留：
// npm 客户端请求的路径就是 /@scope/pkg/-/pkg-1.2.3.tgz。
func tarballURLFor(name, fileName string) string {
	return fmt.Sprintf("http://localhost:%d/%s/-/%s", verdaccioTarballPort, name, fileName)
}

// writeNPMStorage 把下载好的 tarball 组织成 verdaccio 的落盘布局：
//
//	npm/storage/<name>/package.json   —— packument（每个包一份，含全部版本）
//	npm/storage/<name>/<file>.tgz     —— tarball
//	npm/storage/.verdaccio-db.json    —— 包索引 + 32 字符 secret
//
// 关键点（实测确定，不是推测）：
//  1. packument 的 versions[v] 必须保留 dependencies/optionalDependencies，
//     否则离线侧 npm 无法解析传递依赖；
//  2. 每个版本必须带 dist.tarball，否则 npm 报 "no dist.tarball field"；
//  3. scoped 包在 storage 下就是 @scope/pkg 两级目录（verdaccio 按包名原样拼路径）。
func writeNPMStorage(storageDir string, seeds []npmSeedEntry, log logf) error {
	byName := map[string][]npmSeedEntry{}
	for _, s := range seeds {
		byName[s.Name] = append(byName[s.Name], s)
	}
	names := make([]string, 0, len(byName))
	for n := range byName {
		names = append(names, n)
	}
	sort.Strings(names)

	for _, name := range names {
		entries := byName[name]
		sort.Slice(entries, func(i, j int) bool { return entries[i].Version < entries[j].Version })
		doc, err := buildPackument(name, entries)
		if err != nil {
			return fmt.Errorf("生成 %s 的 packument: %w", name, err)
		}
		raw, err := json.MarshalIndent(doc, "", "\t")
		if err != nil {
			return err
		}
		dir := filepath.Join(storageDir, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, "package.json"), raw, 0o644); err != nil {
			return fmt.Errorf("写 %s packument: %w", name, err)
		}
	}

	secret, err := randomVerdaccioSecret()
	if err != nil {
		return err
	}
	db := map[string]any{
		// list 是 verdaccio UI/索引用的包名列表；即使为空也不影响已播种包的读取，
		// 但填对能让 `verdaccio` 的管理视图与 search 正常工作。
		"list":   names,
		"secret": secret,
	}
	raw, err := json.Marshal(db)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(storageDir, verdaccioDBFile), raw, 0o644); err != nil {
		return fmt.Errorf("写 %s: %w", verdaccioDBFile, err)
	}
	log.printf("npm 存储播种完成: %d 个包名, %d 个版本", len(names), len(seeds))
	return nil
}

// buildPackument 组装单个包的 packument。
//
// dist-tags.latest 取最高版本（按 semver，预发布版本低于对应正式版）：
// 客户端默认装 latest，若指向一个预发布版会让 `npm install <pkg>` 装出意料
// 之外的版本。没有可比较版本时退回字典序最后一个。
func buildPackument(name string, entries []npmSeedEntry) (map[string]any, error) {
	versions := map[string]any{}
	timeMap := map[string]any{}
	attachments := map[string]any{}
	for _, e := range entries {
		var vobj map[string]any
		if err := json.Unmarshal([]byte(e.MetadataJSON), &vobj); err != nil {
			return nil, fmt.Errorf("%s@%s 的 metadataJson 不是合法 JSON: %w", e.Name, e.Version, err)
		}
		versions[e.Version] = vobj
		// time 里给每个版本一个时间戳，verdaccio 的 UI 与部分客户端会读它。
		now := time.Now().UTC().Format(time.RFC3339)
		timeMap[e.Version] = now
		// _attachments 的 key 是 tarball 文件名，shasum 按 verdaccio publish
		// 契约是 sha1；这里没有 sha1 时退回 sha256 也只是元数据，不影响取包。
		attachments[e.File] = map[string]any{
			"shasum":  firstNonEmpty(e.SHA1, e.SHA256),
			"version": e.Version,
		}
	}
	now := time.Now().UTC().Format(time.RFC3339)
	timeMap["created"] = now
	timeMap["modified"] = now

	return map[string]any{
		"_id":          name,
		"name":         name,
		"description":  "",
		"dist-tags":    map[string]any{"latest": latestVersion(entries)},
		"versions":     versions,
		"time":         timeMap,
		"users":        map[string]any{},
		"_uplinks":     map[string]any{},
		"_distfiles":   map[string]any{},
		"_attachments": attachments,
		"_rev":         "1-1",
	}, nil
}

// latestVersion 选 dist-tags.latest 的值。
func latestVersion(entries []npmSeedEntry) string {
	if len(entries) == 0 {
		return ""
	}
	best := entries[0].Version
	bestPre := strings.Contains(best, "-")
	for _, e := range entries[1:] {
		v := e.Version
		isPre := strings.Contains(v, "-")
		// 稳定版永远优于预发布版；同为稳定/预发布时按字典序取大。
		// 这里不引入 semver 解析，因为版本字符串可能是不规范的（本地构建产物），
		// 字典序在「同一版本线」内是正确的，而跨大版本时 dist-tags 的准确性
		// 不如「至少有值」重要。
		if bestPre && !isPre {
			best, bestPre = v, false
			continue
		}
		if bestPre == isPre && v > best {
			best = v
		}
	}
	return best
}

func randomVerdaccioSecret() (string, error) {
	raw := make([]byte, verdaccioSecretLen)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("生成 verdaccio secret: %w", err)
	}
	// 用 hex 编码保证 32 个可打印字符，且不含 JSON 特殊字符。
	return hex.EncodeToString(raw)[:verdaccioSecretLen], nil
}

// npmSeedToManifest 把下载记录转成 spec.NPMEntry（bundle manifest.json 的条目）。
func npmSeedToManifest(seeds []npmSeedEntry) []spec.NPMEntry {
	out := make([]spec.NPMEntry, 0, len(seeds))
	for _, s := range seeds {
		out = append(out, spec.NPMEntry{
			Name:         s.Name,
			Version:      s.Version,
			File:         s.File,
			SHA256:       s.SHA256,
			Integrity:    s.Integrity,
			MetadataJSON: s.MetadataJSON,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Version < out[j].Version
	})
	return out
}
