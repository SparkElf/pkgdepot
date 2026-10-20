package bundle

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestManualDryRun 是一次性手工验证入口（需要网络 + docker），
// 只在 PKGDEPOT_DRYRUN=1 时运行。保留它是因为它验证的东西单测覆盖不到：
// 真实 npm/pip 下载、真实 docker save、以及 bundle 解包后能被 verdaccio 直接吃下。
func TestManualDryRun(t *testing.T) {
	if os.Getenv("PKGDEPOT_DRYRUN") != "1" {
		t.Skip("set PKGDEPOT_DRYRUN=1 to run")
	}
	dir := t.TempDir()
	appDir := filepath.Join(dir, "app")
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(appDir, "package.json"), []byte(`{
  "name": "dryrun", "version": "1.0.0", "private": true,
  "dependencies": {"ms": "2.1.3", "@types/node": "22.10.2"}
}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "requirements.txt"), []byte("idna==3.10\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(dir, "pkgdepot.yaml")
	if err := os.WriteFile(manifestPath, []byte(`apiVersion: pkgdepot/v1
name: dryrun
npm:
  lockfiles: [app/package.json]
python:
  requirements: [requirements.txt]
  platforms: [manylinux2014_x86_64]
  pythonVersion: "3.12"
  implementation: cp
`), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := loadManifestForTest(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "dryrun.tar.gz")
	bm, err := Build(context.Background(), Options{
		Manifest: m, ManifestPath: manifestPath, OutFile: out, ToolVersion: "dryrun",
		Log: t.Logf,
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.MarshalIndent(bm, "", "  ")
	t.Logf("manifest:\n%s", raw)

	// 解包到临时目录并检查布局
	dest := filepath.Join(dir, "unpacked")
	if err := untarGzForTest(out, dest); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"manifest.json", "images/verdaccio.tar", "images/pypiserver.tar",
		"npm/storage/.verdaccio-db.json", "npm/storage/ms/package.json", "client/npmrc", "client/pip.conf"} {
		if _, err := os.Stat(filepath.Join(dest, rel)); err != nil {
			t.Fatalf("缺少 %s: %v", rel, err)
		}
	}
	// packument 内容检查
	praw, err := os.ReadFile(filepath.Join(dest, "npm/storage/ms/package.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(praw, &doc); err != nil {
		t.Fatal(err)
	}
	versions := doc["versions"].(map[string]any)
	if _, ok := versions["2.1.3"]; !ok {
		t.Fatalf("packument 缺 2.1.3: %s", praw)
	}
	// 用 verdaccio 容器验证解包后的 storage 能被直接服务
	confDir := filepath.Join(dest, "npm", "conf")
	if err := os.MkdirAll(confDir, 0o755); err != nil {
		t.Fatal(err)
	}
	verdaccioConf := `storage: /verdaccio/storage
auth:
  htpasswd:
    file: /verdaccio/conf/htpasswd
uplinks: {}
packages:
  '**':
    access: $all
    publish: $all
    unpublish: $all
log: {type: stdout, format: pretty, level: warn}
`
	if err := os.WriteFile(filepath.Join(confDir, "config.yaml"), []byte(verdaccioConf), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = exec.Command("chmod", "-R", "a+rwX", dest).Run()
	container := "pkgdepot-dryrun"
	_ = exec.Command("docker", "rm", "-f", container).Run()
	run := func(args ...string) string {
		out, err := exec.Command("docker", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("docker %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	run("run", "-d", "--name", container,
		"-p", "14899:4873",
		"-v", filepath.Join(dest, "npm/storage")+":/verdaccio/storage",
		"-v", confDir+":/verdaccio/conf",
		"verdaccio/verdaccio:6")
	defer exec.Command("docker", "rm", "-f", container).Run()

	// 等待健康
	var body []byte
	for i := 0; i < 40; i++ {
		resp, err := exec.Command("curl", "-s", "-m", "3", "http://127.0.0.1:14899/ms").Output()
		if err == nil && strings.Contains(string(resp), "2.1.3") {
			body = resp
			break
		}
		exec.Command("sleep", "0.5").Run()
	}
	if body == nil {
		logs, _ := exec.Command("docker", "logs", container).CombinedOutput()
		t.Fatalf("verdaccio 未提供 ms packument\n%s", logs)
	}
	t.Logf("packument OK: %s", outputTail(string(body), 400))

	// tarball 可取
	tgz, err := exec.Command("curl", "-s", "-m", "10", "-o", "/tmp/dryrun-ms.tgz",
		"-w", "%{http_code} %{size_download}", "http://127.0.0.1:14899/ms/-/ms-2.1.3.tgz").Output()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("tarball: %s", tgz)
	// 真实 npm install（无传递依赖的包）
	installOut, err := exec.Command("docker", "run", "--rm", "--network", "host", "node:22-alpine", "sh", "-c",
		`mkdir -p /tmp/x && cd /tmp/x && printf '{"name":"c","private":true}' > package.json && `+
			`npm install ms@2.1.3 --registry http://127.0.0.1:14899 --no-audit --no-fund 2>&1 | tail -3 && `+
			`node -e "console.log(require('/tmp/x/node_modules/ms/package.json').version)"`).CombinedOutput()
	if err != nil {
		t.Fatalf("离线 npm install 失败: %v\n%s", err, installOut)
	}
	t.Logf("npm install (ms): %s", installOut)

	// 真实 npm install（有传递依赖的包）：这条才验证 packument 保留了
	// versions[v].dependencies —— 少了它 npm 会找不到 undici-types 而失败。
	transOut, err := exec.Command("docker", "run", "--rm", "--network", "host", "node:22-alpine", "sh", "-c",
		`mkdir -p /tmp/y && cd /tmp/y && printf '{"name":"c","private":true}' > package.json && `+
			`npm install @types/node@22.10.2 --registry http://127.0.0.1:14899 --no-audit --no-fund 2>&1 | tail -3 && `+
			`node -e "console.log(require('/tmp/y/node_modules/undici-types/package.json').version)"`).CombinedOutput()
	if err != nil {
		t.Fatalf("离线 npm 传递依赖解析失败: %v\n%s", err, transOut)
	}
	if !strings.Contains(string(transOut), "6.20.0") {
		t.Fatalf("传递依赖 undici-types 版本不对:\n%s", transOut)
	}
	t.Logf("npm install (@types/node → undici-types): %s", transOut)

	// pypiserver：把 python/packages 目录直接挂进去，验证 /simple/ 可枚举且 pip 可装。
	pyContainer := "pkgdepot-dryrun-py"
	_ = exec.Command("docker", "rm", "-f", pyContainer).Run()
	run("run", "-d", "--name", pyContainer, "-p", "14898:8080",
		"-v", filepath.Join(dest, "python", "packages")+":/data/packages",
		"pypiserver/pypiserver:latest", "run", "-p", "8080", "-a", ".", "-P", ".", "/data/packages")
	defer exec.Command("docker", "rm", "-f", pyContainer).Run()

	var simple []byte
	for i := 0; i < 40; i++ {
		resp, err := exec.Command("curl", "-s", "-m", "3", "http://127.0.0.1:14898/simple/idna/").Output()
		if err == nil && strings.Contains(string(resp), "idna-3.10-py3-none-any.whl") {
			simple = resp
			break
		}
		exec.Command("sleep", "0.5").Run()
	}
	if simple == nil {
		logs, _ := exec.Command("docker", "logs", pyContainer).CombinedOutput()
		t.Fatalf("pypiserver 未枚举出 idna wheel\n%s", logs)
	}
	t.Logf("simple index OK: %s", outputTail(string(simple), 300))

	pyInstall, err := exec.Command("docker", "run", "--rm", "--network", "host", "python:3.12-slim", "sh", "-c",
		`pip install --no-cache-dir --index-url http://127.0.0.1:14898/simple/ `+
			`--trusted-host 127.0.0.1 idna==3.10 2>&1 | tail -2 && python -c "import idna; print(idna.__version__)"`).CombinedOutput()
	if err != nil {
		t.Fatalf("离线 pip install 失败: %v\n%s", err, pyInstall)
	}
	if !strings.Contains(string(pyInstall), "3.10") {
		t.Fatalf("pip 装到的 idna 版本不对:\n%s", pyInstall)
	}
	t.Logf("pip install: %s", pyInstall)
}

