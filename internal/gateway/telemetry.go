package gateway

import (
	"context"
	"sync"
	"time"

	"github.com/jonasthim/dilla/internal/auth"
	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/id"
)

const telemetryWorkers = 8
const telemetryDeviceCap = 128
const telemetryShapeCap = 1024
const telemetryMaxAge = 10 * time.Second
const telemetryWorkTimeout = 2 * time.Second

type callStatsAcceptor func(context.Context, auth.Session, id.ID, CallStatsReport) error

type telemetrySample struct {
	conn   *conn
	call   id.ID
	report CallStatsReport
	at     time.Time
}
type telemetrySlot struct {
	active  *telemetrySample
	latest  *telemetrySample
	running bool
	cancel  context.CancelFunc
}

// TelemetrySnapshot is aggregate, content-free queue observability.
type TelemetrySnapshot struct {
	Devices, InFlight  int
	Coalesced, Dropped uint64
}

type telemetryPool struct {
	g                  *Gateway
	accept             callStatsAcceptor
	mu                 sync.Mutex
	slots              map[id.ID]*telemetrySlot
	ready              chan id.ID
	ctx                context.Context
	cancel             context.CancelFunc
	started, stopped   bool
	wg                 sync.WaitGroup
	done               chan struct{}
	coalesced, dropped uint64
}

// WithCallStats is set by the composition root before serving requests. The fixed pool starts
// lazily on its first report and is owned by Shutdown, not by a socket or an unbounded job list.
func (g *Gateway) WithCallStats(accept func(context.Context, auth.Session, id.ID, CallStatsReport) error) *Gateway {
	g.mu.Lock()
	defer g.mu.Unlock()
	if accept != nil && g.telemetry == nil {
		ctx, cancel := context.WithCancel(context.Background())
		g.telemetry = &telemetryPool{g: g, accept: accept, slots: map[id.ID]*telemetrySlot{}, ready: make(chan id.ID, telemetryDeviceCap), ctx: ctx, cancel: cancel, done: make(chan struct{})}
	}
	return g
}

func (g *Gateway) telemetryPool() *telemetryPool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.telemetry
}

func (g *Gateway) TelemetryStats() TelemetrySnapshot {
	p := g.telemetryPool()
	if p == nil {
		return TelemetrySnapshot{}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	out := TelemetrySnapshot{Devices: len(p.slots), Coalesced: p.coalesced, Dropped: p.dropped}
	for _, slot := range p.slots {
		if slot.running {
			out.InFlight++
		}
	}
	return out
}

func (p *telemetryPool) enqueue(job *telemetrySample) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopped {
		p.dropped++
		return
	}
	if slot := p.slots[job.conn.deviceID]; slot != nil {
		if slot.running {
			if slot.latest != nil {
				p.coalesced++
			}
			slot.latest = job
		} else {
			slot.active = job
			p.coalesced++
		}
		return
	}
	if len(p.slots) >= telemetryDeviceCap {
		p.dropped++
		return
	}
	if !p.started {
		p.started = true
		for i := 0; i < telemetryWorkers; i++ {
			p.wg.Add(1)
			go p.run()
		}
		go func() { p.wg.Wait(); close(p.done) }()
	}
	p.slots[job.conn.deviceID] = &telemetrySlot{active: job}
	select {
	case p.ready <- job.conn.deviceID:
	default:
		delete(p.slots, job.conn.deviceID)
		p.dropped++
	}
}

func (p *telemetryPool) run() {
	defer p.wg.Done()
	for {
		select {
		case <-p.ctx.Done():
			return
		case device := <-p.ready:
			p.mu.Lock()
			slot := p.slots[device]
			if slot == nil || slot.running || p.stopped {
				p.mu.Unlock()
				continue
			}
			job := slot.active
			ctx, cancel := context.WithTimeout(p.ctx, telemetryWorkTimeout)
			slot.running = true
			slot.cancel = cancel
			p.mu.Unlock()
			if p.g.opts.Clock.Now().Sub(job.at) <= telemetryMaxAge {
				if session, err := p.g.currentSignalSession(ctx, job.conn); err == nil && ctx.Err() == nil {
					_ = p.accept(ctx, session, job.call, job.report) // A refused/lost coarse sample is never retried.
				}
			}
			cancel()
			p.mu.Lock()
			slot.running = false
			slot.cancel = nil
			if slot.latest != nil && !p.stopped {
				slot.active = slot.latest
				slot.latest = nil
				select {
				case p.ready <- device:
				default:
					delete(p.slots, device)
					p.dropped++
				}
			} else {
				delete(p.slots, device)
			}
			p.mu.Unlock()
		}
	}
}

// Removal cancels an in-flight query and discards this socket's queued sample. Another live
// socket of the same device may still supply its own latest input; no sample migrates on resume.
func (p *telemetryPool) disconnect(c *conn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	slot := p.slots[c.deviceID]
	if slot == nil {
		return
	}
	if slot.latest != nil && slot.latest.conn == c {
		slot.latest = nil
		p.dropped++
	}
	if slot.active.conn == c {
		if slot.running {
			if slot.cancel != nil {
				slot.cancel()
			}
		} else {
			delete(p.slots, c.deviceID)
			p.dropped++
		}
	}
}

func (p *telemetryPool) stop(ctx context.Context) error {
	p.mu.Lock()
	p.stopped = true
	p.cancel()
	for device, slot := range p.slots {
		slot.latest = nil
		if slot.cancel != nil {
			slot.cancel()
		}
		if !slot.running {
			delete(p.slots, device)
		}
	}
	started := p.started
	p.mu.Unlock()
	if !started {
		return nil
	}
	select {
	case <-p.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (g *Gateway) handleCallStats(c *conn, in Inbound) {
	if g.connectionWire(c) < 5 {
		g.signalError(c, "E_VERSION")
		return
	}
	if in.CID != 0 || in.GroupID != nil || len(in.Payload) != 2 || 1+len(in.Payload[0])+len(in.Payload[1]) > telemetryShapeCap {
		g.signalError(c, "E_FRAME_SHAPE")
		return
	}
	var call id.ID
	var report CallStatsReport
	if err := cborx.Unmarshal(in.Payload[0], &call); err != nil {
		g.signalError(c, "E_FRAME_SHAPE")
		return
	}
	if err := cborx.Unmarshal(in.Payload[1], &report); err != nil || report.Validate() != nil {
		g.signalError(c, "E_FRAME_SHAPE")
		return
	}
	c.mu.Lock()
	eligible := c.state == stateReady && !c.isBot && c.scope == auth.ScopeEnrolled
	c.mu.Unlock()
	if !eligible {
		return
	}
	if pool := g.telemetryPool(); pool != nil {
		pool.enqueue(&telemetrySample{conn: c, call: call, report: report, at: g.opts.Clock.Now()})
	}
}
