package skillexport

import (
	"bytes"
	"context"
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/template"

	"github.com/SparkElf/pkgdepot/internal/spec"
)

// Options is the frozen integration contract with the CLI (docs/interfaces.md).
type Options struct {
	Host        *spec.Host
	HostName    string
	OutDir      string
	Inventory   *spec.Inventory
	ToolVersion string
}

// 生成文件的相对路径与模板一一对应。写盘顺序固定，便于上层按稳定顺序输出。
var outputs = []struct {
	tmpl string
	rel  string
}{
	{tmpl: "SKILL.md.tmpl", rel: "SKILL.md"},
	{tmpl: "packages.md.tmpl", rel: filepath.Join("references", "packages.md")},
	{tmpl: "troubleshooting.md.tmpl", rel: filepath.Join("references", "troubleshooting.md")},
}

//go:embed templates/*.tmpl
var templateFS embed.FS

// 模板全是静态内容，在包初始化时解析一次；解析失败属于编译期就能发现的错误。
var templates = template.Must(template.New("skillexport").ParseFS(templateFS, "templates/*.tmpl"))

// Write 生成内网 AI 的取包技能目录，返回写入的文件绝对路径列表。
//
// 内容完全由 Options 决定：registry 地址来自 Host，包清单来自 Inventory。
// 同样的输入重复导出得到逐字节相同的文件，方便把技能目录纳入版本管理。
func Write(ctx context.Context, opts Options) ([]string, error) {
	if opts.Host == nil {
		return nil, fmt.Errorf("skill export: host is required")
	}
	if strings.TrimSpace(opts.OutDir) == "" {
		return nil, fmt.Errorf("skill export: output directory is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	data := newTemplateData(opts)
	outDir, err := filepath.Abs(opts.OutDir)
	if err != nil {
		return nil, fmt.Errorf("skill export: resolve output directory: %w", err)
	}
	refDir := filepath.Join(outDir, "references")
	if err := os.MkdirAll(refDir, 0o755); err != nil {
		return nil, fmt.Errorf("skill export: create %s: %w", refDir, err)
	}

	files := make([]string, 0, len(outputs))
	for _, o := range outputs {
		var buf bytes.Buffer
		if err := templates.ExecuteTemplate(&buf, o.tmpl, data); err != nil {
			return nil, fmt.Errorf("skill export: render %s: %w", o.tmpl, err)
		}
		target := filepath.Join(outDir, o.rel)
		if err := os.WriteFile(target, buf.Bytes(), 0o644); err != nil {
			return nil, fmt.Errorf("skill export: write %s: %w", target, err)
		}
		files = append(files, target)
	}
	return files, nil
}

// templateData is the template view of one export. Everything the templates
// print is precomputed here so the templates stay presentation-only.
type templateData struct {
	HostName    string
	SkillName   string
	ToolVersion string
	NPMURL      string
	PythonURL   string
	TrustedHost string
	BaseDir     string
	NPMPort     int
	PythonPort  int

	Verified  bool
	NPM       []spec.PackageRef
	Python    []spec.PackageRef
	HasNPM    bool
	HasPython bool

	ExampleNPM    string
	ExamplePython string
}

func newTemplateData(opts Options) templateData {
	// 导出的技能是长期留存的文件，URL 必须已经具体化。hosts.yaml 的读取方会
	// ApplyDefaults，这里在副本上再取一次默认值，保证端口/地址不会以 0 出现在
	// 生成内容里，同时不改动调用方持有的 Host。
	host := *opts.Host
	host.ApplyDefaults()

	d := templateData{
		HostName:    opts.HostName,
		SkillName:   skillName(opts.HostName),
		ToolVersion: opts.ToolVersion,
		NPMURL:      host.NPMURL(),
		PythonURL:   host.PythonURL(),
		TrustedHost: host.Address,
		BaseDir:     host.BaseDir,
		NPMPort:     host.NPMPort,
		PythonPort:  host.PythonPort,
		Verified:    opts.Inventory != nil,
	}
	if d.ToolVersion == "" {
		d.ToolVersion = "dev"
	}
	if opts.Inventory != nil {
		d.NPM = normalize(opts.Inventory.NPM)
		d.Python = normalize(opts.Inventory.Python)
	}
	d.HasNPM = len(d.NPM) > 0
	d.HasPython = len(d.Python) > 0
	if d.HasNPM {
		d.ExampleNPM = d.NPM[0].Name + "@" + d.NPM[0].Version
	}
	if d.HasPython {
		d.ExamplePython = d.Python[0].Name + "==" + d.Python[0].Version
	}
	return d
}

// normalize 去重并按 name、version 排序。Inventory 的顺序来自 map 遍历，
// 不排序会让重复导出产生不同的文件内容。
func normalize(refs []spec.PackageRef) []spec.PackageRef {
	out := make([]spec.PackageRef, 0, len(refs))
	seen := make(map[string]struct{}, len(refs))
	for _, r := range refs {
		key := r.Name + "\x00" + r.Version
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, spec.PackageRef{Name: r.Name, Version: r.Version})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Version < out[j].Version
	})
	return out
}

// skillName 把 hosts.yaml 的条目名转成合法技能名：小写字母与数字，单个短横线
// 分隔，固定以 -pkgdepot 结尾。主机名是用户自由输入的，可能含大写、点、下划线
// 或中文。
func skillName(hostName string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(hostName) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			dash = false
			continue
		}
		if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	base := strings.Trim(b.String(), "-")
	if base == "" {
		base = "host"
	}
	return base + "-pkgdepot"
}
