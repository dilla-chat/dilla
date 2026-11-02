//go:build linux && cgo

package frontend

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha512"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jonasthim/dilla/internal/desktopsandbox"
)

const observerSource = `package main
import("crypto/sha512";"encoding/base64";"encoding/json";"io";"os";"sort";"strings";"strconv";"syscall")
var identity="initial"
func main(){data,e:=io.ReadAll(io.LimitReader(os.Stdin,49<<20));if e!=nil{os.Exit(9)};self,e:=os.ReadFile("/proc/self/exe");if e!=nil{os.Exit(10)};sum:=sha512.Sum512(data);own:=sha512.Sum512(self);inherited:=syscall.Flock(3,syscall.LOCK_EX|syscall.LOCK_NB)==nil;other,e:=os.OpenFile("/proc/self/fd/3",os.O_RDWR,0);blocked:=false;if e==nil{err:=syscall.Flock(int(other.Fd()),syscall.LOCK_EX|syscall.LOCK_NB);blocked=err==syscall.EWOULDBLOCK;other.Close()};syscall.CloseOnExec(3);var info syscall.Stat_t;syscall.Fstat(0,&info);keys:=[]string{};for _,v:=range os.Environ(){keys=append(keys,strings.SplitN(v,"=",2)[0])};sort.Strings(keys);json.NewEncoder(os.Stdout).Encode(map[string]any{"identity":identity,"inputSha512":base64.StdEncoding.EncodeToString(sum[:]),"selfSha512":base64.StdEncoding.EncodeToString(own[:]),"lockInherited":inherited,"contenderBlocked":blocked,"regularInput":info.Mode&syscall.S_IFMT==syscall.S_IFREG,"inputDevice":strconv.FormatUint(info.Dev,10),"inputInode":strconv.FormatUint(info.Ino,10),"readonlyInput":func()bool{flags,_,err:=syscall.Syscall(syscall.SYS_FCNTL,0,syscall.F_GETFL,0);return err==0&&flags&syscall.O_ACCMODE==syscall.O_RDONLY}(),"args":os.Args,"envKeys":keys})}`

var observerOnce sync.Once
var observerImage []byte
var observerFailure error

func observer(t *testing.T) []byte {
	t.Helper()
	observerOnce.Do(func() {
		dir := t.TempDir()
		var err error
		source := filepath.Join(dir, "main.go")
		if err = os.WriteFile(source, []byte(observerSource), 0o600); err != nil {
			observerFailure = err
			return
		}
		output := filepath.Join(dir, "observer")
		command := exec.CommandContext(t.Context(), "go", "build", "-trimpath", "-buildvcs=false", "-o", output, source)
		command.Env = append(os.Environ(), "CGO_ENABLED=0")
		if b, e := command.CombinedOutput(); e != nil {
			observerFailure = e
			t.Log(string(b))
			return
		}
		observerImage, observerFailure = os.ReadFile(output)
	})
	if observerFailure != nil {
		t.Fatal(observerFailure)
	}
	return bytes.Clone(observerImage)
}
func hashBuffer(b []byte) string {
	sum := sha512.Sum512(b)
	return base64.StdEncoding.EncodeToString(sum[:])
}

type capsuleFixture struct {
	constants BuildConstants
	raw       []byte
	capsule   desktopsandbox.Capsule
	trust     desktopsandbox.Trust
	private   ed25519.PrivateKey
}

