package bundle

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// 构建侧要反复调用外部命令（npm / pip / docker）。统一走这里的目的有两个：
// 一是错误里必须带命令输出尾部，否则用户只看得到 "exit status 1"；
// 二是所有命令都挂 ctx，取消构建时不会留下僵尸下载进程。
func runCommand(ctx context.Context, dir, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("%s %s 失败: %w\n%s",
			name, strings.Join(args, " "), err, outputTail(stderr.String()+stdout.String(), 4000))
	}
	return stdout.String(), nil
}

func runNPM(ctx context.Context, dir string, args ...string) (string, error) {
	return runCommand(ctx, dir, "npm", args...)
}

// runPip 兼容只装了 pip3 的环境；找不到 pip 时给出可执行的提示。
func runPip(ctx context.Context, dir string, args ...string) (string, error) {
	name := "pip"
	if _, err := exec.LookPath(name); err != nil {
		name = "pip3"
		if _, err := exec.LookPath(name); err != nil {
			return "", fmt.Errorf("找不到 pip 或 pip3 命令（Python 闭包需要 pip >= 22.2）")
		}
	}
	return runCommand(ctx, dir, name, args...)
}

func runDockerCmd(ctx context.Context, args ...string) (string, error) {
	return runCommand(ctx, "", "docker", args...)
}

// outputTail 截取输出尾部。外部命令的问题几乎总出现在最后几行，
// 保留尾部比保留头部更有诊断价值，也避免把几 MB 日志塞进错误串。
func outputTail(s string, max int) string {
	s = strings.TrimRight(s, "\n")
	if len(s) <= max {
		return s
	}
	return "…" + s[len(s)-max:]
}

// forEachParallel 以固定并发跑 count 个任务并返回第一个错误。
// 手写而不是引入 errgroup，是为了让 internal/bundle 只依赖标准库 + semver。
func forEachParallel(count, workers int, fn func(i int) error) error {
	if workers < 1 {
		workers = 1
	}
	if count <= 0 {
		return nil
	}
	if workers > count {
		workers = count
	}
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
	)
	sem := make(chan struct{}, workers)
	for i := 0; i < count; i++ {
		mu.Lock()
		stop := firstErr != nil
		mu.Unlock()
		if stop {
			break
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := fn(i); err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	return firstErr
}

// fileDigest 记录一个文件的三种摘要。
//
// 同时留三种是因为三条链路各自要不同的形式：manifest.json 要 sha256 hex，
// npm 客户端要 SRI（sha512 base64），老 lockfile / 部分 registry 只给 sha1 shasum。
// 一次读取算全，避免为了补一个字段重读 100MB 的 tarball。
type fileDigest struct {
	SHA256Hex string
	SHA512B64 string
	SHA1Hex   string
	Size      int64
}

// Integrity 返回 npm Subresource Integrity 字符串。
func (d fileDigest) Integrity() string { return "sha512-" + d.SHA512B64 }

func digestOfReaders(sha256Sum, sha1Sum, sha512Sum []byte, size int64) fileDigest {
	return fileDigest{
		SHA256Hex: hex.EncodeToString(sha256Sum),
		SHA512B64: base64.StdEncoding.EncodeToString(sha512Sum),
		SHA1Hex:   hex.EncodeToString(sha1Sum),
		Size:      size,
	}
}

func hashFile(path string) (fileDigest, error) {
	f, err := os.Open(path)
	if err != nil {
		return fileDigest{}, err
	}
	defer f.Close()
	h256, h1, h512 := sha256.New(), sha1.New(), sha512.New()
	n, err := io.Copy(io.MultiWriter(h256, h1, h512), f)
	if err != nil {
		return fileDigest{}, fmt.Errorf("读取 %s: %w", path, err)
	}
	return digestOfReaders(h256.Sum(nil), h1.Sum(nil), h512.Sum(nil), n), nil
}

// fileSHA256 是给镜像归档用的轻量版本：只算 manifest 需要的 sha256 与大小。
func fileSHA256(path string) (string, int64, error) {
	d, err := hashFile(path)
	if err != nil {
		return "", 0, err
	}
	return d.SHA256Hex, d.Size, nil
}

// copyFileAndHash 复制的同时计算摘要：python/packages 与 npm tarball 都需要
// "落盘字节的 sha256"，边拷边算比写完再读一遍省一半 IO。
func copyFileAndHash(src, dst string) (fileDigest, error) {
	in, err := os.Open(src)
	if err != nil {
		return fileDigest{}, err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fileDigest{}, err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fileDigest{}, err
	}
	h256, h1, h512 := sha256.New(), sha1.New(), sha512.New()
	n, err := io.Copy(io.MultiWriter(out, h256, h1, h512), in)
	if err != nil {
		out.Close()
		return fileDigest{}, err
	}
	if err := out.Close(); err != nil {
		return fileDigest{}, err
	}
	return digestOfReaders(h256.Sum(nil), h1.Sum(nil), h512.Sum(nil), n), nil
}

func defaultString(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// humanBytes 只用于日志可读性，精确字节数在 manifest.json 里。
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
