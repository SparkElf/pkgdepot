package skillexport

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SparkElf/pkgdepot/internal/spec"
)

// testHost returns a host whose URLs are distinctive, so an assertion on the
// registry URL can only pass if the template actually rendered it.
func testHost() *spec.Host {
	return &spec.Host{
		Address:    "10.20.30.40",
		Port:       22,
		User:       "ops",
		BaseDir:    "/srv/pkgdepot",
		NPMPort:    4873,
		PythonPort: 8080,
	}
}

func testInventory() *spec.Inventory {
	return &spec.Inventory{
		NPM: []spec.PackageRef{
			{Name: "left-pad", Version: "1.3.0"},
			{Name: "@scope/tool", Version: "2.0.1"},
		},
		Python: []spec.PackageRef{
			{Name: "requests", Version: "2.32.3"},
			{Name: "numpy", Version: "2.1.0"},
		},
	}
}

// writeAndRead exports into a temp dir and returns the generated file contents
// keyed by output path relative to the export root.
func writeAndRead(t *testing.T, opts Options) ([]string, map[string]string) {
	t.Helper()
	files, err := Write(context.Background(), opts)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	contents := make(map[string]string, len(files))
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		rel, err := filepath.Rel(opts.OutDir, f)
		if err != nil {
			t.Fatalf("rel %s: %v", f, err)
		}
		contents[rel] = string(raw)
	}
	return files, contents
}

// TestWriteCreatesTheThreeFiles pins the exported layout. Consumers (the CLI
// output and the e2e case) address these exact names.
func TestWriteCreatesTheThreeFiles(t *testing.T) {
	out := filepath.Join(t.TempDir(), "skill")
	files, contents := writeAndRead(t, Options{
		Host:        testHost(),
		HostName:    "intra-01",
		OutDir:      out,
		Inventory:   testInventory(),
		ToolVersion: "v1.2.3",
	})

	if len(files) != 3 {
		t.Fatalf("want 3 files, got %d: %v", len(files), files)
	}
	for _, rel := range []string{"SKILL.md", filepath.Join("references", "packages.md"), filepath.Join("references", "troubleshooting.md")} {
		if _, ok := contents[rel]; !ok {
			t.Errorf("missing output %q (have %v)", rel, keys(contents))
		}
	}
}

// TestSkillFrontmatter checks the SKILL.md metadata contract: exactly the two
// fields DSH accepts, a kebab-case name, and a non-empty description.
func TestSkillFrontmatter(t *testing.T) {
	_, contents := writeAndRead(t, Options{
		Host:        testHost(),
		HostName:    "Intra_Cluster.01",
		OutDir:      filepath.Join(t.TempDir(), "skill"),
		Inventory:   testInventory(),
		ToolVersion: "v0.1.0",
	})

	skill := contents["SKILL.md"]
	fm, body := splitFrontmatter(t, skill)
	lines := strings.Split(strings.TrimSpace(fm), "\n")
	if len(lines) != 2 {
		t.Fatalf("frontmatter must have exactly name+description, got %d lines:\n%s", len(lines), fm)
	}
	if !strings.HasPrefix(lines[0], "name: ") {
		t.Errorf("first frontmatter field must be name, got %q", lines[0])
	}
	if !strings.HasPrefix(lines[1], "description: ") {
		t.Errorf("second frontmatter field must be description, got %q", lines[1])
	}

	name := strings.TrimSpace(strings.TrimPrefix(lines[0], "name: "))
	if name != "intra-cluster-01-pkgdepot" {
		t.Errorf("skill name = %q, want %q", name, "intra-cluster-01-pkgdepot")
	}
	if strings.TrimSpace(strings.TrimPrefix(lines[1], "description: ")) == "" {
		t.Error("description must not be empty")
	}
	if !strings.Contains(body, "# "+name) {
		t.Errorf("body must start with the skill heading %q", name)
	}
}

// TestWriteEmbedsRegistryURLs is the core data-driven assertion: the URLs come
// from the Host, never from a hard-coded template constant.
func TestWriteEmbedsRegistryURLs(t *testing.T) {
	_, contents := writeAndRead(t, Options{
		Host:        testHost(),
		HostName:    "intra-01",
		OutDir:      filepath.Join(t.TempDir(), "skill"),
		Inventory:   testInventory(),
		ToolVersion: "v1.2.3",
	})

	wantNPM := "http://10.20.30.40:4873/"
	wantPy := "http://10.20.30.40:8080/simple/"
	for _, rel := range []string{"SKILL.md", filepath.Join("references", "packages.md"), filepath.Join("references", "troubleshooting.md")} {
		text := contents[rel]
		if !strings.Contains(text, wantNPM) {
			t.Errorf("%s does not contain npm URL %q", rel, wantNPM)
		}
		if !strings.Contains(text, wantPy) {
			t.Errorf("%s does not contain python URL %q", rel, wantPy)
		}
	}
	if !strings.Contains(contents["SKILL.md"], "trusted-host 10.20.30.40") {
		t.Error("SKILL.md must show the trusted-host matching the host address")
	}
}

