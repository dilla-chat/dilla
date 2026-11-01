package server_test

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jonasthim/dilla/internal/config"
	"github.com/jonasthim/dilla/internal/server"
)

func bodyReader(w http.ResponseWriter, r *http.Request) {
	if _, err := server.ReadBody(w, r, 1024); err != nil {
		server.WriteError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func bodyServer(t *testing.T, h http.Handler, tls bool) *httptest.Server {
	t.Helper()
	cfg := config.Default()
	cfg.Server.ReadBodyTimeout = "80ms"
	ts := httptest.NewUnstartedServer(h)
	ts.Config = server.NewHTTPServer(h, cfg)
	if tls {
		ts.EnableHTTP2 = true
		ts.StartTLS()
	} else {
		ts.Start()
	}
	t.Cleanup(ts.Close)
	return ts
}

func TestBodyDeadlineRefusesWithheldBytes(t *testing.T) {
	ts := bodyServer(t, http.HandlerFunc(bodyReader), false)
	c, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", ts.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(time.Second))
	fmt.Fprint(c, "POST / HTTP/1.1\r\nHost: localhost\r\nContent-Type: application/cbor\r\nContent-Length: 1\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestTimeout {
		t.Fatalf("status=%d, want 408", resp.StatusCode)
	}
	io.Copy(io.Discard, resp.Body)
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("timed-out connection remains readable")
	}
}

func TestBodyDeadlineClearsForKeepAlive(t *testing.T) {
	ts := bodyServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			bodyReader(w, r)
		} else {
			w.WriteHeader(204)
		}
	}), false)
	c, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", ts.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(time.Second))
	br := bufio.NewReader(c)
	fmt.Fprint(c, "POST / HTTP/1.1\r\nHost: localhost\r\nContent-Type: application/cbor\r\nContent-Length: 1\r\n\r\nx")
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	time.Sleep(150 * time.Millisecond)
	fmt.Fprint(c, "GET / HTTP/1.1\r\nHost: localhost\r\n\r\n")
	resp, err = http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 204 {
		t.Fatalf("next request status=%d", resp.StatusCode)
	}
}

func TestBodyDeadlineAllowsSpecializedBudget(t *testing.T) {
	entered := make(chan struct{})
	ts := bodyServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := http.NewResponseController(w).SetReadDeadline(time.Now().Add(time.Second)); err != nil {
			t.Error(err)
		}
		close(entered)
		bodyReader(w, r)
	}), false)
	pr, pw := io.Pipe()
	defer pr.Close()
	defer pw.Close()
	req, _ := http.NewRequestWithContext(t.Context(), "POST", ts.URL, pr)
	req.Header.Set("Content-Type", "application/cbor")
	result := make(chan error, 1)
	go func() {
		resp, err := ts.Client().Do(req)
		if err == nil {
			defer resp.Body.Close()
			if resp.StatusCode != 204 {
				err = fmt.Errorf("status=%d", resp.StatusCode)
			}
		}
		result <- err
	}()
	<-entered
	time.Sleep(160 * time.Millisecond)
	pw.Write([]byte("x"))
	pw.Close()
	if err := <-result; err != nil {
		t.Fatal(err)
	}
}

func TestBodyDeadlineHTTP2StreamIsolation(t *testing.T) {
	entered := make(chan struct{})
	ts := bodyServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow" {
			close(entered)
			bodyReader(w, r)
		} else {
			w.WriteHeader(204)
		}
	}), true)
	pr, pw := io.Pipe()
	defer pr.Close()
	defer pw.Close()
	req, _ := http.NewRequestWithContext(t.Context(), "POST", ts.URL+"/slow", pr)
	req.Header.Set("Content-Type", "application/cbor")
	result := make(chan error, 1)
	go func() {
		resp, err := ts.Client().Do(req)
		if err == nil {
			defer resp.Body.Close()
			if resp.StatusCode != 408 || resp.ProtoMajor != 2 {
				err = fmt.Errorf("slow response=%s %d", resp.Proto, resp.StatusCode)
			}
		}
		result <- err
	}()
	<-entered
	fastReq, _ := http.NewRequestWithContext(t.Context(), "GET", ts.URL+"/fast", nil)
	resp, err := ts.Client().Do(fastReq)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.ProtoMajor != 2 || resp.StatusCode != 204 {
		t.Fatalf("fast response=%s %d", resp.Proto, resp.StatusCode)
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("stalled HTTP/2 body never expired")
	}
	resp, err = ts.Client().Do(fastReq)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

func TestBodyDeadlineUnsupportedTransportFailsClosed(t *testing.T) {
	called := false
	srv := server.NewHTTPServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }), config.Default())
	w := httptest.NewRecorder()
	srv.Handler.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), "POST", "/", strings.NewReader("x")))
	if called || w.Code != 503 {
		t.Fatalf("unsupported deadline: called=%v status=%d", called, w.Code)
	}
}

func TestBodyDeadlineLeavesUpgradeAlone(t *testing.T) {
	srv := server.NewHTTPServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(101) }), config.Default())
	req := httptest.NewRequestWithContext(t.Context(), "GET", "/gateway", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	w := httptest.NewRecorder()
	srv.Handler.ServeHTTP(w, req)
	if w.Code != 101 {
		t.Fatalf("upgrade status=%d", w.Code)
	}
}

// Refusing a body before reading it still has to bound net/http's drain.
func TestBodyDeadlineBoundsEarlyRefusalDrain(t *testing.T) {
	ts := bodyServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(403) }), false)
	c, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", ts.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(time.Second))
	fmt.Fprint(c, "POST / HTTP/1.1\r\nHost: localhost\r\nContent-Length: 1\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
}

type deadlineRecorder struct {
	*httptest.ResponseRecorder
	deadlines []time.Time
}

func (w *deadlineRecorder) SetReadDeadline(d time.Time) error {
	w.deadlines = append(w.deadlines, d)
	return nil
}

func TestBodyDeadlineDefaultAndZeroStayBounded(t *testing.T) {
	for _, value := range []config.Duration{"30s", "0s"} {
		t.Run(string(value), func(t *testing.T) {
			cfg := config.Default()
			cfg.Server.ReadBodyTimeout = value
			srv := server.NewHTTPServer(http.HandlerFunc(bodyReader), cfg)
			w := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
			start := time.Now()
			srv.Handler.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), "POST", "/", strings.NewReader("x")))
			if len(w.deadlines) != 2 {
				t.Fatalf("deadlines=%v", w.deadlines)
			}
			if budget := w.deadlines[0].Sub(start); budget < 29*time.Second || budget > 31*time.Second {
				t.Fatalf("budget=%s", budget)
			}
			if !w.deadlines[1].IsZero() {
				t.Fatal("deadline not cleared")
			}
		})
	}
}
