//go:build e2e

package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// buildFixture creates a realistic project fixture: one small npm dependency and
// one small Python dependency with a pinned requirements file. Small packages
// keep the case fast while still exercising real resolution, real tarball
// downloads and real transitive dependencies.
func buildFixture(t *testing.T) (dir, manifestPath string) {
	t.Helper()
	dir = t.TempDir()

	writeFile(t, filepath.Join(dir, "app", "package.json"), `{
  "name": "pkgdepot-e2e-app",
  "version": "1.0.0",
  "private": true,
  "dependencies": {
    "ms": "2.1.3"
  }
}
`)
	writeFile(t, filepath.Join(dir, "requirements.txt"), "idna==3.10\n")

	manifestPath = filepath.Join(dir, "pkgdepot.yaml")
	writeFile(t, manifestPath, `apiVersion: pkgdepot/v1
name: e2e
npm:
  lockfiles:
    - app/package.json
python:
  requirements:
    - requirements.txt
  platforms:
    - manylinux2014_x86_64
  pythonVersion: "3.12"
  implementation: cp
`)
	return dir, manifestPath
}

// TestApplyCreatesWorkingRegistries is the primary user goal: an operator carries
// a bundle into an isolated network, points pkgdepot at a host, and ends up with
// two private registries that real offline clients can install from.
func TestApplyCreatesWorkingRegistries(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e")
	}
	h := newHarness(t, "e2e-primary")
	npmPort, pyPort := freePort(t), freePort(t)
	h.registerHost(npmPort, pyPort)

	dir, manifestPath := buildFixture(t)
	bundlePath := filepath.Join(dir, "e2e-bundle.tar.gz")

	// 1) Build the bundle on the "connected" side.
	out := h.mustRun("bundle", "-f", manifestPath, "-o", bundlePath)
	if !strings.Contains(out, "npm:") {
		t.Fatalf("bundle output did not report npm packages:\n%s", out)
	}
	if fi, err := os.Stat(bundlePath); err != nil {
		t.Fatalf("bundle not written: %v", err)
	} else if fi.Size() == 0 {
		t.Fatal("bundle is empty")
	}

	// 2) Deploy to the host over real SSH.
	h.mustRun("apply", "--host", h.hostName, "--bundle", bundlePath)

	// 3) The registries must now serve what the bundle promised. This is read
	// through the real HTTP ports, not through the CLI's own bookkeeping.
	assertNPMRegistryServes(t, npmPort, "ms", "2.1.3")
	assertPyPIRegistryServes(t, pyPort, "idna", "3.10")

	// 4) A real client with no internet route installs from the private registry.
	h.joinNetwork(t, npmPort, pyPort)
	npmHost := h.registryContainerName(t, npmPort)
	pyHost := h.registryContainerName(t, pyPort)
	npmOut := h.offlineContainer(t, "node:22-alpine",
		`mkdir -p /tmp/x && cd /tmp/x && `+
			`printf '{"name":"c","private":true}' > package.json && `+
			`npm install ms@2.1.3 --registry http://`+npmHost+`:4873 --no-audit --no-fund 2>&1 | tail -3 && `+
			`node -e "console.log(require('/tmp/x/node_modules/ms/package.json').version)"`)
	if !strings.Contains(npmOut, "2.1.3") {
		t.Fatalf("offline npm install did not produce ms 2.1.3:\n%s", npmOut)
	}

	pyOut := h.offlineContainer(t, "python:3.12-slim",
		`pip install --no-cache-dir --index-url http://`+pyHost+`:8080/simple/ `+
			`--trusted-host `+pyHost+` idna==3.10 2>&1 | tail -2 && `+
			`python -c "import idna; print(idna.__version__)"`)
	if !strings.Contains(pyOut, "3.10") {
		t.Fatalf("offline pip install did not produce idna 3.10:\n%s", pyOut)
	}

	// 5) A repeat verification must agree: this proves the served bytes match
	// the bundle manifest, not just that *something* responded.
	verifyOut := h.mustRun("verify", "--host", h.hostName, "--bundle", bundlePath)
	if !strings.Contains(verifyOut, "校验通过") {
		t.Fatalf("verify did not report success:\n%s", verifyOut)
	}

	// 6) The exported skill must describe this host, not a template.
	skillDir := filepath.Join(dir, "skill")
	h.mustRun("skill", "export", "--host", h.hostName, "-o", skillDir)
	skill, err := os.ReadFile(filepath.Join(skillDir, "SKILL.md"))
	if err != nil {
		t.Fatalf("skill not exported: %v", err)
	}
	body := string(skill)
	for _, want := range []string{"ms", "idna", itoa(npmPort)} {
		if !strings.Contains(body, want) {
			t.Fatalf("exported skill missing %q:\n%s", want, body)
		}
	}
}

