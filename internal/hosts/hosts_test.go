package hosts

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SparkElf/pkgdepot/internal/spec"
)

// isolate 把 os.UserConfigDir 指到临时目录。这些测试保护的是"主机清单落在哪个
// 文件、往返后字段还在不在"，用真实用户目录会污染开发机配置。
func isolate(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("HOME", dir)
	t.Setenv("AppData", dir)
	return dir
}

func TestPathUsesUserConfigDir(t *testing.T) {
	dir := isolate(t)
	p, err := Path()
	if err != nil {
		t.Fatalf("Path: %v", err)
	}
	want := filepath.Join(dir, "pkgdepot", "hosts.yaml")
	if p != want {
		t.Fatalf("Path = %q, want %q", p, want)
	}
}

func TestLoadMissingFileIsEmpty(t *testing.T) {
	isolate(t)
	hf, err := Load()
	if err != nil {
		t.Fatalf("Load on missing file: %v", err)
	}
	if hf.APIVersion != spec.HostsAPIVersion {
		t.Errorf("apiVersion = %q, want %q", hf.APIVersion, spec.HostsAPIVersion)
	}
	if hf.Hosts == nil || len(hf.Hosts) != 0 {
		t.Fatalf("hosts = %v, want empty non-nil map", hf.Hosts)
	}
}

func TestAddGetRoundTrip(t *testing.T) {
	isolate(t)
	h := &spec.Host{
		Address:      "10.0.0.5",
		User:         "ops",
		IdentityFile: "/tmp/key",
	}
	if err := Add("inner-1", h); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Add 不能对调用方传入的结构体产生副作用。
	if h.Port != 0 || h.BaseDir != "" {
		t.Fatalf("Add mutated caller struct: %+v", h)
	}

	got, err := Get("inner-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Address != "10.0.0.5" || got.User != "ops" || got.IdentityFile != "/tmp/key" {
		t.Fatalf("Get returned %+v", got)
	}
	// Get 必须补齐默认值，调用方才能直接拼客户端地址。
	if got.Port != 22 || got.BaseDir != spec.DefaultBaseDir ||
		got.NPMPort != spec.DefaultNPMPort || got.PythonPort != spec.DefaultPythonPort {
		t.Fatalf("defaults not applied: %+v", got)
	}
	if want := "http://10.0.0.5:4873/"; got.NPMURL() != want {
		t.Errorf("NPMURL = %q, want %q", got.NPMURL(), want)
	}
}

func TestSaveIsPrivate(t *testing.T) {
	isolate(t)
	if err := Add("h", &spec.Host{Address: "1.2.3.4", User: "root"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	p, err := Path()
	if err != nil {
		t.Fatalf("Path: %v", err)
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatalf("stat hosts file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("hosts.yaml permission = %o, want 600", perm)
	}
}

func TestAddOverwritesSameName(t *testing.T) {
	isolate(t)
	if err := Add("dup", &spec.Host{Address: "1.1.1.1", User: "a"}); err != nil {
		t.Fatalf("first Add: %v", err)
	}
	if err := Add("dup", &spec.Host{Address: "2.2.2.2", User: "b"}); err != nil {
		t.Fatalf("second Add: %v", err)
	}
	hf, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if n := len(hf.Hosts); n != 1 {
		t.Fatalf("hosts count = %d, want 1", n)
	}
	got, err := Get("dup")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Address != "2.2.2.2" || got.User != "b" {
		t.Fatalf("overwrite failed: %+v", got)
	}
}

func TestUpdateRequiresExistingHost(t *testing.T) {
	isolate(t)
	if err := Update("ghost", &spec.Host{Address: "1.2.3.4", User: "u"}); err == nil {
		t.Fatal("Update on unknown host must fail")
	}

	if err := Add("real", &spec.Host{Address: "1.2.3.4", User: "u"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	h, err := Get("real")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	// 回写 LastApply 是 CLI 在 apply/verify 之后的固定动作，必须完整落盘。
	h.LastApply = &spec.Apply{
		AppliedAt:    time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC),
		BundleName:   "pkg-20261020.tar.gz",
		BundleDigest: strings.Repeat("ab", 32),
		Inventory: spec.Inventory{
			NPM:    []spec.PackageRef{{Name: "left-pad", Version: "1.3.0"}},
			Python: []spec.PackageRef{{Name: "requests", Version: "2.32.3"}},
		},
	}
	if err := Update("real", h); err != nil {
		t.Fatalf("Update: %v", err)
	}

	again, err := Get("real")
	if err != nil {
		t.Fatalf("Get after update: %v", err)
	}
	if again.LastApply == nil {
		t.Fatal("LastApply lost after round trip")
	}
	if again.LastApply.BundleName != "pkg-20261020.tar.gz" {
		t.Errorf("BundleName = %q", again.LastApply.BundleName)
	}
	if len(again.LastApply.Inventory.NPM) != 1 || again.LastApply.Inventory.NPM[0].Name != "left-pad" {
		t.Errorf("inventory npm = %+v", again.LastApply.Inventory.NPM)
	}
	if again.LastApply.AppliedAt.IsZero() {
		t.Error("AppliedAt lost (or never set)")
	}
	if !again.LastApply.AppliedAt.Equal(time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)) {
		t.Errorf("AppliedAt = %v", again.LastApply.AppliedAt)
	}
}

func TestRemove(t *testing.T) {
	isolate(t)
	if err := Remove("nope"); err == nil {
		t.Fatal("Remove on unknown host must fail")
	}
	if err := Add("gone", &spec.Host{Address: "1.2.3.4", User: "u"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := Remove("gone"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := Get("gone"); err == nil {
		t.Fatal("Get after Remove must fail")
	}
}

func TestNameValidation(t *testing.T) {
	isolate(t)
	for _, bad := range []string{"", "   ", "a/b", `a\b`, "a b"} {
		if err := Add(bad, &spec.Host{Address: "1.2.3.4", User: "u"}); err == nil {
			t.Errorf("Add(%q) must fail", bad)
		}
	}
	// 合法名字两侧空白应被裁掉。
	if err := Add("  ok  ", &spec.Host{Address: "1.2.3.4", User: "u"}); err != nil {
		t.Fatalf("Add with padded name: %v", err)
	}
	if _, err := Get("ok"); err != nil {
		t.Fatalf("Get trimmed name: %v", err)
	}
}