func makeCapsule(t *testing.T, generation int64) capsuleFixture {
	t.Helper()
	program := observer(t)
	pub, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	m := desktopsandbox.Manifest{Format: 1, Kind: "dilla-linux-sandbox", Generation: generation, Platform: "linux", Arch: "x64", CompatibilityEpoch: 1, SandboxAPI: 1, PublishedAt: now.Add(-time.Hour).Format(time.RFC3339), ExpiresAt: now.Add(24 * time.Hour).Format(time.RFC3339), Maintainer: desktopsandbox.Component{Size: int64(len(program)), SHA512: hashBuffer(program)}, Helper: desktopsandbox.Component{Size: int64(len(program)), SHA512: hashBuffer(program)}}
	metadata, _ := json.Marshal(m)
	var framed bytes.Buffer
	framed.WriteString("DLSBOX1\x00")
	for _, v := range []any{uint32(len(metadata)), uint32(0), uint64(len(program)), uint64(len(program))} {
		if err = binary.Write(&framed, binary.LittleEndian, v); err != nil {
			t.Fatal(err)
		}
	}
	framed.Write(metadata)
	framed.Write(program)
	framed.Write(program)
	raw := framed.Bytes()
	c := BuildConstants{CapsuleSize: int64(len(raw)), CapsuleSHA512: hashBuffer(raw), ManifestSHA512: hashBuffer(metadata), MaintainerOffset: int64(32 + len(metadata)), MaintainerSize: int64(len(program)), MaintainerSHA512: hashBuffer(program), Generation: generation, CompatibilityEpoch: 1, SandboxAPI: 1, PublishedAt: m.PublishedAt, ExpiresAt: m.ExpiresAt}
	return capsuleFixture{c, raw, desktopsandbox.Capsule{Manifest: metadata, Maintainer: program, Helper: bytes.Clone(program)}, desktopsandbox.Trust{PublicKeys: []ed25519.PublicKey{pub}, Initial: &desktopsandbox.InitialTrust{Generation: generation, CompatibilityEpoch: 1, SandboxAPI: 1, PublishedAt: m.PublishedAt, ExpiresAt: m.ExpiresAt, Helper: m.Helper, SelfMaintainerSHA512: m.Maintainer.SHA512}}, private}
}
func fixtureRoot(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "dilla", "appimage")
}

// Literal rewriting happens only in this compiled test fixture, before Python
// runs. Production has no root/uid/deadline argument, env flag or config override.
func fixtureProgram(t *testing.T, c BuildConstants, root string) string {
	t.Helper()
	code, e := BootstrapProgram(c)
	if e != nil {
		t.Fatal(e)
	}
	quoted, _ := json.Marshal(root)
	for _, pair := range [][2]string{{`ANCHOR = "/"`, "ANCHOR = " + func() string { b, _ := json.Marshal(filepath.Dir(filepath.Dir(root))); return string(b) }()}, {`ROOT = "/usr/lib/dilla/appimage"`, "ROOT = " + string(quoted)}, {"OWNER_UID = 0", "OWNER_UID = " + strconv.Itoa(os.Geteuid())}, {"OWNER_GID = 0", "OWNER_GID = " + strconv.Itoa(os.Getegid())}, {"INPUT_TIMEOUT = 30.0", "INPUT_TIMEOUT = 3.0"}, {"LOCK_TIMEOUT = 5.0", "LOCK_TIMEOUT = 0.3"}} {
		if strings.Count(code, pair[0]) != 1 {
			t.Fatal("fixture compile boundary changed", pair[0])
		}
		code = strings.Replace(code, pair[0], pair[1], 1)
	}
	return code
}
func python(t *testing.T, code string, input []byte) ([]byte, []byte, error) {
	t.Helper()
	command := exec.CommandContext(t.Context(), "/usr/bin/python3", "-I", "-S", "-c", code)
	command.Stdin = bytes.NewReader(input)
	command.Env = []string{"PATH=/not/a/program/path", "PYTHONPATH=/not/trusted", "LD_LIBRARY_PATH=/not/trusted", "DILLA_UPDATE_HEALTH_NONCE=PRIVATE_SECRET"}
	var output, fail bytes.Buffer
	command.Stdout = &output
	command.Stderr = &fail
	err := command.Run()
	return output.Bytes(), fail.Bytes(), err
}
func decodeObservation(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var v map[string]any
	if e := json.Unmarshal(raw, &v); e != nil {
		t.Fatalf("invalid owned observation: %v", e)
	}
	return v
}

