//go:build linux && cgo

package frontend

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
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
func decideLaunch(state string, strict bool) launchDecision {
	switch state {
	case "missing":
		if strict {
			return decisionExec
		}
		return decisionProbe
	case "ready":
		return decisionExec
	case "maintenance":
		if !strict {
			return decisionRepair
		}
	}
	return decisionStop
}

var roleNonce = regexp.MustCompile(`\A[0-9a-f]{64}\z`)
var errRole = errors.New("invalid reserved launch role")

func strictRole(args, env []string) (bool, error) {
	seen := map[string]bool{}
	strict := false
	for _, arg := range args {
		if strings.HasPrefix(arg, "--dilla-sandbox-") || arg == "--setup-sandbox" || strings.HasPrefix(arg, "--setup-sandbox=") || strings.HasPrefix(arg, "--sandbox-") {
			return false, errRole
		}
		if !strings.HasPrefix(arg, "--update-") {
			continue
		}
		key, value, has := strings.Cut(arg, "=")
		if seen[key] {
			return false, errRole
		}
		seen[key] = true
		switch key {
		case "--update-watchdog", "--update-launch":
			if !has || !roleNonce.MatchString(value) {
				return false, errRole
			}
			strict = true
		case "--update-recovery":
			if has {
				return false, errRole
			}
			strict = true
		default:
			return false, errRole
		}
	}
	if seen["--update-recovery"] && !seen["--update-watchdog"] || seen["--update-watchdog"] && seen["--update-launch"] {
		return false, errRole
	}
	markers := map[string]bool{}
	for _, entry := range env {
		key, value, has := strings.Cut(entry, "=")
		switch key {
		case "DILLA_UPDATE_STARTUP_NONCE", "DILLA_UPDATE_RECOVERED_NONCE", "DILLA_UPDATE_PREVIOUS_PID":
			if !has || markers[key] {
				return false, errRole
			}
			markers[key] = true
			strict = true
			if key != "DILLA_UPDATE_PREVIOUS_PID" && !roleNonce.MatchString(value) {
				return false, errRole
			}
			if key == "DILLA_UPDATE_PREVIOUS_PID" {
				n, e := strconv.ParseUint(value, 10, 31)
				if e != nil || n < 2 || strconv.FormatUint(n, 10) != value {
					return false, errRole
				}
			}
		}
	}
	if markers["DILLA_UPDATE_STARTUP_NONCE"] && markers["DILLA_UPDATE_RECOVERED_NONCE"] {
		return false, errRole
	}
	return strict, nil
}

type dispatchDeps struct {
	status func() (string, error)
	probe  func() (ProbeResult, error)
	ensure func() error
	exec   func([]string) error
}

var errLaunch = errors.New("sandbox prerequisite not satisfied")

func dispatch(args []string, strict bool, d dispatchDeps) error {
	if d.status == nil || d.probe == nil || d.ensure == nil || d.exec == nil {
		return errLaunch
	}
	state, e := d.status()
	if e != nil {
		return errLaunch
	}
	switch decideLaunch(state, strict) {
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
	if len(e.unsigned) == 0 {
		return errLaunch
	}
	strict, err := strictRole(args, os.Environ())
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
			if last.resident {
				reason = RepairSetup
			}
			out, err := Ensure(e, reason)
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
			return syscall.Exec(owned.electron, append([]string{owned.electron}, argv...), env)
		},
	}
	return dispatch(args, strict, deps)
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
