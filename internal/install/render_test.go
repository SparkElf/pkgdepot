package install

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SparkElf/pkgdepot/internal/spec"
)

// 这些测试保护的是"目标机上到底被写成什么样"的合同。compose/config/apply.env
// 渲染错误在 e2e 里表现为容器起不来或端口不对，是最难定位的一类问题，
// 所以这里完全不依赖网络与 SSH，直接断言字节内容与解析结果。

func testHost() *spec.Host {
	return &spec.Host{
		Address:      "10.20.30.40",
		Port:         22,
		User:         "ops",
		BaseDir:      "/opt/pkgdepot",
		NPMPort:      4873,
		PythonPort:   8080,
		IdentityFile: "/tmp/key",
	}
}

func testManifest() *spec.BundleManifest {
	return &spec.BundleManifest{
		APIVersion:  spec.BundleAPIVersion,
		Name:        "demo",
		CreatedAt:   time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC),
		ToolVersion: "test",
		Images: []spec.ImageEntry{
			{Ref: spec.DefaultVerdaccioImage, File: "verdaccio.tar", Size: 1, SHA256: "aa"},
			{Ref: spec.DefaultPypiserverImage, File: "pypiserver.tar", Size: 1, SHA256: "bb"},
		},
		NPM: []spec.NPMEntry{
			{Name: "left-pad", Version: "1.3.0", File: "left-pad-1.3.0.tgz", SHA256: "cc", Integrity: "sha512-xx"},
		},
		Python: []spec.PyEntry{
			{Name: "requests", Version: "2.32.3", File: "requests-2.32.3-py3-none-any.whl", SHA256: "dd"},
		},
	}
}

func rawJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}

func TestBuildDeployFilesRendersBundleDerivedAssets(t *testing.T) {
	h := testHost()
	bm := testManifest()
	rawManifest := rawJSON(t, bm)

	files, err := buildDeployFiles(h, bm, rawManifest)
	if err != nil {
		t.Fatalf("buildDeployFiles: %v", err)
	}

	// manifest.json 必须原样落盘：重新序列化会改变字节，让目标机上的副本
	// 与 bundle 内的权威文件不一致。
	if !bytes.Equal(files.BundleManifest, rawManifest) {
		t.Error("BundleManifest 未保持原始字节")
	}

	compose := string(files.Compose)
	for _, want := range []string{
		"verdaccio/verdaccio:6",
		"pypiserver/pypiserver:latest",
		`"4873:4873"`,
		`"8080:8080"`,
		"user: \"0:0\"",
		"pull_policy: never",
	} {
		if !strings.Contains(compose, want) {
			t.Errorf("compose 缺少 %q", want)
		}
	}

	// 端口必须跟着 Host 走，不能写死默认值。
	custom := testHost()
	custom.NPMPort, custom.PythonPort = 14873, 18080
	customFiles, err := buildDeployFiles(custom, bm, rawManifest)
	if err != nil {
		t.Fatalf("buildDeployFiles(custom ports): %v", err)
	}
	for _, want := range []string{`"14873:4873"`, `"18080:8080"`} {
		if !strings.Contains(string(customFiles.Compose), want) {
			t.Errorf("自定义端口的 compose 缺少 %q", want)
		}
	}

	// apply.env 是 install.sh 唯一的参数来源，键名必须与脚本读取的一致。
	env := string(files.ApplyEnv)
	for _, want := range []string{
		"PKGDEPOT_BASE_DIR='/opt/pkgdepot'",
		"PKGDEPOT_NPM_PORT=4873",
		"PKGDEPOT_PYTHON_PORT=8080",
		"PKGDEPOT_PROJECT='pkgdepot-pkgdepot-",
		"PKGDEPOT_SKIP_WAIT=0",
	} {
		if !strings.Contains(env, want) {
			t.Errorf("apply.env 缺少 %q:\n%s", want, env)
		}
	}

	if !strings.Contains(string(files.InstallSH), "docker load -i") {
		t.Error("InstallSH 内容异常")
	}
	if !strings.Contains(string(files.VerdaccioConfig), "storage: /verdaccio/storage") {
		t.Error("VerdaccioConfig 内容异常")
	}
}

func TestApplyEnvIsSourceable(t *testing.T) {
	// baseDir 里允许出现空格（虽然少见），apply.env 必须仍然可被 sh source。
	h := testHost()
	h.BaseDir = "/opt/pkg depot/x"
	files, err := buildDeployFiles(h, testManifest(), rawJSON(t, testManifest()))
	if err != nil {
		t.Fatalf("buildDeployFiles: %v", err)
	}
	env := string(files.ApplyEnv)
	if !strings.Contains(env, "PKGDEPOT_BASE_DIR='/opt/pkg depot/x'") {
		t.Fatalf("带空格的 baseDir 未被正确引用:\n%s", env)
	}
	if strings.Contains(env, `"`) {
		t.Errorf("apply.env 不应出现双引号（会被 sh 当普通字符）:\n%s", env)
	}
}

