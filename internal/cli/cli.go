// Package cli assembles the pkgdepot command surface.
//
// Every subcommand is wired here and nothing else: the deploy, bundle, and
// export packages own behaviour, this package owns argument parsing, exit codes
// and human-readable output. That split keeps the command list in one file and
// the mechanisms independently testable.
package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/SparkElf/pkgdepot/internal/bundle"
	"github.com/SparkElf/pkgdepot/internal/hosts"
	"github.com/SparkElf/pkgdepot/internal/install"
	"github.com/SparkElf/pkgdepot/internal/skillexport"
	"github.com/SparkElf/pkgdepot/internal/spec"
)

// Version is injected at build time via -ldflags; the dev value is explicit so
// bug reports can tell a source build from a release.
var Version = "dev"

// App carries the output streams so tests and the binary share one entrypoint.
type App struct {
	Stdout io.Writer
	Stderr io.Writer

	// version is resolved once per invocation.
	version string
}

// New builds an App writing to the given streams.
func New(stdout, stderr io.Writer) *App {
	return &App{Stdout: stdout, Stderr: stderr, version: Version}
}

const usage = `pkgdepot - 一键在内网建立 npm / PyPI 私有包仓库

用法:
  pkgdepot plan    -f pkgdepot.yaml                 解析依赖清单并展示闭包摘要
  pkgdepot bundle  -f pkgdepot.yaml -o <file>       构建可离线携带的 bundle
  pkgdepot host add <name> --address <ip> --user <u> [--port 22] [--key ~/.ssh/id_ed25519]
  pkgdepot host list
  pkgdepot host remove <name>
  pkgdepot apply   --host <name> --bundle <file> [--skip-verify]
  pkgdepot status  --host <name>
  pkgdepot verify  --host <name> --bundle <file>
  pkgdepot env     --host <name> [--npm|--python]
  pkgdepot skill export --host <name> -o <dir>
  pkgdepot uninstall --host <name>
  pkgdepot version

在不指定 --host 的命令里，--host 等于 hosts.yaml 中的条目名。
`

// Run dispatches args (without the program name) and returns a process exit
// code. Errors are printed here so every command reports failures uniformly.
func (a *App) Run(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(a.Stdout, usage)
		return 0
	}
	ctx := context.Background()
	cmd := args[0]
	rest := args[1:]

	var err error
	switch cmd {
	case "plan":
		err = a.cmdPlan(ctx, rest)
	case "bundle":
		err = a.cmdBundle(ctx, rest)
	case "host":
		err = a.cmdHost(ctx, rest)
	case "apply":
		err = a.cmdApply(ctx, rest)
	case "status":
		err = a.cmdStatus(ctx, rest)
	case "verify":
		err = a.cmdVerify(ctx, rest)
	case "env":
		err = a.cmdEnv(ctx, rest)
	case "skill":
		err = a.cmdSkill(ctx, rest)
	case "uninstall":
		err = a.cmdUninstall(ctx, rest)
	case "version", "--version", "-v":
		fmt.Fprintf(a.Stdout, "pkgdepot %s\n", a.version)
	case "help", "--help", "-h":
		fmt.Fprint(a.Stdout, usage)
	default:
		fmt.Fprintf(a.Stderr, "pkgdepot: unknown command %q\n\n%s", cmd, usage)
		return 2
	}
	if err != nil {
		fmt.Fprintf(a.Stderr, "pkgdepot: %v\n", err)
		return 1
	}
	return 0
}

// flags is a tiny hand-rolled parser. The command set is small and fixed, so a
// dependency-free parser keeps `pkgdepot --help` fast and the binary small.
type flags struct {
	values map[string]string
	bools  map[string]bool
	pos    []string
}

func parseFlags(args []string, boolNames ...string) (*flags, error) {
	f := &flags{values: map[string]string{}, bools: map[string]bool{}}
	isBool := map[string]bool{}
	for _, b := range boolNames {
		isBool[b] = true
	}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			f.pos = append(f.pos, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			f.pos = append(f.pos, arg)
			continue
		}
		name := strings.TrimLeft(arg, "-")
		if eq := strings.Index(name, "="); eq >= 0 {
			f.values[name[:eq]] = name[eq+1:]
			continue
		}
		if isBool[name] {
			f.bools[name] = true
			continue
		}
		if i+1 >= len(args) {
			return nil, fmt.Errorf("flag --%s requires a value", name)
		}
		i++
		f.values[name] = args[i]
	}
	return f, nil
}

