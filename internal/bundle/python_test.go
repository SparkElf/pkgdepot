package bundle

import (
	"strings"
	"testing"

	"github.com/SparkElf/pkgdepot/internal/spec"
)

// TestPythonItemFromWheelName 保护 wheel 名解析：name-version 是 packument /
// inventory / verify 都要用的字段，标签段（cp312 / py3-none-any / manylinux）
// 绝不能混进版本号。
func TestPythonItemFromWheelName(t *testing.T) {
	cases := []struct {
		file    string
		name    string
		version string
	}{
		{"idna-3.10-py3-none-any.whl", "idna", "3.10"},
		{"requests-2.32.3-py3-none-any.whl", "requests", "2.32.3"},
		// 平台专属 wheel：标签很长，版本号仍是 3.5.2
		{"charset_normalizer-3.5.2-cp312-cp312-manylinux2014_x86_64.manylinux_2_17_x86_64.manylinux_2_28_x86_64.whl",
			"charset-normalizer", "3.5.2"},
		{"numpy-1.26.4-cp312-cp312-musllinux_1_2_x86_64.whl", "numpy", "1.26.4"},
		{"pydantic_core-2.27.2-cp312-cp312-manylinux_2_17_x86_64.manylinux2014_x86_64.whl",
			"pydantic-core", "2.27.2"},
		// 带 build tag 的 wheel（PEP 427 允许第 3 段是 build tag）
		{"foo-1.0.0-1-py3-none-any.whl", "foo", "1.0.0"},
		// 预发布版本
		{"proto-1.2.3rc1-py3-none-any.whl", "proto", "1.2.3rc1"},
		// 大写文件名（PyPI 上存在）
		{"PyYAML-6.0.2-cp312-cp312-manylinux_2_17_x86_64.whl", "pyyaml", "6.0.2"},
	}
	for _, c := range cases {
		got, err := pythonItemFromFile(c.file)
		if err != nil {
			t.Errorf("pythonItemFromFile(%q): %v", c.file, err)
			continue
		}
		if got.Name != c.name {
			t.Errorf("%q 的包名 = %q，期望 %q", c.file, got.Name, c.name)
		}
		if got.Version != c.version {
			t.Errorf("%q 的版本 = %q，期望 %q", c.file, got.Version, c.version)
		}
		if got.File != c.file {
			t.Errorf("%q 的 File 字段被改写为 %q", c.file, got.File)
		}
	}
}

// TestPythonItemFromSDistName 覆盖 sdist：发行名里允许出现 '-'，
// 所以必须从右往左找「像版本号」的段，不能按第一个 '-' 切。
func TestPythonItemFromSDistName(t *testing.T) {
	cases := []struct {
		file    string
		name    string
		version string
	}{
		{"requests-2.32.3.tar.gz", "requests", "2.32.3"},
		{"charset-normalizer-3.5.2.tar.gz", "charset-normalizer", "3.5.2"},
		{"python-dateutil-2.9.0.post0.tar.gz", "python-dateutil", "2.9.0.post0"},
		{"zope.interface-6.1.tar.gz", "zope-interface", "6.1"},
		{"my-package-1.0.0rc2.zip", "my-package", "1.0.0rc2"},
	}
	for _, c := range cases {
		got, err := pythonItemFromFile(c.file)
		if err != nil {
			t.Errorf("pythonItemFromFile(%q): %v", c.file, err)
			continue
		}
		if got.Name != c.name || got.Version != c.version {
			t.Errorf("%q 解析为 (%q, %q)，期望 (%q, %q)", c.file, got.Name, got.Version, c.name, c.version)
		}
	}
}

// TestPythonItemRejectsNonDist 保证 pip 下载目录里的中间产物不会被当成包搬进
// bundle（pypiserver 会把它们也枚举出来，客户端拉到就报错）。
func TestPythonItemRejectsNonDist(t *testing.T) {
	for _, f := range []string{"requests-2.32.3-py3-none-any.whl.metadata", "report.json", "requests", "requests.egg-info"} {
		if _, err := pythonItemFromFile(f); err == nil {
			t.Errorf("%q 不应被识别为 python 分发文件", f)
		}
		if isPythonDistFile(f) {
			t.Errorf("isPythonDistFile(%q) 应为 false", f)
		}
	}
	for _, f := range []string{"a-1.0-py3-none-any.whl", "a-1.0.tar.gz", "a-1.0.zip"} {
		if !isPythonDistFile(f) {
			t.Errorf("isPythonDistFile(%q) 应为 true", f)
		}
	}
}