func TestApplyEnvRejectsInvalidHost(t *testing.T) {
	cases := map[string]*spec.Host{
		"相对 baseDir": {Address: "a", User: "u", BaseDir: "relative/dir", NPMPort: 4873, PythonPort: 8080},
		"端口相同":       {Address: "a", User: "u", BaseDir: "/opt/x", NPMPort: 8080, PythonPort: 8080},
		"端口越界":       {Address: "a", User: "u", BaseDir: "/opt/x", NPMPort: 99999, PythonPort: 8080},
	}
	for name, h := range cases {
		if _, err := buildDeployFiles(h, testManifest(), rawJSON(t, testManifest())); err == nil {
			t.Errorf("%s: 应当报错", name)
		}
	}
}

func TestBuildDeployFilesUsesBundleImageRefs(t *testing.T) {
	// bundle 可能携带自定义镜像引用（内网 mirror）。compose 必须用 bundle 里
	// 记录的那个 ref，否则 docker load 之后按默认 ref 找不到镜像。
	bm := testManifest()
	bm.Images[0].Ref = "registry.local:5000/verdaccio:6.10.6"
	bm.Images[1].Ref = "registry.local:5000/pypiserver:v2.4.2"

	files, err := buildDeployFiles(testHost(), bm, rawJSON(t, bm))
	if err != nil {
		t.Fatalf("buildDeployFiles: %v", err)
	}
	compose := string(files.Compose)
	if !strings.Contains(compose, "registry.local:5000/verdaccio:6.10.6") {
		t.Errorf("compose 未使用 bundle 的 verdaccio ref:\n%s", compose)
	}
	if !strings.Contains(compose, "registry.local:5000/pypiserver:v2.4.2") {
		t.Errorf("compose 未使用 bundle 的 pypiserver ref:\n%s", compose)
	}
}

func TestClientFilesUseHostURLs(t *testing.T) {
	h := testHost()
	files, err := buildDeployFiles(h, testManifest(), rawJSON(t, testManifest()))
	if err != nil {
		t.Fatalf("buildDeployFiles: %v", err)
	}
	npmrc := string(files.ClientNPMRC)
	if !strings.Contains(npmrc, "registry=http://10.20.30.40:4873/") {
		t.Errorf("client npmrc 地址错误:\n%s", npmrc)
	}
	// client/npmrc 里不能残留 bundle 的模板占位符。
	if strings.Contains(npmrc, "{{") {
		t.Errorf("client npmrc 残留占位符:\n%s", npmrc)
	}
	pip := string(files.ClientPipConf)
	if !strings.Contains(pip, "index-url = http://10.20.30.40:8080/simple/") {
		t.Errorf("client pip.conf index-url 错误:\n%s", pip)
	}
	// trusted-host 只能是主机名/IP，不能带 scheme 或端口。
	if !strings.Contains(pip, "trusted-host = 10.20.30.40") {
		t.Errorf("client pip.conf trusted-host 错误:\n%s", pip)
	}
	if strings.Contains(pip, "{{") {
		t.Errorf("client pip.conf 残留占位符:\n%s", pip)
	}
}