func TestActualEmittedBootstrapExecutesProtectedBytesAndCarriesLockedFD(t *testing.T) {
	f := makeCapsule(t, 1)
	root := fixtureRoot(t)
	code := fixtureProgram(t, f.constants, root)
	out, fail, err := python(t, code, f.raw)
	if err != nil {
		t.Fatalf("emitted bootstrap failed: %v %s", err, fail)
	}
	v := decodeObservation(t, out)
	if v["inputSha512"] != f.constants.CapsuleSHA512 || v["selfSha512"] != f.constants.MaintainerSHA512 || v["lockInherited"] != true || v["contenderBlocked"] != true || v["regularInput"] != true || v["readonlyInput"] != true {
		t.Fatalf("lost descriptor identity/lock: %+v", v)
	}
	if strings.Join(anyStrings(v["envKeys"]), ",") != "LANG,LC_ALL,PATH" || strings.Join(anyStrings(v["args"]), ",") != "sandbox-maintainer" {
		t.Fatal("unsafe child argv/environment", v)
	}
	raw, e := os.ReadFile(filepath.Join(root, ".sandbox-staging/bootstrap-anchor.json"))
	if e != nil {
		t.Fatal(e)
	}
	a, e := desktopsandbox.ParseBootstrapAnchor(raw)
	if e != nil {
		t.Fatal("Python anchor does not match Go wire contract", e)
	}
	if v["inputDevice"] != a.Capsule.Device || v["inputInode"] != a.Capsule.Inode {
		t.Fatal("received a different protected stdin inode")
	}
	trust := f.trust
	trust.Bootstrap = &a
	if _, e = desktopsandbox.VerifyCapsule(f.capsule, trust, desktopsandbox.Receipt{}, time.Now()); e != nil {
		t.Fatal("Python anchor failed core admission check", e)
	}
	for _, directory := range []string{filepath.Dir(root), root} {
		info, e := os.Stat(directory)
		if e != nil {
			t.Fatal(e)
		}
		if info.Mode().Perm() != 0o755 {
			t.Fatal("system helper parent is not traversable", info.Mode())
		}
	}
}
func anyStrings(value any) []string {
	result := []string{}
	for _, v := range value.([]any) {
		result = append(result, v.(string))
	}
	return result
}

func TestActualBootstrapRejectsTamperedTruncatedExcessAndSpecialInput(t *testing.T) {
	f := makeCapsule(t, 1)
	for _, kind := range []string{"tampered", "truncated", "excess"} {
		t.Run(kind, func(t *testing.T) {
			input := bytes.Clone(f.raw)
			switch kind {
			case "tampered":
				input[len(input)-1] ^= 1
			case "truncated":
				input = input[:len(input)-1]
			case "excess":
				input = append(input, 0)
			}
			root := fixtureRoot(t)
			out, fail, e := python(t, fixtureProgram(t, f.constants, root), input)
			if e == nil || len(out) != 0 || bytes.Contains(fail, []byte("PRIVATE_SECRET")) {
				t.Fatal("unsafe input executed", e)
			}
			if _, e = os.Lstat(filepath.Join(root, ".sandbox-staging/bootstrap-anchor.json")); !os.IsNotExist(e) {
				t.Fatal("invalid input acquired durable admission")
			}
		})
	}
	root := fixtureRoot(t)
	command := exec.CommandContext(t.Context(), "/usr/bin/python3", "-I", "-S", "-c", fixtureProgram(t, f.constants, root))
	zero, e := os.Open("/dev/zero")
	if e != nil {
		t.Fatal(e)
	}
	defer zero.Close()
	command.Stdin = zero
	if e = command.Run(); e == nil {
		t.Fatal("special input was accepted")
	}
}

func TestActualBootstrapDoesNotAdmitMismatchedLiteralTuple(t *testing.T) {
	f := makeCapsule(t, 1)
	for _, kind := range []string{"generation", "manifest hash", "publication"} {
		t.Run(kind, func(t *testing.T) {
			c := f.constants
			switch kind {
			case "generation":
				c.Generation = 2
			case "manifest hash":
				c.ManifestSHA512 = testHash("different manifest")
			case "publication":
				c.PublishedAt = time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
			}
			root := fixtureRoot(t)
			out, _, err := python(t, fixtureProgram(t, c, root), f.raw)
			if err == nil || len(out) != 0 {
				t.Fatal("literal tuple mismatch acquired authority")
			}
			if _, err = os.Lstat(filepath.Join(root, ".sandbox-staging/bootstrap-anchor.json")); !os.IsNotExist(err) {
				t.Fatal("mismatched tuple became durable")
			}
		})
	}
}

func seedInitialAnchor(t *testing.T, f capsuleFixture, root string) {
	t.Helper()
	code := fixtureProgram(t, f.constants, root)
	needle := "    execute(program, lock, capsule)"
	index := strings.LastIndex(code, needle)
	if index < 0 {
		t.Fatal("initial exec boundary changed")
	}
	code = code[:index] + strings.Replace(code[index:], needle, "    os._exit(73)", 1)
	out, fail, err := python(t, code, f.raw)
	var exit *exec.ExitError
	ok := errors.As(err, &exit)
	if !ok || exit.ExitCode() != 73 || len(out) != 0 {
		t.Fatalf("failed to stop after durable anchor: %v %s", err, fail)
	}
}

