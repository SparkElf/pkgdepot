package remote

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/SparkElf/pkgdepot/internal/spec"
)

// 这些测试覆盖不依赖网络的纯逻辑部分（引号/路径/摘要/认证装配），以及
// 一个可选的真实 sshd 集成测试（环境变量控制）。传输层的"能不能在没装 SFTP
// 子系统的主机上工作"是核心合同，但真实 sshd 不能保证在 CI 里可用，
// 所以真实连接部分用 PKGDEPOT_TEST_SSH 环境变量显式开启。

func TestShellQuote(t *testing.T) {
	// 这份用例的价值在于：目标机执行的脚本里包含路径、单引号、$ 与换行，
	// 任何一处转义错误都会让经典命令注入或让 install.sh 静默变成别的命令。
	cases := map[string]string{
		"":               "''",
		"simple":         "'simple'",
		"a b":            "'a b'",
		"it's":           `'it'\''s'`,
		"a'b'c":          `'a'\''b'\''c'`,
		"$HOME":          "'$HOME'",
		"`whoami`":       "'`whoami`'",
		"line1\nline2":   "'line1\nline2'",
		"a; rm -rf /tmp": "'a; rm -rf /tmp'",
		"路径/带中文":         "'路径/带中文'",
	}
	for in, want := range cases {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %q, want %q", in, got, want)
		}
	}
	// 每条结果都必须以单个成对的单引号开头结尾（除了内部转义块）。
	for in := range cases {
		got := shellQuote(in)
		if !strings.HasPrefix(got, "'") || !strings.HasSuffix(got, "'") {
			t.Errorf("shellQuote(%q) = %q 不是单引号包起来的形式", in, got)
		}
	}
}

func TestExpandHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("没有 home 目录")
	}
	cases := map[string]string{
		"~/key":    filepath.Join(home, "key"),
		"~":        home,
		"/abs/key": "/abs/key",
		"rel/key":  "rel/key",
	}
	for in, want := range cases {
		got, err := expandHome(in)
		if err != nil {
			t.Errorf("expandHome(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("expandHome(%q) = %q, want %q", in, got, want)
		}
	}
	if _, err := expandHome(""); err == nil {
		t.Error("空路径应当报错")
	}
	// 只展开开头的 ~，中间的不能动。
	if got, _ := expandHome("/a/~/b"); got != "/a/~/b" {
		t.Errorf("expandHome 误改了中间的 ~: %q", got)
	}
}

func TestAuthMethodsRequiresCredentials(t *testing.T) {
	// 没有 key、没有 agent、没有默认私钥时必须给出可操作的错误，而不是空 Auth。
	h := &spec.Host{Address: "10.0.0.1", User: "ops"}
	t.Setenv("SSH_AUTH_SOCK", "")
	t.Setenv("HOME", t.TempDir())
	_, err := authMethods(h)
	if err == nil {
		t.Fatal("没有任何凭据时应当报错")
	}
	if !strings.Contains(err.Error(), "没有可用凭据") {
		t.Errorf("错误信息应提示如何提供凭据: %v", err)
	}
}

func TestAuthMethodsReadsExplicitKey(t *testing.T) {
	// 生成一个临时私钥，确认显式 --key 被真正读进 AuthMethod。
	keyPath := writeTestKey(t)
	h := &spec.Host{Address: "10.0.0.1", User: "ops", IdentityFile: keyPath}
	methods, err := authMethods(h)
	if err != nil {
		t.Fatalf("authMethods: %v", err)
	}
	if len(methods) == 0 {
		t.Fatal("没有返回任何 AuthMethod")
	}
}

