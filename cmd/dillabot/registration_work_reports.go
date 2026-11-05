package main

import (
	"sync"

	"github.com/dilla-chat/dilla/internal/id"
)

// registrationWorkReports remembers the last pending-work error per installation
// so a reconcile pass that hits the same error stays quiet instead of logging
// it every few seconds.
var registrationWorkReports = &changeReports{}

type changeReports struct {
	mu   sync.Mutex
	last map[id.ID]string
}

// changed records err for key and reports whether it is a non-nil error that
// differs from the previous pass. A nil error resets the key so a return of the
// same error is reported again.
func (r *changeReports) changed(key id.ID, err error) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err == nil {
		delete(r.last, key)
		return false
	}
	if r.last == nil {
		r.last = map[id.ID]string{}
	}
	text := err.Error()
	if r.last[key] == text {
		return false
	}
	r.last[key] = text
	return true
}
