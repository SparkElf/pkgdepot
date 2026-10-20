package install

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/url"
	"path"
	"strings"

	"github.com/SparkElf/pkgdepot/internal/install/assets"
	"github.com/SparkElf/pkgdepot/internal/spec"
)

// 目标机上的相对布局。远端路径一律用 path.Join（远端是 Linux，不能用
// filepath，否则在 Windows 上开发时会拼出反斜杠）。
const (
	remoteBundleFile    = "bundle.tar.gz"
	remoteApplyEnv      = "apply.env"
	remoteComposeFile   = spec.RemoteComposeFile
	remoteHTTPClientDNC = "client"
)

// deployFiles 描述 apply 要写到目标机上的全部文件。
//
// 单独抽出来是为了让"生成什么字节"与"怎么通过 SFTP 送过去"可以分开测试：
// 目标机的 compose/config/客户端配置内容错误是最难在 e2e 里定位的问题，
// 这里不依赖任何网络就能断言。键都是相对 baseDir 的路径。
type deployFiles struct {
	Compose   []byte
	InstallSH []byte
	// VerdaccioConfig 是 npm/conf/config.yaml。
	VerdaccioConfig []byte
	// BundleManifest 是 bundle 内 manifest.json 的原始字节。
	BundleManifest []byte
	// Htpasswd 是空的认证文件；内容留空（内网不做密码登录）。
	Htpasswd []byte
	// ClientNPMRC / ClientPipConf 是给目标机及同网段开发机用的客户端配置。
	ClientNPMRC   []byte
	ClientPipConf []byte
	// ApplyEnv 是 install.sh 读取的参数文件。
	ApplyEnv []byte
}

// unixModes 是各文件落到目标机后的权限。
const (
	modeExecutable = 0o755
	modeReadable   = 0o644
)

// buildDeployFiles 渲染 apply 需要的全部文件。
//
// manifest 必须是 bundle 里的原始字节：重新序列化会改变字节顺序，让目标机上的
// manifest.json 与 bundle 内的权威副本不一致（verify 与排障都依赖它是同一份）。
func buildDeployFiles(h *spec.Host, bm *spec.BundleManifest, rawManifest []byte) (*deployFiles, error) {
	if h == nil {
		return nil, fmt.Errorf("生成部署文件: 主机信息为空")
	}
	if bm == nil {
		return nil, fmt.Errorf("生成部署文件: bundle manifest 为空")
	}
	if len(rawManifest) == 0 {
		return nil, fmt.Errorf("生成部署文件: bundle manifest 内容为空")
	}
	stored := *h
	stored.ApplyDefaults()

	compose, err := assets.Compose(assets.ComposeOptions{
		VerdaccioImage:  bmImageRef(bm, spec.DefaultVerdaccioImage),
		PypiserverImage: bmImageRef(bm, spec.DefaultPypiserverImage),
		NPMPort:         stored.NPMPort,
		PythonPort:      stored.PythonPort,
	})
	if err != nil {
		return nil, err
	}

	npmrc, err := assets.ClientNPMRC(stored.NPMURL())
	if err != nil {
		return nil, err
	}
	pipConf, err := assets.ClientPipConf(stored.PythonURL(), hostOnly(stored.Address))
	if err != nil {
		return nil, err
	}

	env, err := renderApplyEnv(&stored)
	if err != nil {
		return nil, err
	}

	return &deployFiles{
		Compose:         compose,
		InstallSH:       assets.InstallSH(),
		VerdaccioConfig: assets.VerdaccioConfigYAML(),
		BundleManifest:  rawManifest,
		Htpasswd:        []byte{},
		ClientNPMRC:     npmrc,
		ClientPipConf:   pipConf,
		ApplyEnv:        env,
	}, nil
}

// bmImageRef 取 bundle manifest 里该默认镜像对应的实际引用。
//
// bundle 可能携带被 pkgdepot.yaml 覆盖过的镜像（例如内网 mirror 路径），
// docker load 之后镜像在本地的 tag 就是这个引用，compose 必须用同一个，
// 否则会按默认 tag 找不到镜像而尝试联网拉取（离线机直接失败）。
//
// 匹配只能靠启发式：ImageEntry 里没有 role 字段（spec 是冻结的），所以按
// "引用完全相同 → 文件名（images/verdaccio.tar 这类固定命名）→ 仓库名关键字"
// 三级回退。三级都不中才用默认值。
func bmImageRef(bm *spec.BundleManifest, def string) string {
	want := imageRole(def)
	for _, img := range bm.Images {
		if img.Ref == "" {
			continue
		}
		if img.Ref == def {
			return img.Ref
		}
		if want != "" {
			if want == imageRole(img.Ref) || want == imageRole(path.Base(img.File)) {
				return img.Ref
			}
		}
		if sameRepo(img.Ref, def) {
			return img.Ref
		}
	}
	return def
}

