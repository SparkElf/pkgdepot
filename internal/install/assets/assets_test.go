package assets

import (
	"bytes"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// 这些测试保护的是"目标机真的能起来"的合同，而不是模板文本本身：
// compose 必须是合法 YAML 且带着已知必需的字段，install.sh 必须语法正确且
// 覆盖了实测得到的两个坑（verdaccio root、pypiserver -P .）。

func TestComposeRendersValidYAML(t *testing.T) {
	raw, err := Compose(ComposeOptions{
		VerdaccioImage:  "verdaccio/verdaccio:6",
		PypiserverImage: "pypiserver/pypiserver:latest",
		NPMPort:         4873,
		PythonPort:      8080,
	})
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	var doc struct {
		Services map[string]struct {
			Image       string   `yaml:"image"`
			User        string   `yaml:"user"`
			Restart     string   `yaml:"restart"`
			Command     []string `yaml:"command"`
			PullPolicy  string   `yaml:"pull_policy"`
			Ports       []string `yaml:"ports"`
			Volumes     []string `yaml:"volumes"`
			Healthcheck *struct {
				Test []string `yaml:"test"`
			} `yaml:"healthcheck"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("compose 不是合法 YAML: %v\n%s", err, raw)
	}

	v, ok := doc.Services["verdaccio"]
	if !ok {
		t.Fatalf("compose 缺少 verdaccio 服务:\n%s", raw)
	}
	// 以默认 uid 10001 运行会打不开 root 拥有的 bind mount config（已实测）。
	if v.User != "0:0" {
		t.Errorf("verdaccio user = %q, want 0:0（否则 bind mount 权限不足）", v.User)
	}
	if v.Image != "verdaccio/verdaccio:6" {
		t.Errorf("verdaccio image = %q", v.Image)
	}
	if v.Restart != "unless-stopped" {
		t.Errorf("verdaccio restart = %q", v.Restart)
	}
	if v.PullPolicy != "never" {
		t.Errorf("verdaccio pull_policy = %q, want never（离线机无网可拉）", v.PullPolicy)
	}
	if !containsSub(v.Ports, "4873:4873") {
		t.Errorf("verdaccio ports = %v, want 4873:4873", v.Ports)
	}
	if !containsSub(v.Volumes, "npm/conf:/verdaccio/conf") || !containsSub(v.Volumes, "npm/storage:/verdaccio/storage") {
		t.Errorf("verdaccio volumes = %v", v.Volumes)
	}

	p, ok := doc.Services["pypiserver"]
	if !ok {
		t.Fatalf("compose 缺少 pypiserver 服务:\n%s", raw)
	}
	if !containsSub(p.Ports, "8080:8080") {
		t.Errorf("pypiserver ports = %v", p.Ports)
	}
	if !containsSub(p.Volumes, "python/packages:/data/packages") {
		t.Errorf("pypiserver volumes = %v", p.Volumes)
	}
	// -P . 缺失时 pypiserver 直接拒绝启动（已实测的错误信息见任务说明）。
	if strings.Join(p.Command, " ") != "run -p 8080 -a . -P . /data/packages" {
		t.Errorf("pypiserver command = %v", p.Command)
	}
}

func TestComposeUsesOverriddenImagesAndPorts(t *testing.T) {
	raw, err := Compose(ComposeOptions{
		VerdaccioImage:  "registry.local/verdaccio:6.10.6",
		PypiserverImage: "registry.local/pypiserver:v2",
		NPMPort:         14873,
		PythonPort:      18080,
	})
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	s := string(raw)
	for _, want := range []string{
		`image: "registry.local/verdaccio:6.10.6"`,
		`image: "registry.local/pypiserver:v2"`,
		`"14873:4873"`,
		`"18080:8080"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("compose 缺少 %q:\n%s", want, s)
		}
	}
}

func TestComposeRejectsBadOptions(t *testing.T) {
	base := ComposeOptions{
		VerdaccioImage:  "v",
		PypiserverImage: "p",
		NPMPort:         4873,
		PythonPort:      8080,
	}
	cases := map[string]ComposeOptions{
		"空镜像":   {PypiserverImage: "p", NPMPort: 4873, PythonPort: 8080},
		"端口为 0": {VerdaccioImage: "v", PypiserverImage: "p", NPMPort: 0, PythonPort: 8080},
		"端口相同":  {VerdaccioImage: "v", PypiserverImage: "p", NPMPort: 8080, PythonPort: 8080},
		"端口越界":  {VerdaccioImage: "v", PypiserverImage: "p", NPMPort: 70000, PythonPort: 8080},
	}
	for name, opts := range cases {
		if _, err := Compose(opts); err == nil {
			t.Errorf("%s: Compose 应当报错（base=%+v）", name, base)
		}
	}
}

func TestVerdaccioConfigEssentials(t *testing.T) {
	cfg := string(VerdaccioConfigYAML())
	var doc struct {
		Storage  string         `yaml:"storage"`
		Auth     map[string]any `yaml:"auth"`
		Uplinks  map[string]any `yaml:"uplinks"`
		Packages map[string]struct {
			Access    string `yaml:"access"`
			Publish   string `yaml:"publish"`
			Unpublish string `yaml:"unpublish"`
		} `yaml:"packages"`
	}
	if err := yaml.Unmarshal([]byte(cfg), &doc); err != nil {
		t.Fatalf("verdaccio config 不是合法 YAML: %v", err)
	}
	// storage 必须是挂载点本身：bundle 的 npm/storage 已按 verdaccio 包根格式落盘。
	if doc.Storage != "/verdaccio/storage" {
		t.Errorf("storage = %q, want /verdaccio/storage", doc.Storage)
	}
	// 离线：任何 uplink 都会变成超时。
	if len(doc.Uplinks) != 0 {
		t.Errorf("uplinks 必须为空（离线），got %v", doc.Uplinks)
	}
	allow, ok := doc.Packages["**"]
	if !ok {
		t.Fatalf("缺少 packages '**' 规则:\n%s", cfg)
	}
	if allow.Access != "$all" || allow.Publish != "$all" {
		t.Errorf("packages '**' = %+v, want access/publish $all", allow)
	}
	if _, ok := doc.Auth["htpasswd"]; !ok {
		t.Errorf("auth.htpasswd 缺失:\n%s", cfg)
	}
	// htpasswd 路径必须与 compose 的 npm/conf 挂载点一致，否则 verdaccio 找不到文件。
	if !strings.Contains(cfg, "/verdaccio/conf/htpasswd") {
		t.Errorf("htpasswd 路径不在挂载点内:\n%s", cfg)
	}
}

func TestClientTemplates(t *testing.T) {
	npmrc, err := ClientNPMRC("http://10.0.0.5:4873/")
	if err != nil {
		t.Fatalf("ClientNPMRC: %v", err)
	}
	if !strings.Contains(string(npmrc), "registry=http://10.0.0.5:4873/") {
		t.Errorf("npmrc 缺 registry:\n%s", npmrc)
	}

	pip, err := ClientPipConf("http://10.0.0.5:8080/simple/", "10.0.0.5")
	if err != nil {
		t.Fatalf("ClientPipConf: %v", err)
	}
	s := string(pip)
	if !strings.Contains(s, "index-url = http://10.0.0.5:8080/simple/") {
		t.Errorf("pip.conf 缺 index-url:\n%s", s)
	}
	if !strings.Contains(s, "trusted-host = 10.0.0.5") {
		t.Errorf("pip.conf 缺 trusted-host:\n%s", s)
	}
	// 模板里不能残留占位符，否则目标机拿到的配置直接不可用。
	for _, bad := range []string{"{{", "}}"} {
		if strings.Contains(s, bad) {
			t.Errorf("pip.conf 残留模板标记 %q:\n%s", bad, s)
		}
	}
}

func TestInstallScriptContract(t *testing.T) {
	script := string(InstallSH())
	// 目标机只有 sh + docker + tar：脚本不能出现 bash 专有语法。
	if strings.HasPrefix(script, "#!") && !strings.HasPrefix(script, "#!/bin/sh") {
		t.Errorf("install.sh 必须以 /bin/sh 开始")
	}
	for _, bad := range []string{"[[ ", "function ", "set -o pipefail"} {
		if strings.Contains(script, bad) {
			t.Errorf("install.sh 含非 POSIX 语法 %q", bad)
		}
	}
	for _, want := range []string{
		"docker load -i",
		"docker compose version",
		"docker-compose",
		"apply.env",
		"set -eu",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("install.sh 缺少 %q", want)
		}
	}
}

func TestInstallScriptIsExecutableText(t *testing.T) {
	script := InstallSH()
	if len(script) == 0 {
		t.Fatal("InstallSH 为空")
	}
	if bytes.Contains(script, []byte("\r\n")) {
		t.Fatal("install.sh 含 CRLF，目标机 /bin/sh 会把 \\r 当命令内容")
	}
	// 返回的是副本：调用方改坏它不能污染后续渲染。
	script[0] = 'X'
	if InstallSH()[0] == 'X' {
		t.Fatal("InstallSH 返回了内部切片，调用方可修改共享状态")
	}
}

func containsSub(items []string, sub string) bool {
	for _, it := range items {
		if strings.Contains(it, sub) {
			return true
		}
	}
	return false
}
