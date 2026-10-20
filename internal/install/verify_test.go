package install

import (
	"context"
	"crypto/sha1"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SparkElf/pkgdepot/internal/spec"
)

// fakeRegistry 用 httptest 同时扮演 verdaccio 与 pypiserver。
//
// 响应格式取自实测的真实服务输出：
//   - verdaccio: GET /<name> 返回 packument，dist.tarball 已被重写成请求方 Host；
//     GET /<name>/-/<file> 返回 tarball 字节。
//   - pypiserver: GET /simple/<normalized>/ 返回 HTML，链接带 #sha256= 片段；
//     文件路径是 /packages/<file>。
//
// 用真实格式而不是随手编的 JSON/HTML，才能测出"我们的解析代码与服务实际输出
// 是否对得上"——这正是 verify 最容易静默出错的地方。
type fakeRegistry struct {
	npm   map[string][]byte // 包名 -> packument JSON
	files map[string][]byte // 相对 URL（不带前导 /）-> 内容
}

func (f *fakeRegistry) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimPrefix(r.URL.Path, "/")
	if data, ok := f.files[key]; ok {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(data)
		return
	}
	if data, ok := f.npm[key]; ok {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(data)
		return
	}
	w.WriteHeader(http.StatusNotFound)
	_, _ = fmt.Fprint(w, "not found")
}

func sri512(data []byte) string {
	sum := sha512.Sum512(data)
	return "sha512-" + base64.StdEncoding.EncodeToString(sum[:])
}

func sha256Hex(data []byte) string {
	return hashHex(data)
}

func TestVerifyHappyPath(t *testing.T) {
	tarball := []byte("fake npm tarball bytes")
	shasum := sha1.Sum(tarball) //nolint:gosec // npm 的 shasum 字段就是 sha1
	whl := []byte("fake wheel bytes")

	packument := fmt.Sprintf(`{
	  "name": "left-pad",
	  "versions": {
	    "1.3.0": {
	      "name": "left-pad",
	      "version": "1.3.0",
	      "dependencies": {},
	      "dist": {
	        "shasum": %q,
	        "integrity": %q,
	        "tarball": "http://ignored/left-pad/-/left-pad-1.3.0.tgz"
	      }
	    }
	  },
	  "dist-tags": {"latest": "1.3.0"}
	}`, hex.EncodeToString(shasum[:]), sri512(tarball))

	srv := httptest.NewServer(&fakeRegistry{
		npm: map[string][]byte{
			"left-pad":                   []byte(packument),
			"@demo/scope-pkg":            []byte(`{"name":"@demo/scope-pkg","versions":{"2.0.0":{"dist":{"shasum":"x","integrity":"","tarball":"http://h/@demo/scope-pkg/-/scope-pkg-2.0.0.tgz"}}}}`),
			"@demo/scope-pkg-needs-file": nil,
		},
		files: map[string][]byte{
			"left-pad/-/left-pad-1.3.0.tgz": tarball,
		},
	})
	defer srv.Close()

	bm := &spec.BundleManifest{
		APIVersion: spec.BundleAPIVersion,
		NPM: []spec.NPMEntry{{
			Name: "left-pad", Version: "1.3.0", File: "left-pad-1.3.0.tgz",
			SHA256: sha256Hex(tarball), Integrity: sri512(tarball),
		}},
		Python: []spec.PyEntry{},
	}

	// 直接对 fake 服务做验证：为了复用 Verify 的 URL 拼装逻辑，把 host 指到
	// httptest 的端口上。
	h := hostForServer(t, srv)

	// 先单独验证 npm（python 部分用一个真实可用的索引服务）。
	idxSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/simple/requests/" {
			fmt.Fprintf(w, `<html><body><a href="/packages/requests-2.32.3-py3-none-any.whl#sha256=%s">requests-2.32.3-py3-none-any.whl</a></body></html>`, sha256Hex(whl))
			return
		}
		if r.URL.Path == "/packages/requests-2.32.3-py3-none-any.whl" {
			_, _ = w.Write(whl)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer idxSrv.Close()

	h.PythonPort = portOf(t, idxSrv)
	h.Address = "127.0.0.1"

	bm.Python = []spec.PyEntry{{
		Name: "requests", Version: "2.32.3",
		File: "requests-2.32.3-py3-none-any.whl", SHA256: sha256Hex(whl),
	}}

	inv, err := Verify(context.Background(), h, bm, nil)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(inv.NPM) != 1 || inv.NPM[0].Name != "left-pad" || inv.NPM[0].Version != "1.3.0" {
		t.Errorf("inventory npm = %+v", inv.NPM)
	}
	if len(inv.Python) != 1 || inv.Python[0].Name != "requests" {
		t.Errorf("inventory python = %+v", inv.Python)
	}
}

