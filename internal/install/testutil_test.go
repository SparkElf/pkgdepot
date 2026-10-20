package install

import (
	"encoding/hex"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"

	"github.com/SparkElf/pkgdepot/internal/spec"
)

// hostForServer 把 spec.Host 指向 httptest 服务，让 Verify/Apply 的 URL 拼装
// 逻辑与真实部署走完全相同的代码路径。
func hostForServer(t *testing.T, srv *httptest.Server) *spec.Host {
	t.Helper()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse httptest URL %q: %v", srv.URL, err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("httptest URL 没有端口: %q", srv.URL)
	}
	return &spec.Host{Address: u.Hostname(), Port: 22, User: "test", NPMPort: port, PythonPort: port}
}

// portOf 取 httptest 服务的端口。
func portOf(t *testing.T, srv *httptest.Server) int {
	t.Helper()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse httptest URL: %v", err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("httptest URL 没有端口: %q", srv.URL)
	}
	return port
}

func hashHex(data []byte) string {
	h := sha256New()
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}