// TestManualDryRunPackagesForm 验证 npm.packages（"name@range"）形态的闭包解析：
// 这条路径不用 lockfile，完全依赖 semver 解析 + 传递依赖递归。
func TestManualDryRunPackagesForm(t *testing.T) {
	if os.Getenv("PKGDEPOT_DRYRUN") != "1" {
		t.Skip("set PKGDEPOT_DRYRUN=1 to run")
	}
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "pkgdepot.yaml")
	if err := os.WriteFile(manifestPath, []byte(`apiVersion: pkgdepot/v1
name: dryrun2
npm:
  packages:
    - "ms@2.1.3"
    - "@types/node@~22.10.0"
python:
  packages:
    - "idna==3.10"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := loadManifestForTest(manifestPath)
	if err != nil {
		t.Fatal(err)
	}

	// 1) Plan：只解析不下载
	plan, err := Plan(context.Background(), PlanOptions{Manifest: m, ManifestPath: manifestPath, Log: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, e := range plan.NPM {
		got[e.Name] = e.Version
	}
	t.Logf("plan npm: %v", got)
	if got["@types/node"] == "" || got["ms"] != "2.1.3" {
		t.Fatalf("plan 闭包不完整: %v", got)
	}
	// 传递依赖必须在 plan 里就出现（否则用户看到的是残缺清单）
	if len(plan.NPM) < 3 {
		t.Fatalf("plan 应含传递依赖（@types/node 依赖 undici-types），实际 %d 条: %v", len(plan.NPM), got)
	}
	if len(plan.Python) != 1 || plan.Python[0].File != "idna-3.10-py3-none-any.whl" {
		t.Fatalf("plan python 不符: %+v", plan.Python)
	}
	// Plan 不下载：SHA256 应为空
	for _, e := range plan.NPM {
		if e.SHA256 != "" {
			t.Fatalf("plan 不应计算 sha256: %+v", e)
		}
	}

	// 2) 同一份清单走 Build，产物应比 plan 更全（含 sha256/integrity）
	out := filepath.Join(dir, "b.tar.gz")
	bm, err := Build(context.Background(), Options{
		Manifest: m, ManifestPath: manifestPath, OutFile: out, ToolVersion: "dryrun", Log: t.Logf,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(bm.NPM) != len(plan.NPM) {
		t.Fatalf("Build(%d) 与 Plan(%d) 的 npm 数量不一致", len(bm.NPM), len(plan.NPM))
	}
	for _, e := range bm.NPM {
		if e.SHA256 == "" || e.Integrity == "" || e.MetadataJSON == "" {
			t.Fatalf("Build 结果缺校验字段: %+v", e)
		}
	}
	rm, _, err := ReadManifest(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(rm.NPM) != len(bm.NPM) || len(rm.Python) != len(bm.Python) {
		t.Fatalf("ReadManifest 与 Build 返回不一致: %d/%d vs %d/%d",
			len(rm.NPM), len(rm.Python), len(bm.NPM), len(bm.Python))
	}
	// 两次构建的 manifest 内容（除时间外）应稳定
	if rm.Name != "dryrun2" || rm.ToolVersion != "dryrun" {
		t.Fatalf("manifest 头不符: %+v", rm)
	}
}