func TestHistoricalAnchorRecoveryUsesPriorProtectedBytesWithoutNewWrites(t *testing.T) {
	f := makeCapsule(t, 1)
	root := fixtureRoot(t)
	seedInitialAnchor(t, f, root)
	before, e := os.ReadFile(filepath.Join(root, ".sandbox-staging/bootstrap-anchor.json"))
	if e != nil {
		t.Fatal(e)
	}
	incoming := f.constants
	incoming.Generation = 2
	incoming.CapsuleSHA512 = testHash("different incoming build")
	code := fixtureProgram(t, incoming, root)
	code = strings.Replace(code, "try:\n    run()", `def cannot_write(*args, **kwargs):
    raise OSError(errno.ENOSPC, "injected owned fixture full disk")
os.write = cannot_write
try:
    run()`, 1)
	out, fail, err := python(t, code, []byte("PRIVATE_SECRET ignored incoming data"))
	if err != nil {
		t.Fatalf("prior anchor could not recover without allocation: %v %s", err, fail)
	}
	v := decodeObservation(t, out)
	if v["inputSha512"] != f.constants.CapsuleSHA512 || v["selfSha512"] != f.constants.MaintainerSHA512 || v["regularInput"] != true {
		t.Fatal("executed incoming or unprotected bytes", v)
	}
	after, e := os.ReadFile(filepath.Join(root, ".sandbox-staging/bootstrap-anchor.json"))
	if e != nil || !bytes.Equal(before, after) {
		t.Fatal("recovery rewrote existing admission")
	}
}

func TestNativeSiblingDoesNotBecomeAppImageAuthority(t *testing.T) {
	f := makeCapsule(t, 1)
	root := fixtureRoot(t)
	native := filepath.Join(filepath.Dir(root), "native")
	if e := os.MkdirAll(native, 0o755); e != nil {
		t.Fatal(e)
	}
	file := filepath.Join(native, "package-owned")
	if e := os.WriteFile(file, []byte("DO_NOT_TOUCH_NATIVE"), 0o600); e != nil {
		t.Fatal(e)
	}
	out, fail, e := python(t, fixtureProgram(t, f.constants, root), f.raw)
	if e != nil {
		t.Fatalf("native sibling blocked independent first setup: %v %s", e, fail)
	}
	decodeObservation(t, out)
	after, e := os.ReadFile(file)
	if e != nil || string(after) != "DO_NOT_TOUCH_NATIVE" {
		t.Fatal("modified package-owned sibling")
	}
}

func rootReceipt(t *testing.T, f capsuleFixture, anchorHash string) []byte {
	t.Helper()
	var m desktopsandbox.Manifest
	if e := json.Unmarshal(f.capsule.Manifest, &m); e != nil {
		t.Fatal(e)
	}
	raw, e := json.Marshal(desktopsandbox.Receipt{Format: 1, Authorization: "initial-digest", InitialAnchorSHA512: anchorHash, Manifest: base64.StdEncoding.EncodeToString(f.capsule.Manifest), Signature: "", AcceptedAt: time.Now().Add(-time.Minute).UnixMilli(), MaximumGeneration: m.Generation, MaintainerSHA512: m.Maintainer.SHA512, HelperSHA512: m.Helper.SHA512, CompatibilityEpoch: 1, SandboxAPI: 1, State: "active", CompletedOperation: "install", TransactionSHA512: testHash("completed initial transaction")})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = desktopsandbox.ParseReceipt(raw); e != nil {
		t.Fatal(e)
	}
	return raw
}

func TestCommittedResidentDispatchDoesNotRequireDeletedBootstrapStages(t *testing.T) {
	f := makeCapsule(t, 1)
	root := fixtureRoot(t)
	seedInitialAnchor(t, f, root)
	stage := filepath.Join(root, ".sandbox-staging")
	raw, e := os.ReadFile(filepath.Join(stage, "bootstrap-anchor.json"))
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(root, "sandbox-maintainer"), f.capsule.Maintainer, 0o755); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(root, "sandbox-receipt.json"), rootReceipt(t, f, hashBuffer(raw)), 0o644); e != nil {
		t.Fatal(e)
	}
	for _, name := range []string{"bootstrap-capsule", "bootstrap-maintainer"} {
		if e = os.Remove(filepath.Join(stage, name)); e != nil {
			t.Fatal(e)
		}
	}
	code := fixtureProgram(t, f.constants, root)
	out, fail, e := python(t, code, []byte("new data must not choose executable"))
	if e != nil {
		t.Fatalf("committed resident cleanup depended on deleted stages: %v %s", e, fail)
	}
	v := decodeObservation(t, out)
	if v["selfSha512"] != f.constants.MaintainerSHA512 {
		t.Fatal("did not execute recognized resident")
	}
}

