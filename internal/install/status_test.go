package install

import (
	"strings"
	"testing"
)

// 这些测试只覆盖 status 里可离线验证的解析逻辑。真正的连接行为由真实 sshd
// 的手工验证覆盖（没有 SFTP 子系统的主机上跑通 apply/status/uninstall）。

func TestParseComposePSJSONLines(t *testing.T) {
	// compose v2 的 `ps --format json` 是每行一个 JSON 对象（已实测）。
	out := `{"Name":"p-verdaccio-1","Service":"verdaccio","State":"running","Health":"healthy","Image":"verdaccio/verdaccio:6"}
{"Name":"p-pypiserver-1","Service":"pypiserver","State":"running","Health":"starting","Image":"pypiserver/pypiserver:latest"}
not json at all
`
	list := parseComposePS(out)
	if len(list) != 2 {
		t.Fatalf("解析到 %d 个容器，want 2: %+v", len(list), list)
	}
	if list[0].Service != "verdaccio" || list[0].Health != "healthy" {
		t.Errorf("第一个容器 = %+v", list[0])
	}
	if list[1].Health != "starting" {
		t.Errorf("第二个容器 = %+v", list[1])
	}
}

func TestParseComposePSEmpty(t *testing.T) {
	if got := parseComposePS(""); len(got) != 0 {
		t.Errorf("空输入应当返回空列表, got %+v", got)
	}
	// 未部署时 compose 可能输出空行或提示文本，不能因此 panic 或报错。
	if got := parseComposePS("\n\nno such project\n"); len(got) != 0 {
		t.Errorf("非 JSON 输出应当被忽略, got %+v", got)
	}
}

func TestFormatDu(t *testing.T) {
	// install/apply 落下的 du 输出格式（KB）。
	out := formatDu("npm/storage 120\npython/packages 8\nimages 320000\ntotal 320200\n")
	for _, want := range []string{"npm/storage", "python/packages", "images", "total"} {
		if !strings.Contains(out, want) {
			t.Errorf("formatDu 输出缺少 %q:\n%s", want, out)
		}
	}
	// 顺序必须与脚本输出一致（不是字典序）。
	idxNPM := strings.Index(out, "npm/storage")
	idxPy := strings.Index(out, "python/packages")
	if idxNPM > idxPy {
		t.Errorf("formatDu 打乱了目录顺序:\n%s", out)
	}
	if got := formatDu(""); got != "" {
		t.Errorf("空输入应返回空串, got %q", got)
	}
	// 单行格式不对时跳过而不是崩。
	if got := formatDu("garbage line\n"); got != "" {
		t.Errorf("无法解析的行应被跳过, got %q", got)
	}
}
