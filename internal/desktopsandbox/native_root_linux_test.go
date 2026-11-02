//go:build linux && dilla_sandbox_native

package desktopsandbox

// CI ONLY: this opt-in binary has a literal disposable namespace. No production
// API accepts this layout, test key, actor role or fault hook.
import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

const nativeRoot = "/usr/lib/dilla-sandbox-maintenance-ci"
const nobodyID = 65534

var nativeRunID, nativeAttempt, nativeHead, nativePublished, nativeExpires, nativeHelper1Hash, nativeHelper2Hash string
var nativeNames = []string{"usr", "lib", "dilla-sandbox-maintenance-ci"}

func nativeLayout() fileLayout {
	return fileLayout{anchor: "/", parts: append(append([]string{}, nativeNames...), "appimage"), uid: 0, gid: 0, helperMode: 0o4755}
}
func nativeFail(t *testing.T, e error) {
	t.Helper()
	if e != nil {
		t.Fatal(e)
	}
}
func nativeCommon(t *testing.T) int {
	t.Helper()
	if unix.Geteuid() != 0 || nativeRunID == "" || nativeAttempt == "" || len(nativeHead) != 40 {
		t.Fatal("unconfigured disposable root fixture")
	}
	fd, e := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	nativeFail(t, e)
	f := &fixedFiles{layout: appImageLayout()}
	nativeFail(t, f.captureMount(fd))
	nativeFail(t, f.safeDir(fd, 0o755, false))
	for _, part := range nativeNames {
		next, e := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if e != nil {
			_ = unix.Close(fd)
			t.Fatal(e)
		}
		nativeFail(t, f.safeDir(next, 0o755, false))
		nativeFail(t, f.sameDevice(next))
		_ = unix.Close(fd)
		fd = next
	}
	return fd
}
func nativeRead(t *testing.T, parent int, name string, limit int64, modes ...uint32) ([]byte, unix.Stat_t) {
	t.Helper()
	fd, e := unix.Openat(parent, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	nativeFail(t, e)
	defer func() { _ = unix.Close(fd) }()
	var s unix.Stat_t
	nativeFail(t, unix.Fstat(fd, &s))
	f := &fixedFiles{layout: appImageLayout()}
	nativeFail(t, f.captureMount(parent))
	if !f.safeInfo(s, limit, modes...) {
		t.Fatal("unsafe fixed native input")
	}
	nativeFail(t, f.sameDevice(fd))
	nativeFail(t, f.safeAttributes(fd))
	raw := make([]byte, s.Size)
	for off := 0; off < len(raw); {
		n, e := unix.Pread(fd, raw[off:], int64(off))
		nativeFail(t, e)
		if n == 0 {
			t.Fatal(io.ErrUnexpectedEOF)
		}
		off += n
	}
	var after unix.Stat_t
	nativeFail(t, unix.Fstat(fd, &after))
	if !sameStat(s, after) {
		t.Fatal("native input changed")
	}
	return raw, s
}

type nativeOwner struct {
	Format       int    `json:"format"`
	RunID        string `json:"runId"`
	Attempt      string `json:"attempt"`
	SHA          string `json:"sha"`
	Nonce        string `json:"nonce"`
	Device       string `json:"device"`
	Inode        string `json:"inode"`
	Phase        string `json:"phase"`
	NativeSHA512 string `json:"nativeSha512"`
}

func nativeOwnership(t *testing.T, common int) nativeOwner {
	t.Helper()
	raw, _ := nativeRead(t, common, ".ci-owner.json", 4096, 0o600)
	_, e := exactObject(raw, "format", "runId", "attempt", "sha", "nonce", "device", "inode", "phase", "nativeSha512")
	nativeFail(t, e)
	nativeFail(t, validateJSON(raw))
	var owner nativeOwner
	nativeFail(t, json.Unmarshal(raw, &owner))
	var s unix.Stat_t
	nativeFail(t, unix.Fstat(common, &s))
	if owner.Format != 1 || owner.RunID != nativeRunID || owner.Attempt != nativeAttempt || owner.SHA != nativeHead || len(owner.Nonce) != 64 || owner.Device != strconv.FormatUint(s.Dev, 10) || owner.Inode != strconv.FormatUint(s.Ino, 10) || owner.Phase != "verified" || !validDigest(owner.NativeSHA512) {
		t.Fatal("foreign or unverified CI ownership")
	}
	nonce, e := hex.DecodeString(owner.Nonce)
	nativeFail(t, e)
	if hex.EncodeToString(nonce) != owner.Nonce {
		t.Fatal("invalid protected nonce")
	}
	runner, e := unix.Openat(common, ".runner", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	nativeFail(t, e)
	defer func() { _ = unix.Close(runner) }()
	f := &fixedFiles{layout: appImageLayout()}
	nativeFail(t, f.safeDir(runner, 0o700, true))
	nativeFail(t, f.captureMount(common))
	nativeFail(t, f.sameDevice(runner))
	raw, _ = nativeRead(t, runner, "native.test", MaxMaintainer, 0o700)
	if hashBytes(raw) != owner.NativeSHA512 {
		t.Fatal("verified native ELF changed")
	}
	nativeFail(t, verifyELF(raw))
	return owner
}
func nativeInputs(t *testing.T) (Capsule, Capsule, Trust) {
	t.Helper()
	common := nativeCommon(t)
	defer func() { _ = unix.Close(common) }()
	owner := nativeOwnership(t, common)
	runner, e := unix.Openat(common, ".runner", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	nativeFail(t, e)
	defer func() { _ = unix.Close(runner) }()
	manager, _ := nativeRead(t, runner, "native.test", MaxMaintainer, 0o700)
	one, _ := nativeRead(t, runner, "credential-v1", MaxHelper, 0o555)
	two, _ := nativeRead(t, runner, "credential-v2", MaxHelper, 0o555)
	if hashBytes(one) != nativeHelper1Hash || hashBytes(two) != nativeHelper2Hash || hashBytes(manager) != owner.NativeSHA512 {
		t.Fatal("compiled fixture input mismatch")
	}
	seed := sha256.Sum256([]byte("Dilla disposable Linux maintenance CI fixture key; never production"))
	key := ed25519.NewKeyFromSeed(seed[:])
	makeCapsule := func(helper []byte, generation int64) Capsule {
		m := Manifest{Format: 1, Kind: "dilla-linux-sandbox", Generation: generation, Platform: "linux", Arch: "x64", CompatibilityEpoch: 1, SandboxAPI: 1, PublishedAt: nativePublished, ExpiresAt: nativeExpires, Maintainer: Component{int64(len(manager)), hashBytes(manager)}, Helper: Component{int64(len(helper)), hashBytes(helper)}}
		raw, e := json.Marshal(m)
		nativeFail(t, e)
		sig := ed25519.Sign(key, append([]byte(signatureDomain), raw...))
		return Capsule{raw, sig, manager, helper}
	}
	initial := makeCapsule(one, 1)
	initial.Signature = nil
	next := makeCapsule(two, 2)
	trust := Trust{PublicKeys: []ed25519.PublicKey{key.Public().(ed25519.PublicKey)}, Initial: &InitialTrust{Generation: 1, CompatibilityEpoch: 1, SandboxAPI: 1, PublishedAt: nativePublished, ExpiresAt: nativeExpires, Helper: Component{int64(len(one)), hashBytes(one)}, SelfMaintainerSHA512: hashBytes(manager)}}
	return initial, next, trust
}
func nativeManager(t *testing.T) *Maintainer {
	t.Helper()
	_, _, trust := nativeInputs(t)
	f, e := openFixedFiles(nativeLayout(), true)
	nativeFail(t, e)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	nativeFail(t, f.lock(ctx, false))
	return &Maintainer{files: f, trust: trust, now: time.Now}
}
func nativeSeed(t *testing.T, recoverNow bool) *Maintainer {
	t.Helper()
	common := nativeCommon(t)
	defer func() { _ = unix.Close(common) }()
	nativeOwnership(t, common)
	nativeFail(t, unix.Mkdirat(common, "appimage", 0o755))
	nativeFail(t, unix.Mkdirat(common, "native", 0o755))
	nativeDir, e := unix.Openat(common, "native", unix.O_DIRECTORY|unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	nativeFail(t, e)
	fd, e := unix.Openat(nativeDir, "sentinel", unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o644)
	nativeFail(t, e)
	_, e = unix.Write(fd, []byte("native fixture untouched\n"))
	nativeFail(t, e)
	nativeFail(t, unix.Fsync(fd))
	nativeFail(t, unix.Close(fd))
	nativeFail(t, unix.Fsync(nativeDir))
	nativeFail(t, unix.Close(nativeDir))
	nativeFail(t, unix.Fsync(common))
	initial, _, _ := nativeInputs(t)
	m := nativeManager(t)
	capFile, e := m.files.writeNew(stageDir, "bootstrap-capsule", frame(initial), 0o600)
	nativeFail(t, e)
	defer closePinned(capFile)
	manager, e := m.files.writeNew(stageDir, "bootstrap-maintainer", initial.Maintainer, 0o700)
	nativeFail(t, e)
	defer closePinned(manager)
	anchor := BootstrapAnchor{Format: 1, Kind: "dilla-linux-sandbox-bootstrap", Operation: "initial", Generation: 1, AcceptedAt: time.Now().UnixMilli(), ManifestSHA512: hashBytes(initial.Manifest), CompatibilityEpoch: 1, SandboxAPI: 1, Capsule: capFile.identity, Maintainer: manager.identity}
	raw, e := json.Marshal(anchor)
	nativeFail(t, e)
	p, e := m.files.writeNew(stageDir, "bootstrap-anchor.json", raw, 0o600)
	nativeFail(t, e)
	closePinned(p)
	nativeFail(t, m.files.sync(stageDir))
	if recoverNow {
		_, e = m.Recover(t.Context())
		nativeFail(t, e)
	}
	return m
}

type nativeHolder struct {
	cmd           *exec.Cmd
	input         io.WriteCloser
	output        *os.File
	reader        *bufio.Reader
	device, inode string
}

func nativeLine(t *testing.T, file *os.File, reader *bufio.Reader) string {
	t.Helper()
	nativeFail(t, file.SetReadDeadline(time.Now().Add(2*time.Second)))
	line, e := reader.ReadSlice('\n')
	nativeFail(t, e)
	if len(line) > 256 {
		t.Fatal("oversized credential response")
	}
	return strings.TrimSpace(string(line))
}
func nativeStartHolder(t *testing.T) *nativeHolder {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), nativeRoot+"/observer")
	cmd.Env = []string{"LANG=C", "LC_ALL=C"}
	cmd.Dir = "/"
	cmd.SysProcAttr = &unixSysProcAttrNobody
	input, e := cmd.StdinPipe()
	nativeFail(t, e)
	out, e := cmd.StdoutPipe()
	nativeFail(t, e)
	file, ok := out.(*os.File)
	if !ok {
		t.Fatal("expected owned pipe")
	}
	h := &nativeHolder{cmd: cmd, input: input, output: file, reader: bufio.NewReaderSize(file, 512)}
	nativeFail(t, cmd.Start())
	t.Cleanup(func() { _ = input.Close(); _ = cmd.Process.Kill(); _ = cmd.Wait() })
	fields := strings.Fields(nativeLine(t, file, h.reader))
	if len(fields) != 11 || fields[0] != "HELD" || strings.Join(fields[3:], " ") != "65534 65534 65534 65534 65534 65534 0 0" {
		t.Fatal("unprivileged holder credentials invalid")
	}
	h.device, h.inode = fields[1], fields[2]
	var info unix.Stat_t
	nativeFail(t, unix.Stat(nativeRoot+"/appimage/chrome-sandbox", &info))
	if h.device != strconv.FormatUint(info.Dev, 10) || h.inode != strconv.FormatUint(info.Ino, 10) {
		t.Fatal("holder opened other inode")
	}
	return h
}

// Empty supplementary groups, no no-new-privileges or tracing: positive SUID
// control must genuinely produce EUID0 before accepting negative revocation.
var unixSysProcAttrNobody = syscall.SysProcAttr{Credential: &syscall.Credential{Uid: nobodyID, Gid: nobodyID, Groups: []uint32{}}}

func nativeCredential(t *testing.T, line string, want int, generation string) int {
	t.Helper()
	parts := strings.Fields(line)
	if len(parts) != 9 || parts[0] != "CRED" || parts[1] != "65534" || parts[2] != strconv.Itoa(want) || parts[3] != strconv.Itoa(want) || parts[4] != "65534" || parts[5] != "65534" || parts[6] != "65534" || parts[7] != "0" || parts[8] != generation {
		t.Fatal("credential execution did not match expected kernel state")
	}
	return want
}
func (h *nativeHolder) execute(t *testing.T, want int, generation string) int {
	t.Helper()
	_, e := h.input.Write([]byte{'E'})
	nativeFail(t, e)
	return nativeCredential(t, nativeLine(t, h.output, h.reader), want, generation)
}
func nativeFresh(t *testing.T) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, nativeRoot+"/appimage/chrome-sandbox")
	cmd.Env = []string{"LANG=C", "LC_ALL=C"}
	cmd.Dir = "/"
	cmd.SysProcAttr = &unixSysProcAttrNobody
	raw, e := cmd.Output()
	nativeFail(t, e)
	return nativeCredential(t, strings.TrimSpace(string(raw)), 0, "2")
}