func TestAuthMethodsMissingKeyFile(t *testing.T) {
	// 指定了不存在的 key 必须直接报错，而不是悄悄回退到别的凭据：
	// 静默回退会让"我明明配了 key"变成极难排查的问题。
	h := &spec.Host{Address: "10.0.0.1", User: "ops", IdentityFile: "/nonexistent/key"}
	_, err := authMethods(h)
	if err == nil {
		t.Fatal("指定不存在的私钥时应当报错")
	}
	if !strings.Contains(err.Error(), "/nonexistent/key") {
		t.Errorf("错误信息应包含 key 路径: %v", err)
	}
}

func TestDialRejectsEmptyHost(t *testing.T) {
	if _, err := Dial(nil); err == nil {
		t.Error("nil host 应当报错")
	}
	if _, err := Dial(&spec.Host{User: "u"}); err == nil {
		t.Error("空地址应当报错")
	}
}

func TestDialErrorMentionsTarget(t *testing.T) {
	// 连不上时必须带上目标地址与端口，内网排障第一眼看着就知道打的是哪台。
	// 用一个必定解析失败的地址：不能依赖"某个端口恰好关着"，否则在防火墙
	// 丢弃 SYN 的环境里这条测试会卡满 30s 拨号超时。
	h := &spec.Host{Address: "no-such-host.invalid", Port: 22, User: "nobody"}
	_, err := Dial(h)
	if err == nil {
		t.Fatal("不可解析的主机名应当报错")
	}
	if !strings.Contains(err.Error(), "no-such-host.invalid:22") {
		t.Errorf("连接错误没有带上目标地址: %v", err)
	}
	if !strings.Contains(err.Error(), "nobody@") {
		t.Errorf("连接错误没有带上用户名: %v", err)
	}
}

func TestClosedClientErrors(t *testing.T) {
	// 关闭后的客户端必须给出"连接已关闭"，而不是 panic 或空错误。
	c := &Client{host: "test"}
	if err := c.Close(); err != nil {
		t.Errorf("Close on zero client: %v", err)
	}
	if _, err := c.Run(context.Background(), "echo hi"); err == nil {
		t.Error("关闭后 Run 应当报错")
	}
	if err := c.WriteFile(context.Background(), "/x", nil, 0o644); err == nil {
		t.Error("关闭后 WriteFile 应当报错")
	}
	if _, err := c.ReadFile(context.Background(), "/x"); err == nil {
		t.Error("关闭后 ReadFile 应当报错")
	}
	if _, err := c.Stat(context.Background(), "/x"); err == nil {
		t.Error("关闭后 Stat 应当报错")
	}
	if err := c.DockerAvailable(context.Background()); err == nil {
		t.Error("关闭后 DockerAvailable 应当报错")
	}
}

func TestRemoteFileInfo(t *testing.T) {
	fi := &remoteFileInfo{name: "a.txt", size: 12, mode: 0o644}
	if fi.Name() != "a.txt" || fi.Size() != 12 || fi.IsDir() || fi.Mode().Perm() != 0o644 {
		t.Errorf("remoteFileInfo = %+v", fi)
	}
	d := &remoteFileInfo{name: "d", dir: true, mode: 0o755}
	if !d.IsDir() {
		t.Error("目录标记丢失")
	}
}

func TestIndentOutput(t *testing.T) {
	if got := indentOutput(""); got != "  (无输出)" {
		t.Errorf("indentOutput(\"\") = %q", got)
	}
	if got := indentOutput("a\nb\n"); got != "  a\n  b" {
		t.Errorf("indentOutput = %q", got)
	}
}

func TestCtxReaderCancels(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	r := &ctxReader{ctx: ctx, r: strings.NewReader("data")}
	cancel()
	if _, err := r.Read(make([]byte, 4)); err == nil {
		t.Fatal("已取消的 ctx 应当让 Read 失败")
	}
}

// --- 可选的真实 sshd 集成测试 --------------------------------------------
//
// 设置 PKGDEPOT_TEST_SSH="addr:port:user:keypath" 后运行：
//
//	PKGDEPOT_TEST_SSH=127.0.0.1:2222:root:/tmp/e2e-ssh/user_ed25519 \
//	  go test ./internal/remote/ -run TestLiveSSH -v
//
// 这一段刻意不检查 SFTP：整个包的合同就是"没有 SFTP 子系统也能用"。