// TestCanonicalPyName 保证包名按 PEP 503 规范化：PyPI 的 /simple/<name>/ 路径
// 只认规范化形式，写错就 404。
func TestCanonicalPyName(t *testing.T) {
	cases := map[string]string{
		"charset_normalizer":     "charset-normalizer",
		"Charset.Normalizer":     "charset-normalizer",
		"zope.interface":         "zope-interface",
		"requests":               "requests",
		"my__pkg..name":          "my-pkg-name",
		"-leading-and-trailing-": "leading-and-trailing",
	}
	for in, want := range cases {
		if got := canonicalPyName(in); got != want {
			t.Errorf("canonicalPyName(%q) = %q，期望 %q", in, got, want)
		}
	}
}

// TestPythonConstraintArgsEnforcesBinary 保护 pip 参数组合的正确性：
// 一旦出现 --platform/--python-version/--implementation，pip 强制要求
// --only-binary=:all:，少一个就整条命令报错退出。
func TestPythonConstraintArgsEnforcesBinary(t *testing.T) {
	args := pythonConstraintArgs(spec.PythonInput{
		Platforms:      []string{"manylinux2014_x86_64", "manylinux2014_x86_64", " "},
		PythonVersion:  "3.12",
		Implementation: "cp",
	})
	joined := " " + joinArgs(args) + " "
	for _, want := range []string{
		"--only-binary :all:",
		"--python-version 3.12",
		"--implementation cp",
		"--platform manylinux2014_x86_64",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("缺少参数 %q（实际: %v）", want, args)
		}
	}
	// 重复/空平台必须去重，否则同一个 --platform 传两次（pip 允许但无意义）
	count := 0
	for i, a := range args {
		if a == "--platform" {
			count++
			if i+1 >= len(args) {
				t.Fatalf("--platform 缺少值: %v", args)
			}
		}
	}
	if count != 1 {
		t.Errorf("--platform 应出现 1 次（去重后），实际 %d 次: %v", count, args)
	}

	// 未声明约束时也要落到默认值：默认 3.12 / cp，否则 pip 会沿用构建机的
	// 解释器标签，产出的 wheel 在目标机上装不上。
	def := pythonConstraintArgs(spec.PythonInput{})
	defJoined := " " + joinArgs(def) + " "
	for _, want := range []string{"--python-version 3.12", "--implementation cp", "--only-binary :all:"} {
		if !strings.Contains(defJoined, want) {
			t.Errorf("默认参数缺少 %q（实际: %v）", want, def)
		}
	}
}

func joinArgs(args []string) string {
	out := ""
	for i, a := range args {
		if i > 0 {
			out += " "
		}
		out += a
	}
	return out
}

// TestReadPipReportDedupes 保证同一个文件在报告里出现多次时只保留一条
// （pip 的 report 在 extras/多约束场景下可能重复），且 File 从 URL 提取正确。
func TestReadPipReportDedupes(t *testing.T) {
	dir := t.TempDir()
	report := `{
	  "install": [
	    {"metadata":{"name":"idna","version":"3.10"},
	     "download_info":{"url":"https://pypi.example/packages/aa/idna-3.10-py3-none-any.whl"}},
	    {"metadata":{"name":"idna","version":"3.10"},
	     "download_info":{"url":"https://pypi.example/packages/aa/idna-3.10-py3-none-any.whl"}},
	    {"metadata":{"name":"charset_normalizer","version":"3.5.2"},
	     "download_info":{"url":"https://pypi.example/packages/bb/charset_normalizer-3.5.2-cp312-cp312-manylinux2014_x86_64.whl#sha256=abc"}}
	  ]
	}`
	path := dir + "/report.json"
	if err := writeFileString(path, report); err != nil {
		t.Fatal(err)
	}
	items, err := readPipReport(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("应去重为 2 条，实际 %d: %+v", len(items), items)
	}
	byName := map[string]pipDownloadItem{}
	for _, it := range items {
		byName[it.Name] = it
	}
	if got := byName["idna"]; got.File != "idna-3.10-py3-none-any.whl" || got.Version != "3.10" {
		t.Fatalf("idna 解析错误: %+v", got)
	}
	// URL 带 #sha256= 片段时 File 必须只取文件名部分。
	if got := byName["charset-normalizer"]; got.File != "charset_normalizer-3.5.2-cp312-cp312-manylinux2014_x86_64.whl" {
		t.Fatalf("带 fragment 的 URL 未正确提取文件名: %+v", got)
	}
}
