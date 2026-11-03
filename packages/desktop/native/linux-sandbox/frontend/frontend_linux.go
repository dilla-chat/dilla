//go:build linux && cgo

package frontend

/*
#cgo pkg-config: gtk+-3.0
#include <gtk/gtk.h>
static int dilla_explain_initial_setup(void) {
 if (!gtk_init_check(NULL, NULL)) return 0;
 GtkWidget *dialog = gtk_message_dialog_new(NULL, GTK_DIALOG_MODAL,
   GTK_MESSAGE_INFO, GTK_BUTTONS_NONE, "Dilla needs a secure system component");
 gtk_window_set_title(GTK_WINDOW(dialog), "Dilla secure setup");
 gtk_message_dialog_format_secondary_text(GTK_MESSAGE_DIALOG(dialog),
   "Dilla will ask your desktop for administrator authorization to install its sandbox component. "
   "The first system prompt identifies the trusted system Python interpreter. "
   "The component is shared by AppImage installations. Your accounts and messages are unchanged.");
 gtk_dialog_add_buttons(GTK_DIALOG(dialog), "Cancel", GTK_RESPONSE_CANCEL,
   "Continue", GTK_RESPONSE_ACCEPT, NULL);
 gtk_dialog_set_default_response(GTK_DIALOG(dialog), GTK_RESPONSE_CANCEL);
 int response = gtk_dialog_run(GTK_DIALOG(dialog));
 gtk_widget_destroy(dialog);
 while (gtk_events_pending()) gtk_main_iteration();
 return response == GTK_RESPONSE_ACCEPT ? 1 : 2;
}
*/
import "C"

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha512"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/dilla-chat/dilla/internal/desktopsandbox"
)

const authorizationExecutable = "/usr/bin/pkexec"
const residentExecutable = "/usr/lib/dilla/appimage/sandbox-maintainer"

var (
	ErrCancelled                = errors.New("sandbox setup canceled")
	ErrSignedSidecarRequired    = errors.New("signed sandbox setup metadata is required")
	ErrSetupFailed              = errors.New("sandbox setup did not produce a verified ready component")
	ErrAuthorizationUnavailable = errors.New("desktop administrator authorization is unavailable")
	ErrNoDisplay                = errors.New("desktop setup explanation is unavailable")
)

type EmbeddedCapsule struct {
	unsigned     []byte
	signed       []byte
	constants    BuildConstants
	helperSHA512 string
	keys         []ed25519.PublicKey
}
type SetupReason int

const (
	InitialSetup SetupReason = iota
	RepairSetup
	UpdateSetup
)

type SetupOutcome int

const (
	AlreadyReady SetupOutcome = iota
	Prepared
	Cancelled
	Failed
)

// Status is the fixed public --check response, never a filesystem authority.
// The native maintainer must validate actual root-owned component state first.
type Status struct {
	Format             int    `json:"format"`
	Status             string `json:"status"`
	Generation         int64  `json:"generation"`
	CompatibilityEpoch int64  `json:"compatibilityEpoch"`
	SandboxAPI         int64  `json:"sandboxApi"`
	MaintainerSHA512   string `json:"maintainerSha512"`
	HelperSHA512       string `json:"helperSha512"`
	resident           bool
}
type authorizationRequest struct {
	executable string
	args       []string
	input      []byte
}
type authorizationResult struct {
	exitCode    int
	brokenPipe  bool
	writeFailed bool
}
type ensureDependencies struct {
	check     func() (Status, error)
	explain   func() error
	authorize func(authorizationRequest) authorizationResult
}

