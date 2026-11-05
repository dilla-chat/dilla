package main

import (
	"sync"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/service"
	"github.com/dilla-chat/dilla/internal/id"
)

// registrationWorkReports remembers the last pending-work error per installation
// so a reconcile pass that hits the same error stays quiet instead of logging
// it every few seconds.
var registrationWorkReports = &changeReports{}

// registrationBackoff outlives the per-pass coordinators.
var registrationBackoff = service.NewInteractionRegistrationBackoff()

// registrationWorkRepeatAfter bounds how long an unchanged error stays silent.
const registrationWorkRepeatAfter = 30 * time.Minute

type changeReports struct {
	mu   sync.Mutex
	last map[id.ID]changeReport
	now  func() time.Time
}

type changeReport struct {
	text string
	at   time.Time
}

// changed records err for key and reports whether it is a non-nil error that
// differs from the previous pass. A nil error resets the key so a return of the
// same error is reported again; an unchanged error is reported again after
// registrationWorkRepeatAfter.
func (r *changeReports) changed(key id.ID, err error) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err == nil {
		delete(r.last, key)
		return false
	}
	if r.last == nil {
		r.last = map[id.ID]changeReport{}
	}
	now := time.Now()
	if r.now != nil {
		now = r.now()
	}
	text := err.Error()
	if prev, ok := r.last[key]; ok && prev.text == text && now.Sub(prev.at) < registrationWorkRepeatAfter {
		return false
	}
	r.last[key] = changeReport{text: text, at: now}
	return true
}
