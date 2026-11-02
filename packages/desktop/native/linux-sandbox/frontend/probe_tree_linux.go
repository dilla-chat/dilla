//go:build linux && cgo

package frontend

import (
	"errors"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// All numeric process paths below originate from the kernel children lists of
// already owned processes. Process directories stay open through observation.
type ownedProc struct {
	pid, ppid, pgrp int
	start, state    string
	dir, pidfd      int
}
type ownedTree struct {
	rootFD   int
	nodes    map[int]*ownedProc
	deadline time.Time
	links    map[int]string
}

func (t *ownedTree) close() {
	for _, p := range t.nodes {
		_ = unix.Close(p.dir)
		_ = unix.Close(p.pidfd)
	}
	if t.rootFD >= 0 {
		_ = unix.Close(t.rootFD)
	}
}
func readAt(dir int, name string, limit int) (string, error) {
	fd, e := unix.Openat(dir, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if e != nil {
		return "", e
	}
	f := os.NewFile(uintptr(fd), name)
	defer func() { _ = f.Close() }()
	b, e := io.ReadAll(io.LimitReader(f, int64(limit+1)))
	if e != nil || len(b) > limit {
		return "", errProbeReport
	}
	return string(b), nil
}
func listAt(dir int, name string, limit int) ([]string, error) {
	fd, e := unix.Openat(dir, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if e != nil {
		return nil, e
	}
	f := os.NewFile(uintptr(fd), name)
	defer func() { _ = f.Close() }()
	names, e := f.Readdirnames(limit + 1)
	if e != nil && !errors.Is(e, io.EOF) {
		return nil, e
	}
	if len(names) > limit {
		return nil, errProbeReport
	}
	sort.Strings(names)
	return names, nil
}
func parseOwnedStat(raw string) (state string, ppid, pgrp int, start string, err error) {
	i := strings.LastIndexByte(raw, ')')
	if i < 0 {
		return "", 0, 0, "", errProbeReport
	}
	f := strings.Fields(raw[i+1:])
	if len(f) < 20 {
		return "", 0, 0, "", errProbeReport
	}
	ppid, err = strconv.Atoi(f[1])
	if err != nil {
		return "", 0, 0, "", errProbeReport
	}
	pgrp, err = strconv.Atoi(f[2])
	if err != nil {
		return "", 0, 0, "", errProbeReport
	}
	if _, err = strconv.ParseUint(f[19], 10, 64); err != nil {
		return "", 0, 0, "", errProbeReport
	}
	return f[0], ppid, pgrp, f[19], nil
}
func (t *ownedTree) pin(pid, expectedParent int) (*ownedProc, error) {
	if pid < 2 || !time.Now().Before(t.deadline) {
		return nil, errProbeReport
	}
	fd, e := unix.Openat(t.rootFD, strconv.Itoa(pid), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if e != nil {
		return nil, e
	}
	ok := false
	defer func() {
		if !ok {
			_ = unix.Close(fd)
		}
	}()
	raw, e := readAt(fd, "stat", 8192)
	if e != nil {
		return nil, e
	}
	state, parent, group, start, e := parseOwnedStat(raw)
	if e != nil || parent != expectedParent || state == "Z" || state == "X" {
		return nil, errProbeReport
	}
	pidfd, e := unix.PidfdOpen(pid, 0)
	if e != nil {
		return nil, e
	}
	defer func() {
		if !ok {
			_ = unix.Close(pidfd)
		}
	}()
	raw2, e := readAt(fd, "stat", 8192)
	if e != nil {
		return nil, e
	}
	state2, parent2, group2, start2, e := parseOwnedStat(raw2)
	if e != nil || start != start2 || parent != parent2 || group != group2 || state2 == "Z" || state2 == "X" || unix.PidfdSendSignal(pidfd, 0, nil, 0) != nil {
		return nil, errProbeReport
	}
	p := &ownedProc{pid, parent, group, start, state2, fd, pidfd}
	t.nodes[pid] = p
	ok = true
	return p, nil
}
func readTaskChildren(procDir int, tid string) (string, string, error) {
	fd, e := unix.Openat(procDir, "task/"+tid, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if e != nil {
		return "", "", e
	}
	defer func() { _ = unix.Close(fd) }()
	before, e := readAt(fd, "stat", 8192)
	if e != nil {
		return "", "", e
	}
	state, _, _, start, e := parseOwnedStat(before)
	if e != nil || state == "Z" || state == "X" {
		return "", "", errProbeReport
	}
	children, e := readAt(fd, "children", 32768)
	if e != nil {
		return "", "", e
	}
	after, e := readAt(fd, "stat", 8192)
	if e != nil {
		return "", "", e
	}
	state, _, _, afterStart, e := parseOwnedStat(after)
	if e != nil || state == "Z" || state == "X" || start != afterStart {
		return "", "", errProbeReport
	}
	return start, children, nil
}
func (t *ownedTree) readLinks(pid int) ([]int, string, error) {
	if !time.Now().Before(t.deadline) {
		return nil, "", errProbeReport
	}
	p := t.nodes[pid]
	if p == nil {
		return nil, "", errProbeReport
	}
	tasks, e := listAt(p.dir, "task", 512)
	if e != nil || len(tasks) == 0 {
		return nil, "", errProbeReport
	}
	children := []int{}
	var signature strings.Builder
	for _, tid := range tasks {
		n, e := strconv.Atoi(tid)
		if e != nil || n < 1 || strconv.Itoa(n) != tid {
			return nil, "", errProbeReport
		}
		threadStart, data, e := readTaskChildren(p.dir, tid)
		if e != nil {
			return nil, "", errProbeReport
		}
		words := strings.Fields(data)
		sort.Strings(words)
		signature.WriteString(tid)
		signature.WriteByte('@')
		signature.WriteString(threadStart)
		signature.WriteByte(':')
		for _, item := range words {
			child, e := strconv.Atoi(item)
			if e != nil || child < 2 || strconv.Itoa(child) != item || len(children) >= 512 {
				return nil, "", errProbeReport
			}
			children = append(children, child)
			signature.WriteString(item)
			signature.WriteByte(',')
		}
		signature.WriteByte(';')
	}
	return children, signature.String(), nil
}
func (t *ownedTree) children(pid int) ([]int, error) {
	p := t.nodes[pid]
	children, signature, e := t.readLinks(pid)
	if e != nil {
		return nil, e
	}
	t.links[pid] = signature
	for _, child := range children {
		if len(t.nodes) >= 512 {
			return nil, errProbeReport
		}
		if _, exists := t.nodes[child]; exists {
			return nil, errProbeReport
		}
		if _, e = t.pin(child, pid); e != nil {
			return nil, errProbeReport
		}
	}
	before, e := readAt(p.dir, "stat", 8192)
	if e != nil {
		return nil, e
	}
	state, parent, group, start, e := parseOwnedStat(before)
	if e != nil || state == "Z" || state == "X" || parent != p.ppid || group != p.pgrp || start != p.start {
		return nil, errProbeReport
	}
	return children, nil
}
func verifyStableLinks(recorded map[int]string, read func(int) (string, error)) error {
	for pid, expected := range recorded {
		actual, e := read(pid)
		if e != nil || actual != expected {
			return errProbeReport
		}
	}
	return nil
}
func openOwnedTree(mainPID, uid int, deadline time.Time) (*ownedTree, error) {
	root, e := unix.Open("/proc", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if e != nil {
		return nil, e
	}
	tree := &ownedTree{rootFD: root, nodes: map[int]*ownedProc{}, deadline: deadline, links: map[int]string{}}
	if _, e = tree.pin(mainPID, os.Getpid()); e != nil {
		tree.close()
		return nil, e
	}
	return tree, nil
}
func inspectOwnedTree(mainPID, rendererPID, uid int, deadline time.Time, known map[int]knownProcess) error {
	t, e := openOwnedTree(mainPID, uid, deadline)
	if e != nil {
		return errProbeReport
	}
	defer t.close()
	return walkOwned(mainPID, rendererPID, t.children, func(pid int) error {
		if e := verifyStableLinks(t.links, func(id int) (string, error) { _, signature, e := t.readLinks(id); return signature, e }); e != nil {
			return e
		}
		renderer := t.nodes[pid]
		for _, p := range t.nodes {
			if p.pgrp != mainPID {
				return errProbeReport
			}
			raw, e := readAt(p.dir, "status", 65536)
			if e != nil {
				return errProbeReport
			}
			st, e := parseProcessStatus(raw, uid, p.pid == pid)
			if e != nil || st.ppid != p.ppid {
				return errProbeReport
			}
		}
		cmd, e := readAt(renderer.dir, "cmdline", 65536)
		if e != nil {
			return errProbeReport
		}
		found := false
		for _, arg := range strings.Split(cmd, "\x00") {
			if arg == "--type=renderer" {
				found = true
			}
		}
		if !found {
			return errProbeReport
		}
		for _, p := range t.nodes {
			raw, e := readAt(p.dir, "stat", 8192)
			if e != nil {
				return errProbeReport
			}
			state, parent, group, start, e := parseOwnedStat(raw)
			if e != nil || state == "Z" || state == "X" || parent != p.ppid || group != p.pgrp || start != p.start || unix.PidfdSendSignal(p.pidfd, 0, nil, 0) != nil {
				return errProbeReport
			}
		}
		if e := verifyStableLinks(t.links, func(pid int) (string, error) { _, signature, e := t.readLinks(pid); return signature, e }); e != nil {
			return e
		}
		if !time.Now().Before(deadline) {
			return errProbeReport
		}
		for id, p := range t.nodes {
			if id != mainPID {
				fd, e := unix.Dup(p.pidfd)
				if e != nil {
					for _, record := range known {
						_ = unix.Close(record.pidfd)
					}
					clear(known)
					return errProbeReport
				}
				unix.CloseOnExec(fd)
				known[id] = knownProcess{start: p.start, pidfd: fd}
			}
		}
		return nil
	})
}

// collectCleanupTree derives every live descendant from the still-owned main
// before group signaling, without consulting reported PIDs. Unstable or escaped
// topology fails closed; no signal is sent by this function.
func collectCleanupTree(mainPID, uid int, deadline time.Time, known map[int]knownProcess) error {
	t, e := openOwnedTree(mainPID, uid, deadline)
	if e != nil {
		return errProbeCleanup
	}
	defer t.close()
	queue := []int{mainPID}
	seen := map[int]bool{mainPID: true}
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		children, e := t.children(pid)
		if e != nil {
			return errProbeCleanup
		}
		for _, child := range children {
			if seen[child] || len(seen) >= 512 {
				return errProbeCleanup
			}
			seen[child] = true
			queue = append(queue, child)
		}
	}
	for id, p := range t.nodes {
		if p.pgrp != mainPID {
			return errProbeCleanup
		}
		raw, e := readAt(p.dir, "status", 65536)
		if e != nil {
			return errProbeCleanup
		}
		st, e := parseProcessStatus(raw, uid, false)
		if e != nil || st.ppid != p.ppid {
			return errProbeCleanup
		}
		if id == mainPID {
			continue
		}
		if prior, ok := known[id]; ok {
			if prior.start != p.start || unix.PidfdSendSignal(prior.pidfd, 0, nil, 0) != nil {
				return errProbeCleanup
			}
		} else {
			fd, e := unix.Dup(p.pidfd)
			if e != nil {
				return errProbeCleanup
			}
			unix.CloseOnExec(fd)
			known[id] = knownProcess{start: p.start, pidfd: fd}
		}
	}
	if e := verifyStableLinks(t.links, func(pid int) (string, error) { _, sig, e := t.readLinks(pid); return sig, e }); e != nil {
		return errProbeCleanup
	}
	if !time.Now().Before(deadline) {
		return errProbeCleanup
	}
	return nil
}