// TestVerifyDetectsTamperedBundle proves verification is real: if the bundle
// manifest claims a digest that the served bytes do not have, apply must fail
// and name the offending package.
func TestVerifyDetectsTamperedBundle(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e")
	}
	h := newHarness(t, "e2e-tamper")
	npmPort, pyPort := freePort(t), freePort(t)
	h.registerHost(npmPort, pyPort)

	dir, manifestPath := buildFixture(t)
	bundlePath := filepath.Join(dir, "tamper-bundle.tar.gz")
	h.mustRun("bundle", "-f", manifestPath, "-o", bundlePath)
	h.mustRun("apply", "--host", h.hostName, "--bundle", bundlePath, "--skip-verify")

	// Corrupt the served tarball behind the registry's back: the manifest digest
	// no longer matches what clients would receive.
	storage := filepath.Join(h.baseDir, "npm", "storage", "ms")
	entries, err := os.ReadDir(storage)
	if err != nil {
		t.Fatalf("read storage %s: %v", storage, err)
	}
	var tgz string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tgz") {
			tgz = filepath.Join(storage, e.Name())
		}
	}
	if tgz == "" {
		t.Fatalf("no tarball found in %s", storage)
	}
	if err := os.WriteFile(tgz, []byte("corrupted"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, code := h.run("verify", "--host", h.hostName, "--bundle", bundlePath)
	if code == 0 {
		t.Fatalf("verify accepted a tampered package:\n%s", out)
	}
	if !strings.Contains(out, "ms") {
		t.Fatalf("verify failure did not name the offending package:\n%s", out)
	}
}

// TestSkillExportGuidesMissingDeps covers the AI-facing goal: given a host with
// a verified inventory, the exported skill tells the agent what exists and,
// when a package is missing, that it must be added on the connected side rather
// than fetched from the internet.
func TestSkillExportGuidesMissingDeps(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e")
	}
	h := newHarness(t, "e2e-skill")
	npmPort, pyPort := freePort(t), freePort(t)
	h.registerHost(npmPort, pyPort)

	dir, manifestPath := buildFixture(t)
	bundlePath := filepath.Join(dir, "skill-bundle.tar.gz")
	h.mustRun("bundle", "-f", manifestPath, "-o", bundlePath)
	h.mustRun("apply", "--host", h.hostName, "--bundle", bundlePath)

	skillDir := filepath.Join(dir, "skill")
	h.mustRun("skill", "export", "--host", h.hostName, "-o", skillDir)

	for _, name := range []string{"SKILL.md", "references/packages.md"} {
		if _, err := os.Stat(filepath.Join(skillDir, name)); err != nil {
			t.Fatalf("missing %s in exported skill: %v", name, err)
		}
	}
	refs, err := os.ReadFile(filepath.Join(skillDir, "references", "packages.md"))
	if err != nil {
		t.Fatal(err)
	}
	pkgs := string(refs)
	if !strings.Contains(pkgs, "ms") || !strings.Contains(pkgs, "idna") {
		t.Fatalf("package reference does not list the served packages:\n%s", pkgs)
	}
	body, err := os.ReadFile(filepath.Join(skillDir, "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "127.0.0.1:"+itoa(npmPort)) {
		t.Fatalf("exported skill does not point at the real registry:\n%s", body)
	}
}

// TestReapplyIsIdempotent covers the operational reality: bundles get rebuilt
// and redeployed. Re-applying must not destroy the registry or lose packages,
// and newly added packages must appear.
func TestReapplyIsIdempotent(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e")
	}
	h := newHarness(t, "e2e-reapply")
	npmPort, pyPort := freePort(t), freePort(t)
	h.registerHost(npmPort, pyPort)

	dir, manifestPath := buildFixture(t)
	first := filepath.Join(dir, "first.tar.gz")
	h.mustRun("bundle", "-f", manifestPath, "-o", first)
	h.mustRun("apply", "--host", h.hostName, "--bundle", first)
	assertNPMRegistryServes(t, npmPort, "ms", "2.1.3")

	// Extend the fixture with a second npm package, rebuild, redeploy.
	writeFile(t, filepath.Join(dir, "app", "package.json"), `{
  "name": "pkgdepot-e2e-app",
  "version": "1.0.0",
  "private": true,
  "dependencies": {
    "ms": "2.1.3",
    "is-number": "7.0.0"
  }
}
`)
	second := filepath.Join(dir, "second.tar.gz")
	h.mustRun("bundle", "-f", manifestPath, "-o", second)
	h.mustRun("apply", "--host", h.hostName, "--bundle", second)

	// Old package still served, new package now served.
	assertNPMRegistryServes(t, npmPort, "ms", "2.1.3")
	assertNPMRegistryServes(t, npmPort, "is-number", "7.0.0")

	verifyOut := h.mustRun("verify", "--host", h.hostName, "--bundle", second)
	if !strings.Contains(verifyOut, "校验通过") {
		t.Fatalf("second verify failed:\n%s", verifyOut)
	}
}

