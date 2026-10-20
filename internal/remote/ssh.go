package remote

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/SparkElf/pkgdepot/internal/spec"
)

// dialTimeout 是 TCP + SSH 握手的上限。内网目标机常处于"能连但很慢"的状态，
// 30s 足够区分"慢"与"不通"，也不会让 CLI 挂死。
const dialTimeout = 30 * time.Second

// Client 是到一个目标主机的长连接。
//
// 只使用 SSH 的 exec 通道（session.Run / session.Stdin / session.Output），不依赖
// SFTP 子系统：硬化过的内网 sshd 经常显式关闭 `Subsystem sftp`，而 exec 通道是
// sshd 的核心能力（关掉它等于不能用 ssh 跑命令）。文件传输因此统一走
// `cat > path` 与 `cat path`，对二进制是安全的（用 1MB 随机二进制实测过 sha256 一致）。
type Client struct {
	host string // "user@addr:port"，只用于错误信息
	ssh  *ssh.Client
}

// Dial 建立到 h 的 SSH 连接。
//
// 认证优先用 IdentityFile（密码/shadow 在内网多机场景不可运维）；文件不存在
// 或未配置时依次尝试 ssh-agent 与 ~/.ssh 下的默认私钥，这是操作员机器上最常见的
// 两种可用凭据。
//
// 主机密钥校验使用 ssh.InsecureIgnoreHostKey：内网首次连接没有可信 known_hosts
// 来源，v1 不引入 TOFU 存盘（见 docs/architecture.md ADR-7，v1 不做鉴权）。
func Dial(h *spec.Host) (*Client, error) {
	if h == nil {
		return nil, fmt.Errorf("ssh: 主机信息为空")
	}
	if strings.TrimSpace(h.Address) == "" {
		return nil, fmt.Errorf("ssh: 主机地址为空")
	}
	stored := *h
	stored.ApplyDefaults()

	auth, err := authMethods(&stored)
	if err != nil {
		return nil, err
	}

	addr := net.JoinHostPort(stored.Address, fmt.Sprint(stored.Port))
	cfg := &ssh.ClientConfig{
		User:            stored.User,
		Auth:            auth,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), //nolint:gosec // 内网首连，ADR-7
		Timeout:         dialTimeout,
	}
	conn, err := ssh.Dial("tcp", addr, cfg)
	if err != nil {
		return nil, fmt.Errorf("ssh 连接 %s@%s 失败: %w", stored.User, addr, err)
	}
	return &Client{host: fmt.Sprintf("%s@%s", stored.User, addr), ssh: conn}, nil
}

// Close 关闭 SSH 连接。
func (c *Client) Close() error {
	if c == nil || c.ssh == nil {
		return nil
	}
	err := c.ssh.Close()
	c.ssh = nil
	return err
}

// newSession 统一新建会话并检查连接状态，让各方法的错误都能归因到"连接已失效"。
func (c *Client) newSession() (*ssh.Session, error) {
	if c == nil || c.ssh == nil {
		return nil, fmt.Errorf("ssh 连接已关闭")
	}
	sess, err := c.ssh.NewSession()
	if err != nil {
		return nil, fmt.Errorf("ssh 新建会话失败 (%s): %w", c.host, err)
	}
	return sess, nil
}

// Run 在目标机执行一段 shell 脚本，返回合并后的 stdout+stderr。
//
// 脚本以 `sh -c <quoted>` 的形式交给远端：调用方写的是普通多行脚本，不必关心
// 远端登录 shell 是什么（可能是 bash/csh/dash）。非零退出码视为失败，错误里
// 带上退出码和输出——apply 失败时操作员只有这一处线索。
func (c *Client) Run(ctx context.Context, script string) (string, error) {
	return c.run(ctx, script)
}

// RunScript 与 Run 行为一致。单独留一个名字是接口合同的要求（B1），便于调用方
// 表达"这是脚本而非单条命令"的意图。
func (c *Client) RunScript(ctx context.Context, script string) (string, error) {
	return c.run(ctx, script)
}

