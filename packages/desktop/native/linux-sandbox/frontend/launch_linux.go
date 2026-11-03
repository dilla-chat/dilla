//go:build linux && cgo

package frontend

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
)

type launchDecision uint8

const (
	decisionStop launchDecision = iota
	decisionExec
	decisionProbe
	decisionRepair
)

// decideLaunch receives a validated compatibility state. Incompatible ready
// state must be converted to unsafe before reaching this decision.
func decideLaunch(state string) launchDecision {
	switch state {
	case "missing":
		return decisionProbe
	case "ready":
		return decisionExec
	case "maintenance":
		return decisionRepair
	}
	return decisionStop
}

var roleNonce = regexp.MustCompile(`\A[0-9a-f]{64}\z`)
var errRole = errors.New("invalid reserved launch role")

func validateLaunchArgs(args []string) error {
	for _, arg := range args {
		if strings.HasPrefix(arg, "--dilla-sandbox-") || arg == "--setup-sandbox" || strings.HasPrefix(arg, "--setup-sandbox=") || strings.HasPrefix(arg, "--sandbox-") || strings.HasPrefix(arg, "--update-") {
			return errRole
		}
	}
	return nil
}

type dispatchDeps struct {
	status func() (string, error)
	probe  func() (ProbeResult, error)
	ensure func() error
	exec   func([]string) error
}

var errLaunch = errors.New("sandbox prerequisite not satisfied")

func dispatch(args []string, d dispatchDeps) error {
	if d.status == nil || d.probe == nil || d.ensure == nil || d.exec == nil {
		return errLaunch
	}
	state, e := d.status()
	if e != nil {
		return errLaunch
	}
	switch decideLaunch(state) {
	case decisionExec:
		return d.exec(args)
	case decisionProbe:
		result, e := d.probe()
		if e != nil {
			return errLaunch
		}
		if result == ProbeSandboxed {
			return d.exec(args)
		}
		if result != ProbeSandboxUnavailable {
			return errLaunch
		}
	case decisionRepair:
	default:
		return errLaunch
	}
	if d.ensure() != nil {
		return errLaunch
	}
	state, e = d.status()
	if e != nil || state != "ready" {
		return errLaunch
	}
	result, e := d.probe()
	if e != nil || result != ProbeSandboxed {
		return errLaunch
	}
	return d.exec(args)
}

func launchState(s Status, e EmbeddedCapsule) string {
	if len(e.unsigned) == 0 || !validStatus(s) {
		return "unsafe"
	}
	if s.Status == "missing" && s.resident {
		return "unsafe"
	}
	if s.Status != "ready" {
		return s.Status
	}
	if !s.resident {
		return "unsafe"
	}
	if satisfied(s, e) {
		return "ready"
	}
	if s.Generation == e.constants.Generation {
		return "unsafe"
	}
	return "maintenance"
}

// DispatchLaunch is called by the unprivileged launcher with its compiled
// capsule. Runtime arguments cannot choose paths, trust, or setup payloads.
func DispatchLaunch(args []string, e EmbeddedCapsule) error {
	return DispatchLaunchVersion(args, e, "")
}

// DispatchLaunchVersion receives only the product version compiled by packaging.
func DispatchLaunchVersion(args []string, e EmbeddedCapsule, compiledVersion string) error {
	// electron-updater executes the new image for integration when the user quits
	// without relaunching. This stock marker must never open UI or authorize setup.
	if os.Getenv("APPIMAGE_EXIT_AFTER_INSTALL") == "true" {
		if len(args) != 0 {
			return errRole
		}
		return nil
	}
	if len(e.unsigned) == 0 {
		return errLaunch
	}
	nonce, check, err := checkUpdateRole(args, os.Environ())
	if err != nil {
		return err
	}
	if check {
		return prepareUpdate(nonce, e, true)
	}
	nonce, prepare, err := prepareUpdateRole(args, os.Environ())
	if err != nil {
		return err
	}
	if prepare {
		return prepareUpdate(nonce, e, false)
	}
	err = validateLaunchArgs(args)
	if err != nil {
		return err
	}
	owned, err := ownExecutable()
	if err != nil {
		return err
	}
	var last Status
	deps := dispatchDeps{
		status: func() (string, error) {
			s, err := checkStatus()
			last = s
			if err != nil {
				return "unsafe", err
			}
			return launchState(s, e), nil
		},
		probe: func() (ProbeResult, error) { return RunSandboxProbe(owned) },
		ensure: func() error {
			reason := InitialSetup
			capsule := e
			if last.resident {
				reason = RepairSetup
				// Pending maintenance resumes an already authorized resident intent.
				// A new upgrade from ready needs a current dedicated signature.
				if last.Status == "ready" {
					var err error
					capsule, err = manualSignedCapsule(compiledVersion, e)
					if err != nil {
						return errLaunch
					}
				}
			}
			out, err := Ensure(capsule, reason)
			if err != nil || (out != Prepared && out != AlreadyReady) {
				return errLaunch
			}
			return nil
		},
		exec: func(argv []string) error {
			if err := owned.recheck(); err != nil {
				return err
			}
			env := launchEnvironment(os.Environ(), filepath.Dir(owned.electron))
			// #nosec G204 -- fixed sibling rechecked on the owned read-only mount; argv is passed directly without a shell.
			return syscall.Exec(owned.electron, append([]string{owned.electron}, argv...), env)
		},
	}
	return dispatch(args, deps)
}
func launchEnvironment(env []string, appdir string) []string {
	result := make([]string, 0, len(env)+1)
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		switch key {
		case "APPDIR", "CHROME_DEVEL_SANDBOX", "ELECTRON_RUN_AS_NODE", "NODE_OPTIONS", "NODE_PATH", "LD_PRELOAD", "LD_AUDIT":
			continue
		}
		result = append(result, entry)
	}
	return append(result, "APPDIR="+appdir)
}
