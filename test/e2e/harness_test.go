//go:build e2e

// Package e2e exercises pkgdepot against a real sshd, a real docker daemon and
// real offline consumers. It is the repository's only functional acceptance
// layer: a case must prove a user-visible outcome (a package that installs, a
// verification that fails on tampered bytes), not that a process stayed up.
//
// The build tag keeps these multi-minute, network-dependent cases out of the
// default `go test ./...` run. Run them explicitly:
//
//	go test ./test/e2e/ -tags e2e -v -timeout 45m
package e2e

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// harness is the shared, per-test-run environment: one sshd, one binary, one
// isolated docker network. Each case gets its own base dir on the "host" so the
// cases cannot accelerate each other through leftover state.
type harness struct {
	t        *testing.T
	root     string // scratch dir for keys, configs, bundles
	bin      string // built pkgdepot binary
	sshdPort int
	keyPath  string
	hostName string
	network  string
	baseDir  string
	cleanup  []func()
}

// newHarness builds the binary, starts an isolated sshd, and creates an
// --internal docker network that can reach only the test registries. The
// internal network is what makes the offline claim real: containers on it have
// no route to the internet.
func newHarness(t *testing.T, hostName string) *harness {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}
	if _, err := os.Stat("/usr/sbin/sshd"); err != nil {
		t.Skip("sshd not available")
	}

	h := &harness{
		t:        t,
		root:     t.TempDir(),
		hostName: hostName,
		network:  "pkgdepot-e2e-" + sanitize(t.Name()),
		baseDir:  filepath.Join(t.TempDir(), "base"),
	}
	t.Cleanup(h.teardown)

	h.bin = h.buildBinary()
	h.keyPath = h.startSSHD()
	h.makeNetwork()

	// Point pkgdepot at a scratch config dir so cases never touch the
	// operator's real ~/.config/pkgdepot/hosts.yaml.
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(h.root, "config"))
	return h
}

func sanitize(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

func (h *harness) cleanupFn(fn func()) { h.cleanup = append(h.cleanup, fn) }

func (h *harness) teardown() {
	for i := len(h.cleanup) - 1; i >= 0; i-- {
		h.cleanup[i]()
	}
}

// buildBinary compiles the CLI the way a user would obtain it, so the cases also
// cover the actual command surface and exit codes.
func (h *harness) buildBinary() string {
	h.t.Helper()
	out := filepath.Join(h.root, "pkgdepot")
	cmd := exec.Command("go", "build", "-trimpath", "-o", out, "./cmd/pkgdepot")
	cmd.Dir = repoRoot(h.t)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if b, err := cmd.CombinedOutput(); err != nil {
		h.t.Fatalf("build pkgdepot: %v\n%s", err, b)
	}
	return out
}

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	// The test runs in <repo>/test/e2e.
	return filepath.Clean(filepath.Join(wd, "..", ".."))
}

// startSSHD runs a private sshd on a free loopback port with its own host key
// and authorized key, so the case needs no external SSH setup and cannot be
// affected by (or affect) the machine's real sshd configuration.
func (h *harness) startSSHD() string {
	h.t.Helper()
	dir := filepath.Join(h.root, "sshd")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		h.t.Fatal(err)
	}

	hostKey := filepath.Join(dir, "host_ed25519")
	userKey := filepath.Join(dir, "user_ed25519")
	mustRun(h.t, "ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", hostKey)
	mustRun(h.t, "ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", userKey)
	pub, err := os.ReadFile(userKey + ".pub")
	if err != nil {
		h.t.Fatal(err)
	}
	authKeys := filepath.Join(dir, "authorized_keys")
	if err := os.WriteFile(authKeys, pub, 0o600); err != nil {
		h.t.Fatal(err)
	}

	port := freePort(h.t)
	cfg := filepath.Join(dir, "sshd_config")
	conf := fmt.Sprintf(`Port %d
ListenAddress 127.0.0.1
HostKey %s
PidFile %s
AuthorizedKeysFile %s
PasswordAuthentication no
PermitRootLogin prohibit-password
StrictModes no
UsePAM no
LogLevel ERROR
`, port, hostKey, filepath.Join(dir, "sshd.pid"), authKeys)
	if err := os.WriteFile(cfg, []byte(conf), 0o600); err != nil {
		h.t.Fatal(err)
	}
	cmd := exec.Command("/usr/sbin/sshd", "-f", cfg, "-E", filepath.Join(dir, "sshd.log"))
	if b, err := cmd.CombinedOutput(); err != nil {
		h.t.Fatalf("start sshd: %v\n%s", err, b)
	}
	h.sshdPort = port
	h.cleanupFn(func() {
		if pidRaw, err := os.ReadFile(filepath.Join(dir, "sshd.pid")); err == nil {
			var pid int
			fmt.Sscanf(string(pidRaw), "%d", &pid)
			if pid > 0 {
				_ = exec.Command("kill", fmt.Sprint(pid)).Run()
			}
		}
	})

	// Wait until the port accepts a connection.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 500*time.Millisecond)
		if err == nil {
			c.Close()
			return userKey
		}
		time.Sleep(100 * time.Millisecond)
	}
	h.t.Fatalf("sshd on port %d did not start", port)
	return ""
}

