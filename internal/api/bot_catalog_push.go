package api

import (
	"context"
	"sync"

	"github.com/dilla-chat/dilla/internal/id"
)

// botCatalogPushConcurrency bounds how many communities' catalog fan-outs run at once.
const botCatalogPushConcurrency = 4

// BotCatalogPusher runs the op 61 fan-outs of published catalogs in the background, so the Bot's
// PUT answers once its write commits. It is bounded twice: at most one fan-out per community runs or
// waits at a time (a publish during one asks for one more run, which reads the newest catalog, so
// bursts coalesce), and at most botCatalogPushConcurrency run across communities.
type BotCatalogPusher struct {
	mu      sync.Mutex
	running map[id.ID]bool
	again   map[id.ID]bool
	sem     chan struct{}
	wg      sync.WaitGroup
}

// NewBotCatalogPusher is an idle pusher. Register builds one when Deps has none.
func NewBotCatalogPusher() *BotCatalogPusher {
	return &BotCatalogPusher{running: map[id.ID]bool{}, again: map[id.ID]bool{}, sem: make(chan struct{}, botCatalogPushConcurrency)}
}

// schedule runs push for cid in the background, or, when one is already running or waiting for
// cid, asks it to run once more when it finishes.
func (p *BotCatalogPusher) schedule(cid id.ID, push func(context.Context)) {
	p.mu.Lock()
	if p.running[cid] {
		p.again[cid] = true
		p.mu.Unlock()
		return
	}
	p.running[cid] = true
	p.wg.Add(1)
	p.mu.Unlock()
	go func() {
		defer p.wg.Done()
		for {
			p.sem <- struct{}{}
			push(context.Background())
			<-p.sem
			p.mu.Lock()
			if !p.again[cid] {
				delete(p.running, cid)
				p.mu.Unlock()
				return
			}
			delete(p.again, cid)
			p.mu.Unlock()
		}
	}()
}

// Wait returns once every scheduled fan-out has finished (tests and shutdown).
func (p *BotCatalogPusher) Wait() { p.wg.Wait() }