func (f *flags) str(name string) string { return f.values[name] }

func (f *flags) require(name string) (string, error) {
	v := f.values[name]
	if v == "" {
		return "", fmt.Errorf("missing required flag --%s", name)
	}
	return v, nil
}

func (f *flags) intVal(name string, def int) (int, error) {
	v := f.values[name]
	if v == "" {
		return def, nil
	}
	var n int
	if _, err := fmt.Sscanf(v, "%d", &n); err != nil {
		return 0, fmt.Errorf("flag --%s must be an integer, got %q", name, v)
	}
	return n, nil
}

func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
}

// --- plan / bundle -------------------------------------------------------

func (a *App) cmdPlan(ctx context.Context, args []string) error {
	f, err := parseFlags(args)
	if err != nil {
		return err
	}
	manifestPath := f.str("f")
	if manifestPath == "" {
		manifestPath = f.str("file")
	}
	if manifestPath == "" {
		return fmt.Errorf("missing required flag -f <pkgdepot.yaml>")
	}
	m, err := spec.LoadManifest(manifestPath)
	if err != nil {
		return err
	}
	plan, err := bundle.Plan(ctx, bundle.PlanOptions{
		Manifest:     m,
		ManifestPath: manifestPath,
		Log:          logf,
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(a.Stdout, "清单: %s (%s)\n", m.Name, manifestPath)
	fmt.Fprintf(a.Stdout, "镜像: %s, %s\n", m.Images.Verdaccio, m.Images.Pypiserver)
	fmt.Fprintf(a.Stdout, "npm:  %d 个包\n", len(plan.NPM))
	for _, p := range plan.NPM {
		fmt.Fprintf(a.Stdout, "  %s@%s\n", p.Name, p.Version)
	}
	fmt.Fprintf(a.Stdout, "python: %d 个文件\n", len(plan.Python))
	for _, p := range plan.Python {
		fmt.Fprintf(a.Stdout, "  %s (%s)\n", p.File, p.Version)
	}
	return nil
}

func (a *App) cmdBundle(ctx context.Context, args []string) error {
	f, err := parseFlags(args)
	if err != nil {
		return err
	}
	manifestPath := f.str("f")
	if manifestPath == "" {
		manifestPath = f.str("file")
	}
	if manifestPath == "" {
		return fmt.Errorf("missing required flag -f <pkgdepot.yaml>")
	}
	out := f.str("o")
	if out == "" {
		out = f.str("out")
	}
	m, err := spec.LoadManifest(manifestPath)
	if err != nil {
		return err
	}
	if out == "" {
		out = fmt.Sprintf("%s-%s.tar.gz", m.Name, time.Now().Format("20060102"))
	}
	abs, err := filepath.Abs(out)
	if err != nil {
		return err
	}
	bm, err := bundle.Build(ctx, bundle.Options{
		Manifest:     m,
		ManifestPath: manifestPath,
		OutFile:      abs,
		ToolVersion:  a.version,
		Log:          logf,
	})
	if err != nil {
		return err
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.Stdout, "bundle: %s (%d bytes)\n", abs, fi.Size())
	fmt.Fprintf(a.Stdout, "  images: %d\n  npm:    %d\n  python: %d\n",
		len(bm.Images), len(bm.NPM), len(bm.Python))
	fmt.Fprintf(a.Stdout, "\n下一步: 把该文件拷入内网，执行 pkgdepot apply --host <name> --bundle %s\n", filepath.Base(abs))
	return nil
}

// --- host ----------------------------------------------------------------

func (a *App) cmdHost(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("host subcommand required: add|list|remove")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "add":
		if len(rest) == 0 {
			return fmt.Errorf("host add requires a name")
		}
		name := rest[0]
		f, err := parseFlags(rest[1:])
		if err != nil {
			return err
		}
		addr, err := f.require("address")
		if err != nil {
			return err
		}
		user, err := f.require("user")
		if err != nil {
			return err
		}
		port, err := f.intVal("port", 0)
		if err != nil {
			return err
		}
		npmPort, err := f.intVal("npm-port", 0)
		if err != nil {
			return err
		}
		pyPort, err := f.intVal("python-port", 0)
		if err != nil {
			return err
		}
		h := &spec.Host{
			Address:      addr,
			Port:         port,
			User:         user,
			IdentityFile: f.str("key"),
			BaseDir:      f.str("base-dir"),
			NPMPort:      npmPort,
			PythonPort:   pyPort,
		}
		if err := hosts.Add(name, h); err != nil {
			return err
		}
		p, _ := hosts.Path()
		fmt.Fprintf(a.Stdout, "已保存主机 %q 到 %s\n", name, p)
		fmt.Fprintf(a.Stdout, "  ssh      %s@%s:%d\n", h.User, h.Address, h.Port)
		fmt.Fprintf(a.Stdout, "  npm      %s\n", h.NPMURL())
		fmt.Fprintf(a.Stdout, "  python   %s\n", h.PythonURL())
		return nil

	case "list":
		hf, err := hosts.Load()
		if err != nil {
			return err
		}
		names := hf.Names()
		if len(names) == 0 {
			fmt.Fprintln(a.Stdout, "（尚未配置任何主机，用 pkgdepot host add 添加）")
			return nil
		}
		for _, n := range names {
			h := hf.Hosts[n]
			h.ApplyDefaults()
			last := "从未部署"
			if h.LastApply != nil {
				last = fmt.Sprintf("%s / %s", h.LastApply.AppliedAt.Format("2006-01-02 15:04"), h.LastApply.BundleName)
			}
			fmt.Fprintf(a.Stdout, "%-16s %s@%s:%d  npm=%s python=%s  上次: %s\n",
				n, h.User, h.Address, h.Port, h.NPMURL(), h.PythonURL(), last)
		}
		return nil

	case "remove":
		if len(rest) == 0 {
			return fmt.Errorf("host remove requires a name")
		}
		if err := hosts.Remove(rest[0]); err != nil {
			return err
		}
		fmt.Fprintf(a.Stdout, "已移除主机 %q\n", rest[0])
		return nil
	default:
		return fmt.Errorf("unknown host subcommand %q (want add|list|remove)", sub)
	}
}

// --- apply / verify / status / uninstall ---------------------------------

func (a *App) resolveHost(f *flags) (string, *spec.Host, error) {
	name := f.str("host")
	if name == "" {
		return "", nil, fmt.Errorf("missing required flag --host <name>")
	}
	h, err := hosts.Get(name)
	if err != nil {
		return "", nil, err
	}
	return name, h, nil
}

func (a *App) cmdApply(ctx context.Context, args []string) error {
	f, err := parseFlags(args, "skip-verify")
	if err != nil {
		return err
	}
	name, h, err := a.resolveHost(f)
	if err != nil {
		return err
	}
	bundlePath, err := f.require("bundle")
	if err != nil {
		return err
	}
	abs, err := filepath.Abs(bundlePath)
	if err != nil {
		return err
	}
	if _, err := os.Stat(abs); err != nil {
		return fmt.Errorf("bundle %s: %w", abs, err)
	}
	inv, err := install.Apply(ctx, install.ApplyOptions{
		Host:       h,
		BundlePath: abs,
		SkipVerify: f.bools["skip-verify"],
		Log:        logf,
	})
	if err != nil {
		return err
	}
	fi, _ := os.Stat(abs)
	h.LastApply = &spec.Apply{
		AppliedAt:  time.Now(),
		BundleName: filepath.Base(abs),
		Inventory:  *inv,
	}
	if fi != nil {
		if sum, err := fileSHA256(abs); err == nil {
			h.LastApply.BundleDigest = sum
		}
	}
	if err := hosts.Update(name, h); err != nil {
		return err
	}
	fmt.Fprintf(a.Stdout, "\n主机 %q 已就绪\n", name)
	fmt.Fprintf(a.Stdout, "  npm      %s\n", h.NPMURL())
	fmt.Fprintf(a.Stdout, "  python   %s\n", h.PythonURL())
	fmt.Fprintf(a.Stdout, "  npm 包    %d 个\n", len(inv.NPM))
	fmt.Fprintf(a.Stdout, "  python 包 %d 个\n", len(inv.Python))
	fmt.Fprintf(a.Stdout, "\n在目标网段内配置客户端:  eval \"$(pkgdepot env --host %s)\"\n", name)
	fmt.Fprintf(a.Stdout, "为内网 AI 生成取包技能:  pkgdepot skill export --host %s -o ./skills\n", name)
	return nil
}

func (a *App) cmdVerify(ctx context.Context, args []string) error {
	f, err := parseFlags(args)
	if err != nil {
		return err
	}
	name, h, err := a.resolveHost(f)
	if err != nil {
		return err
	}
	bundlePath, err := f.require("bundle")
	if err != nil {
		return err
	}
	bm, _, err := bundle.ReadManifest(bundlePath)
	if err != nil {
		return err
	}
	inv, err := install.Verify(ctx, h, bm, logf)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.Stdout, "校验通过: npm %d 个版本, python %d 个文件\n", len(inv.NPM), len(inv.Python))
	h.LastApply = &spec.Apply{
		AppliedAt:  time.Now(),
		BundleName: filepath.Base(bundlePath),
		Inventory:  *inv,
	}
	if err := hosts.Update(name, h); err != nil {
		return err
	}
	return nil
}

