package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestInstanceRequestDiagnosticsPreservesHandlerAndCountsOnlyExactDiscovery(t *testing.T) {
	var counts instanceRequestDiagnostics
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Owned", "unchanged")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte("PRIVATE_SECRET body"))
	})
	for _, item := range []struct{ method, path string }{{"GET", "/v1/instance?PRIVATE_SECRET"}, {"POST", "/v1/instance"}, {"GET", "/v1/instance/limits"}, {"GET", "/PRIVATE_SECRET"}} {
		recorder := httptest.NewRecorder()
		counts.public(next).ServeHTTP(recorder, httptest.NewRequestWithContext(t.Context(), item.method, item.path, nil))
		if recorder.Code != http.StatusAccepted || recorder.Header().Get("X-Owned") != "unchanged" || recorder.Body.String() != "PRIVATE_SECRET body" {
			t.Fatal("diagnostics changed the actual response")
		}
	}
	before := counts.snapshot()
	if before.Begun != 1 || before.Ended != 1 || before.Canceled != 0 || before.LastBeginUnixMillis <= 0 || before.LastEndUnixMillis < before.LastBeginUnixMillis {
		t.Fatalf("unexpected fixed-route counts: %+v", before)
	}
	control := httptest.NewRecorder()
	counts.control(http.NotFoundHandler()).ServeHTTP(control, httptest.NewRequestWithContext(t.Context(), "GET", "/debug/instance-requests?PRIVATE_SECRET", nil))
	var got instanceRequestSnapshot
	if err := json.Unmarshal(control.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got != before || strings.Contains(control.Body.String(), "PRIVATE_SECRET") {
		t.Fatal("control report did not preserve only numeric counts")
	}
	wrong := httptest.NewRecorder()
	counts.control(http.NotFoundHandler()).ServeHTTP(wrong, httptest.NewRequestWithContext(t.Context(), "POST", "/debug/instance-requests", nil))
	if wrong.Code != http.StatusNotFound {
		t.Fatal("diagnostic authority expanded beyond its fixed GET")
	}
}

func TestInstanceRequestDiagnosticsObservesCancellationBeforeBlockedHandlerReturns(t *testing.T) {
	var counts instanceRequestDiagnostics
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	begun, release, ended := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(ended)
		counts.public(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { close(begun); <-release })).ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(ctx, "GET", "/v1/instance", nil))
	}()
	<-begun
	cancel()
	deadline := time.Now().Add(time.Second)
	for counts.snapshot().Canceled == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	pending := counts.snapshot()
	if pending.Begun != 1 || pending.Ended != 0 || pending.Canceled != 1 {
		close(release)
		<-ended
		t.Fatalf("pending canceled request was not distinguished: %+v", pending)
	}
	close(release)
	<-ended
	after := counts.snapshot()
	if after.Begun != 1 || after.Ended != 1 || after.Canceled != 1 {
		t.Fatalf("unexpected terminal counts: %+v", after)
	}
}
