//go:build linux && cgo

package frontend

import (
	"strconv"
	"strings"
)

type procStatus struct{ ppid int }

func parseProcessStatus(raw string, uid int, renderer bool) (procStatus, error) {
	var out procStatus
	fields := map[string]string{}
	for _, line := range strings.Split(raw, "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch key {
		case "PPid", "Uid", "Seccomp", "NoNewPrivs", "CapEff":
			if _, exists := fields[key]; exists {
				return out, errProbeReport
			}
			fields[key] = strings.TrimSpace(value)
		}
	}
	parent, e := strconv.Atoi(fields["PPid"])
	if e != nil || parent < 1 {
		return out, errProbeReport
	}
	out.ppid = parent
	u := strings.Fields(fields["Uid"])
	if len(u) != 4 {
		return out, errProbeReport
	}
	for _, v := range u {
		n, e := strconv.Atoi(v)
		if e != nil || n != uid || uid == 0 {
			return out, errProbeReport
		}
	}
	if renderer && (fields["Seccomp"] != "2" || fields["NoNewPrivs"] != "1" || strings.Trim(fields["CapEff"], "0") != "" || fields["CapEff"] == "") {
		return out, errProbeReport
	}
	return out, nil
}
