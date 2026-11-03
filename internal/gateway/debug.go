package gateway

import (
	"time"

	"github.com/dilla-chat/dilla/internal/id"
)

// DebugReport is the gateway's side of one device in one group, for a harness that has to explain
// why a client stopped receiving while its socket stayed open: was the device in the group's
// fan-out list, did each of its connections get the frames (enqueued), and did the writer put them
// on the wire (written)? It is read-only and assembled from the same state fan-out reads; the
// composition root exposes it only through the test host's control listener, never on /v1.
type DebugReport struct {
	// Device and Group are hex, as the control listener reads and writes ids.
	Device string `json:"device"`
	Group  string `json:"group"`

	// Conns is every LIVE connection of the device (the registry's), in registry order.
	Conns []DebugConn `json:"conns"`
	// Suspended counts the device's connections parked in the resume window.
	Suspended int `json:"suspended"`

	// InMembers is whether the device is in the group's fan-out list (SetGroupMembers), and
	// Members that list's length.
	InMembers bool `json:"in_members"`
	Members   int  `json:"members"`
	// Leaf is the leaf index SetGroupLeaves recorded for the device, nil when none is.
	Leaf *uint32 `json:"leaf"`
}

// DebugConn is one connection's state. Its writer's counters are the CURRENT writer's: a resume
// replaces the writer, and the counters start again from zero with it.
type DebugConn struct {
	State    string    `json:"state"`
	LastLive time.Time `json:"last_live"`
	Ready    time.Time `json:"ready"`
	// N is the last n stamped on a replayable frame for this connection.
	N          uint64 `json:"n"`
	Subscribed int    `json:"subscribed"`

	RingFrames int    `json:"ring_frames"`
	RingBytes  int    `json:"ring_bytes"`
	RingFloor  uint64 `json:"ring_floor"`
	RingTop    uint64 `json:"ring_top"`

	HasWriter      bool   `json:"has_writer"`
	QueuedFrames   int    `json:"queued_frames"`
	QueuedBytes    int    `json:"queued_bytes"`
	Enqueued       uint64 `json:"enqueued"`
	Written        uint64 `json:"written"`
	WriterGateOpen bool   `json:"writer_gate_open"`
	WriterStopped  bool   `json:"writer_stopped"`
	WriterFinished bool   `json:"writer_finished"`
}

var stateNames = map[connState]string{stateNew: "new", stateReady: "ready", stateClosed: "closed"}

// Debug reports what the gateway holds about deviceID and its membership of groupID. It changes
// nothing: no ring is evicted, no writer touched.
func (g *Gateway) Debug(deviceID, groupID id.ID) DebugReport {
	r := DebugReport{Device: deviceID.String(), Group: groupID.String(), Conns: []DebugConn{}}
	for _, c := range g.reg.connsOfDevice(deviceID) {
		r.Conns = append(r.Conns, c.debug())
	}
	g.suspended.Range(func(_, v any) bool {
		if c, ok := v.(*conn); ok && c.deviceID == deviceID {
			r.Suspended++
		}
		return true
	})
	in, size, leaf, ok := g.reg.memberOf(groupID, deviceID)
	r.InMembers, r.Members = in, size
	if ok {
		r.Leaf = &leaf
	}
	return r
}

// debug snapshots one connection. The connection's own fields are read under its lock; the ring
// and the writer have their own, and are read after it is released.
func (c *conn) debug() DebugConn {
	c.mu.Lock()
	d := DebugConn{
		State:      stateNames[c.state],
		LastLive:   c.lastLive,
		Ready:      c.ready,
		N:          c.n,
		Subscribed: len(c.groups),
	}
	w := c.writer
	c.mu.Unlock()
	d.RingFrames, d.RingBytes, d.RingFloor, d.RingTop = c.ring.stats()
	if w != nil {
		s := w.stats()
		d.HasWriter = true
		d.QueuedFrames, d.QueuedBytes = s.queuedFrames, s.queuedBytes
		d.Enqueued, d.Written = s.enqueued, s.written
		d.WriterGateOpen, d.WriterStopped, d.WriterFinished = s.gateOpen, s.stopped, s.done
	}
	return d
}