// TestEnvAndStatus covers the operator's day-two questions: how do I point a
// client at this, and is it actually running?
func TestEnvAndStatus(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e")
	}
	h := newHarness(t, "e2e-status")
	npmPort, pyPort := freePort(t), freePort(t)
	h.registerHost(npmPort, pyPort)

	dir, manifestPath := buildFixture(t)
	bundlePath := filepath.Join(dir, "status.tar.gz")
	h.mustRun("bundle", "-f", manifestPath, "-o", bundlePath)
	h.mustRun("apply", "--host", h.hostName, "--bundle", bundlePath)

	envOut := h.mustRun("env", "--host", h.hostName)
	for _, want := range []string{"NPM_CONFIG_REGISTRY", "PIP_INDEX_URL", itoa(npmPort), itoa(pyPort)} {
		if !strings.Contains(envOut, want) {
			t.Fatalf("env output missing %q:\n%s", want, envOut)
		}
	}

	statusOut := h.mustRun("status", "--host", h.hostName)
	if !strings.Contains(statusOut, itoa(npmPort)) {
		t.Fatalf("status does not report the npm registry:\n%s", statusOut)
	}
	// Status must reflect the real running containers, so assert the container
	// count the harness can see directly.
	psOut, err := execOutput("docker", "ps", "--filter", "name=pkgdepot", "--format", "{{.Names}}")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(psOut) == "" {
		t.Fatalf("no pkgdepot containers running after apply")
	}

	// Uninstall stops the registries but preserves data.
	h.mustRun("uninstall", "--host", h.hostName)
	if _, err := os.Stat(filepath.Join(h.baseDir, "npm", "storage")); err != nil {
		t.Fatalf("uninstall removed package data: %v", err)
	}
}

// --- assertions -----------------------------------------------------------

func assertNPMRegistryServes(t *testing.T, port int, pkg, version string) {
	t.Helper()
	raw, err := httpGet("http://127.0.0.1:" + itoa(port) + "/" + pkg)
	if err != nil {
		t.Fatalf("GET packument %s: %v", pkg, err)
	}
	var doc struct {
		Versions map[string]struct {
			Dist struct {
				Tarball string `json:"tarball"`
			} `json:"dist"`
		} `json:"versions"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse packument %s: %v\n%s", pkg, err, raw)
	}
	if _, ok := doc.Versions[version]; !ok {
		t.Fatalf("%s@%s not present in registry (have %v)", pkg, version, keysOf(doc.Versions))
	}
	if doc.Versions[version].Dist.Tarball == "" {
		t.Fatalf("%s@%s packument has no dist.tarball; npm clients reject this", pkg, version)
	}
}

func assertPyPIRegistryServes(t *testing.T, port int, pkg, version string) {
	t.Helper()
	raw, err := httpGet("http://127.0.0.1:" + itoa(port) + "/simple/" + pkg + "/")
	if err != nil {
		t.Fatalf("GET simple index %s: %v", pkg, err)
	}
	body := string(raw)
	if !strings.Contains(body, pkg) || !strings.Contains(body, version) {
		t.Fatalf("simple index for %s does not list %s:\n%s", pkg, version, body)
	}
}

func keysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func itoa(v int) string {
	return strconvItoa(v)
}