// NewEmbeddedCapsule accepts ONLY the application's compiled initial bytes and
// public keys, never renderer-selected programs/paths. It checks structural trust
// at publication time, not current admission time: committed old helpers remain
// usable offline. The privileged bootstrap/maintainer checks freshness again.
func NewEmbeddedCapsule(raw []byte, keys []ed25519.PublicKey) (EmbeddedCapsule, error) {
	c, err := desktopsandbox.ParseCapsule(bytes.NewReader(raw), desktopsandbox.Limits{})
	if err != nil || len(c.Signature) != 0 {
		return EmbeddedCapsule{}, errBuildConstants
	}
	var m desktopsandbox.Manifest
	if json.Unmarshal(c.Manifest, &m) != nil {
		return EmbeddedCapsule{}, errBuildConstants
	}
	pub, err := time.Parse(time.RFC3339Nano, m.PublishedAt)
	if err != nil {
		return EmbeddedCapsule{}, errBuildConstants
	}
	self := sha512.Sum512(c.Maintainer)
	selfHash := base64.StdEncoding.EncodeToString(self[:])
	trust := desktopsandbox.Trust{PublicKeys: keys, Initial: &desktopsandbox.InitialTrust{Generation: m.Generation, CompatibilityEpoch: m.CompatibilityEpoch, SandboxAPI: m.SandboxAPI, PublishedAt: m.PublishedAt, ExpiresAt: m.ExpiresAt, Helper: m.Helper, SelfMaintainerSHA512: selfHash}}
	if _, err = desktopsandbox.VerifyCapsule(c, trust, desktopsandbox.Receipt{}, pub); err != nil {
		return EmbeddedCapsule{}, errBuildConstants
	}
	whole := sha512.Sum512(raw)
	metadata := sha512.Sum512(c.Manifest)
	constants := BuildConstants{CapsuleSize: int64(len(raw)), CapsuleSHA512: base64.StdEncoding.EncodeToString(whole[:]), ManifestSHA512: base64.StdEncoding.EncodeToString(metadata[:]), MaintainerOffset: 32 + int64(len(c.Manifest)), MaintainerSize: int64(len(c.Maintainer)), MaintainerSHA512: selfHash, Generation: m.Generation, CompatibilityEpoch: m.CompatibilityEpoch, SandboxAPI: m.SandboxAPI, PublishedAt: m.PublishedAt, ExpiresAt: m.ExpiresAt}
	if _, err = BootstrapProgram(constants); err != nil {
		return EmbeddedCapsule{}, err
	}
	cloned := make([]ed25519.PublicKey, len(keys))
	for i := range keys {
		cloned[i] = bytes.Clone(keys[i])
	}
	return EmbeddedCapsule{unsigned: bytes.Clone(raw), constants: constants, helperSHA512: m.Helper.SHA512, keys: cloned}, nil
}

// WithSignature attaches only a dedicated detached signature to those same
// compiled bytes. No alternate component/key/path is accepted by this API.
func (e EmbeddedCapsule) WithSignature(signature []byte) (EmbeddedCapsule, error) {
	if len(e.unsigned) == 0 || len(signature) != 64 {
		return EmbeddedCapsule{}, ErrSignedSidecarRequired
	}
	c, err := desktopsandbox.ParseCapsule(bytes.NewReader(e.unsigned), desktopsandbox.Limits{})
	if err != nil {
		return EmbeddedCapsule{}, errBuildConstants
	}
	c.Signature = bytes.Clone(signature)
	pub, err := time.Parse(time.RFC3339Nano, e.constants.PublishedAt)
	if err != nil {
		return EmbeddedCapsule{}, errBuildConstants
	}
	if _, err = desktopsandbox.VerifyCapsule(c, desktopsandbox.Trust{PublicKeys: e.keys}, desktopsandbox.Receipt{}, pub); err != nil {
		return EmbeddedCapsule{}, ErrSignedSidecarRequired
	}
	header := bytes.Clone(e.unsigned[:32])
	binary.LittleEndian.PutUint32(header[12:], 64)
	encoded := make([]byte, 0, len(e.unsigned)+64)
	encoded = append(encoded, header...)
	encoded = append(encoded, c.Manifest...)
	encoded = append(encoded, c.Signature...)
	encoded = append(encoded, c.Maintainer...)
	encoded = append(encoded, c.Helper...)
	e.signed = encoded
	return e, nil
}