func (c *Client) run(ctx context.Context, script string) (string, error) {
	sess, err := c.newSession()
	if err != nil {
		return "", err
	}
	defer sess.Close()

	// stdout/stderr 的 copy 是两条独立 goroutine（x/crypto 的实现），共用同一个
	// bytes.Buffer 会数据竞争：实测表现为输出被截断（"hello" 变成 "hell"）甚至
	// 整个丢失。必须用加锁的 writer，这正是库自己的 CombinedOutput 内部做的事。
	var out syncWriter
	sess.Stdout = &out
	sess.Stderr = &out

	done := make(chan error, 1)
	go func() { done <- sess.Run("sh -c " + shellQuote(script)) }()

	select {
	case <-ctx.Done():
		_ = sess.Signal(ssh.SIGKILL)
		_ = sess.Close()
		<-done
		return out.String(), fmt.Errorf("ssh 执行被取消 (%s): %w", c.host, ctx.Err())
	case err := <-done:
		text := strings.TrimRight(out.String(), "\n")
		if err != nil {
			var exitErr *ssh.ExitError
			if errors.As(err, &exitErr) {
				return text, fmt.Errorf("远端命令以退出码 %d 失败 (%s)\n%s",
					exitErr.ExitStatus(), c.host, indentOutput(text))
			}
			return text, fmt.Errorf("远端命令执行失败 (%s): %w\n%s", c.host, err, indentOutput(text))
		}
		return text, nil
	}
}

// output 执行脚本并返回**未经裁剪**的 stdout 字节。
// ReadFile 必须拿到原始字节，而 run 会去掉末尾换行以便日志显示。
func (c *Client) output(ctx context.Context, script string) ([]byte, error) {
	sess, err := c.newSession()
	if err != nil {
		return nil, err
	}
	defer sess.Close()

	var stdout, stderr syncWriter
	sess.Stdout = &stdout
	sess.Stderr = &stderr

	done := make(chan error, 1)
	go func() { done <- sess.Run("sh -c " + shellQuote(script)) }()

	select {
	case <-ctx.Done():
		_ = sess.Signal(ssh.SIGKILL)
		_ = sess.Close()
		<-done
		return nil, ctx.Err()
	case err := <-done:
		if err != nil {
			msg := strings.TrimSpace(stderr.String())
			if msg == "" {
				msg = strings.TrimSpace(stdout.String())
			}
			return nil, fmt.Errorf("%w\n%s", err, indentOutput(msg))
		}
		return []byte(stdout.String()), nil
	}
}

// UploadFile 把本地文件流式传到远端。
//
// 走 `cat > <remote>`：把本地内容写进 session.Stdin，远端 cat 落盘。
// bundles 动辄数百 MB，必须流式（io.Copy 由 ssh 包内部完成），不能整读进内存。
// 写完显式 chmod 0644：远端默认 umask 可能给出 0600，导致同机其他运维账号读不到。
func (c *Client) UploadFile(ctx context.Context, localPath, remotePath string) error {
	src, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("打开本地文件 %s: %w", localPath, err)
	}
	defer src.Close()

	info, err := src.Stat()
	if err != nil {
		return fmt.Errorf("读取本地文件信息 %s: %w", localPath, err)
	}

	// 先建父目录：远端只有 cat，不会自动建目录。
	if err := c.MkdirAll(ctx, path.Dir(remotePath)); err != nil {
		return fmt.Errorf("上传 %s 前准备远端目录: %w", remotePath, err)
	}

	// 先写临时文件再改名：上传中断不会留下"看起来完整"的半截 bundle。
	tmp := remotePath + ".part"

	if err := c.streamTo(ctx, src, tmp, info.Size(), 0o644); err != nil {
		// 失败时尽量清掉半截文件，避免下次误判"已存在"。
		_, _ = c.Run(ctx, "rm -f "+shellQuote(tmp))
		return fmt.Errorf("上传 %s -> %s: %w", localPath, remotePath, err)
	}

	if _, err := c.Run(ctx, "mv -f "+shellQuote(tmp)+" "+shellQuote(remotePath)); err != nil {
		_, _ = c.Run(ctx, "rm -f "+shellQuote(tmp))
		return fmt.Errorf("上传 %s 后重命名 %s -> %s: %w", localPath, tmp, remotePath, err)
	}
	return nil
}

