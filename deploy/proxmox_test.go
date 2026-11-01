package deploy_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func bashOrSkip(t *testing.T) string {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is not installed on this box: skipping the Proxmox helper checks")
	}
	return bash
}

func writeStub(t *testing.T, dir, bash, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!"+bash+"\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestTheProxmoxHelperIsValidBash(t *testing.T) {
	bash := bashOrSkip(t)
	out, err := exec.CommandContext(t.Context(), bash, "-n", "proxmox-lxc.sh").CombinedOutput()
	if err != nil {
		t.Fatalf("bash -n proxmox-lxc.sh: %v\n%s", err, out)
	}
}

func TestTheProxmoxHelperRefusesAnythingButAProxmoxHost(t *testing.T) {
	bash := bashOrSkip(t)
	// An empty directory as the whole PATH: no pct, so the first guard fires.
	cmd := exec.CommandContext(t.Context(), bash, "proxmox-lxc.sh", "--domain=chat.example.org", "--public-ip=192.0.2.10", "--agree-tos")
	cmd.Env = []string{"PATH=" + t.TempDir()}
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err == nil {
		t.Fatal("the helper ran to completion without pct on PATH")
	}
	if !strings.Contains(stderr.String(), "run this on a Proxmox host as root") {
		t.Errorf("stderr = %q, want the Proxmox-host refusal", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want nothing", stdout.String())
	}
}

func TestTheProxmoxHelperRefusesAnExistingContainer(t *testing.T) {
	bash := bashOrSkip(t)
	bin := t.TempDir()
	writeStub(t, bin, bash, "id", "echo 0")
	writeStub(t, bin, bash, "pct", "exit 0") // `pct status 101` succeeds: the CTID is taken
	cmd := exec.CommandContext(t.Context(), bash, "proxmox-lxc.sh", "--ctid=101", "--domain=chat.example.org", "--public-ip=192.0.2.10", "--agree-tos", "--version=v0.1.0")
	cmd.Env = []string{"PATH=" + bin}
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err == nil {
		t.Fatal("the helper went ahead with a CTID that is already used")
	}
	if !strings.Contains(stderr.String(), "already exists") {
		t.Errorf("stderr = %q, want the already-exists refusal", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want nothing", stdout.String())
	}
}

// The helper's contract with its caller: stdout is the bootstrap invite link and
// nothing else, however chatty pct, pveam and dillad init are. The stubs print
// to stdout on every call, so any command the script forgot to send to stderr
// shows up here.
func TestTheProxmoxHelperPrintsOnlyTheInviteLinkOnStdout(t *testing.T) {
	bash := bashOrSkip(t)
	bin := t.TempDir()
	writeStub(t, bin, bash, "id", "echo 0")
	writeStub(t, bin, bash, "pvesh", "echo 4242")
	writeStub(t, bin, bash, "pveam", `
case "$1" in
available) echo "system          debian-13-standard_13.1-2_amd64.tar.zst" ;;
list) echo "local:vztmpl/debian-13-standard_13.1-2_amd64.tar.zst" ;;
*) echo "pveam noise" ;;
esac`)
	calls := filepath.Join(t.TempDir(), "pct.log")
	writeStub(t, bin, bash, "pct", `
echo "pct $*" >>`+calls+`
echo "pct noise: $*"
case "$1" in
status) exit 2 ;;
exec)
	case "$*" in
	*"/root/dilla-install/install.sh"*) echo "install: progress" >&2; printf 'https://chat.example.org/i/SECRETCODE\n' ;;
	esac ;;
esac`)
	for _, name := range []string{"mktemp", "uname", "dirname", "awk", "sort", "tail", "grep", "sed", "head", "rm", "cat"} {
		path, err := exec.LookPath(name)
		if err != nil {
			t.Skipf("%s is not installed on this box: skipping the helper run", name)
		}
		if err := os.Symlink(path, filepath.Join(bin, name)); err != nil {
			t.Fatal(err)
		}
	}
	work := t.TempDir()
	binary, core := filepath.Join(work, "dillad"), filepath.Join(work, "core.wasm")
	for _, f := range []string{binary, core} {
		if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.CommandContext(t.Context(), bash, "proxmox-lxc.sh", "--domain=chat.example.org", "--public-ip=192.0.2.10",
		"--agree-tos", "--insecure-skip-verify", "--binary="+binary, "--core="+core)
	cmd.Env = []string{"PATH=" + bin}
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("the helper failed: %v\nstderr:\n%s", err, stderr.String())
	}
	if got, want := stdout.String(), "https://chat.example.org/i/SECRETCODE\n"; got != want {
		t.Fatalf("stdout = %q, want exactly %q", got, want)
	}
	if strings.Contains(stderr.String(), "SECRETCODE") {
		t.Fatalf("the invite code reached stderr, which is logged:\n%s", stderr.String())
	}
	log, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"pct push 4242 " + filepath.Join(mustAbs(t, "."), "install.sh") + " /root/dilla-install/install.sh",
		"pct exec 4242 -- sh /root/dilla-install/install.sh --public --domain=chat.example.org --public-ip=192.0.2.10 --agree-tos --insecure-skip-verify --binary=/root/dilla-install/dillad --core=/root/dilla-install/dilla_core_wasi.wasm",
	} {
		if !strings.Contains(string(log), want) {
			t.Errorf("pct was never called as %q; calls:\n%s", want, log)
		}
	}
}

func mustAbs(t *testing.T, p string) string {
	t.Helper()
	abs, err := filepath.Abs(p)
	if err != nil {
		t.Fatal(err)
	}
	return abs
}

// The helper hands the mode's arguments to install.sh --check-args before it
// creates anything, so a bad behind-proxy invocation fails on the host.
func TestTheProxmoxHelperValidatesTheInstallArgumentsBeforeCreatingAContainer(t *testing.T) {
	bash := bashOrSkip(t)
	bin := t.TempDir()
	writeStub(t, bin, bash, "id", "echo 0")
	writeStub(t, bin, bash, "pct", "echo PCT-CALLED >&2; exit 0")
	for _, name := range []string{"dirname", "cat"} {
		path, err := exec.LookPath(name)
		if err != nil {
			t.Skipf("%s is not installed on this box", name)
		}
		if err := os.Symlink(path, filepath.Join(bin, name)); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.CommandContext(t.Context(), bash, "proxmox-lxc.sh", "--ctid=101", "--behind-proxy", "--domain=chat.example.org", "--listen=0.0.0.0:8080")
	cmd.Env = []string{"PATH=" + bin}
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err == nil {
		t.Fatal("the helper accepted --behind-proxy without --trusted-proxy")
	}
	if !strings.Contains(stderr.String(), "--trusted-proxy") || strings.Contains(stderr.String(), "PCT-CALLED") {
		t.Errorf("stderr = %q, want install.sh's --trusted-proxy refusal and no pct call", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want nothing", stdout.String())
	}
}