// TestWriteEmbedsHostSpecificSettings covers the remaining host-derived values:
// a non-default base dir and non-default ports must appear in the export, and
// the defaults must not leak in their place.
func TestWriteEmbedsHostSpecificSettings(t *testing.T) {
	host := testHost()
	host.BaseDir = "/data/pkgdepot"
	host.NPMPort = 14873
	host.PythonPort = 18080

	_, contents := writeAndRead(t, Options{
		Host:        host,
		HostName:    "intra-01",
		OutDir:      filepath.Join(t.TempDir(), "skill"),
		Inventory:   testInventory(),
		ToolVersion: "v1.2.3",
	})

	skill := contents["SKILL.md"]
	faults := contents[filepath.Join("references", "troubleshooting.md")]
	if !strings.Contains(skill, "/data/pkgdepot") {
		t.Error("SKILL.md must name the host's base dir")
	}
	if !strings.Contains(skill, "http://10.20.30.40:14873/") {
		t.Error("SKILL.md must use the host's npm port")
	}
	if !strings.Contains(skill, "http://10.20.30.40:18080/simple/") {
		t.Error("SKILL.md must use the host's python port")
	}
	if !strings.Contains(faults, "14873") || !strings.Contains(faults, "18080") {
		t.Error("troubleshooting.md must quote the host's actual ports")
	}
}

// TestWriteListsEveryInventoryPackage guards against a template that renders the
// registry URL but silently drops the package list.
func TestWriteListsEveryInventoryPackage(t *testing.T) {
	inv := testInventory()
	_, contents := writeAndRead(t, Options{
		Host:        testHost(),
		HostName:    "intra-01",
		OutDir:      filepath.Join(t.TempDir(), "skill"),
		Inventory:   inv,
		ToolVersion: "v1.2.3",
	})

	for _, rel := range []string{"SKILL.md", filepath.Join("references", "packages.md")} {
		text := contents[rel]
		for _, p := range inv.NPM {
			if !strings.Contains(text, p.Name) {
				t.Errorf("%s missing npm package %q", rel, p.Name)
			}
			if !strings.Contains(text, p.Version) {
				t.Errorf("%s missing npm version %q for %s", rel, p.Version, p.Name)
			}
		}
		for _, p := range inv.Python {
			if !strings.Contains(text, p.Name) {
				t.Errorf("%s missing python package %q", rel, p.Name)
			}
			if !strings.Contains(text, p.Version) {
				t.Errorf("%s missing python version %q for %s", rel, p.Version, p.Name)
			}
		}
	}
	// The rendering convention for the two ecosystems differs and is what a
	// reader copies, so pin both shapes.
	pkgs := contents[filepath.Join("references", "packages.md")]
	for _, p := range inv.NPM {
		if !strings.Contains(pkgs, p.Name+"@"+p.Version) {
			t.Errorf("packages.md missing npm spec %s@%s", p.Name, p.Version)
		}
	}
	for _, p := range inv.Python {
		if !strings.Contains(pkgs, p.Name+"=="+p.Version) {
			t.Errorf("packages.md missing python spec %s==%s", p.Name, p.Version)
		}
	}
}

// TestWriteGuidesMissingDeps proves the "which host do I fix this on" guidance
// exists and points back to the offline build side rather than the public
// internet.
func TestWriteGuidesMissingDeps(t *testing.T) {
	_, contents := writeAndRead(t, Options{
		Host:        testHost(),
		HostName:    "intra-01",
		OutDir:      filepath.Join(t.TempDir(), "skill"),
		Inventory:   testInventory(),
		ToolVersion: "v1.2.3",
	})

	skill := contents["SKILL.md"]
	for _, want := range []string{
		"references/packages.md",
		"pkgdepot.yaml",
		"pkgdepot bundle",
		"pkgdepot apply --host intra-01",
		"pkgdepot skill export --host intra-01",
	} {
		if !strings.Contains(skill, want) {
			t.Errorf("SKILL.md missing missing-dependency guidance %q", want)
		}
	}
	if strings.Contains(skill, "npmjs.com") || strings.Contains(skill, "pypi.org") {
		t.Error("SKILL.md must not send the offline host to a public registry")
	}
}