func (h *nativeHolder) close(t *testing.T) {
	t.Helper()
	if h.cmd.ProcessState != nil {
		return
	}
	_ = h.input.Close()
	done := make(chan error, 1)
	go func() { done <- h.cmd.Wait() }()
	select {
	case e := <-done:
		nativeFail(t, e)
	case <-time.After(2 * time.Second):
		_ = h.cmd.Process.Kill()
		<-done
		t.Fatal("owned holder cleanup timed out")
	}
}
func nativeEntries(t *testing.T, fd int, allowed map[string]bool) []string {
	t.Helper()
	copyFD, e := unix.Openat(fd, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	nativeFail(t, e)
	file := os.NewFile(uintptr(copyFD), "fixed native fixture directory")
	defer func() { _ = file.Close() }()
	names, e := file.Readdirnames(17)
	if e != nil && !errors.Is(e, io.EOF) {
		t.Fatal(e)
	}
	if len(names) > 16 {
		t.Fatal("oversized native namespace")
	}
	for _, name := range names {
		if !allowed[name] {
			t.Fatal("unexpected native namespace child")
		}
	}
	return names
}
func nativeRemoveFile(t *testing.T, parent int, name string, limit int64) {
	t.Helper()
	raw, s := nativeRead(t, parent, name, limit, 0o600, 0o644, 0o700, 0o755, 0o555, 0o4755)
	_ = raw
	fd, e := unix.Openat(parent, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	nativeFail(t, e)
	defer func() { _ = unix.Close(fd) }()
	var current unix.Stat_t
	nativeFail(t, unix.Fstat(fd, &current))
	if !sameStat(s, current) {
		t.Fatal("cleanup identity changed")
	}
	if s.Mode&0o6000 != 0 {
		if name != "chrome-sandbox" && name != "helper.next" {
			t.Fatal("unexpected privileged fixture leaf")
		}
		nativeFail(t, unix.Fchmod(fd, 0o755))
		nativeFail(t, unix.Fsync(fd))
		nativeFail(t, unix.Fstat(fd, &s))
	}
	nativeFail(t, unix.Fstatat(parent, name, &current, unix.AT_SYMLINK_NOFOLLOW))
	if !sameStat(s, current) {
		t.Fatal("cleanup late pathname change")
	}
	nativeFail(t, unix.Unlinkat(parent, name, 0))
	nativeFail(t, unix.Fsync(parent))
}
func nativeRemoveDir(t *testing.T, parent int, name string, mode uint32, files map[string]bool) {
	t.Helper()
	fd, e := unix.Openat(parent, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if errors.Is(e, unix.ENOENT) {
		return
	}
	nativeFail(t, e)
	defer func() { _ = unix.Close(fd) }()
	f := &fixedFiles{layout: appImageLayout()}
	nativeFail(t, f.safeDir(fd, mode, mode == 0o700))
	nativeFail(t, f.captureMount(parent))
	nativeFail(t, f.sameDevice(fd))
	for _, child := range nativeEntries(t, fd, files) {
		nativeRemoveFile(t, fd, child, MaxCapsule)
	}
	var held, named unix.Stat_t
	nativeFail(t, unix.Fstat(fd, &held))
	nativeFail(t, unix.Fstatat(parent, name, &named, unix.AT_SYMLINK_NOFOLLOW))
	if held.Dev != named.Dev || held.Ino != named.Ino {
		t.Fatal("cleanup directory identity changed")
	}
	nativeFail(t, unix.Unlinkat(parent, name, unix.AT_REMOVEDIR))
	nativeFail(t, unix.Fsync(parent))
}
func nativeAssertSentinel(t *testing.T) {
	t.Helper()
	common := nativeCommon(t)
	defer func() { _ = unix.Close(common) }()
	fd, e := unix.Openat(common, "native", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	nativeFail(t, e)
	defer func() { _ = unix.Close(fd) }()
	f := &fixedFiles{layout: appImageLayout()}
	nativeFail(t, f.safeDir(fd, 0o755, false))
	nativeFail(t, f.captureMount(common))
	nativeFail(t, f.sameDevice(fd))
	raw, _ := nativeRead(t, fd, "sentinel", 128, 0o644)
	if string(raw) != "native fixture untouched\n" {
		t.Fatal("native sibling was modified")
	}
}
func nativeCaseCleanup(t *testing.T) {
	t.Helper()
	common := nativeCommon(t)
	defer func() { _ = unix.Close(common) }()
	nativeOwnership(t, common)
	var probe unix.Stat_t
	e := unix.Fstatat(common, "appimage", &probe, unix.AT_SYMLINK_NOFOLLOW)
	if !errors.Is(e, unix.ENOENT) {
		nativeFail(t, e)
		fd, e := unix.Openat(common, "appimage", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		nativeFail(t, e)
		f := &fixedFiles{layout: appImageLayout()}
		nativeFail(t, f.safeDir(fd, 0o755, false))
		nativeFail(t, f.captureMount(common))
		nativeFail(t, f.sameDevice(fd))
		allowed := map[string]bool{".sandbox-staging": true, ".sandbox-lock": true, "sandbox-maintainer": true, "chrome-sandbox": true, "sandbox-receipt.json": true, "sandbox-transaction.json": true}
		names := nativeEntries(t, fd, allowed)
		nativeRemoveDir(t, fd, ".sandbox-staging", 0o700, map[string]bool{"bootstrap-anchor.json": true, "bootstrap-anchor.next": true, "bootstrap-capsule": true, "bootstrap-maintainer": true, "maintainer.next": true, "helper.next": true, "receipt.next": true, "intent.next": true})
		for _, name := range names {
			if name != ".sandbox-staging" {
				nativeRemoveFile(t, fd, name, MaxCapsule)
			}
		}
		nativeFail(t, unix.Fsync(fd))
		nativeFail(t, unix.Close(fd))
		nativeFail(t, unix.Unlinkat(common, "appimage", unix.AT_REMOVEDIR))
		nativeFail(t, unix.Fsync(common))
	}
	e = unix.Fstatat(common, "native", &probe, unix.AT_SYMLINK_NOFOLLOW)
	if !errors.Is(e, unix.ENOENT) {
		nativeFail(t, e)
		fd, e := unix.Openat(common, "native", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		nativeFail(t, e)
		raw, _ := nativeRead(t, fd, "sentinel", 128, 0o644)
		if string(raw) != "native fixture untouched\n" {
			t.Fatal("native sibling changed")
		}
		nativeFail(t, unix.Close(fd))
		nativeRemoveDir(t, common, "native", 0o755, map[string]bool{"sentinel": true})
	}
}
func nativeProductionAbsent(t *testing.T) {
	t.Helper()
	for _, name := range []string{"/usr/lib/dilla/appimage", "/usr/lib/dilla/native"} {
		var s unix.Stat_t
		if e := unix.Lstat(name, &s); !errors.Is(e, unix.ENOENT) {
			t.Fatal("production namespace must remain absent")
		}
	}
}
func TestDisposableRootCleanup(t *testing.T) {
	common := nativeCommon(t)
	nativeOwnership(t, common)
	nativeFail(t, unix.Close(common))
	nativeProductionAbsent(t)
	nativeCaseCleanup(t)
	common = nativeCommon(t)
	defer func() { _ = unix.Close(common) }()
	nativeOwnership(t, common)
	nativeEntries(t, common, map[string]bool{".ci-owner.json": true, ".ci-owner.next": true, ".runner": true, "observer": true})
	nativeRemoveDir(t, common, ".runner", 0o700, map[string]bool{"native.test": true, "credential-v1": true, "credential-v2": true, "late-old": true})
	nativeRemoveFile(t, common, "observer", MaxHelper)
	var s unix.Stat_t
	if e := unix.Fstatat(common, ".ci-owner.next", &s, unix.AT_SYMLINK_NOFOLLOW); e == nil {
		nativeRemoveFile(t, common, ".ci-owner.next", 4096)
	} else if !errors.Is(e, unix.ENOENT) {
		t.Fatal(e)
	}
	nativeRemoveFile(t, common, ".ci-owner.json", 4096)
	if len(nativeEntries(t, common, map[string]bool{})) != 0 {
		t.Fatal("fixture root not empty")
	}
	parent, e := unix.Open("/usr/lib", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	nativeFail(t, e)
	defer func() { _ = unix.Close(parent) }()
	var held, named unix.Stat_t
	nativeFail(t, unix.Fstat(common, &held))
	nativeFail(t, unix.Fstatat(parent, "dilla-sandbox-maintenance-ci", &named, unix.AT_SYMLINK_NOFOLLOW))
	if held.Dev != named.Dev || held.Ino != named.Ino {
		t.Fatal("foreign cleanup root")
	}
	nativeFail(t, unix.Unlinkat(parent, "dilla-sandbox-maintenance-ci", unix.AT_REMOVEDIR))
	nativeFail(t, unix.Fsync(parent))
}

var nativeCrashPoints = map[string]string{"demoted": "sync-file:old-helper", "helper": "exchange:chrome-sandbox", "receipt": "exchange:sandbox-receipt.json", "remove": "unlink:root:chrome-sandbox", "initial": "publish:sandbox-transaction.json", "terminal": "unlink:root:sandbox-transaction.json"}

func TestDisposableRootActor(t *testing.T) {
	if len(os.Args) == 0 {
		t.Fatal("missing fixed actor role")
	}
	role := os.Args[len(os.Args)-1]
	point, ok := nativeCrashPoints[role]
	if !ok {
		t.Fatal("unknown fixed actor role")
	}
	m := nativeManager(t)
	defer func() { _ = m.Close() }()
	m.files.fault = func(op string, after bool) error {
		if op == point && after {
			fmt.Println("DILLA_NATIVE_CRASH_READY")
			for {
				time.Sleep(time.Second)
			}
		}
		return nil
	}
	var e error
	switch role {
	case "initial":
		_, e = m.Recover(t.Context())
	case "remove":
		e = m.Remove(t.Context())
	default:
		_, candidate, _ := nativeInputs(t)
		v, x := VerifyCapsule(candidate, m.trust, Receipt{}, time.Now())
		nativeFail(t, x)
		_, e = m.Install(t.Context(), v)
	}
	t.Fatal("fixed crash point not reached", e)
}
func nativeCrash(t *testing.T, role string) {
	t.Helper()
	binary := nativeRoot + "/appimage/sandbox-maintainer"
	if role == "initial" {
		binary = nativeRoot + "/.runner/native.test"
	}
	cmd := exec.CommandContext(t.Context(), binary, "-test.run=^TestDisposableRootActor$", "-test.timeout=30s", "--", role)
	cmd.Env = []string{"LANG=C", "LC_ALL=C"}
	cmd.Dir = "/"
	out, e := cmd.StdoutPipe()
	nativeFail(t, e)
	file, ok := out.(*os.File)
	if !ok {
		t.Fatal("expected owned actor pipe")
	}
	nativeFail(t, cmd.Start())
	defer func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()
	reader := bufio.NewReaderSize(file, 512)
	ready := false
	for i := 0; i < 8; i++ {
		if nativeLine(t, file, reader) == "DILLA_NATIVE_CRASH_READY" {
			ready = true
			break
		}
	}
	if !ready {
		t.Fatal("owned actor did not reach exact crash boundary")
	}
	nativeFail(t, cmd.Process.Kill())
	e = cmd.Wait()
	var exited *exec.ExitError
	if !errors.As(e, &exited) || !strings.Contains(e.Error(), "killed") {
		t.Fatal("owned actor did not die by SIGKILL")
	}
}
func nativeRecoveryMatrix(t *testing.T) int {
	t.Helper()
	completed := 0
	for _, role := range []string{"demoted", "helper", "receipt", "remove", "initial", "terminal"} {
		t.Run("crash-"+role, func(t *testing.T) {
			m := nativeSeed(t, role != "initial")
			var holder *nativeHolder
			if role != "initial" {
				holder = nativeStartHolder(t)
				holder.execute(t, 0, "1")
			}
			nativeFail(t, m.Close())
			nativeCrash(t, role)
			if holder != nil {
				holder.execute(t, nobodyID, "1")
			}
			m = nativeManager(t)
			r, e := m.Recover(t.Context())
			nativeFail(t, e)
			if role == "remove" {
				if r.State != "removed" || r.MaximumGeneration != 1 {
					t.Fatal("removal recovery lost floor")
				}
			} else {
				generation := int64(2)
				if role == "initial" {
					generation = 1
				}
				if r.State != "active" || r.MaximumGeneration != generation {
					t.Fatal("install recovery lost admitted generation")
				}
			}
			nativeFail(t, m.Close())
			if holder != nil {
				holder.execute(t, nobodyID, "1")
				holder.close(t)
			}
			nativeAssertSentinel(t)
			nativeCaseCleanup(t)
			completed++
		})
	}
	if completed != 6 {
		t.Fatal("kernel interruption matrix incomplete")
	}
	return completed
}
func nativeHostile(t *testing.T) (int, bool) {
	t.Helper()
	m := nativeSeed(t, true)
	nativeFail(t, m.Close())
	helper := nativeRoot + "/appimage/chrome-sandbox"
	stage := nativeRoot + "/appimage/.sandbox-staging"
	nativeFail(t, unix.Mount(helper, helper, "", unix.MS_BIND, ""))
	func() {
		defer func() { nativeFail(t, unix.Unmount(helper, 0)) }()
		f, e := openFixedFiles(nativeLayout(), true)
		nativeFail(t, e)
		defer func() { _ = f.close() }()
		if p, e := f.pin(rootDir, "chrome-sandbox", MaxHelper, 0o4755); !errors.Is(e, ErrState) {
			closePinned(p)
			t.Fatal("same-device bind mount did not reach identity rejection", e)
		}
	}()
	nativeFail(t, unix.Mount("tmpfs", stage, "tmpfs", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, "size=1048576,mode=0700"))
	func() {
		defer func() { nativeFail(t, unix.Unmount(stage, 0)) }()
		f, e := openFixedFiles(nativeLayout(), true)
		if !errors.Is(e, ErrState) {
			if f != nil {
				_ = f.close()
			}
			t.Fatal("cross-device staging did not reach identity rejection", e)
		}
	}()
	m = nativeManager(t)
	_, candidate, _ := nativeInputs(t)
	oldReceipt, e := m.files.read(rootDir, "sandbox-receipt.json", recordLimit, 0o644)
	nativeFail(t, e)
	// Move the already-demoted exact old inode into private .runner, replace
	// only its known public name with a different NON-SUID inode, then HOLD.
	common := nativeCommon(t)
	runner, e := unix.Openat(common, ".runner", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	nativeFail(t, e)
	m.files.fault = func(op string, after bool) error {
		if op == "exchange:chrome-sandbox" && !after {
			nativeFail(t, unix.Renameat(m.files.rootFD, "chrome-sandbox", runner, "late-old"))
			fd, e := unix.Openat(m.files.rootFD, "chrome-sandbox", unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o755)
			nativeFail(t, e)
			_, e = unix.Write(fd, []byte("substitute"))
			nativeFail(t, e)
			nativeFail(t, unix.Fsync(fd))
			nativeFail(t, unix.Close(fd))
		}
		return nil
	}
	v, e := VerifyCapsule(candidate, m.trust, Receipt{}, time.Now())
	nativeFail(t, e)
	if _, e = m.Install(t.Context(), v); !errors.Is(e, ErrState) {
		t.Fatal("late inode substitution did not HOLD")
	}
	current, e := m.files.read(rootDir, "sandbox-receipt.json", recordLimit, 0o644)
	nativeFail(t, e)
	if !bytes.Equal(oldReceipt, current) {
		t.Fatal("late inode substitution advanced receipt")
	}
	substitute, e := m.files.read(rootDir, "chrome-sandbox", 128, 0o755)
	nativeFail(t, e)
	if string(substitute) != "substitute" {
		t.Fatal("late guard deleted substitute")
	}
	nativeRemoveFile(t, runner, "late-old", MaxHelper)
	nativeFail(t, unix.Close(runner))
	nativeFail(t, unix.Close(common))
	nativeFail(t, m.Close())
	nativeAssertSentinel(t)
	nativeCaseCleanup(t)
	return 2, true
}
func TestDisposableRootTransaction(t *testing.T) {
	nativeProductionAbsent(t)
	common := nativeCommon(t)
	nativeOwnership(t, common)
	nativeFail(t, unix.Close(common))
	unix.Umask(0o022)
	m := nativeSeed(t, true)
	holder := nativeStartHolder(t)
	positive := holder.execute(t, 0, "1")
	demoted := -1
	m.files.fault = func(op string, after bool) error {
		if op == "sync-file:old-helper" && after {
			demoted = holder.execute(t, nobodyID, "1")
		}
		return nil
	}
	_, candidate, _ := nativeInputs(t)
	v, e := VerifyCapsule(candidate, m.trust, Receipt{}, time.Now())
	nativeFail(t, e)
	_, e = m.Install(t.Context(), v)
	nativeFail(t, e)
	exchanged := holder.execute(t, nobodyID, "1")
	fresh := nativeFresh(t)
	second := nativeStartHolder(t)
	second.execute(t, 0, "2")
	unlinked := -1
	m.files.fault = func(op string, after bool) error {
		if op == "unlink:root:chrome-sandbox" && after {
			unlinked = second.execute(t, nobodyID, "2")
		}
		return nil
	}
	nativeFail(t, m.Remove(t.Context()))
	second.execute(t, nobodyID, "2")
	holder.execute(t, nobodyID, "1")
	holder.close(t)
	second.close(t)
	nativeFail(t, m.Close())
	nativeAssertSentinel(t)
	nativeCaseCleanup(t)
	recoveries := nativeRecoveryMatrix(t)
	mounts, late := nativeHostile(t)
	nativeProductionAbsent(t)
	proof := map[string]any{"format": 1, "kind": "dilla-sandbox-maintenance-kernel", "positive": positive, "demoted": demoted, "exchanged": exchanged, "unlinked": unlinked, "fresh": fresh, "recoveries": recoveries, "mounts": mounts, "lateInode": late, "nativeUntouched": true, "productionAbsent": true}
	raw, e := json.Marshal(proof)
	nativeFail(t, e)
	fmt.Println("DILLA_SANDBOX_MAINTENANCE_PROOF " + string(raw))
}

// Safe unprivileged preflight: validates build-time configuration only; it never
// opens the CI root or invokes any maintenance, credential or mount operation.
func TestNativeCompiledConfiguration(t *testing.T) {
	if unix.Geteuid() == 0 {
		t.Fatal("configuration preflight must be unprivileged")
	}
	if nativeRunID == "" || nativeAttempt == "" || len(nativeHead) != 40 || !validDigest(nativeHelper1Hash) || !validDigest(nativeHelper2Hash) || nativeHelper1Hash == nativeHelper2Hash {
		t.Fatal("missing compile-distinct native configuration")
	}
	_, _, e := manifestTimes(Manifest{PublishedAt: nativePublished, ExpiresAt: nativeExpires})
	nativeFail(t, e)
}