// Ensure does not run Electron or mutate application profiles. Authorization
// happens only through fixed OS programs; success needs a new native ready check.
func Ensure(e EmbeddedCapsule, reason SetupReason) (SetupOutcome, error) {
	return ensure(e, reason, ensureDependencies{check: checkStatus, explain: showInitialExplanation, authorize: authorize})
}
func ensure(e EmbeddedCapsule, reason SetupReason, deps ensureDependencies) (SetupOutcome, error) {
	if len(e.unsigned) == 0 || reason < InitialSetup || reason > UpdateSetup || deps.check == nil || deps.explain == nil || deps.authorize == nil {
		return Failed, errBuildConstants
	}
	before, err := deps.check()
	if err != nil || !validStatus(before) {
		return Failed, ErrSetupFailed
	}
	if satisfied(before, e) {
		return AlreadyReady, nil
	}
	if before.Status == "ready" && before.Generation == e.constants.Generation {
		return Failed, ErrSetupFailed
	}
	if before.Status == "unsafe" {
		return Failed, ErrSetupFailed
	}
	input := e.unsigned
	var args []string
	if before.resident {
		if len(e.signed) == 0 && before.Status != "maintenance" {
			return Failed, ErrSignedSidecarRequired
		}
		args = []string{"--disable-internal-agent", residentExecutable}
		if len(e.signed) > 0 {
			input = e.signed
		}
	} else {
		args, err = BootstrapArgv(e.constants)
		if err != nil {
			return Failed, err
		}
		if err = deps.explain(); err != nil {
			if errors.Is(err, ErrCancelled) {
				return Cancelled, ErrCancelled
			}
			return Failed, ErrNoDisplay
		}
	}
	result := deps.authorize(authorizationRequest{authorizationExecutable, args, bytes.Clone(input)})
	if result.exitCode == 126 {
		return Cancelled, ErrCancelled
	}
	if result.exitCode == 127 {
		return Failed, ErrAuthorizationUnavailable
	}
	if result.exitCode != 0 || result.writeFailed && !result.brokenPipe {
		return Failed, ErrSetupFailed
	}
	after, err := deps.check()
	if err != nil || !validStatus(after) || !satisfied(after, e) {
		return Failed, ErrSetupFailed
	}
	// EPIPE is tolerated only here: the recognized resident may have completed
	// prior authorized recovery without consuming new bytes. Exit0 alone is insufficient.
	return Prepared, nil
}
func validHash(s string) bool {
	b, err := base64.StdEncoding.Strict().DecodeString(s)
	return err == nil && len(b) == 64 && base64.StdEncoding.EncodeToString(b) == s
}
func validStatus(s Status) bool {
	if s.Format != 1 {
		return false
	}
	switch s.Status {
	case "missing", "maintenance", "removed", "unsafe", "ready":
	default:
		return false
	}
	if s.Generation == 0 {
		return s.Status != "ready" && s.Status != "removed" && s.CompatibilityEpoch == 0 && s.SandboxAPI == 0 && s.MaintainerSHA512 == "" && s.HelperSHA512 == ""
	}
	return s.Status != "missing" && s.Generation > 0 && s.Generation <= desktopsandbox.MaxSafeInteger && s.CompatibilityEpoch == 1 && s.SandboxAPI == 1 && validHash(s.MaintainerSHA512) && validHash(s.HelperSHA512)
}
func satisfied(s Status, e EmbeddedCapsule) bool {
	return s.Status == "ready" && s.Generation >= e.constants.Generation && (s.Generation != e.constants.Generation || s.MaintainerSHA512 == e.constants.MaintainerSHA512 && s.HelperSHA512 == e.helperSHA512)
}
func parseStatus(raw []byte) (Status, error) {
	if len(raw) == 0 || len(raw) > 4096 {
		return Status{}, ErrSetupFailed
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		return Status{}, ErrSetupFailed
	}
	seen := map[string]bool{}
	allowed := map[string]bool{"format": true, "status": true, "generation": true, "compatibilityEpoch": true, "sandboxApi": true, "maintainerSha512": true, "helperSha512": true}
	for decoder.More() {
		token, e := decoder.Token()
		key, ok := token.(string)
		if e != nil || !ok || !allowed[key] || seen[key] {
			return Status{}, ErrSetupFailed
		}
		seen[key] = true
		var value json.RawMessage
		if decoder.Decode(&value) != nil || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return Status{}, ErrSetupFailed
		}
	}
	end, err := decoder.Token()
	if err != nil || end != json.Delim('}') || len(seen) != len(allowed) {
		return Status{}, ErrSetupFailed
	}
	if _, err = decoder.Token(); err != io.EOF {
		return Status{}, ErrSetupFailed
	}
	var s Status
	if json.Unmarshal(raw, &s) != nil || !validStatus(s) {
		return Status{}, ErrSetupFailed
	}
	return s, nil
}

type limitedOutput struct {
	bytes.Buffer
	overflow bool
}

