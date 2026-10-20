// Package assets 保存部署到目标主机的静态资产：compose.yaml、install.sh、
// verdaccio 配置与客户端配置模板。
//
// 这些文件用 go:embed 编进二进制，而不是在运行时从 bundle 里读：bundle 由构建侧
// 生成，部署侧需要能独立修正 compose/安装脚本（例如镜像引用、健康检查），
// 两边版本因此可以不一致。apply 时部署侧资产覆盖 bundle 里的同名文件。
package assets

import (
	"bytes"
	"embed"
	"fmt"
	"text/template"
)

//go:embed templates/*.tmpl
var templateFS embed.FS

// VerdaccioConfig 是 verdaccio 的 config.yaml 原文（无占位符，见文件内注释）。
//
//go:embed templates/verdaccio.config.yaml
var verdaccioConfig []byte

// InstallScript 返回目标机上执行的安装脚本原文。脚本只依赖 sh + docker + tar，
// 参数在 apply.env 里给出（见 install.sh 内的说明）。
//
//go:embed templates/install.sh
var installScript []byte

// templates 在包初始化时解析一次：模板损坏属于编译期能发现的错误，不该留到部署时。
var templates = template.Must(template.New("assets").ParseFS(templateFS, "templates/*.tmpl"))

// ComposeOptions 渲染 compose.yaml 所需的最小变量集。
//
// 端口是要配置的：同一台主机上可能已经跑着别的服务；镜像引用来自 bundle manifest，
// 因为 bundle 可能携带自定义镜像（pkgdepot.yaml 的 images 覆盖）。
type ComposeOptions struct {
	VerdaccioImage  string
	PypiserverImage string
	NPMPort         int
	PythonPort      int
}

// Compose 渲染 compose.yaml。
func Compose(opts ComposeOptions) ([]byte, error) {
	if opts.VerdaccioImage == "" || opts.PypiserverImage == "" {
		return nil, fmt.Errorf("渲染 compose.yaml: 镜像引用不能为空")
	}
	if opts.NPMPort <= 0 || opts.NPMPort > 65535 {
		return nil, fmt.Errorf("渲染 compose.yaml: npm 端口 %d 非法", opts.NPMPort)
	}
	if opts.PythonPort <= 0 || opts.PythonPort > 65535 {
		return nil, fmt.Errorf("渲染 compose.yaml: python 端口 %d 非法", opts.PythonPort)
	}
	if opts.NPMPort == opts.PythonPort {
		return nil, fmt.Errorf("渲染 compose.yaml: npm 与 python 端口不能相同（都是 %d）", opts.NPMPort)
	}
	return render("compose.yaml", opts)
}

// ClientNPMRC 渲染 npm 客户端配置。apply 之后把它放到目标机的 client/npmrc，
// 也可以拷到同网段开发机上直接使用。
func ClientNPMRC(npmURL string) ([]byte, error) {
	if npmURL == "" {
		return nil, fmt.Errorf("渲染 npmrc: npm URL 不能为空")
	}
	return render("client-npmrc", struct{ NPMURL string }{NPMURL: npmURL})
}

// ClientPipConf 渲染 pip 配置。trusted-host 只填主机部分：内网 v1 不走 TLS
// （ADR-7），pip 默认拒绝 http 索引，必须显式信任。
func ClientPipConf(indexURL, host string) ([]byte, error) {
	if indexURL == "" || host == "" {
		return nil, fmt.Errorf("渲染 pip.conf: index URL 与 host 不能为空")
	}
	return render("client-pip.conf", struct {
		IndexURL    string
		TrustedHost string
	}{IndexURL: indexURL, TrustedHost: host})
}

// VerdaccioConfigYAML 返回 verdaccio 配置原文。
func VerdaccioConfigYAML() []byte {
	return append([]byte(nil), verdaccioConfig...)
}

// InstallSH 返回 install.sh 原文。
func InstallSH() []byte {
	return append([]byte(nil), installScript...)
}

func render(name string, data any) ([]byte, error) {
	var buf bytes.Buffer
	if err := templates.ExecuteTemplate(&buf, name+".tmpl", data); err != nil {
		return nil, fmt.Errorf("渲染 %s: %w", name, err)
	}
	return buf.Bytes(), nil
}
