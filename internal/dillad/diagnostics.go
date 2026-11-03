package dillad

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/blob"
	"github.com/dilla-chat/dilla/internal/config"
	"github.com/dilla-chat/dilla/internal/mlswasi"
	"github.com/dilla-chat/dilla/internal/ops"
)

// diagnostics is the report GET /v1/admin/diagnostics answers (P2-5): the `dillad doctor` legs
// that can be answered from inside the serving process, under doctor's leg names and in doctor's
// order — the database and its schema version, the data directory's mode, the wasi core the
// delivery service validates in (the runtime this process loaded, not a fresh one), the UDP
// configuration and the blob store's consistency; withCallLegs adds the instance's own legs after
// them. The legs that probe the network or read state
// another process holds (config parsing, the SQLite pragmas as a fresh connection sees them, clock
// skew against remote peers, the certificate, a TURN allocation) stay `dillad doctor`'s: an admin
// request must not make the instance dial out.
func diagnostics(o Options, wasm *mlswasi.Runtime, blobs *blob.Store) func(context.Context) ops.Report {
	return func(ctx context.Context) ops.Report {
		var r ops.Report
		add := func(l ops.Leg) { r.Legs = append(r.Legs, l) }
		red := func(name string, err error) { add(ops.Leg{Name: name, Status: ops.Red, Detail: err.Error()}) }

		if v, err := o.Repo.SchemaVersion(ctx); err != nil {
			red("database", err)
		} else {
			add(ops.Leg{Name: "database", Status: ops.Green, Detail: fmt.Sprintf("schema version %d", v)})
		}
		// StateDirectoryMode=0700 (facts-ops.md §6.3): group and world have no access at all.
		dir := o.Config.Instance.DataDir
		if info, err := os.Stat(dir); err != nil {
			red("data_dir", err)
		} else if perm := info.Mode().Perm(); perm&0o077 != 0 {
			red("data_dir", fmt.Errorf("%s has mode %04o; group and world must have no access", dir, perm))
		} else {
			add(ops.Leg{Name: "data_dir", Status: ops.Green, Detail: fmt.Sprintf("%s mode %04o", dir, perm)})
		}
		if info, err := wasm.ABI(ctx); err != nil {
			red("wasi", err)
		} else {
			add(ops.Leg{Name: "wasi", Status: ops.Green,
				Detail: fmt.Sprintf("abi_version=%d core_version=%s", info.ABIVersion, info.CoreVersion)})
		}
		add(ops.UDPLeg(*o.Config))
		add(ops.WebAuthnLeg(*o.Config))
		add(ops.BlobConsistencyLeg(ctx, o.Repo, blobs))
		return r
	}
}

// callStatsWindow is how far back the calls leg looks.
const callStatsWindow = 15 * time.Minute

// turnState is what the turn leg reads; *obs.Metrics is one.
type turnState interface {
	TURNState() (allocations int, quotaRefusals uint64)
}

// turnRelayLeg is the instance's relay leg: the relay's own counters. It is not `dillad doctor`'s
// "turn" leg, an allocation probe the instance never makes (review M4).
const turnRelayLeg = "turn_relay"

// withCallLegs appends the instance's own calls and turn_relay legs (DEV-59, ruling F10), which
// `dillad doctor` does not have, to base's report. Both read state this process already holds — the
// stats devices reported, the relay's own counters — so the admin request still dials nothing.
func withCallLegs(base func(context.Context) ops.Report, stats *api.CallStats, turnCfg config.TURN, ts turnState) func(context.Context) ops.Report {
	return func(ctx context.Context) ops.Report {
		r := base(ctx)
		r.Legs = append(r.Legs, callsLeg(stats.Summary(ctx, callStatsWindow)))
		allocations, refusals := ts.TURNState()
		r.Legs = append(r.Legs, turnLeg(turnCfg, allocations, refusals))
		return r
	}
}

func callsLeg(s api.StatsSummary) ops.Leg {
	if s.Reports == 0 {
		return ops.Leg{Name: "calls", Status: ops.Green, Detail: "no call reported stats in the last 15 minutes"}
	}
	detail := fmt.Sprintf("%d live calls, %d reports in the last 15 minutes, %d with a relay; RTT p50 %d ms, p95 %d ms; %d decrypt failures against %d frames encrypted%s",
		s.LiveCalls, s.Reports, s.RelayReports, s.P50RTTms, s.P95RTTms, s.DecryptFailures, s.FramesEncrypted, failureRatio(s))
	if s.DecryptFailures > 0 {
		return ops.Leg{Name: "calls", Status: ops.Yellow, Detail: detail,
			Fix: "a client failed to decrypt media: compare dilla_call_decrypt_failures_total with the clients' logs; a steady rate points at key distribution, not the network"}
	}
	return ops.Leg{Name: "calls", Status: ops.Green, Detail: detail}
}

// failureRatio is the decrypt failures per 1000 frames encrypted across the window's reports — a
// rough rate, since a frame one device encrypts is decrypted by every other — or "" with no frames.
func failureRatio(s api.StatsSummary) string {
	if s.FramesEncrypted == 0 {
		return ""
	}
	return fmt.Sprintf(" (%.1f per 1000)", float64(s.DecryptFailures)*1000/float64(s.FramesEncrypted))
}

func turnLeg(c config.TURN, allocations int, refusals uint64) ops.Leg {
	if !c.Enabled {
		return ops.Leg{Name: turnRelayLeg, Status: ops.Green,
			Detail: "the TURN relay is off (turn.enabled = false): a client that cannot reach UDP 7882 cannot join a call"}
	}
	detail := fmt.Sprintf("%d live relay allocations; %d refused at the per-device quota of %d since start",
		allocations, refusals, c.AllocationsPerDevice)
	if refusals > 0 {
		return ops.Leg{Name: turnRelayLeg, Status: ops.Yellow, Detail: detail,
			Fix: fmt.Sprintf("raise turn.allocations_per_device (now %d): a device needs about two allocations per network it gathers on", c.AllocationsPerDevice)}
	}
	return ops.Leg{Name: turnRelayLeg, Status: ops.Green, Detail: detail}
}