// WriteFile 写一小段内容（配置、脚本、apply.env）并设置权限。
//
// 与 UploadFile 共用同一条 `cat >` 通道，区别是数据整块在内存里、并且可以
// 指定权限（install.sh 需要 0755）。同样先写临时文件再 mv，避免"要么完整、
// 要么不存在"的配置文件被读到半截。
func (c *Client) WriteFile(ctx context.Context, remotePath string, data []byte, mode fs.FileMode) error {
	if err := c.MkdirAll(ctx, path.Dir(remotePath)); err != nil {
		return fmt.Errorf("写入 %s 前准备远端目录: %w", remotePath, err)
	}
	tmp := remotePath + ".part"
	if err := c.streamTo(ctx, bytes.NewReader(data), tmp, int64(len(data)), mode); err != nil {
		_, _ = c.Run(ctx, "rm -f "+shellQuote(tmp))
		return fmt.Errorf("写入远端文件 %s: %w", remotePath, err)
	}
	if _, err := c.Run(ctx, "mv -f "+shellQuote(tmp)+" "+shellQuote(remotePath)); err != nil {
		_, _ = c.Run(ctx, "rm -f "+shellQuote(tmp))
		return fmt.Errorf("重命名远端文件 %s -> %s: %w", tmp, remotePath, err)
	}
	return nil
}

// streamTo 把 r 的内容通过 `cat > remotePath` 写到远端，并在成功后 chmod。
//
// wantSize 用来做一次上传后的尺寸校验：SSH 通道最隐蔽的失败模式是
// "stdin 提前 EOF 但 cat 仍以 0 退出"，尺寸对不上能立刻把截断暴露出来。
func (c *Client) streamTo(ctx context.Context, r io.Reader, remotePath string, wantSize int64, mode fs.FileMode) error {
	sess, err := c.newSession()
	if err != nil {
		return err
	}
	defer sess.Close()

	// 权限在同一个脚本里设：这样上传的失败点只有一处，且不需要第二次往返。
	script := "set -eu; umask 022; cat > " + shellQuote(remotePath) +
		"; chmod " + strconv.FormatUint(uint64(mode.Perm()), 8) + " " + shellQuote(remotePath)
	sess.Stdin = &ctxReader{ctx: ctx, r: r}
	var stderr syncWriter
	sess.Stderr = &stderr

	done := make(chan error, 1)
	go func() { done <- sess.Run(script) }()

	select {
	case <-ctx.Done():
		_ = sess.Close()
		<-done
		return fmt.Errorf("上传被取消: %w", ctx.Err())
	case err := <-done:
		if err != nil {
			msg := strings.TrimSpace(stderr.String())
			if msg == "" {
				msg = "远端没有输出"
			}
			return fmt.Errorf("写入 %s 失败 (%s): %w\n  %s", remotePath, c.host, err, msg)
		}
	}

	// 尺寸校验：cat 写出的字节数必须与本地一致。
	if wantSize >= 0 {
		gotSize, err := c.fileSize(ctx, remotePath)
		if err != nil {
			return fmt.Errorf("上传后校验 %s 尺寸失败: %w", remotePath, err)
		}
		if gotSize != wantSize {
			return fmt.Errorf("上传后尺寸不一致 %s: 本地 %d 字节，远端 %d 字节（传输被截断）",
				remotePath, wantSize, gotSize)
		}
	}
	return nil
}

// ReadFile 读回远端文件（inventory.json、状态文件等）。
func (c *Client) ReadFile(ctx context.Context, remotePath string) ([]byte, error) {
	raw, err := c.output(ctx, "cat "+shellQuote(remotePath))
	if err != nil {
		return nil, fmt.Errorf("读取远端文件 %s 失败: %w", remotePath, err)
	}
	return raw, nil
}

// MkdirAll 递归建目录。
func (c *Client) MkdirAll(ctx context.Context, remotePath string) error {
	if strings.TrimSpace(remotePath) == "" || remotePath == "." || remotePath == "/" {
		return nil
	}
	if _, err := c.run(ctx, "mkdir -p "+shellQuote(remotePath)); err != nil {
		return fmt.Errorf("创建远端目录 %s 失败: %w", remotePath, err)
	}
	return nil
}