func TestVerifyNPMFailures(t *testing.T) {
	tarball := []byte("good tarball")
	goodSum := sha256Hex(tarball)
	goodSRI := sri512(tarball)

	mkPackument := func(name, version, tarballURL, integrity string) []byte {
		return []byte(fmt.Sprintf(`{"name":%q,"versions":{%q:{"dist":{"shasum":"aa","integrity":%q,"tarball":%q}}}}`,
			name, version, integrity, tarballURL))
	}

	cases := []struct {
		name    string
		entry   spec.NPMEntry
		npm     map[string][]byte
		files   map[string][]byte
		wantSub string
	}{
		{
			name:    "版本不存在",
			entry:   spec.NPMEntry{Name: "pkg", Version: "9.9.9", File: "pkg-9.9.9.tgz", SHA256: goodSum},
			npm:     map[string][]byte{"pkg": mkPackument("pkg", "1.0.0", "http://h/pkg/-/pkg-1.0.0.tgz", goodSRI)},
			wantSub: "packument 里没有该版本",
		},
		{
			name:    "包不存在",
			entry:   spec.NPMEntry{Name: "ghost", Version: "1.0.0", File: "ghost-1.0.0.tgz", SHA256: goodSum},
			npm:     map[string][]byte{},
			wantSub: "registry 没有这个包",
		},
		{
			name:    "缺 dist.tarball",
			entry:   spec.NPMEntry{Name: "pkg", Version: "1.0.0", File: "pkg-1.0.0.tgz", SHA256: goodSum},
			npm:     map[string][]byte{"pkg": mkPackument("pkg", "1.0.0", "", goodSRI)},
			wantSub: "缺 dist.tarball",
		},
		{
			name:    "tarball 缺失",
			entry:   spec.NPMEntry{Name: "pkg", Version: "1.0.0", File: "pkg-1.0.0.tgz", SHA256: goodSum},
			npm:     map[string][]byte{"pkg": mkPackument("pkg", "1.0.0", "http://h/pkg/-/pkg-1.0.0.tgz", goodSRI)},
			wantSub: "registry 没有 tarball",
		},
		{
			name:  "sha256 不匹配",
			entry: spec.NPMEntry{Name: "pkg", Version: "1.0.0", File: "pkg-1.0.0.tgz", SHA256: strings.Repeat("00", 32)},
			npm:   map[string][]byte{"pkg": mkPackument("pkg", "1.0.0", "http://h/pkg/-/pkg-1.0.0.tgz", goodSRI)},
			files: map[string][]byte{
				"pkg/-/pkg-1.0.0.tgz": tarball,
			},
			wantSub: "sha256 不匹配",
		},
		{
			name: "integrity 与 packument 不一致",
			entry: spec.NPMEntry{Name: "pkg", Version: "1.0.0", File: "pkg-1.0.0.tgz",
				SHA256: goodSum, Integrity: sri512([]byte("different"))},
			npm:     map[string][]byte{"pkg": mkPackument("pkg", "1.0.0", "http://h/pkg/-/pkg-1.0.0.tgz", goodSRI)},
			files:   map[string][]byte{"pkg/-/pkg-1.0.0.tgz": tarball},
			wantSub: "dist.integrity 与 bundle 清单不一致",
		},
		{
			name:    "清单缺校验和",
			entry:   spec.NPMEntry{Name: "pkg", Version: "1.0.0", File: "pkg-1.0.0.tgz"},
			npm:     map[string][]byte{"pkg": mkPackument("pkg", "1.0.0", "http://h/pkg/-/pkg-1.0.0.tgz", "")},
			wantSub: "缺少 sha256/integrity",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(&fakeRegistry{npm: tc.npm, files: tc.files})
			defer srv.Close()
			h := hostForServer(t, srv)
			bm := &spec.BundleManifest{
				APIVersion: spec.BundleAPIVersion,
				NPM:        []spec.NPMEntry{tc.entry},
			}
			_, err := Verify(context.Background(), h, bm, nil)
			if err == nil {
				t.Fatalf("Verify 应当失败")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("错误信息缺少 %q:\n%v", tc.wantSub, err)
			}
			// 错误必须点名包名，内网排障时这是唯一线索。
			if !strings.Contains(err.Error(), tc.entry.Name) {
				t.Errorf("错误信息没有点名包 %q:\n%v", tc.entry.Name, err)
			}
		})
	}
}