func TestActualBootstrapHoldsOnUnknownAuthorityAndUnsafeStages(t *testing.T) {
	f := makeCapsule(t, 1)
	for _, kind := range []string{"unknown leaf", "missing anchored program", "changed anchored program", "symlinked capsule", "malformed anchor", "oversized anchor", "unknown resident"} {
		t.Run(kind, func(t *testing.T) {
			root := fixtureRoot(t)
			if kind == "unknown leaf" || kind == "unknown resident" {
				if e := os.MkdirAll(root, 0o755); e != nil {
					t.Fatal(e)
				}
				name := "unknown-owner-data"
				data := []byte("PRIVATE_SECRET must stay")
				mode := os.FileMode(0o600)
				if kind == "unknown resident" {
					name = "sandbox-maintainer"
					data = f.capsule.Maintainer
					mode = 0o755
				}
				if e := os.WriteFile(filepath.Join(root, name), data, mode); e != nil {
					t.Fatal(e)
				}
			} else {
				seedInitialAnchor(t, f, root)
				stage := filepath.Join(root, ".sandbox-staging")
				switch kind {
				case "missing anchored program":
					if e := os.Remove(filepath.Join(stage, "bootstrap-maintainer")); e != nil {
						t.Fatal(e)
					}
				case "changed anchored program":
					if e := os.WriteFile(filepath.Join(stage, "bootstrap-maintainer"), []byte("PRIVATE_SECRET changed"), 0o700); e != nil {
						t.Fatal(e)
					}
				case "symlinked capsule":
					if e := os.Remove(filepath.Join(stage, "bootstrap-capsule")); e != nil {
						t.Fatal(e)
					}
					if e := os.Symlink("/dev/zero", filepath.Join(stage, "bootstrap-capsule")); e != nil {
						t.Fatal(e)
					}
				case "malformed anchor":
					if e := os.WriteFile(filepath.Join(stage, "bootstrap-anchor.json"), []byte(`{"format":1,"format":1,"PRIVATE_SECRET":1}`), 0o600); e != nil {
						t.Fatal(e)
					}
				case "oversized anchor":
					if e := os.WriteFile(filepath.Join(stage, "bootstrap-anchor.json"), bytes.Repeat([]byte("x"), 4097), 0o600); e != nil {
						t.Fatal(e)
					}
				}
			}
			out, fail, e := python(t, fixtureProgram(t, f.constants, root), f.raw)
			if e == nil || len(out) > 0 || bytes.Contains(fail, []byte("PRIVATE_SECRET")) {
				t.Fatal("unsafe authority executed or leaked input", e)
			}
		})
	}
}

func TestActualBootstrapBoundsStalledAndUnclosedInput(t *testing.T) {
	f := makeCapsule(t, 1)
	for _, kind := range []string{"before bytes", "after exact bytes"} {
		t.Run(kind, func(t *testing.T) {
			root := fixtureRoot(t)
			code := strings.Replace(fixtureProgram(t, f.constants, root), "INPUT_TIMEOUT = 3.0", "INPUT_TIMEOUT = 0.15", 1)
			command := exec.CommandContext(t.Context(), "/usr/bin/python3", "-I", "-S", "-c", code)
			pipe, e := command.StdinPipe()
			if e != nil {
				t.Fatal(e)
			}
			if e = command.Start(); e != nil {
				t.Fatal(e)
			}
			defer pipe.Close()
			if kind == "after exact bytes" {
				if _, e = pipe.Write(f.raw); e != nil {
					t.Fatal(e)
				}
			}
			started := time.Now()
			if e = command.Wait(); e == nil {
				t.Fatal("accepted input without EOF")
			}
			if time.Since(started) > 2*time.Second {
				t.Fatal("input deadline did not bound wait")
			}
			if _, e = os.Lstat(filepath.Join(root, ".sandbox-staging/bootstrap-anchor.json")); !os.IsNotExist(e) {
				t.Fatal("stalled input acquired durable admission")
			}
		})
	}
}