func liveClient(t *testing.T) *Client {
	t.Helper()
	raw := os.Getenv("PKGDEPOT_TEST_SSH")
	if raw == "" {
		t.Skip("PKGDEPOT_TEST_SSH 未设置，跳过真实 sshd 测试")
	}
	parts := strings.Split(raw, ":")
	if len(parts) != 4 {
		t.Fatalf("PKGDEPOT_TEST_SSH 格式应为 addr:port:user:keypath，得到 %q", raw)
	}
	port, err := strconv.Atoi(parts[1])
	if err != nil {
		t.Fatalf("解析端口 %q: %v", parts[1], err)
	}
	h := &spec.Host{Address: parts[0], Port: port, User: parts[2], IdentityFile: parts[3]}
	c, err := Dial(h)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestLiveSSHTransport(t *testing.T) {
	c := liveClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	out, err := c.Run(ctx, "echo live-ok")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.TrimSpace(out) != "live-ok" {
		t.Errorf("Run out = %q", out)
	}

	dir, err := os.MkdirTemp("", "pkgdepot-remote-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	remoteBase := "/tmp/pkgdepot-remote-test"

	if err := c.MkdirAll(ctx, remoteBase); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	// 二进制往返：包含全部 256 种字节值，证明 cat 通道是二进制安全的。
	blob := make([]byte, 64<<10)
	for i := range blob {
		blob[i] = byte(i)
	}
	local := filepath.Join(dir, "blob.bin")
	if err := os.WriteFile(local, blob, 0o644); err != nil {
		t.Fatal(err)
	}
	remoteFile := remoteBase + "/blob.bin"
	if err := c.UploadFile(ctx, local, remoteFile); err != nil {
		t.Fatalf("UploadFile: %v", err)
	}
	fi, err := c.Stat(ctx, remoteFile)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if fi.Size() != int64(len(blob)) {
		t.Errorf("远端尺寸 = %d, want %d", fi.Size(), len(blob))
	}
	back, err := c.ReadFile(ctx, remoteFile)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	// ReadFile 走 cat，必须逐字节一致（不能在末尾被裁剪或改写）。
	if len(back) != len(blob) {
		t.Fatalf("ReadFile 长度 = %d, want %d", len(back), len(blob))
	}
	for i := range blob {
		if back[i] != blob[i] {
			t.Fatalf("第 %d 字节不一致: got %02x want %02x", i, back[i], blob[i])
		}
	}

	// 反复覆盖写（旧的 SFTP Rename 实现会在第二次失败）。
	for i := 0; i < 3; i++ {
		if err := c.WriteFile(ctx, remoteBase+"/conf.txt", []byte("v"+string(rune('0'+i))), 0o644); err != nil {
			t.Fatalf("第 %d 次 WriteFile: %v", i, err)
		}
	}

	if _, err := c.Stat(ctx, remoteBase+"/does-not-exist"); !os.IsNotExist(err) {
		t.Errorf("Stat 缺失文件应满足 os.IsNotExist, got %v", err)
	}

	// 非零退出必须带回退出码。
	if _, err := c.Run(ctx, "exit 42"); err == nil {
		t.Error("非零退出应当报错")
	} else if !strings.Contains(err.Error(), "42") {
		t.Errorf("错误信息应包含退出码: %v", err)
	}
}

// writeTestKey 在临时目录生成一把真实的 ed25519 私钥（PEM），仅用于驱动
// authMethods 的读文件与解析路径。用固定字符串当私钥是不行的：x/crypto 会严格
// 校验结构，"ssh: no key found" 正是这么被发现的。
func writeTestKey(t *testing.T) string {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("生成测试私钥: %v", err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatalf("序列化测试私钥: %v", err)
	}
	p := filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(p, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}