func (a *App) cmdStatus(ctx context.Context, args []string) error {
	f, err := parseFlags(args)
	if err != nil {
		return err
	}
	_, h, err := a.resolveHost(f)
	if err != nil {
		return err
	}
	out, err := install.Status(ctx, h)
	if err != nil {
		return err
	}
	fmt.Fprint(a.Stdout, out)
	return nil
}

func (a *App) cmdUninstall(ctx context.Context, args []string) error {
	f, err := parseFlags(args)
	if err != nil {
		return err
	}
	name, h, err := a.resolveHost(f)
	if err != nil {
		return err
	}
	if err := install.Uninstall(ctx, h); err != nil {
		return err
	}
	h.LastApply = nil
	if err := hosts.Update(name, h); err != nil {
		return err
	}
	fmt.Fprintf(a.Stdout, "主机 %q 的 registry 已停止（数据目录保留）\n", name)
	return nil
}

// --- env -----------------------------------------------------------------

func (a *App) cmdEnv(ctx context.Context, args []string) error {
	f, err := parseFlags(args, "npm", "python")
	if err != nil {
		return err
	}
	_, h, err := a.resolveHost(f)
	if err != nil {
		return err
	}
	wantNPM := f.bools["npm"] || (!f.bools["npm"] && !f.bools["python"])
	wantPy := f.bools["python"] || (!f.bools["npm"] && !f.bools["python"])
	if wantNPM {
		fmt.Fprintf(a.Stdout, "export NPM_CONFIG_REGISTRY=%q\n", h.NPMURL())
	}
	if wantPy {
		fmt.Fprintf(a.Stdout, "export PIP_INDEX_URL=%q\n", h.PythonURL())
		fmt.Fprintf(a.Stdout, "export PIP_TRUSTED_HOST=%q\n", h.Address)
	}
	return nil
}