func TestEnsureUsesOnlyFixedAuthorizationProgramsAndRequiresFreshReadyProof(t *testing.T) {
	f := makeCapsule(t, 1)
	embedded, e := NewEmbeddedCapsule(f.raw, f.trust.PublicKeys)
	if e != nil {
		t.Fatal(e)
	}
	ready := Status{Format: 1, Status: "ready", Generation: 1, CompatibilityEpoch: 1, SandboxAPI: 1, MaintainerSHA512: f.constants.MaintainerSHA512, HelperSHA512: f.constants.MaintainerSHA512}
	checks, explanations, calls := 0, 0, 0
	dependencies := ensureDependencies{check: func() (Status, error) {
		checks++
		if checks == 1 {
			return Status{Format: 1, Status: "missing"}, nil
		}
		return ready, nil
	}, explain: func() error { explanations++; return nil }, authorize: func(request authorizationRequest) authorizationResult {
		calls++
		if request.executable != "/usr/bin/pkexec" || len(request.args) != 6 || request.args[1] != "/usr/bin/python3" || request.args[2] != "-I" || request.args[3] != "-S" || !bytes.Equal(request.input, f.raw) {
			t.Fatal("unsafe authorization invocation")
		}
		return authorizationResult{exitCode: 0, brokenPipe: true}
	}}
	outcome, e := ensure(embedded, InitialSetup, dependencies)
	if e != nil || outcome != Prepared || checks != 2 || explanations != 1 || calls != 1 {
		t.Fatal("did not bind success to actual ready state", outcome, e, checks, explanations, calls)
	}
	dependencies.check = func() (Status, error) { return ready, nil }
	calls = 0
	explanations = 0
	outcome, e = ensure(embedded, UpdateSetup, dependencies)
	if e != nil || outcome != AlreadyReady || calls != 0 || explanations != 0 {
		t.Fatal("ready component requested authorization")
	}
}

func TestEnsureCannotTreatCancelFailureOrEPIPEAsInstallationSuccess(t *testing.T) {
	f := makeCapsule(t, 1)
	embedded, e := NewEmbeddedCapsule(f.raw, f.trust.PublicKeys)
	if e != nil {
		t.Fatal(e)
	}
	for _, kind := range []string{"dialog canceled", "authorization canceled", "authorization unavailable", "child failed", "broken pipe without ready", "invalid ready protocol"} {
		t.Run(kind, func(t *testing.T) {
			count := 0
			deps := ensureDependencies{check: func() (Status, error) {
				count++
				if count == 1 {
					return Status{Format: 1, Status: "missing"}, nil
				}
				return Status{Format: 1, Status: "maintenance"}, nil
			}, explain: func() error { return nil }, authorize: func(authorizationRequest) authorizationResult { return authorizationResult{exitCode: 0} }}
			switch kind {
			case "dialog canceled":
				deps.explain = func() error { return ErrCancelled }
				deps.authorize = func(authorizationRequest) authorizationResult {
					t.Fatal("authorized after canceled explanation")
					return authorizationResult{}
				}
			case "authorization canceled":
				deps.authorize = func(authorizationRequest) authorizationResult { return authorizationResult{exitCode: 126} }
			case "authorization unavailable":
				deps.authorize = func(authorizationRequest) authorizationResult { return authorizationResult{exitCode: 127} }
			case "child failed":
				deps.authorize = func(authorizationRequest) authorizationResult { return authorizationResult{exitCode: 1} }
			case "broken pipe without ready":
				deps.authorize = func(authorizationRequest) authorizationResult {
					return authorizationResult{exitCode: 0, brokenPipe: true}
				}
			case "invalid ready protocol":
				deps.check = func() (Status, error) {
					return Status{Format: 99, Status: "ready", Generation: 1, CompatibilityEpoch: 1, SandboxAPI: 1}, nil
				}
			}
			if result, e := ensure(embedded, InitialSetup, deps); e == nil || result == Prepared || result == AlreadyReady {
				t.Fatal("failed preparation became ready", result, e)
			}
		})
	}
}

