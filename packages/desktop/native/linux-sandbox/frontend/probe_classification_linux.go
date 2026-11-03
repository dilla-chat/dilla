//go:build linux && cgo

package frontend

import "regexp"

type ProbeResult string

const (
	ProbeSandboxed          ProbeResult = "sandboxed"
	ProbeSandboxUnavailable ProbeResult = "sandbox-unavailable"
	ProbeFailed             ProbeResult = "failed"
	ProbeTimedOut           ProbeResult = "timed-out"
)

// The invariant message and source line are pinned to the Electron 45.2.1
// observation. The PID and Chromium timestamp are the only variable fields.
// This correlates a trusted child failure; it is not executable attestation.
var unavailableFatal = regexp.MustCompile(`\A\[[0-9]+:[0-9]{4}/[0-9]{6}\.[0-9]{6}:FATAL:content/browser/zygote_host/zygote_host_impl_linux\.cc:130\] No usable sandbox! If you are running on Ubuntu 23\.10\+ or another Linux distro that has disabled unprivileged user namespaces with AppArmor, see https://chromium\.googlesource\.com/chromium/src/\+/main/docs/security/apparmor-userns-restrictions\.md\. Otherwise see https://chromium\.googlesource\.com/chromium/src/\+/main/docs/linux/suid_sandbox_development\.md for more information on developing with the \(older\) SUID sandbox\. If you want to live dangerously and need an immediate workaround, you can try using --no-sandbox\.\n\z`)

func classifyFailure(owned, ack, malformed, timedOut bool, signal int, stderr string) ProbeResult {
	if timedOut {
		return ProbeTimedOut
	}
	if owned && !ack && !malformed && signal == 5 && len(stderr) <= 65536 && unavailableFatal.MatchString(stderr) {
		return ProbeSandboxUnavailable
	}
	return ProbeFailed
}