func (h *harness) makeNetwork() {
	h.t.Helper()
	// --internal: no gateway to the outside world. Registries join it after
	// apply, so consumers on this network can reach only the private hosts.
	// A leftover network from an interrupted run is removed first so a retry is
	// not blocked by state the previous attempt left behind.
	_ = exec.Command("docker", "network", "rm", h.network).Run()
	if b, err := exec.Command("docker", "network", "create", "--internal", h.network).CombinedOutput(); err != nil {
		h.t.Fatalf("create internal network: %v\n%s", err, b)
	}
	h.cleanupFn(func() { _ = exec.Command("docker", "network", "rm", h.network).Run() })
}

// run invokes the real binary and returns combined output plus the exit code.
func (h *harness) run(args ...string) (string, int) {
	h.t.Helper()
	cmd := exec.Command(h.bin, args...)
	cmd.Env = append(os.Environ(), "XDG_CONFIG_HOME="+filepath.Join(h.root, "config"))
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		var ee *exec.ExitError
		if ok := asExitError(err, &ee); ok {
			code = ee.ExitCode()
		} else {
			h.t.Fatalf("run pkgdepot %v: %v", args, err)
		}
	}
	h.t.Logf("$ pkgdepot %s\n=> exit %d\n%s", strings.Join(args, " "), code, out)
	return string(out), code
}

// mustRun invokes the binary and fails the case on a non-zero exit.
func (h *harness) mustRun(args ...string) string {
	h.t.Helper()
	out, code := h.run(args...)
	if code != 0 {
		h.t.Fatalf("pkgdepot %s exited %d", strings.Join(args, " "), code)
	}
	return out
}

func asExitError(err error, target **exec.ExitError) bool {
	if ee, ok := err.(*exec.ExitError); ok {
		*target = ee
		return true
	}
	return false
}

func mustRun(t *testing.T, name string, args ...string) {
	t.Helper()
	if b, err := exec.Command(name, args...).CombinedOutput(); err != nil {
		t.Fatalf("%s: %v\n%s", name, err, b)
	}
}

// freePort asks the kernel for an unused loopback port. The port could still be
// taken between probe and bind, but the test runs on a quiet CI host; retrying
// at the call site would add complexity for a risk with no observed failure.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// registerHost adds the test host via the real CLI so the case also covers the
// hosts.yaml contract.
func (h *harness) registerHost(npmPort, pyPort int) {
	h.mustRun("host", "add", h.hostName,
		"--address", "127.0.0.1",
		"--user", "root",
		"--port", fmt.Sprint(h.sshdPort),
		"--key", h.keyPath,
		"--base-dir", h.baseDir,
		"--npm-port", fmt.Sprint(npmPort),
		"--python-port", fmt.Sprint(pyPort),
	)
}

// offlineContainer runs a command inside a container attached only to the
// --internal network, which is the real proof that the private registry — and
// nothing else — served the install.
func (h *harness) offlineContainer(t *testing.T, image string, script string) string {
	t.Helper()
	cmd := exec.Command("docker", "run", "--rm", "--network", h.network, "--entrypoint", "sh", image, "-c", script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("offline container (%s): %v\n%s", image, err, out)
	}
	return string(out)
}

// joinNetwork attaches the deployed registries to the internal-only network so
// offline consumers can reach them.
//
// The registries are discovered by the ports pkgdepot published, not by
// container name: names come from the compose project directory, which is a
// pkgdepot implementation detail the test must not depend on.
func (h *harness) joinNetwork(t *testing.T, npmPort, pyPort int) {
	t.Helper()
	for _, want := range []int{npmPort, pyPort} {
		id := h.containerPublishing(t, want)
		if b, err := exec.Command("docker", "network", "connect", h.network, id).CombinedOutput(); err != nil {
			if !strings.Contains(string(b), "already exists") {
				t.Fatalf("attach container %s (port %d) to %s: %v\n%s", id, want, h.network, err, b)
			}
		}
	}
}

// containerPublishing finds the container whose published HOST port equals port.
// Docker renders port mappings as "<host-ip>:<host-port>-><container-port>/tcp",
// so the host port is the segment immediately before "->".
func (h *harness) containerPublishing(t *testing.T, port int) string {
	t.Helper()
	out, err := execOutput("docker", "ps", "--format", "{{.ID}}\t{{.Ports}}")
	if err != nil {
		t.Fatal(err)
	}
	needle := strconvItoa(port) + "->"
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if !strings.Contains(line, needle) {
			continue
		}
		id, _, _ := strings.Cut(line, "\t")
		return strings.TrimSpace(id)
	}
	t.Fatalf("no container publishes host port %d; deploy did not expose the registry", port)
	return ""
}

// registryContainerName resolves the container name publishing the given host
// port, so an offline consumer can reach it by name on the internal network
// (127.0.0.1 inside a sibling container is the consumer itself).
func (h *harness) registryContainerName(t *testing.T, port int) string {
	t.Helper()
	id := h.containerPublishing(t, port)
	name, err := execOutput("docker", "inspect", "--format", "{{.Name}}", id)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimPrefix(strings.TrimSpace(name), "/")
}

// writeFile is a small helper to keep case bodies readable.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