func TestHostOnly(t *testing.T) {
	cases := map[string]string{
		"10.0.0.5":          "10.0.0.5",
		"10.0.0.5:4873":     "10.0.0.5",
		"http://10.0.0.5/x": "10.0.0.5",
		"https://h:8080/":   "h",
		"[fe80::1]":         "fe80::1",
		"[fe80::1]:22":      "fe80::1",
		"registry.internal": "registry.internal",
	}
	for in, want := range cases {
		if got := hostOnly(in); got != want {
			t.Errorf("hostOnly(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRepoOf(t *testing.T) {
	cases := map[string]string{
		"verdaccio/verdaccio:6":        "verdaccio/verdaccio",
		"registry.local:5000/v:6.10.6": "registry.local:5000/v",
		"registry.local:5000/v":        "registry.local:5000/v",
		"v@sha256:abc":                 "v",
		"v":                            "v",
	}
	for in, want := range cases {
		if got := repoOf(in); got != want {
			t.Errorf("repoOf(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNormalizePyName(t *testing.T) {
	cases := map[string]string{
		"requests":           "requests",
		"PyYAML":             "pyyaml",
		"zope.interface":     "zope-interface",
		"my__pkg":            "my-pkg",
		"a.-_b":              "a-b",
		"backports.zoneinfo": "backports-zoneinfo",
	}
	for in, want := range cases {
		if got := normalizePyName(in); got != want {
			t.Errorf("normalizePyName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestExtractAndFindPyLinks(t *testing.T) {
	// pypiserver 的真实输出（已实测）：href 带 #sha256= 片段。
	body := `
    <html><body>
    <a href="/packages/demo_pkg-1.0.0-py3-none-any.whl#sha256=abc">demo_pkg-1.0.0-py3-none-any.whl</a><br>
    <a href="pkg-2.0.0.tar.gz">pkg-2.0.0.tar.gz</a>
    </body></html>`
	links := extractLinks(body)
	if len(links) != 2 {
		t.Fatalf("extractLinks = %v", links)
	}
	got, ok := findPyFileLink(links, "demo_pkg-1.0.0-py3-none-any.whl")
	if !ok {
		t.Fatalf("未匹配到带 #sha256 的链接: %v", links)
	}
	if !strings.HasPrefix(got, "/packages/") {
		t.Errorf("匹配到错误链接 %q", got)
	}
	if _, ok := findPyFileLink(links, "missing-1.0.whl"); ok {
		t.Error("不应匹配不存在的文件")
	}

	// 相对链接必须被解析成绝对 URL，verify 才能直接下载。
	abs := resolveLink("http://10.0.0.5:8080/simple/pkg/", "pkg-2.0.0.tar.gz")
	if abs != "http://10.0.0.5:8080/simple/pkg/pkg-2.0.0.tar.gz" {
		t.Errorf("resolveLink = %q", abs)
	}
}

func TestParseSRI(t *testing.T) {
	// sha512- 前缀 + base64。已知向量：sha512("") = z4PhNX7vuL3xVChQ1m2AB9Yg5AULVxXcg/SpIdNs6c5H0NE8XYXysP+DGNKHfuwvY7kxvUdBeoGlODJ6+SfaPg==
	algo, hexSum, err := parseSRI("sha512-z4PhNX7vuL3xVChQ1m2AB9Yg5AULVxXcg/SpIdNs6c5H0NE8XYXysP+DGNKHfuwvY7kxvUdBeoGlODJ6+SfaPg==")
	if err != nil {
		t.Fatalf("parseSRI: %v", err)
	}
	if algo != "sha512" {
		t.Errorf("algo = %q", algo)
	}
	if hexSum != "cf83e1357eefb8bdf1542850d66d8007d620e4050b5715dc83f4a921d36ce9ce47d0d13c5d85f2b0ff8318d2877eec2f63b931bd47417a81a538327af927da3e" {
		t.Errorf("hex = %q", hexSum)
	}

	if _, _, err := parseSRI("md5-abc"); err == nil {
		t.Error("不支持的算法应当报错")
	}
	if _, _, err := parseSRI("not-an-sri"); err == nil {
		t.Error("无算法前缀应当报错")
	}
	// 多摘要时取第一个可识别的。
	if algo, _, err := parseSRI("md5-x sha1-2jmj7l5rSw0yVb/vlWAYkK/YBwk="); err != nil || algo != "sha1" {
		t.Errorf("多摘要解析: algo=%q err=%v", algo, err)
	}
}

func TestEscapePackageName(t *testing.T) {
	cases := map[string]string{
		"left-pad":        "left-pad",
		"@scope/pkg":      "@scope/pkg",
		"@scope/pkg name": "@scope/pkg%20name",
	}
	for in, want := range cases {
		if got := escapePackageName(in); got != want {
			t.Errorf("escapePackageName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestInventoryFromManifest(t *testing.T) {
	bm := testManifest()
	bm.NPM = append(bm.NPM, spec.NPMEntry{Name: "aaa", Version: "9.9.9"})
	inv := inventoryFromManifest(bm)
	if len(inv.NPM) != 2 || len(inv.Python) != 1 {
		t.Fatalf("inventory = %+v", inv)
	}
	// 必须排序：同样的 bundle 要产出同样的 hosts.yaml。
	if inv.NPM[0].Name != "aaa" || inv.NPM[1].Name != "left-pad" {
		t.Errorf("npm 未排序: %+v", inv.NPM)
	}
}

func TestDedupeRefs(t *testing.T) {
	refs := []spec.PackageRef{
		{Name: "b", Version: "1"},
		{Name: "a", Version: "2"},
		{Name: "a", Version: "1"},
		{Name: "a", Version: "1"},
	}
	got := dedupeRefs(refs)
	want := []spec.PackageRef{{Name: "a", Version: "1"}, {Name: "a", Version: "2"}, {Name: "b", Version: "1"}}
	if len(got) != len(want) {
		t.Fatalf("dedupeRefs = %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("dedupeRefs[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// --- bundle 读取 ---------------------------------------------------------

// writeBundle 构造一个最小 bundle tar.gz，用于测试 readBundle。
func writeBundle(t *testing.T, files map[string][]byte) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "b.tar.gz")
	f, err := os.Create(p)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for name, data := range files {
		hdr := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(data))}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write header: %v", err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatalf("write body: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gz: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close file: %v", err)
	}
	return p
}

func TestReadBundle(t *testing.T) {
	bm := testManifest()
	raw := rawJSON(t, bm)
	p := writeBundle(t, map[string][]byte{
		"manifest.json":              raw,
		"npm/storage/x/package.json": []byte("{}"),
	})

	gotRaw, gotBM, err := readBundle(p)
	if err != nil {
		t.Fatalf("readBundle: %v", err)
	}
	if !bytes.Equal(gotRaw, raw) {
		t.Error("原始 manifest 字节不一致")
	}
	if gotBM.Name != "demo" || gotBM.ToolVersion != "test" {
		t.Errorf("manifest 解析错误: %+v", gotBM)
	}
	// CreatedAt 必须被正确解析：用 yaml 解码 JSON 会得到零值（已踩过）。
	if gotBM.CreatedAt.IsZero() {
		t.Error("CreatedAt 未解析（可能用错了解码器）")
	}
}

func TestReadBundleHandlesDotPrefix(t *testing.T) {
	// tar 打包方式不同会出现 "./manifest.json"；readBundle 必须能认出来。
	bm := testManifest()
	p := writeBundle(t, map[string][]byte{
		"./manifest.json": rawJSON(t, bm),
	})
	if _, err := readBundleManifest(p); err != nil {
		t.Fatalf("readBundleManifest with ./ prefix: %v", err)
	}
}

func TestReadBundleErrors(t *testing.T) {
	// 缺 manifest.json
	p := writeBundle(t, map[string][]byte{"other.txt": []byte("x")})
	if _, err := readBundleManifest(p); err == nil {
		t.Error("缺少 manifest.json 时应当报错")
	}

	// apiVersion 不对
	bm := testManifest()
	bm.APIVersion = "wrong/v1"
	p2 := writeBundle(t, map[string][]byte{"manifest.json": rawJSON(t, bm)})
	if _, err := readBundleManifest(p2); err == nil {
		t.Error("apiVersion 错误时应当报错")
	}

	// 不是 gzip 的文件
	bad := filepath.Join(t.TempDir(), "bad.tar.gz")
	if err := os.WriteFile(bad, []byte("not gzip"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readBundleManifest(bad); err == nil {
		t.Error("非 gzip 文件应当报错")
	}

	// 不存在的文件
	if _, err := readBundleManifest(filepath.Join(t.TempDir(), "missing.tar.gz")); err == nil {
		t.Error("不存在的 bundle 应当报错")
	}
}

func TestProjectName(t *testing.T) {
	cases := []string{
		"/opt/pkgdepot",
		"/opt/My Depot",
		"/opt/registry_v1",
		"/opt/x",
		"/opt/x/",
		"/srv/pkg depot",
	}
	for _, in := range cases {
		got := projectName(in)
		if !strings.HasPrefix(got, "pkgdepot-") {
			t.Errorf("projectName(%q) = %q, 缺少 pkgdepot- 前缀", in, got)
		}
		// 结果必须只含 compose 允许的字符（小写字母/数字/下划线/连字符）。
		for _, r := range got {
			ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '-'
			if !ok {
				t.Errorf("projectName(%q) = %q 含非法字符 %q", in, got, r)
			}
		}
	}

	// 同名不同路径必须落到不同 project：否则后者 up 会接管前者的容器。
	if a, b := projectName("/opt/x"), projectName("/srv/x"); a == b {
		t.Errorf("不同 baseDir 得到相同 project 名 %q", a)
	}
	// 同一路径必须稳定（重复 apply 要收敛到同一个 project）。
	if a, b := projectName("/opt/pkgdepot"), projectName("/opt/pkgdepot"); a != b {
		t.Errorf("同一 baseDir 得到不同 project 名: %q vs %q", a, b)
	}
	// 尾部斜杠不应改变语义。
	if a, b := projectName("/opt/x"), projectName("/opt/x/"); a != b {
		t.Errorf("尾斜杠改变了 project 名: %q vs %q", a, b)
	}
}

func TestRandomSecretLength(t *testing.T) {
	// verdaccio 6 在 Node 22+ 上要求 secret 恰好 32 字符，否则拒绝启动。
	sec, err := randomSecret()
	if err != nil {
		t.Fatalf("randomSecret: %v", err)
	}
	if len(sec) != 32 {
		t.Errorf("secret 长度 = %d, want 32", len(sec))
	}
	sec2, _ := randomSecret()
	if sec == sec2 {
		t.Error("两次生成的 secret 相同")
	}
}