// Stat 返回远端文件信息；文件不存在时返回的 error 满足 os.IsNotExist。
//
// 一条命令同时拿到类型、尺寸、权限与 mtime，既省往返也避免多次 stat 之间的竞态。
// 用 `stat -c` 而不是 `ls -l`：前者输出结构化字段，不会被文件名里的空格破坏。
func (c *Client) Stat(ctx context.Context, remotePath string) (os.FileInfo, error) {
	q := shellQuote(remotePath)
	script := "if [ -d " + q + " ]; then echo DIR; " +
		"elif [ -f " + q + " ]; then echo FILE; else echo MISSING; fi; " +
		"stat -c '%s %a %Y' " + q + " 2>/dev/null || true"
	out, err := c.run(ctx, script)
	if err != nil {
		return nil, fmt.Errorf("stat 远端路径 %s 失败: %w", remotePath, err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) == 0 {
		return nil, &os.PathError{Op: "stat", Path: remotePath, Err: os.ErrNotExist}
	}
	switch strings.TrimSpace(lines[0]) {
	case "MISSING":
		return nil, &os.PathError{Op: "stat", Path: remotePath, Err: os.ErrNotExist}
	case "DIR":
		return &remoteFileInfo{name: path.Base(remotePath), mode: fs.ModeDir | 0o755, dir: true}, nil
	case "FILE":
		if len(lines) < 2 {
			return nil, &os.PathError{Op: "stat", Path: remotePath, Err: os.ErrNotExist}
		}
		fields := strings.Fields(lines[1])
		if len(fields) != 3 {
			return nil, fmt.Errorf("stat 远端路径 %s: 无法解析输出 %q", remotePath, lines[1])
		}
		size, perr := strconv.ParseInt(fields[0], 10, 64)
		if perr != nil {
			return nil, fmt.Errorf("stat 远端路径 %s: 尺寸字段 %q 非法", remotePath, fields[0])
		}
		perm, perr := strconv.ParseUint(fields[1], 8, 32)
		if perr != nil {
			return nil, fmt.Errorf("stat 远端路径 %s: mode 字段 %q 非法", remotePath, fields[1])
		}
		mt, _ := strconv.ParseInt(fields[2], 10, 64)
		return &remoteFileInfo{
			name:    path.Base(remotePath),
			size:    size,
			mode:    fs.FileMode(perm),
			modTime: time.Unix(mt, 0),
		}, nil
	default:
		return nil, fmt.Errorf("stat 远端路径 %s: 无法识别输出 %q", remotePath, lines[0])
	}
}

// DockerAvailable 确认目标机的 docker daemon 真的可用（而不只是装了 CLI）。
//
// 单独查 `docker version` 的 Server 段：离线主机上 docker CLI 常用但 socket 未启动，
// 这种失败必须在 apply 一开头报清楚，而不是等到 compose up 才暴露。
func (c *Client) DockerAvailable(ctx context.Context) error {
	out, err := c.Run(ctx, "docker version --format '{{.Server.Version}}'")
	if err != nil {
		return fmt.Errorf("目标主机 docker 不可用（需要 docker daemon 在运行，且当前用户有权限访问 docker socket）: %w", err)
	}
	if strings.TrimSpace(out) == "" {
		return fmt.Errorf("目标主机 docker 不可用: docker version 没有返回服务端版本")
	}
	return nil
}

// fileSize 返回远端文件字节数（不存在/不是普通文件时报错）。
func (c *Client) fileSize(ctx context.Context, remotePath string) (int64, error) {
	out, err := c.run(ctx, "stat -c %s "+shellQuote(remotePath))
	if err != nil {
		return 0, err
	}
	n, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("解析 %s 的尺寸 %q: %w", remotePath, strings.TrimSpace(out), err)
	}
	return n, nil
}

// remoteFileInfo 是 os.FileInfo 的最小实现，只填调用方真正会读的字段。
type remoteFileInfo struct {
	name    string
	size    int64
	mode    fs.FileMode
	modTime time.Time
	dir     bool
}