func TestResidentUpgradeNeedsDedicatedSignatureAndNeverFallsBackToPython(t *testing.T) {
	f := makeCapsule(t, 2)
	embedded, e := NewEmbeddedCapsule(f.raw, f.trust.PublicKeys)
	if e != nil {
		t.Fatal(e)
	}
	before := Status{Format: 1, Status: "ready", Generation: 1, CompatibilityEpoch: 1, SandboxAPI: 1, MaintainerSHA512: testHash("prior maintainer"), HelperSHA512: testHash("prior helper"), resident: true}
	deps := ensureDependencies{check: func() (Status, error) { return before, nil }, explain: func() error { t.Fatal("resident update requested initial explanation"); return nil }, authorize: func(authorizationRequest) authorizationResult {
		t.Fatal("unsigned resident upgrade asked for root")
		return authorizationResult{}
	}}
	if _, e = ensure(embedded, UpdateSetup, deps); !errors.Is(e, ErrSignedSidecarRequired) {
		t.Fatal("unsigned resident fallback", e)
	}
	signature := ed25519.Sign(f.private, append([]byte("Dilla Linux sandbox capsule v1\x00"), f.capsule.Manifest...))
	signed, e := embedded.WithSignature(signature)
	if e != nil {
		t.Fatal(e)
	}
	count := 0
	deps.check = func() (Status, error) {
		count++
		if count == 1 {
			return before, nil
		}
		return Status{Format: 1, Status: "ready", Generation: 2, CompatibilityEpoch: 1, SandboxAPI: 1, MaintainerSHA512: f.constants.MaintainerSHA512, HelperSHA512: f.constants.MaintainerSHA512, resident: true}, nil
	}
	deps.authorize = func(request authorizationRequest) authorizationResult {
		if request.executable != "/usr/bin/pkexec" || strings.Join(request.args, ",") != "--disable-internal-agent,/usr/lib/dilla/appimage/sandbox-maintainer" {
			t.Fatal("resident upgrade selected alternate program")
		}
		parsed, e := desktopsandbox.ParseCapsule(bytes.NewReader(request.input), desktopsandbox.Limits{})
		if e != nil || !bytes.Equal(parsed.Signature, signature) {
			t.Fatal("missing dedicated signature")
		}
		return authorizationResult{exitCode: 0}
	}
	if outcome, e := ensure(signed, UpdateSetup, deps); e != nil || outcome != Prepared {
		t.Fatal(outcome, e)
	}
}

func TestEnsureAcceptsNewerCompatibleHelperButDoesNotAuthorizeSameGenerationConflict(t *testing.T) {
	f := makeCapsule(t, 1)
	embedded, e := NewEmbeddedCapsule(f.raw, f.trust.PublicKeys)
	if e != nil {
		t.Fatal(e)
	}
	signature := ed25519.Sign(f.private, append([]byte("Dilla Linux sandbox capsule v1\x00"), f.capsule.Manifest...))
	embedded, e = embedded.WithSignature(signature)
	if e != nil {
		t.Fatal(e)
	}
	status := Status{Format: 1, Status: "ready", Generation: 2, CompatibilityEpoch: 1, SandboxAPI: 1, MaintainerSHA512: testHash("newer maintainer"), HelperSHA512: testHash("newer helper"), resident: true}
	deps := ensureDependencies{check: func() (Status, error) { return status, nil }, explain: func() error { t.Fatal("unexpected explanation"); return nil }, authorize: func(authorizationRequest) authorizationResult {
		t.Fatal("unnecessary or conflicting authorization")
		return authorizationResult{}
	}}
	if outcome, e := ensure(embedded, UpdateSetup, deps); e != nil || outcome != AlreadyReady {
		t.Fatal("newer compatible helper did not satisfy old application", outcome, e)
	}
	status.Generation = 1
	if _, e = ensure(embedded, UpdateSetup, deps); e == nil {
		t.Fatal("same-generation conflicting helper accepted")
	}
}

func TestReadOnlyStatusRejectsAmbiguousSchemaAndWrongStateShape(t *testing.T) {
	s := Status{Format: 1, Status: "ready", Generation: 1, CompatibilityEpoch: 1, SandboxAPI: 1, MaintainerSHA512: testHash("manager"), HelperSHA512: testHash("helper")}
	raw, _ := json.Marshal(s)
	if _, e := parseStatus(raw); e != nil {
		t.Fatal(e)
	}
	for _, bad := range [][]byte{bytes.Replace(raw, []byte(`"format":1`), []byte(`"Format":1`), 1), bytes.Replace(raw, []byte(`"format":1`), []byte(`"format":1,"format":1`), 1), bytes.Replace(raw, []byte(`"status":"ready"`), []byte(`"status":"missing"`), 1), []byte(`{"format":1,"status":"removed","generation":0,"compatibilityEpoch":0,"sandboxApi":0,"maintainerSha512":"","helperSha512":""}`), bytes.Repeat([]byte("x"), 4097)} {
		if _, e := parseStatus(bad); e == nil {
			t.Fatal("invalid root status was accepted")
		}
	}
}

