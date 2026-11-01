package client

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

func TestInteractiveRequestIsNotQueuedBehindSweep(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c, err := New(Config{BaseURL: "https://example.invalid", InstanceID: testID(99), HTTPClient: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			status, body := http.StatusOK, "\x80"
			if strings.HasSuffix(r.URL.Path, "/cursor") {
				status, body = http.StatusNoContent, ""
			}
			return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/cbor"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
		})}})
		if err != nil {
			t.Fatal(err)
		}
		c.discovered.Store(true)
		var sweep sync.WaitGroup
		sweep.Add(120)
		sweepStart := time.Now()
		// Queue a sweep's reads concurrently to expose reservations ahead of a fresh command.
		for range 120 {
			go func() {
				defer sweep.Done()
				if err := c.waitRate(t.Context(), "bot", "GET", "groups/x/info"); err != nil {
					t.Error(err)
				}
			}()
		}
		synctest.Wait()
		defer sweep.Wait()
		start := time.Now()
		ctx, cancel := context.WithCancel(WithInteractive(t.Context()))
		defer cancel()
		session, err := c.SessionForDevice("bot", testID(1))
		if err != nil {
			t.Fatal(err)
		}
		for _, step := range [][2]string{{"GET", "groups/x/messages"}, {"POST", "groups/x/message"}, {"POST", "groups/x/cursor"}} {
			if _, _, err := session.do(ctx, step[0], strings.ReplaceAll(step[1], "/x/", "/"+testID(2).String()+"/"), nil, nil, MaxSmallBody, step[0] == "POST"); err != nil {
				t.Fatalf("%v: %v", step, err)
			}
		}
		if elapsed := time.Since(start); elapsed > 250*time.Millisecond {
			t.Errorf("interactive command waited %s behind sweep", elapsed)
		}
		sweep.Wait()
		if elapsed := time.Since(sweepStart); elapsed < 10125*time.Millisecond {
			t.Fatalf("shared read rate exceeded: %s", elapsed)
		}
	})
}

func TestPriorityTrafficRespectsSharedClassRate(t *testing.T) {
	for _, interactive := range []bool{false, true} {
		t.Run(map[bool]string{false: "background", true: "interactive"}[interactive], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c, err := New(Config{BaseURL: "https://example.invalid", InstanceID: testID(99)})
				if err != nil {
					t.Fatal(err)
				}
				ctx := t.Context()
				if interactive {
					ctx = WithInteractive(ctx)
				}
				start := time.Now()
				for range 120 {
					if err := c.waitRate(ctx, "bot", "GET", "groups/x/info"); err != nil {
						t.Fatal(err)
					}
				}
				if elapsed := time.Since(start); elapsed < 10*time.Second {
					t.Fatalf("120 reads exceeded class bucket: %s", elapsed)
				}
			})
		})
	}
}