func TestVerifyPythonFailures(t *testing.T) {
	whl := []byte("wheel bytes")
	sum := sha256Hex(whl)

	indexWith := func(href string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/simple/mypkg/" {
				fmt.Fprintf(w, `<html><body><a href="%s">mypkg-1.0.0-py3-none-any.whl</a></body></html>`, href)
				return
			}
			if r.URL.Path == "/packages/mypkg-1.0.0-py3-none-any.whl" {
				_, _ = w.Write(whl)
				return
			}
			w.WriteHeader(http.StatusNotFound)
		}
	}

	t.Run("索引里没有文件", func(t *testing.T) {
		srv := httptest.NewServer(indexWith("/packages/other-2.0.0-py3-none-any.whl"))
		defer srv.Close()
		bm := &spec.BundleManifest{Python: []spec.PyEntry{{
			Name: "mypkg", Version: "1.0.0", File: "mypkg-1.0.0-py3-none-any.whl", SHA256: sum,
		}}}
		_, err := Verify(context.Background(), hostForServer(t, srv), bm, nil)
		if err == nil || !strings.Contains(err.Error(), "simple 索引里没有文件") {
			t.Fatalf("期望'索引里没有文件'错误，got %v", err)
		}
	})

	t.Run("包不存在", func(t *testing.T) {
		srv := httptest.NewServer(http.NotFoundHandler())
		defer srv.Close()
		bm := &spec.BundleManifest{Python: []spec.PyEntry{{
			Name: "mypkg", Version: "1.0.0", File: "mypkg-1.0.0-py3-none-any.whl", SHA256: sum,
		}}}
		_, err := Verify(context.Background(), hostForServer(t, srv), bm, nil)
		if err == nil || !strings.Contains(err.Error(), "registry 没有这个包的索引") {
			t.Fatalf("期望'没有索引'错误，got %v", err)
		}
	})

	t.Run("sha256 不匹配", func(t *testing.T) {
		srv := httptest.NewServer(indexWith("/packages/mypkg-1.0.0-py3-none-any.whl"))
		defer srv.Close()
		bm := &spec.BundleManifest{Python: []spec.PyEntry{{
			Name: "mypkg", Version: "1.0.0", File: "mypkg-1.0.0-py3-none-any.whl",
			SHA256: strings.Repeat("11", 32),
		}}}
		_, err := Verify(context.Background(), hostForServer(t, srv), bm, nil)
		if err == nil || !strings.Contains(err.Error(), "sha256 不匹配") {
			t.Fatalf("期望 sha256 不匹配错误，got %v", err)
		}
		if !strings.Contains(err.Error(), "mypkg-1.0.0-py3-none-any.whl") {
			t.Errorf("错误信息没有点名文件: %v", err)
		}
	})

	t.Run("清单缺 sha256", func(t *testing.T) {
		srv := httptest.NewServer(indexWith("/packages/mypkg-1.0.0-py3-none-any.whl"))
		defer srv.Close()
		bm := &spec.BundleManifest{Python: []spec.PyEntry{{
			Name: "mypkg", Version: "1.0.0", File: "mypkg-1.0.0-py3-none-any.whl",
		}}}
		_, err := Verify(context.Background(), hostForServer(t, srv), bm, nil)
		if err == nil || !strings.Contains(err.Error(), "缺少 sha256") {
			t.Fatalf("期望'缺少 sha256'错误，got %v", err)
		}
	})
}

