package main

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

// These counters belong only to dilla-testhost and its existing private control
// listener. They never inspect bodies, headers, identities, or arbitrary paths.
type instanceRequestDiagnostics struct {
	mu    sync.Mutex
	value instanceRequestSnapshot
}
type instanceRequestSnapshot struct {
	Begun               uint64 `json:"begun"`
	Ended               uint64 `json:"ended"`
	Canceled            uint64 `json:"canceled"`
	LastBeginUnixMillis int64  `json:"lastBeginUnixMillis"`
	LastEndUnixMillis   int64  `json:"lastEndUnixMillis"`
}

func (d *instanceRequestDiagnostics) snapshot() instanceRequestSnapshot {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.value
}
func (d *instanceRequestDiagnostics) public(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/instance" {
			next.ServeHTTP(w, r)
			return
		}
		d.mu.Lock()
		d.value.Begun++
		d.value.LastBeginUnixMillis = time.Now().UnixMilli()
		d.mu.Unlock()
		stop := context.AfterFunc(r.Context(), func() { d.mu.Lock(); d.value.Canceled++; d.mu.Unlock() })
		defer func() {
			d.mu.Lock()
			d.value.Ended++
			d.value.LastEndUnixMillis = time.Now().UnixMilli()
			d.mu.Unlock()
		}()
		defer stop()
		next.ServeHTTP(w, r)
	})
}
func (d *instanceRequestDiagnostics) control(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/debug/instance-requests" {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(d.snapshot())
	})
}