func (f *remoteFileInfo) Name() string       { return f.name }
func (f *remoteFileInfo) Size() int64        { return f.size }
func (f *remoteFileInfo) Mode() fs.FileMode  { return f.mode }
func (f *remoteFileInfo) ModTime() time.Time { return f.modTime }
func (f *remoteFileInfo) IsDir() bool        { return f.dir || f.mode.IsDir() }
func (f *remoteFileInfo) Sys() any           { return nil }

// syncWriter 是并发安全的输出缓冲，供 stdout/stderr 两条 copy goroutine 共用。
type syncWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *syncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *syncWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// authMethods 按"显式私钥 → ssh-agent → 默认私钥"的顺序组装认证方式。
// 顺序有讲究：显式配置必须优先，否则操作员在配置里指定的 key 会被 agent 里
// 的错误身份掩盖，报错信息会指向错误的方向。
func authMethods(h *spec.Host) ([]ssh.AuthMethod, error) {
	var methods []ssh.AuthMethod

	if h.IdentityFile != "" {
		keyPath, err := expandHome(h.IdentityFile)
		if err != nil {
			return nil, err
		}
		signer, err := readPrivateKey(keyPath)
		if err != nil {
			return nil, fmt.Errorf("读取 SSH 私钥 %s: %w", keyPath, err)
		}
		methods = append(methods, ssh.PublicKeys(signer))
	}

	if signers := agentSigners(); len(signers) > 0 {
		methods = append(methods, ssh.PublicKeys(signers...))
	}

	if h.IdentityFile == "" {
		// 未显式配置时才回退默认私钥，避免"配置了却没生效"的歧义。
		for _, name := range []string{"id_ed25519", "id_ecdsa", "id_rsa"} {
			p, err := expandHome("~/.ssh/" + name)
			if err != nil {
				continue
			}
			if _, statErr := os.Stat(p); statErr != nil {
				continue
			}
			signer, err := readPrivateKey(p)
			if err != nil {
				continue
			}
			methods = append(methods, ssh.PublicKeys(signer))
		}
	}

	if len(methods) == 0 {
		return nil, fmt.Errorf("ssh: 没有可用凭据（用 --key 指定私钥，或启动 ssh-agent）")
	}
	return methods, nil
}

func readPrivateKey(path string) (ssh.Signer, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ssh.ParsePrivateKey(raw)
}

// agentSigners 从 SSH_AUTH_SOCK 取 agent 中的身份。agent 不可用时静默跳过：
// 它是可选的便利路径，不是错误。
func agentSigners() []ssh.Signer {
	sock := os.Getenv("SSH_AUTH_SOCK")
	if sock == "" {
		return nil
	}
	conn, err := net.DialTimeout("unix", sock, 2*time.Second)
	if err != nil {
		return nil
	}
	ag := agentClient(conn)
	if ag == nil {
		_ = conn.Close()
		return nil
	}
	signers, err := ag.Signers()
	if err != nil {
		return nil
	}
	return signers
}

// shellQuote 把任意脚本包成单引号字符串，供 `sh -c` 使用。
// 只转义单引号：POSIX shell 里单引号内没有任何展开，配合 '\” 拼接即可安全传递
// 多行脚本、$ 变量和反引号。
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// expandHome 展开开头的 ~。SSH 私钥路径几乎总是写在 --key 里，不接受 ~ 会让
// 操作员反复踩坑。
func expandHome(p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("路径为空")
	}
	if p == "~" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("展开 ~: %w", err)
		}
		return home, nil
	}
	if strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("展开 ~: %w", err)
		}
		return filepath.Join(home, p[2:]), nil
	}
	return p, nil
}

func indentOutput(out string) string {
	out = strings.TrimRight(out, "\n")
	if out == "" {
		return "  (无输出)"
	}
	lines := strings.Split(out, "\n")
	for i, l := range lines {
		lines[i] = "  " + l
	}
	return strings.Join(lines, "\n")
}

// ctxReader 让长文件传输能被 ctx 取消：io.Copy 本身不感知 context。
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (r *ctxReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}