// --- skill export --------------------------------------------------------

func (a *App) cmdSkill(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] != "export" {
		return fmt.Errorf("skill subcommand required: export")
	}
	f, err := parseFlags(args[1:])
	if err != nil {
		return err
	}
	name, h, err := a.resolveHost(f)
	if err != nil {
		return err
	}
	out := f.str("o")
	if out == "" {
		out = f.str("out")
	}
	if out == "" {
		out = filepath.Join(name + "-pkgdepot")
	}
	files, err := skillexport.Write(ctx, skillexport.Options{
		Host:        h,
		HostName:    name,
		OutDir:      out,
		Inventory:   inventoryOf(h),
		ToolVersion: a.version,
	})
	if err != nil {
		return err
	}
	sort.Strings(files)
	fmt.Fprintf(a.Stdout, "已导出技能到 %s\n", out)
	for _, f := range files {
		fmt.Fprintf(a.Stdout, "  %s\n", f)
	}
	if inventoryOf(h) == nil {
		fmt.Fprintln(a.Stderr, "提示: 该主机还没有校验过的包清单，先执行 pkgdepot apply 或 pkgdepot verify 让技能包含真实包列表")
	}
	return nil
}

// inventoryOf returns the last verified inventory, or nil when the host has
// never completed an apply/verify. skill export renders an explicitly empty
// state rather than pretending packages exist.
func inventoryOf(h *spec.Host) *spec.Inventory {
	if h.LastApply == nil {
		return nil
	}
	inv := h.LastApply.Inventory
	return &inv
}

// --- helpers -------------------------------------------------------------