func TestGTKExplanationProcess(t *testing.T) {
	if len(os.Args) == 0 || os.Args[len(os.Args)-1] != "owned-gtk-explanation" {
		return
	}
	err := showInitialExplanation()
	if errors.Is(err, ErrNoDisplay) {
		os.Stdout.WriteString("GTK_RESULT unavailable\n")
		return
	}
	if errors.Is(err, ErrCancelled) {
		os.Stdout.WriteString("GTK_RESULT canceled\n")
		return
	}
	if err == nil {
		os.Stdout.WriteString("GTK_RESULT continued\n")
		return
	}
	t.Fatal(err)
}
func TestGTKBindingWithoutDisplayCannotAuthorize(t *testing.T) {
	command := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestGTKExplanationProcess$", "--", "owned-gtk-explanation")
	private := t.TempDir()
	command.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + private, "XDG_CACHE_HOME=" + filepath.Join(private, "cache")}
	out, e := command.CombinedOutput()
	if e != nil || !bytes.Contains(out, []byte("GTK_RESULT unavailable")) {
		t.Fatal("headless GTK did not refuse explanation", e)
	}
}

func TestAcceptedAnchorCanFinishAfterExpiryButCannotInventPastAcceptance(t *testing.T) {
	f := makeCapsule(t, 1)
	root := fixtureRoot(t)
	seedInitialAnchor(t, f, root)
	code := fixtureProgram(t, f.constants, root)
	future := strconv.FormatInt(time.Now().Add(48*time.Hour).UnixNano(), 10)
	code = strings.Replace(code, "try:\n    run()", "time.time_ns = lambda: "+future+"\ntry:\n    run()", 1)
	if out, fail, e := python(t, code, []byte("ignored new input")); e != nil || len(out) == 0 {
		t.Fatalf("historically admitted exact bytes expired during recovery: %v %s", e, fail)
	}
	path := filepath.Join(root, ".sandbox-staging/bootstrap-anchor.json")
	raw, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	a, e := desktopsandbox.ParseBootstrapAnchor(raw)
	if e != nil {
		t.Fatal(e)
	}
	a.AcceptedAt = time.Now().Add(48 * time.Hour).UnixMilli()
	raw, e = json.Marshal(a)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(path, raw, 0o600); e != nil {
		t.Fatal(e)
	}
	if out, _, e := python(t, code, nil); e == nil || len(out) != 0 {
		t.Fatal("expired initial bytes gained fabricated acceptance time")
	}
}

func TestExplanationCannotChangeCompiledCapsuleOrPublicTrust(t *testing.T) {
	f := makeCapsule(t, 1)
	embedded, e := NewEmbeddedCapsule(f.raw, f.trust.PublicKeys)
	if e != nil {
		t.Fatal(e)
	}
	expected := f.constants.CapsuleSHA512
	count := 0
	deps := ensureDependencies{check: func() (Status, error) {
		count++
		if count == 1 {
			return Status{Format: 1, Status: "missing"}, nil
		}
		return Status{Format: 1, Status: "ready", Generation: 1, CompatibilityEpoch: 1, SandboxAPI: 1, MaintainerSHA512: f.constants.MaintainerSHA512, HelperSHA512: f.constants.MaintainerSHA512}, nil
	}, explain: func() error {
		for i := range f.raw {
			f.raw[i] = 0
		}
		for i := range f.trust.PublicKeys[0] {
			f.trust.PublicKeys[0][i] = 0
		}
		return nil
	}, authorize: func(request authorizationRequest) authorizationResult {
		if hashBuffer(request.input) != expected || !strings.Contains(request.args[5], expected) {
			t.Fatal("prompt-time source mutation changed frozen authorization")
		}
		return authorizationResult{exitCode: 0}
	}}
	if _, e = ensure(embedded, InitialSetup, deps); e != nil {
		t.Fatal(e)
	}
}