func (w *limitedOutput) Write(b []byte) (int, error) {
	count := len(b)
	if w.Len()+count > 4096 {
		w.overflow = true
	}
	if available := 4096 - w.Len(); available > 0 {
		if len(b) > available {
			b = b[:available]
		}
		_, _ = w.Buffer.Write(b)
	}
	return count, nil
}
func authorize(request authorizationRequest) authorizationResult {
	if request.executable != authorizationExecutable {
		return authorizationResult{exitCode: 127}
	}
	// Administrator interaction is intentionally not killed on a frontend timer.
	// Privileged input/operation bounds belong to the fixed bootstrap/maintainer.
	command := exec.CommandContext(context.Background(), authorizationExecutable, request.args...)
	command.Env = authorizationEnvironment()
	var output, diagnostic limitedOutput
	command.Stdout = &output
	command.Stderr = &diagnostic
	input, err := command.StdinPipe()
	if err != nil {
		return authorizationResult{exitCode: 127}
	}
	if err = command.Start(); err != nil {
		_ = input.Close()
		return authorizationResult{exitCode: 127}
	}
	written := make(chan error, 1)
	go func() {
		_, writeErr := io.Copy(input, bytes.NewReader(request.input))
		closeErr := input.Close()
		if writeErr == nil {
			writeErr = closeErr
		}
		written <- writeErr
	}()
	err = command.Wait()
	writeErr := <-written
	status := 0
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		status = exit.ExitCode()
	} else if err != nil {
		status = 127
	}
	if output.overflow || diagnostic.overflow {
		status = 1
	}
	return authorizationResult{exitCode: status, brokenPipe: errors.Is(writeErr, syscall.EPIPE), writeFailed: writeErr != nil}
}
func authorizationEnvironment() []string {
	result := []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C", "LC_ALL=C"}
	for _, name := range []string{"DISPLAY", "WAYLAND_DISPLAY", "XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS", "XAUTHORITY"} {
		if value, ok := os.LookupEnv(name); ok {
			result = append(result, name+"="+value)
		}
	}
	return result
}
func checkStatus() (Status, error) {
	present, resident, err := trustedResident()
	if err != nil {
		return Status{}, ErrSetupFailed
	}
	if !resident {
		kind := "missing"
		if present {
			kind = "maintenance"
		}
		return Status{Format: 1, Status: kind}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, residentExecutable, "--check")
	command.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C", "LC_ALL=C"}
	var output, diagnostic limitedOutput
	command.Stdout = &output
	command.Stderr = &diagnostic
	if command.Run() != nil || output.overflow || diagnostic.overflow {
		return Status{}, ErrSetupFailed
	}
	s, err := parseStatus(output.Bytes())
	if err != nil {
		return Status{}, err
	}
	s.resident = true
	return s, nil
}

// This is only an unprivileged read/execute check of fixed protected ancestors.
// The privileged bootstrap repeats full root-file/ACL checks under its lock.
func trustedResident() (bool, bool, error) {
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return false, false, ErrSetupFailed
	}
	defer func() { _ = unix.Close(fd) }()
	parts := strings.Split(strings.TrimPrefix(residentExecutable, "/"), "/")
	present := false
	for index, part := range parts {
		flags := unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_CLOEXEC | unix.O_NONBLOCK
		if index < len(parts)-1 {
			flags |= unix.O_DIRECTORY
		}
		child, e := unix.Openat(fd, part, flags, 0)
		if errors.Is(e, unix.ENOENT) {
			return present, false, nil
		}
		if e != nil {
			return false, false, ErrSetupFailed
		}
		var info unix.Stat_t
		e = unix.Fstat(child, &info)
		valid := e == nil && info.Uid == 0 && info.Mode&0o6022 == 0
		if index == len(parts)-1 {
			valid = valid && info.Mode&unix.S_IFMT == unix.S_IFREG && info.Mode&0o7777 == 0o755 && info.Nlink == 1
		}
		if !valid {
			_ = unix.Close(child)
			return false, false, ErrSetupFailed
		}
		_ = unix.Close(fd)
		fd = child
		if index == len(parts)-2 {
			present = true
		}
	}
	return true, true, nil
}

var gtkLock sync.Mutex

func showInitialExplanation() error {
	gtkLock.Lock()
	defer gtkLock.Unlock()
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	switch int(C.dilla_explain_initial_setup()) {
	case 1:
		return nil
	case 2:
		return ErrCancelled
	default:
		return ErrNoDisplay
	}
}