// imageRole 从镜像引用或文件名里认出"这是哪个组件的镜像"。
// 返回 "verdaccio"/"pypiserver"，认不出时返回空串。
func imageRole(s string) string {
	low := strings.ToLower(s)
	switch {
	case strings.Contains(low, "verdaccio"):
		return "verdaccio"
	case strings.Contains(low, "pypiserver"), strings.Contains(low, "pypi-server"):
		return "pypiserver"
	}
	return ""
}

// sameRepo 比较两个镜像引用是否指向同一个仓库名（忽略 tag/digest）。
func sameRepo(a, b string) bool {
	return repoOf(a) == repoOf(b) && repoOf(a) != ""
}

func repoOf(ref string) string {
	if i := strings.IndexByte(ref, '@'); i >= 0 {
		ref = ref[:i]
	}
	// tag 在最后一个路径段里的冒号之后；registry 端口不算（>1 个冒号才可能是端口）。
	lastSlash := strings.LastIndexByte(ref, '/')
	if i := strings.LastIndexByte(ref, ':'); i > lastSlash {
		return ref[:i]
	}
	return ref
}

// hostOnly 取地址中的主机部分，供 pip 的 trusted-host 使用。
// Address 可能被写成 "10.0.0.5"、"host:port" 甚至 URL，统一归一化。
func hostOnly(addr string) string {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return addr
	}
	if strings.Contains(addr, "://") {
		if u, err := url.Parse(addr); err == nil && u.Hostname() != "" {
			return u.Hostname()
		}
	}
	if h, _, err := net.SplitHostPort(addr); err == nil {
		return strings.Trim(h, "[]")
	}
	return strings.Trim(addr, "[]")
}

// renderApplyEnv 生成 install.sh 读取的参数文件。
//
// 用 KEY=VALUE 的 sh 语法（install.sh 里 `. apply.env`）：目标机只有 sh，
// 这是唯一无需解析器的格式。值都经过校验（端口是整数、baseDir 是绝对路径），
// 但依然用单引号包裹，防止路径里的空格破坏解析。
func renderApplyEnv(h *spec.Host) ([]byte, error) {
	if !path.IsAbs(h.BaseDir) {
		return nil, fmt.Errorf("baseDir 必须是绝对路径，实际 %q", h.BaseDir)
	}
	if h.NPMPort <= 0 || h.NPMPort > 65535 {
		return nil, fmt.Errorf("npm 端口 %d 非法", h.NPMPort)
	}
	if h.PythonPort <= 0 || h.PythonPort > 65535 {
		return nil, fmt.Errorf("python 端口 %d 非法", h.PythonPort)
	}
	if h.NPMPort == h.PythonPort {
		return nil, fmt.Errorf("npm 与 python 端口不能相同（都是 %d）", h.NPMPort)
	}

	var b strings.Builder
	b.WriteString("# 由 pkgdepot apply 生成；install.sh 会 source 这个文件。\n")
	b.WriteString("# 手工排查时可以直接改这里再执行 ./install.sh（不需要重新 apply）。\n")
	fmt.Fprintf(&b, "PKGDEPOT_BASE_DIR=%s\n", shellSingle(h.BaseDir))
	fmt.Fprintf(&b, "PKGDEPOT_NPM_PORT=%d\n", h.NPMPort)
	fmt.Fprintf(&b, "PKGDEPOT_PYTHON_PORT=%d\n", h.PythonPort)
	// compose 的 project 名由 baseDir 派生：同一台机上一个 baseDir 一套栈，
	// 不同 baseDir 的部署不会互相接管容器（compose 默认按目录名当 project 名，
	// 但 baseDir 可能含大写/特殊字符，这里显式给一个安全名字）。
	fmt.Fprintf(&b, "PKGDEPOT_PROJECT=%s\n", shellSingle(projectName(h.BaseDir)))
	b.WriteString("PKGDEPOT_SKIP_WAIT=0\n")
	return []byte(b.String()), nil
}

// projectName 从 baseDir 派生 compose project 名（只允许 [a-z0-9_-]）。
func projectName(baseDir string) string {
	base := path.Base(baseDir)
	var b strings.Builder
	sanitized := sanitizeProject(base)
	// 带上路径摘要：baseDir 的 basename 相同但路径不同的两个部署
	// （例如 /opt/x 与 /srv/x）必须落在不同的 compose project，否则后一次
	// up 会接管前一次的容器。先 Clean 掉尾斜杠与 "." 语义，让等价路径同名。
	sum := sha256.Sum256([]byte(path.Clean(baseDir)))
	b.WriteString("pkgdepot")
	if sanitized != "" {
		b.WriteString("-")
		b.WriteString(sanitized)
	}
	fmt.Fprintf(&b, "-%s", hex.EncodeToString(sum[:3]))
	return b.String()
}

// sanitizeProject 把任意字符串收窄成 compose project 名允许的字符集。
func sanitizeProject(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

// shellSingle 用单引号包裹一个值，供 sh 的 source 使用。
func shellSingle(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// remotePath 拼接目标机上的绝对路径。用 path.Join 而非 filepath.Join：
// 远端一律是 POSIX 路径。
func remotePath(baseDir string, elems ...string) string {
	return path.Join(append([]string{baseDir}, elems...)...)
}
