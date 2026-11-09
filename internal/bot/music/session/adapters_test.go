package session

import (
	"context"
	"io"
	"strings"
	"testing"
)

type youtubePCMStub struct {
	input string
	calls int
}

func (s *youtubePCMStub) Start(_ context.Context, input string) (io.ReadCloser, error) {
	s.input = input
	s.calls++
	return io.NopCloser(strings.NewReader("\x00\x01\x02\x03")), nil
}

func TestYouTubePCMAdaptsCanonicalQueueIDToSandboxURL(t *testing.T) {
	provider := &youtubePCMStub{}
	adapter := YouTubePCM{Source: provider}
	if _, err := adapter.Start(context.Background(), "aqz-KE-bpKQ"); err != nil {
		t.Fatal(err)
	}
	if provider.calls != 1 || provider.input != "https://www.youtube.com/watch?v=aqz-KE-bpKQ" {
		t.Fatalf("provider call=%d input=%q", provider.calls, provider.input)
	}
}

func TestYouTubePCMRejectsNonCanonicalQueueIDBeforeProvider(t *testing.T) {
	provider := &youtubePCMStub{}
	adapter := YouTubePCM{Source: provider}
	if _, err := adapter.Start(context.Background(), "https://www.youtube.com/watch?v=aqz-KE-bpKQ"); err == nil {
		t.Fatal("expected queue URL to be rejected")
	}
	if provider.calls != 0 {
		t.Fatal("invalid queue identifier reached PCM provider")
	}
}
