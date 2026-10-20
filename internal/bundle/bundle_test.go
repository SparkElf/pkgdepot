package bundle

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SparkElf/pkgdepot/internal/spec"
)

// --- packument 生成 -------------------------------------------------------

// TestBuildPackumentPreservesDependencyContract 保护的是「离线侧能不能解析
// 传递依赖」这条硬合同：verdaccio 播种最隐蔽的坑就是 versions[v] 里丢了
// dependencies / optionalDependencies，npm 会静默按「无依赖」处理。
func TestBuildPackumentPreservesDependencyContract(t *testing.T) {
	seeds := []npmSeedEntry{
		{
			Name: "left-pad", Version: "1.3.0", File: "left-pad-1.3.0.tgz",
			SHA256: "aa", SHA1: "bb",
			MetadataJSON: `{"name":"left-pad","version":"1.3.0","dependencies":{"ms":"^2.0.0"},"optionalDependencies":{"fsevents":"^1.0.0"},"dist":{"tarball":"http://localhost:4873/left-pad/-/left-pad-1.3.0.tgz","integrity":"sha512-x","shasum":"bb"}}`,
		},
		{
			Name: "left-pad", Version: "1.2.0", File: "left-pad-1.2.0.tgz",
			SHA256: "cc", SHA1: "dd",
			MetadataJSON: `{"name":"left-pad","version":"1.2.0","dist":{"tarball":"http://localhost:4873/left-pad/-/left-pad-1.2.0.tgz"}}`,
		},
	}
	doc, err := buildPackument("left-pad", seeds)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}

	// 用匿名结构体断言真实契约，而不是断言「有 versions 键」。
	var parsed struct {
		ID       string `json:"_id"`
		Name     string `json:"name"`
		DistTags struct {
			Latest string `json:"latest"`
		} `json:"dist-tags"`
		Versions map[string]struct {
			Name                 string            `json:"name"`
			Version              string            `json:"version"`
			Dependencies         map[string]string `json:"dependencies"`
			OptionalDependencies map[string]string `json:"optionalDependencies"`
			Dist                 struct {
				Tarball   string `json:"tarball"`
				Integrity string `json:"integrity"`
			} `json:"dist"`
		} `json:"versions"`
		Attachments map[string]map[string]any `json:"_attachments"`
		Time        map[string]any            `json:"time"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.ID != "left-pad" || parsed.Name != "left-pad" {
		t.Fatalf("packument 缺少 _id/name: %+v", parsed)
	}
	if parsed.DistTags.Latest != "1.3.0" {
		t.Fatalf("dist-tags.latest 应为最高版本 1.3.0，实际 %q", parsed.DistTags.Latest)
	}
	v := parsed.Versions["1.3.0"]
	if v.Dependencies["ms"] != "^2.0.0" {
		t.Fatalf("versions[1.3.0].dependencies 丢失: %+v", v)
	}
	if v.OptionalDependencies["fsevents"] != "^1.0.0" {
		t.Fatalf("versions[1.3.0].optionalDependencies 丢失: %+v", v)
	}
	if v.Dist.Tarball == "" {
		t.Fatal("versions[1.3.0].dist.tarball 为空——npm 会报 no dist.tarball field")
	}
	// tarball URL 必须是 <name>/-/<file> 形态，verify 会按这个规则拼。
	if want := "http://localhost:4873/left-pad/-/left-pad-1.3.0.tgz"; v.Dist.Tarball != want {
		t.Fatalf("dist.tarball = %q，期望 %q", v.Dist.Tarball, want)
	}
	if _, ok := parsed.Attachments["left-pad-1.3.0.tgz"]; !ok {
		t.Fatalf("_attachments 缺少 tarball 条目: %+v", parsed.Attachments)
	}
	for _, k := range []string{"created", "modified", "1.3.0", "1.2.0"} {
		if _, ok := parsed.Time[k]; !ok {
			t.Fatalf("time 缺少 %q: %+v", k, parsed.Time)
		}
	}
}

// TestWriteNPMStorageLayout 断言磁盘布局本身：verdaccio 只认这个结构，
// 目录名差一层、secret 长度不对都会让容器起不来。
func TestWriteNPMStorageLayout(t *testing.T) {
	storage := t.TempDir()
	seeds := []npmSeedEntry{
		{Name: "ms", Version: "2.1.3", File: "ms-2.1.3.tgz", SHA256: "aa", SHA1: "bb",
			MetadataJSON: `{"name":"ms","version":"2.1.3","dist":{"tarball":"http://localhost:4873/ms/-/ms-2.1.3.tgz"}}`},
		{Name: "@types/node", Version: "22.10.2", File: "node-22.10.2.tgz", SHA256: "cc", SHA1: "dd",
			MetadataJSON: `{"name":"@types/node","version":"22.10.2","dependencies":{"undici-types":"~6.20.0"},"dist":{"tarball":"http://localhost:4873/@types/node/-/node-22.10.2.tgz"}}`},
	}
	// tarball 文件本身也要存在（writeNPMStorage 只写 packument，实际文件由
	// downloadOneNPM 落盘）；这里模拟完整落盘后的目录。
	for _, s := range seeds {
		dir := filepath.Join(storage, s.Name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, s.File), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeNPMStorage(storage, seeds, nil); err != nil {
		t.Fatal(err)
	}

	for _, s := range seeds {
		packument := filepath.Join(storage, s.Name, "package.json")
		if _, err := os.Stat(packument); err != nil {
			t.Fatalf("缺少 %s: %v", packument, err)
		}
		if _, err := os.Stat(filepath.Join(storage, s.Name, s.File)); err != nil {
			t.Fatalf("缺少 tarball %s: %v", s.File, err)
		}
	}

	// scoped 包必须在 @types/node 这样的两级目录下，而不是被压平成 @types-node。
	if _, err := os.Stat(filepath.Join(storage, "@types", "node", "package.json")); err != nil {
		t.Fatalf("scoped 包目录布局错误: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(storage, verdaccioDBFile))
	if err != nil {
		t.Fatal(err)
	}
	var db struct {
		List   []string `json:"list"`
		Secret string   `json:"secret"`
	}
	if err := json.Unmarshal(raw, &db); err != nil {
		t.Fatal(err)
	}
	if len(db.Secret) != verdaccioSecretLen {
		t.Fatalf("verdaccio secret 长度必须恰好 %d（Node 22 强制），实际 %d: %q",
			verdaccioSecretLen, len(db.Secret), db.Secret)
	}
	if len(db.List) != 2 {
		t.Fatalf("db list 应含 2 个包名，实际 %v", db.List)
	}
}

// TestRandomVerdaccioSecretIsWellFormed 反复生成，防止「偶尔短一位」的编码错误。
func TestRandomVerdaccioSecretIsWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		s, err := randomVerdaccioSecret()
		if err != nil {
			t.Fatal(err)
		}
		if len(s) != verdaccioSecretLen {
			t.Fatalf("secret 长度 %d != %d", len(s), verdaccioSecretLen)
		}
		if seen[s] {
			t.Fatalf("生成了重复 secret: %q", s)
		}
		seen[s] = true
		// 必须是 JSON 安全的字符串（不含引号/反斜杠/控制字符）。
		if raw, err := json.Marshal(s); err != nil || string(raw) != `"`+s+`"` {
			t.Fatalf("secret %q 不是 JSON 安全字符串", s)
		}
	}
}

// --- npm 闭包去重 ---------------------------------------------------------

func testVersion(name, version string) *npmVersion {
	return &npmVersion{Name: name, Version: version}
}

// TestFlattenLockfileDedupesByNameAndVersion 保护「同一个包同一版本只出现一次」。
// lockfile v1 的嵌套树必然产生大量重复；不去重会让 bundle 里出现重复 tarball
// 与重复 manifest 条目，apply 后 verify 会重复下载同一份字节。
func TestFlattenLockfileDedupesByNameAndVersion(t *testing.T) {
	lf := &lockFile{
		LockfileVersion: 3,
		Packages: map[string]lockPackage{
			"": {Name: "root", Version: "1.0.0"},
			// 同一 (name,version) 出现两次：npm 的扁平 lock 里理论上不该有，
			// 但 workspace / link 场景下可能重复，去重逻辑必须兜住。
			"node_modules/a":                {Version: "1.0.0", Resolved: "https://r/a-1.0.0.tgz", Integrity: "sha512-a"},
			"node_modules/b":                {Version: "2.0.0", Resolved: "https://r/b-2.0.0.tgz"},
			"node_modules/b/node_modules/a": {Version: "1.0.0", Resolved: "https://r/a-1.0.0.tgz"},
		},
	}
	items, err := flattenLockfile(lf, false, "test")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("去重后应有 2 个条目，实际 %d: %+v", len(items), items)
	}
	got := map[string]bool{}
	for _, it := range items {
		got[it.key()] = true
	}
	for _, want := range []string{"a@1.0.0", "b@2.0.0"} {
		if !got[want] {
			t.Fatalf("缺少 %s（得到 %v）", want, got)
		}
	}
}

// TestFlattenLockfileV3ScopedNames 覆盖 scoped 包的路径还原：
// key 是 node_modules/@scope/pkg，直接从末段取值会得到 "pkg" 而不是 "@scope/pkg"。
func TestFlattenLockfileV3ScopedNames(t *testing.T) {
	lf := &lockFile{
		LockfileVersion: 3,
		Packages: map[string]lockPackage{
			"node_modules/@types/node":        {Version: "22.10.2", Resolved: "https://r/node-22.10.2.tgz"},
			"node_modules/@esbuild/linux-x64": {Version: "0.24.0", Dev: false},
			// 嵌套 node_modules：必须仍还原成 @scope/pkg
			"node_modules/x/node_modules/@types/node": {Version: "20.1.0"},
			// workspace 软链应被跳过
			"node_modules/workspace-pkg": {Link: true},
		},
	}
	items, err := flattenLockfile(lf, false, "test")
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]string{}
	for _, it := range items {
		names[it.Name] = it.Version
	}
	for name, ver := range map[string]string{
		"@types/node":        "22.10.2",
		"@esbuild/linux-x64": "0.24.0",
	} {
		if names[name] != ver {
			t.Fatalf("scoped 包名还原错误：期望 %s@%s，得到 %v", name, ver, names)
		}
	}
	// 三个 (name,version) 都是真实依赖：20.1.0 是另一条分支需要的版本，
	// 不能因为「同名」就丢掉；link 软链条目必须被跳过。
	if len(items) != 3 {
		t.Fatalf("应有 3 条真实依赖（link 被跳过），实际 %d 条: %+v", len(items), items)
	}
	for _, it := range items {
		if it.Name == "workspace-pkg" {
			t.Fatalf("link 条目不应进入闭包: %+v", it)
		}
	}
}

// TestFlattenLockfileV1NestedTree 覆盖 v1 的嵌套 dependencies 形态。
func TestFlattenLockfileV1NestedTree(t *testing.T) {
	lf := &lockFile{
		LockfileVersion: 1,
		Dependencies: map[string]lockDependency{
			"a": {
				Version: "1.0.0", Resolved: "https://r/a-1.0.0.tgz",
				Dependencies: map[string]lockDependency{
					"b": {Version: "2.0.0", Resolved: "https://r/b-2.0.0.tgz"},
					// 嵌套里的 a 也是 1.0.0 → 与顶层重复，必须去重
					"a": {Version: "1.0.0"},
				},
			},
			"devdep": {Version: "9.9.9", Dev: true},
		},
	}
	items, err := flattenLockfile(lf, false, "test")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("v1 应得到 a@1.0.0 与 b@2.0.0 两个条目（dev 被排除、重复被合并），实际 %d: %+v", len(items), items)
	}
	for _, it := range items {
		if it.Name == "devdep" {
			t.Fatalf("includeDev=false 时不应包含 devDependencies: %+v", it)
		}
	}

	// includeDev=true 时应带上
	items, err = flattenLockfile(lf, true, "test")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 {
		t.Fatalf("includeDev=true 应有 3 个条目，实际 %d", len(items))
	}
}

// TestNameFromLockPath 单独覆盖路径解析的边界，避免它藏在别的用例里。
func TestNameFromLockPath(t *testing.T) {
	cases := map[string]string{
		"node_modules/express":                "express",
		"node_modules/@types/node":            "@types/node",
		"node_modules/a/node_modules/b":       "b",
		"node_modules/a/node_modules/@s/b":    "@s/b",
		"node_modules/@s/b/node_modules/@t/u": "@t/u",
		"node_modules/@s/b/sub/dir":           "@s/b",
		"":                                    "",
		"packages/local":                      "",
	}
	for in, want := range cases {
		if got := nameFromLockPath(in); got != want {
			t.Errorf("nameFromLockPath(%q) = %q，期望 %q", in, got, want)
		}
	}
}

// --- npm 版本/mirror 逻辑 -------------------------------------------------

// TestTarballFileNameFollowsUpstreamConvention 保护 File 字段语义：
// verify 会把它拼进 <name>/-/<file>，scoped 包必须是 node-22.10.2.tgz
// （上游约定），而不是 types-node-22.10.2.tgz（npm pack 对 scoped 的本地命名）。
func TestTarballFileNameFollowsUpstreamConvention(t *testing.T) {
	cases := []struct {
		v    *npmVersion
		want string
	}{
		{&npmVersion{Name: "ms", Version: "2.1.3", Dist: npmDist{Tarball: "https://r/ms/-/ms-2.1.3.tgz"}}, "ms-2.1.3.tgz"},
		{&npmVersion{Name: "@types/node", Version: "22.10.2", Dist: npmDist{Tarball: "https://r/@types/node/-/node-22.10.2.tgz"}}, "node-22.10.2.tgz"},
		{&npmVersion{Name: "@scope/pkg", Version: "1.0.0", lockResolved: "https://r/@scope/pkg/-/pkg-1.0.0.tgz"}, "pkg-1.0.0.tgz"},
		// 没有 URL 时按 name 末段兜底
		{&npmVersion{Name: "@scope/pkg", Version: "1.0.0"}, "pkg-1.0.0.tgz"},
	}
	for _, c := range cases {
		if got := c.v.tarballFileName(); got != c.want {
			t.Errorf("%s 的 tarball 文件名 = %q，期望 %q", c.v.key(), got, c.want)
		}
	}
}

// TestCheckSRIMatchesRealEncoding 用一个真实 tarball 的摘要验证 SRI 解析：
// 这里最容易犯的错是把 base64 当成 hex 或者反过来。
func TestCheckSRIMatchesRealEncoding(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.tgz")
	// 内容任意，只要摘要由同一份字节算出。
	if err := os.WriteFile(path, []byte("hello pkgdepot"), 0o644); err != nil {
		t.Fatal(err)
	}
	d, err := hashFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := checkSRI("sha512-"+d.SHA512B64, d); err != nil || !ok {
		t.Fatalf("正确的 SRI 未被接受: ok=%v err=%v", ok, err)
	}
	if ok, _ := checkSRI("sha512-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA==", d); ok {
		t.Fatal("错误的 SRI 被接受了")
	}
	// sha1 shasum 形式（老 lockfile）
	if ok, _ := checkSRI("sha1-"+base64OfHex(t, d.SHA1Hex), d); !ok {
		t.Fatal("sha1 SRI 未被接受")
	}
}

func base64OfHex(t *testing.T, hexStr string) string {
	t.Helper()
	return encodeBase64(mustHex(hexStr))
}

// --- 版本选择 -------------------------------------------------------------

// TestLatestVersionPrefersStable 保证 dist-tags.latest 不会指向预发布版：
// 客户端 `npm install <pkg>` 默认装 latest，指错会装出非预期版本。
func TestLatestVersionPrefersStable(t *testing.T) {
	cases := []struct {
		versions []string
		want     string
	}{
		{[]string{"1.0.0", "1.1.0", "1.0.1"}, "1.1.0"},
		{[]string{"2.0.0-beta.1"}, "2.0.0-beta.1"},
		{[]string{"2.0.0-beta.1", "1.9.0"}, "1.9.0"},
		{[]string{"1.0.0"}, "1.0.0"},
	}
	for _, c := range cases {
		var entries []npmSeedEntry
		for _, v := range c.versions {
			entries = append(entries, npmSeedEntry{Version: v})
		}
		if got := latestVersion(entries); got != c.want {
			t.Errorf("latestVersion(%v) = %q，期望 %q", c.versions, got, c.want)
		}
	}
}

// TestNPMVersionMetadataKeepsPlatformFields 保证写进 packument 的 version 对象
// 保留 os/cpu/bin/engines：丢了会让离线侧把平台专属包当通用包安装。
func TestNPMVersionMetadataKeepsPlatformFields(t *testing.T) {
	raw := map[string]any{
		"name":    "@esbuild/linux-x64",
		"version": "0.24.0",
		"os":      []any{"linux"},
		"cpu":     []any{"x64"},
		"bin":     map[string]any{"esbuild": "bin/esbuild"},
		"dist":    map[string]any{"tarball": "https://r/x.tgz", "integrity": "sha512-upstream"},
	}
	v, err := newNPMVersionFromRaw(raw)
	if err != nil {
		t.Fatal(err)
	}
	meta, err := v.metadataJSON("linux-x64-0.24.0.tgz")
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]any
	if err := json.Unmarshal([]byte(meta), &back); err != nil {
		t.Fatal(err)
	}
	if _, ok := back["os"]; !ok {
		t.Fatalf("metadataJson 丢了 os 字段: %s", meta)
	}
	if _, ok := back["cpu"]; !ok {
		t.Fatalf("metadataJson 丢了 cpu 字段: %s", meta)
	}
	if _, ok := back["bin"]; !ok {
		t.Fatalf("metadataJson 丢了 bin 字段: %s", meta)
	}
	dist, _ := back["dist"].(map[string]any)
	if dist == nil {
		t.Fatalf("metadataJson 缺 dist: %s", meta)
	}
	if got := dist["tarball"]; got != "http://localhost:4873/@esbuild/linux-x64/-/linux-x64-0.24.0.tgz" {
		t.Fatalf("dist.tarball = %v", got)
	}
	// 上游 integrity 必须原样保留，客户端用它校验字节。
	if got := dist["integrity"]; got != "sha512-upstream" {
		t.Fatalf("dist.integrity 未保留: %v", got)
	}
}

// --- manifest 往返 -------------------------------------------------------

// TestBundleManifestRoundTrip 保护「bundle 里的 manifest.json 能被原样读回」：
// 这是 CLI verify 与 install 侧唯一的权威清单，字段丢了等于校验失去意义。
func TestBundleManifestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	orig := &spec.BundleManifest{
		APIVersion:  spec.BundleAPIVersion,
		Name:        "demo",
		CreatedAt:   time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC),
		ToolVersion: "1.2.3",
		Images: []spec.ImageEntry{
			{Ref: "verdaccio/verdaccio:6", File: "images/verdaccio.tar", Size: 123, SHA256: "aa"},
			{Ref: "pypiserver/pypiserver:latest", File: "images/pypiserver.tar", Size: 456, SHA256: "bb"},
		},
		NPM: []spec.NPMEntry{
			{Name: "ms", Version: "2.1.3", File: "ms-2.1.3.tgz", SHA256: "cc",
				Integrity: "sha512-x", MetadataJSON: `{"name":"ms"}`},
			{Name: "@types/node", Version: "22.10.2", File: "node-22.10.2.tgz", SHA256: "dd"},
		},
		Python: []spec.PyEntry{
			{Name: "idna", Version: "3.10", File: "idna-3.10-py3-none-any.whl", SHA256: "ee"},
		},
	}
	content := filepath.Join(dir, "content")
	if err := os.MkdirAll(content, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeBundleManifest(content, orig); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "b.tar.gz")
	if err := writeTarGz(content, out); err != nil {
		t.Fatal(err)
	}

	got, root, err := ReadManifest(out)
	if err != nil {
		t.Fatal(err)
	}
	if root != "" {
		t.Fatalf("ReadManifest 不应落盘，返回的根目录应为空串，实际 %q", root)
	}
	if got.APIVersion != orig.APIVersion || got.Name != orig.Name || got.ToolVersion != orig.ToolVersion {
		t.Fatalf("头部字段不一致: %+v", got)
	}
	if !got.CreatedAt.Equal(orig.CreatedAt) {
		t.Fatalf("CreatedAt 不一致: %v != %v", got.CreatedAt, orig.CreatedAt)
	}
	if len(got.Images) != 2 || got.Images[0].SHA256 != "aa" || got.Images[1].Size != 456 {
		t.Fatalf("images 往返失败: %+v", got.Images)
	}
	if len(got.NPM) != 2 || got.NPM[1].Integrity != "" || got.NPM[0].Integrity != "sha512-x" {
		t.Fatalf("npm 往返失败: %+v", got.NPM)
	}
	if len(got.Python) != 1 || got.Python[0].File != "idna-3.10-py3-none-any.whl" {
		t.Fatalf("python 往返失败: %+v", got.Python)
	}
}

// TestReadManifestRejectsBadInput 保证坏 bundle 给出可行动的错误，而不是静默返回空清单。
func TestReadManifestRejectsBadInput(t *testing.T) {
	dir := t.TempDir()

	// 1) 不是 gzip
	notGz := filepath.Join(dir, "not.tar.gz")
	if err := os.WriteFile(notGz, []byte("not a gzip"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadManifest(notGz); err == nil {
		t.Fatal("非 tar.gz 文件应报错")
	}

	// 2) tar.gz 里没有 manifest.json
	content := filepath.Join(dir, "empty")
	if err := os.MkdirAll(filepath.Join(content, "images"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(content, "images", "x.tar"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	noManifest := filepath.Join(dir, "nomanifest.tar.gz")
	if err := writeTarGz(content, noManifest); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadManifest(noManifest); err == nil || !strings.Contains(err.Error(), "manifest.json") {
		t.Fatalf("缺少 manifest 时应报错并点名，实际: %v", err)
	}

	// 3) apiVersion 不匹配
	badContent := filepath.Join(dir, "badver")
	if err := os.MkdirAll(badContent, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(badContent, "manifest.json"),
		[]byte(`{"apiVersion":"pkgdepot.bundle/v2","name":"x"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	badVer := filepath.Join(dir, "badver.tar.gz")
	if err := writeTarGz(badContent, badVer); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadManifest(badVer); err == nil || !strings.Contains(err.Error(), spec.BundleAPIVersion) {
		t.Fatalf("apiVersion 不匹配时应报错，实际: %v", err)
	}
}

// TestWriteTarGzLayoutMatchesContract 断言 tar 内部路径就是 docs/interfaces.md
// 写的结构：install.sh 解包后按这些相对路径找文件，路径多一层前缀就会全部 miss。
func TestWriteTarGzLayoutMatchesContract(t *testing.T) {
	dir := t.TempDir()
	content := filepath.Join(dir, "c")
	files := []string{
		"manifest.json",
		"images/verdaccio.tar",
		"images/pypiserver.tar",
		"npm/storage/ms/package.json",
		"npm/storage/ms/ms-2.1.3.tgz",
		"npm/storage/.verdaccio-db.json",
		"python/packages/idna-3.10-py3-none-any.whl",
		"client/npmrc",
		"client/pip.conf",
	}
	for _, rel := range files {
		full := filepath.Join(content, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(rel), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	out := filepath.Join(dir, "b.tar.gz")
	if err := writeTarGz(content, out); err != nil {
		t.Fatal(err)
	}
	names := tarEntryNames(t, out)
	for _, want := range files {
		if !names[want] {
			t.Fatalf("tar 内缺少 %q（实际条目: %v）", want, keysSorted(names))
		}
	}
	// 不应该出现绝对路径或以 ./ 开头的条目。
	for name := range names {
		if strings.HasPrefix(name, "/") || strings.HasPrefix(name, "./") {
			t.Fatalf("tar 条目路径不规范: %q", name)
		}
	}
}

// --- client 模板 ---------------------------------------------------------

// TestClientTemplatesUsePlaceholders 保护「同一 bundle 可部署到任意主机」：
// 写死地址会让 bundle 只能装在某台机器上。
func TestClientTemplatesUsePlaceholders(t *testing.T) {
	dir := t.TempDir()
	if err := writeClientConfigs(dir); err != nil {
		t.Fatal(err)
	}
	npmrc, err := os.ReadFile(filepath.Join(dir, bundleNPMRCFile))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(npmrc), "registry="+PlaceholderNPMURL) {
		t.Fatalf("npmrc 未使用占位符: %s", npmrc)
	}
	pipConf, err := os.ReadFile(filepath.Join(dir, bundlePipConfFile))
	if err != nil {
		t.Fatal(err)
	}
	body := string(pipConf)
	for _, want := range []string{"[global]", "index-url = " + PlaceholderPythonURL, "trusted-host = " + PlaceholderPythonHost} {
		if !strings.Contains(body, want) {
			t.Fatalf("pip.conf 缺少 %q:\n%s", want, body)
		}
	}
	// trusted-host 必须是纯主机名占位符，不能是 URL 形态（pip 不认 URL）。
	if strings.Contains(body, "trusted-host = http") {
		t.Fatalf("trusted-host 不能是 URL: %s", body)
	}
}

// --- 并发/取消安全 -------------------------------------------------------

// TestBuildRespectsCanceledContext 保证 Ctrl-C 能真的停下构建，而不是继续
// 下载几 GB 镜像。
func TestBuildRespectsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	m := &spec.Manifest{APIVersion: spec.ManifestAPIVersion, Name: "x"}
	m.NPM.Packages = []string{"left-pad@1.3.0"}
	out := filepath.Join(t.TempDir(), "x.tar.gz")
	if _, err := Build(ctx, Options{Manifest: m, OutFile: out}); err == nil {
		t.Fatal("已取消的 ctx 不应构建成功")
	}
	if _, err := os.Stat(out); err == nil {
		t.Fatal("失败时不应留下 bundle 文件")
	}
}

// TestMetadataJSONDropsDocsButKeepsInstallFields 保护 manifest 体积与安装正确性
// 之间的取舍：readme 这类文档字段必须被裁掉（否则大闭包的 manifest.json 会
// 膨胀到数 MB），而 bin/os/cpu/engines 必须保留（否则平台专属包装错或跑不起来）。
func TestMetadataJSONDropsDocsButKeepsInstallFields(t *testing.T) {
	raw := map[string]any{
		"name": "esbuild", "version": "0.24.0",
		"readme":               strings.Repeat("very long readme ", 1000),
		"description":          "An extremely fast bundler",
		"homepage":             "https://esbuild.github.io/",
		"maintainers":          []any{map[string]any{"name": "evanw"}},
		"bin":                  map[string]any{"esbuild": "bin/esbuild"},
		"engines":              map[string]any{"node": ">=18"},
		"os":                   []any{"linux"},
		"cpu":                  []any{"x64"},
		"hasInstallScript":     true,
		"optionalDependencies": map[string]any{"@esbuild/linux-x64": "0.24.0"},
		"dependencies":         map[string]any{"should-be-kept": "^1.0.0"},
		"dist":                 map[string]any{"tarball": "https://r/x.tgz", "integrity": "sha512-up"},
	}
	v, err := newNPMVersionFromRaw(raw)
	if err != nil {
		t.Fatal(err)
	}
	meta, err := v.metadataJSON("esbuild-0.24.0.tgz")
	if err != nil {
		t.Fatal(err)
	}
	if len(meta) > 4096 {
		t.Fatalf("metadataJson 过大（%d 字节），文档字段没有裁剪", len(meta))
	}
	var back map[string]any
	if err := json.Unmarshal([]byte(meta), &back); err != nil {
		t.Fatal(err)
	}
	for _, banned := range []string{"readme", "description", "homepage", "maintainers"} {
		if _, ok := back[banned]; ok {
			t.Fatalf("metadataJson 不应包含 %q（只服务展示，不参与安装解析）", banned)
		}
	}
	for _, want := range []string{"bin", "engines", "os", "cpu", "hasInstallScript",
		"optionalDependencies", "dependencies", "dist"} {
		if _, ok := back[want]; !ok {
			t.Fatalf("metadataJson 丢了安装相关字段 %q: %s", want, meta)
		}
	}
}

// TestBundleTarHasOnlyContractPaths 保护交付物本身的布局合同。
//
// bundle 是要被拷进内网的文件，多出的顶层目录会跟着一起进内网、被用户当成
// 有效内容。真实的故障场景：pip 的下载暂存目录曾建在打包树内，于是 bundle
// 里出现 python-download/，与 python/packages 内容重复。
func TestBundleTarHasOnlyContractPaths(t *testing.T) {
	dir := t.TempDir()
	content := filepath.Join(dir, "content")
	// 模拟 build 产出的暂存树：只放合同内的路径
	for _, rel := range []string{
		"manifest.json",
		"images/verdaccio.tar",
		"images/pypiserver.tar",
		"npm/storage/.verdaccio-db.json",
		"npm/storage/ms/package.json",
		"npm/storage/ms/ms-2.1.3.tgz",
		"python/packages/idna-3.10-py3-none-any.whl",
		"client/npmrc",
		"client/pip.conf",
	} {
		full := filepath.Join(content, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(rel), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// 1) 干净的暂存树：校验必须通过，且 tar 里只有合同内的顶层路径。
	if err := checkBundleLayout(content); err != nil {
		t.Fatalf("合法布局被拒绝: %v", err)
	}
	out := filepath.Join(dir, "ok.tar.gz")
	if err := writeTarGz(content, out); err != nil {
		t.Fatal(err)
	}
	for name := range tarEntryNames(t, out) {
		top, _, _ := strings.Cut(name, "/")
		if !bundleTopLevel[top] {
			t.Fatalf("bundle 含合同外顶层路径 %q（完整条目 %q）", top, name)
		}
	}
	if names := tarEntryNames(t, out); names["python-download/idna-3.10-py3-none-any.whl"] {
		t.Fatal("bundle 里出现了 python-download/，pip 暂存目录被打进了交付物")
	}

	// 2) 混入合同外目录（模拟 pip 暂存目录落在打包树内）：必须被拒绝。
	if err := os.MkdirAll(filepath.Join(content, "python-download"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(content, "python-download", "idna-3.10-py3-none-any.whl"),
		[]byte("dup"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := checkBundleLayout(content)
	if err == nil {
		t.Fatal("含 python-download/ 的暂存树应被拒绝")
	}
	if !strings.Contains(err.Error(), "python-download") {
		t.Fatalf("错误信息应点名越界目录，实际: %v", err)
	}
}

// TestBuildDoesNotLeakPipStagingDir 是上一条的端到端版本：真实跑一次 Build，
// 断言产物里不存在任何暂存目录。这里用 python.packages（不需要 requirements
// 文件），并跳过镜像导出之外的网络依赖——若网络不可用则跳过。
func TestBuildDoesNotLeakPipStagingDir(t *testing.T) {
	if os.Getenv("PKGDEPOT_NETWORK_TEST") != "1" {
		t.Skip("set PKGDEPOT_NETWORK_TEST=1 to run（需要 npm/pip 网络与本地 docker 镜像）")
	}
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "pkgdepot.yaml")
	if err := os.WriteFile(manifestPath, []byte(`apiVersion: pkgdepot/v1
name: leakcheck
python:
  packages:
    - "idna==3.10"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := spec.LoadManifest(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "b.tar.gz")
	if _, err := Build(context.Background(), Options{
		Manifest: m, ManifestPath: manifestPath, OutFile: out, ToolVersion: "test",
	}); err != nil {
		t.Fatal(err)
	}
	names := tarEntryNames(t, out)
	for name := range names {
		top, _, _ := strings.Cut(name, "/")
		if !bundleTopLevel[top] {
			t.Fatalf("bundle 含合同外顶层路径 %q（完整条目 %q）", top, name)
		}
	}
	if names["python-download/"] || names["python/packages/idna-3.10-py3-none-any.whl"] == false {
		t.Fatalf("python wheel 布局不对: %v", keysSorted(names))
	}
}
