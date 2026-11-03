//go:build linux

package desktopsandbox

import (
	"bufio"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// CI test binary only: the canonical lock is independently opened/checked in
// the current namespace. FD3 must match that exact inode and the ORIGINAL
// mount ID admitted by the root-owned lifecycle record before unshare. This
// exception does not alter production sameMount or create a new lock/OFD.
func nativeLeaseBinding(canonical, inherited unix.Stat_t, currentMount, device, originalMount uint64, boundMount string) error {
	admitted, e := strconv.ParseUint(boundMount, 10, 64)
	if e != nil || admitted == 0 || strconv.FormatUint(admitted, 10) != boundMount || currentMount == 0 || originalMount != admitted || device != canonical.Dev || !sameStat(canonical, inherited) || inherited.Mode != unix.S_IFREG|0o600 || inherited.Uid != 0 || inherited.Gid != 0 || inherited.Nlink != 1 || inherited.Size != 0 {
		return ErrState
	}
	return nil
}

func TestNativeLeaseBindingRequiresProtectedPreNamespaceIdentity(t *testing.T) {
	canonical := unix.Stat_t{Dev: 7, Ino: 11, Mode: unix.S_IFREG | 0o600, Nlink: 1}
	inherited := canonical
	for _, currentMount := range []uint64{101, 202} {
		if e := nativeLeaseBinding(canonical, inherited, currentMount, 7, 101, "101"); e != nil {
			t.Fatalf("same bound lease rejected across canonical mount clone: %v", e)
		}
	}
	if e := sameMount(7, 202, 7, 101); e == nil {
		t.Fatal("production mount validation was relaxed")
	}
	for _, name := range []string{"wrong-inode", "wrong-device", "wrong-owner", "wrong-group", "wrong-mode", "extra-link", "size", "timestamp", "zero-current-mount", "zero-inherited-mount", "wrong-recorded-mount", "zero-recorded-mount", "missing-recorded-mount", "noncanonical-recorded-mount"} {
		t.Run(name, func(t *testing.T) {
			c, i, current, device, original, bound := canonical, inherited, uint64(202), uint64(7), uint64(101), "101"
			switch name {
			case "wrong-inode":
				i.Ino++
			case "wrong-device":
				device++
			case "wrong-owner":
				c.Uid = 1
				i.Uid = 1
			case "wrong-group":
				c.Gid = 1
				i.Gid = 1
			case "wrong-mode":
				c.Mode = unix.S_IFREG | 0o644
				i.Mode = c.Mode
			case "extra-link":
				c.Nlink = 2
				i.Nlink = 2
			case "size":
				c.Size = 1
				i.Size = 1
			case "timestamp":
				i.Ctim.Nsec++
			case "zero-current-mount":
				current = 0
			case "zero-inherited-mount":
				original = 0
			case "wrong-recorded-mount":
				bound = "202"
			case "zero-recorded-mount":
				bound = "0"
			case "missing-recorded-mount":
				bound = ""
			case "noncanonical-recorded-mount":
				bound = "0101"
			}
			if nativeLeaseBinding(c, i, current, device, original, bound) == nil {
				t.Fatal("unbound or unsafe lease accepted")
			}
		})
	}
}

// The owned actor emits this exact fixed admission transcript before it can
// reach the selected fault hook. Unknown/duplicate/reordered frames never
// authorize SIGKILL as an accepted crash-boundary observation.
var nativeCrashAdmissionFrames = []string{
	"DILLA_NATIVE_LIFECYCLE_PHASE root-identity", "DILLA_NATIVE_LIFECYCLE_PHASE run-open", "DILLA_NATIVE_LIFECYCLE_PHASE run-metadata-attributes", "DILLA_NATIVE_LIFECYCLE_PHASE run-capture-mount", "DILLA_NATIVE_LIFECYCLE_PHASE control-open", "DILLA_NATIVE_LIFECYCLE_PHASE control-metadata-attributes", "DILLA_NATIVE_LIFECYCLE_PHASE control-mount", "DILLA_NATIVE_LIFECYCLE_PHASE lock-read", "DILLA_NATIVE_LIFECYCLE_PHASE lease-stat", "DILLA_NATIVE_LIFECYCLE_PHASE lease-inode", "DILLA_NATIVE_LIFECYCLE_PHASE lease-mount-stat", "<lease>", "DILLA_NATIVE_LIFECYCLE_PHASE lease-mount", "DILLA_NATIVE_LIFECYCLE_PHASE lease-access", "DILLA_NATIVE_LIFECYCLE_PHASE lease-lock", "DILLA_NATIVE_LIFECYCLE_PHASE state-read", "DILLA_NATIVE_LIFECYCLE_PHASE state-fields", "DILLA_NATIVE_LIFECYCLE_PHASE state-json", "DILLA_NATIVE_LIFECYCLE_PHASE state-mount-binding", "DILLA_NATIVE_LIFECYCLE_PHASE state-identity", "DILLA_NATIVE_LIFECYCLE_PHASE state-digests", "DILLA_NATIVE_LIFECYCLE_PHASE admitted",
}
var errNativeReady = errors.New("owned actor did not reach exact crash boundary")
var nativeActorAssertion = regexp.MustCompile(`^\s*native_root_linux_test\.go:([1-9][0-9]{0,4}): (.{1,256})$`)

func nativeActorFailureLine(line string) string {
	match := nativeActorAssertion.FindStringSubmatch(line)
	if match == nil {
		return ""
	}
	reason := match[2]
	known := []string{ErrFraming.Error(), ErrManifest.Error(), ErrAuthentication.Error(), ErrExecutable.Error(), ErrPolicy.Error(), ErrState.Error(), ErrTime.Error(), ErrCompatibility.Error(), io.EOF.Error(), io.ErrUnexpectedEOF.Error(), "i/o timeout", "wrong lifecycle lease descriptor", "lifecycle lease must be read-only", "closed, foreign or malformed lifecycle state", "invalid lifecycle digest", "missing lifecycle admission", "missing lifecycle exclusion", "unconfigured disposable root fixture", "unsafe fixed native input", "native input changed", "foreign or unverified CI ownership", "invalid protected nonce", "verified native ELF changed", "compiled fixture input mismatch"}
	for _, value := range known {
		if reason == value || reason == "fixed crash point not reached "+value {
			return "native_root_linux_test.go:" + match[1] + ": " + reason
		}
	}
	if reason == "fixed crash point not reached <nil>" {
		return "native_root_linux_test.go:" + match[1] + ": " + reason
	}
	for code := unix.Errno(1); code <= 133; code++ {
		value := code.Error()
		if reason == value {
			return "native_root_linux_test.go:" + match[1] + ": " + value
		}
		if (strings.HasPrefix(reason, "open /usr/lib/dilla-sandbox-maintenance-ci/") || strings.HasPrefix(reason, "read |")) && strings.HasSuffix(reason, ": "+value) {
			return "native_root_linux_test.go:" + match[1] + ": " + value
		}
	}
	return ""
}
func readNativeCrashReady(file *os.File, observe func(string)) error {
	// One deadline for the entire transcript, not a fresh timeout per line.
	if e := file.SetReadDeadline(time.Now().Add(2 * time.Second)); e != nil {
		return e
	}
	reader := bufio.NewReaderSize(file, 512)
	index, total := 0, 0
	invalid, header := false, false
	for count := 0; count < 32; count++ {
		raw, e := reader.ReadSlice('\n')
		total += len(raw)
		if len(raw) > 256 || total > 8192 {
			return errNativeReady
		}
		if e != nil {
			if invalid {
				return errNativeReady
			}
			return e
		}
		line := strings.TrimSuffix(string(raw), "\n")
		if failure := nativeActorFailureLine(line); failure != "" {
			if observe != nil {
				observe(failure)
			}
			invalid = true
			continue
		}
		if invalid {
			continue
		}
		if index == 0 && !header && line == "=== RUN   TestDisposableRootActor" {
			header = true
			continue
		}
		if line == "DILLA_NATIVE_CRASH_READY" {
			if index == len(nativeCrashAdmissionFrames) {
				return nil
			}
			invalid = true
			continue
		}
		if index >= len(nativeCrashAdmissionFrames) {
			invalid = true
			continue
		}
		expected := nativeCrashAdmissionFrames[index]
		valid := line == expected || expected == "<lease>" && (line == "DILLA_NATIVE_LIFECYCLE_LEASE true false" || line == "DILLA_NATIVE_LIFECYCLE_LEASE true true")
		if !valid {
			invalid = true
			continue
		}
		index++
	}
	return errNativeReady
}

func TestNativeCrashReadyProtocol(t *testing.T) {
	if len(os.Args) > 2 && os.Args[len(os.Args)-2] == "--ready-protocol-child" {
		raw, e := base64.StdEncoding.DecodeString(os.Args[len(os.Args)-1])
		if e != nil {
			os.Exit(2)
		}
		_, _ = os.Stdout.Write(raw)
		os.Exit(0)
	}
	// Cross-check the producer's literal order so a future diagnostic addition
	// cannot silently break this separate owned-child stream contract again.
	source, e := os.ReadFile("native_root_linux_test.go")
	if e != nil {
		t.Fatal(e)
	}
	body := strings.SplitN(strings.SplitN(string(source), "func nativeLifecycle(t *testing.T, closed bool)", 2)[1], "func nativeLeaseFile", 2)[0]
	var produced []string
	for _, match := range regexp.MustCompile(`nativeLifecyclePhase\("([a-z-]+)"\)`).FindAllStringSubmatch(body, -1) {
		produced = append(produced, "DILLA_NATIVE_LIFECYCLE_PHASE "+match[1])
		if match[1] == "lease-mount-stat" {
			produced = append(produced, "<lease>")
		}
	}
	if strings.Join(produced, "\n") != strings.Join(nativeCrashAdmissionFrames, "\n") {
		t.Fatal("actor producer and readiness transcript diverged")
	}
	transcript := strings.Join(nativeCrashAdmissionFrames, "\n") + "\nDILLA_NATIVE_CRASH_READY\n"
	transcript = strings.Replace(transcript, "<lease>", "DILLA_NATIVE_LIFECYCLE_LEASE true false", 1)
	for _, name := range []string{"all-current-prefixes", "optional-go-header", "unknown", "duplicate", "malformed-lease", "incomplete", "oversized", "eof", "known-child-failure", "deadline"} {
		t.Run(name, func(t *testing.T) {
			raw := transcript
			want := name == "all-current-prefixes" || name == "optional-go-header"
			switch name {
			case "optional-go-header":
				raw = "=== RUN   TestDisposableRootActor\n" + raw
			case "unknown":
				raw = "FOREIGN_OUTPUT\n" + raw
			case "duplicate":
				raw = nativeCrashAdmissionFrames[0] + "\n" + raw
			case "malformed-lease":
				raw = strings.Replace(raw, "LEASE true false", "LEASE true foreign", 1)
			case "incomplete":
				raw = "DILLA_NATIVE_CRASH_READY\n"
			case "oversized":
				raw = strings.Repeat("x", 8193) + "\n" + raw
			case "eof":
				raw = strings.TrimSuffix(raw, "DILLA_NATIVE_CRASH_READY\n")
			case "known-child-failure":
				raw = "--- FAIL: TestDisposableRootActor (0.00s)\n    native_root_linux_test.go:120: fixed crash point not reached invalid Linux sandbox protected state\nPRIVATE_ENV=never-retain\n"
			case "deadline":
				raw = ""
			}
			var file *os.File
			var cmd *exec.Cmd
			if name == "deadline" {
				var write *os.File
				var e error
				file, write, e = os.Pipe()
				if e != nil {
					t.Fatal(e)
				}
				defer write.Close()
			} else {
				cmd = exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestNativeCrashReadyProtocol$", "--", "--ready-protocol-child", base64.StdEncoding.EncodeToString([]byte(raw)))
				out, e := cmd.StdoutPipe()
				if e != nil {
					t.Fatal(e)
				}
				file = out.(*os.File)
				if e = cmd.Start(); e != nil {
					t.Fatal(e)
				}
				defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
			}
			defer file.Close()
			var observed []string
			e := readNativeCrashReady(file, func(line string) { observed = append(observed, line) })
			if name == "deadline" && !errors.Is(e, os.ErrDeadlineExceeded) {
				t.Fatalf("deadline was not exercised: %v", e)
			}
			if (e == nil) != want {
				t.Fatalf("readiness accepted=%v wanted=%v: %v", e == nil, want, e)
			}
			if name == "known-child-failure" && (len(observed) != 1 || !strings.Contains(observed[0], ErrState.Error()) || strings.Contains(strings.Join(observed, ""), "PRIVATE_ENV")) {
				t.Fatal("bounded child failure reason was lost or leaked unrelated output")
			}
		})
	}
}