// TestWriteWithoutInventory checks the pre-apply state: the export still works
// and says the package list is unknown instead of inventing packages.
func TestWriteWithoutInventory(t *testing.T) {
	_, contents := writeAndRead(t, Options{
		Host:        testHost(),
		HostName:    "intra-01",
		OutDir:      filepath.Join(t.TempDir(), "skill"),
		Inventory:   nil,
		ToolVersion: "v1.2.3",
	})

	skill := contents["SKILL.md"]
	if !strings.Contains(skill, "还没有完成过") {
		t.Error("SKILL.md must state that no verified inventory exists yet")
	}
	if !strings.Contains(contents[filepath.Join("references", "packages.md")], "尚无校验过的清单") {
		t.Error("packages.md must state that no verified inventory exists yet")
	}
	if strings.Contains(skill, "npm install left-pad") {
		t.Error("an empty inventory must not render package examples")
	}
}

// TestWriteIsDeterministic: two exports of the same input must be byte-equal so
// the skill directory can live in git without spurious diffs.
func TestWriteIsDeterministic(t *testing.T) {
	// Deliberately unsorted, with a duplicate: the export sorts and dedupes.
	inv := &spec.Inventory{
		NPM: []spec.PackageRef{
			{Name: "zeta", Version: "2.0.0"},
			{Name: "alpha", Version: "1.0.0"},
			{Name: "zeta", Version: "2.0.0"},
		},
	}
	base := t.TempDir()
	_, first := writeAndRead(t, Options{
		Host: testHost(), HostName: "h1", OutDir: filepath.Join(base, "a"), Inventory: inv, ToolVersion: "v1",
	})
	_, second := writeAndRead(t, Options{
		Host: testHost(), HostName: "h1", OutDir: filepath.Join(base, "b"), Inventory: inv, ToolVersion: "v1",
	})
	for rel, want := range first {
		if got := second[rel]; got != want {
			t.Errorf("%s differs between identical exports", rel)
		}
	}
	pkgs := first[filepath.Join("references", "packages.md")]
	if strings.Count(pkgs, "zeta") != strings.Count(pkgs, "2.0.0") {
		t.Errorf("duplicate package ref should be deduped:\n%s", pkgs)
	}
	if strings.Index(pkgs, "alpha") > strings.Index(pkgs, "zeta") {
		t.Error("packages must be sorted by name")
	}
}

// TestWriteRejectsMissingHost keeps the failure at the boundary instead of
// producing a skill with empty URLs.
func TestWriteRejectsMissingHost(t *testing.T) {
	_, err := Write(context.Background(), Options{HostName: "h", OutDir: t.TempDir()})
	if err == nil {
		t.Fatal("Write must reject a nil host")
	}
	if !strings.Contains(err.Error(), "host") {
		t.Errorf("error should name the missing field, got %v", err)
	}
}

// TestWriteHonoursCancelledContext keeps the export from doing filesystem work
// after the caller gave up (the CLI cancels on Ctrl-C).
func TestWriteHonoursCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out := filepath.Join(t.TempDir(), "skill")
	_, err := Write(ctx, Options{
		Host: testHost(), HostName: "h", OutDir: out, Inventory: testInventory(),
	})
	if err == nil {
		t.Fatal("Write must return the context error")
	}
	if _, statErr := os.Stat(out); !os.IsNotExist(statErr) {
		t.Error("a cancelled export must not create the output directory")
	}
}

// TestSkillNameTable pins the kebab-case conversion users end up typing.
func TestSkillNameTable(t *testing.T) {
	cases := map[string]string{
		"intra-01":         "intra-01-pkgdepot",
		"Intra_Cluster.01": "intra-cluster-01-pkgdepot",
		"  weird  name  ":  "weird-name-pkgdepot",
		"内网主机":             "host-pkgdepot",
		"":                 "host-pkgdepot",
	}
	for in, want := range cases {
		if got := skillName(in); got != want {
			t.Errorf("skillName(%q) = %q, want %q", in, got, want)
		}
	}
}

func splitFrontmatter(t *testing.T, doc string) (fm, body string) {
	t.Helper()
	if !strings.HasPrefix(doc, "---\n") {
		t.Fatalf("SKILL.md must start with a frontmatter fence:\n%s", doc)
	}
	rest := doc[len("---\n"):]
	end := strings.Index(rest, "\n---\n")
	if end < 0 {
		t.Fatal("SKILL.md frontmatter is not closed")
	}
	return rest[:end], rest[end+len("\n---\n"):]
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