func TestVerifyNormalizesPythonName(t *testing.T) {
	// 清单里的名字可能是 PyYAML / zope.interface，pypiserver 的索引目录是
	// 规范化后的 pyyaml / zope-interface。请求必须走规范化名，否则 301/404。
	whl := []byte("yaml wheel")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/simple/pyyaml/" {
			fmt.Fprintf(w, `<a href="/packages/PyYAML-6.0.1-cp312-cp312-manylinux1_x86_64.whl#sha256=%s">PyYAML-6.0.1-cp312-cp312-manylinux1_x86_64.whl</a>`, sha256Hex(whl))
			return
		}
		if r.URL.Path == "/packages/PyYAML-6.0.1-cp312-cp312-manylinux1_x86_64.whl" {
			_, _ = w.Write(whl)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	bm := &spec.BundleManifest{Python: []spec.PyEntry{{
		Name: "PyYAML", Version: "6.0.1",
		File:   "PyYAML-6.0.1-cp312-cp312-manylinux1_x86_64.whl",
		SHA256: sha256Hex(whl),
	}}}
	inv, err := Verify(context.Background(), hostForServer(t, srv), bm, nil)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(inv.Python) != 1 || inv.Python[0].Name != "PyYAML" {
		t.Errorf("inventory = %+v", inv.Python)
	}
}

func TestVerifyScopedPackage(t *testing.T) {
	// scoped 包名里的 / 是路径分隔符；两种 URL 形式 verdaccio 都接受，
	// 但对 fake 服务必须命中我们实际拼的那一种。
	tarball := []byte("scoped tarball")
	packument := fmt.Sprintf(`{"name":"@demo/scope-pkg","versions":{"2.0.0":{"dist":{"shasum":"aa","integrity":%q,"tarball":"http://h/@demo/scope-pkg/-/scope-pkg-2.0.0.tgz"}}}}`, sri512(tarball))
	srv := httptest.NewServer(&fakeRegistry{
		npm:   map[string][]byte{"@demo/scope-pkg": []byte(packument)},
		files: map[string][]byte{"@demo/scope-pkg/-/scope-pkg-2.0.0.tgz": tarball},
	})
	defer srv.Close()

	bm := &spec.BundleManifest{NPM: []spec.NPMEntry{{
		Name: "@demo/scope-pkg", Version: "2.0.0", File: "scope-pkg-2.0.0.tgz",
		SHA256: sha256Hex(tarball), Integrity: sri512(tarball),
	}}}
	inv, err := Verify(context.Background(), hostForServer(t, srv), bm, nil)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(inv.NPM) != 1 || inv.NPM[0].Name != "@demo/scope-pkg" {
		t.Errorf("inventory = %+v", inv.NPM)
	}
}

func TestVerifyDedupesMultiPlatformPython(t *testing.T) {
	// 同一版本的多平台 wheel 共享 name+version：Inventory 里只应出现一次。
	files := map[string][]byte{
		"/simple/numpy/": []byte(`<a href="/packages/numpy-1.26.0-cp312-cp312-manylinux_x86_64.whl">a</a><a href="/packages/numpy-1.26.0-cp311-cp311-manylinux_x86_64.whl">b</a>`),
	}
	whlA := []byte("numpy cp312")
	whlB := []byte("numpy cp311")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if body, ok := files[r.URL.Path]; ok {
			_, _ = w.Write(body)
			return
		}
		switch r.URL.Path {
		case "/packages/numpy-1.26.0-cp312-cp312-manylinux_x86_64.whl":
			_, _ = w.Write(whlA)
		case "/packages/numpy-1.26.0-cp311-cp311-manylinux_x86_64.whl":
			_, _ = w.Write(whlB)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	bm := &spec.BundleManifest{Python: []spec.PyEntry{
		{Name: "numpy", Version: "1.26.0", File: "numpy-1.26.0-cp312-cp312-manylinux_x86_64.whl", SHA256: sha256Hex(whlA)},
		{Name: "numpy", Version: "1.26.0", File: "numpy-1.26.0-cp311-cp311-manylinux_x86_64.whl", SHA256: sha256Hex(whlB)},
	}}
	inv, err := Verify(context.Background(), hostForServer(t, srv), bm, nil)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(inv.Python) != 1 {
		t.Errorf("多平台 wheel 未去重: %+v", inv.Python)
	}
}

func TestVerifyContextCancel(t *testing.T) {
	// 慢服务 + 已取消的 ctx：必须立刻返回错误而不是等到 5 分钟超时。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	bm := &spec.BundleManifest{NPM: []spec.NPMEntry{{Name: "x", Version: "1.0.0", File: "x.tgz", SHA256: "aa"}}}
	if _, err := Verify(ctx, hostForServer(t, srv), bm, nil); err == nil {
		t.Fatal("已取消的 ctx 应当报错")
	}
}
