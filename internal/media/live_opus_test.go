package media

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"
)

func TestLiveOpusProviderAppliesBoundedBackpressure(t *testing.T) {
	p, err := NewLiveOpusProvider(1)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	if err := p.WriteSample(context.Background(), []byte{0xfc, 1}); err != nil {
		t.Fatal(err)
	}
	written := make(chan error, 1)
	go func() { written <- p.WriteSample(context.Background(), []byte{0xfc, 2}) }()
	select {
	case err := <-written:
		t.Fatalf("second sample returned before the bounded queue had room: %v", err)
	case <-time.After(20 * time.Millisecond):
	}

	sample, err := p.NextSample(context.Background())
	if err != nil || sample.Data[1] != 1 {
		t.Fatalf("first sample = %v, %v", sample.Data, err)
	}
	select {
	case err := <-written:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("producer stayed blocked after a sample was consumed")
	}
	sample, err = p.NextSample(context.Background())
	if err != nil || sample.Data[1] != 2 {
		t.Fatalf("second sample = %v, %v", sample.Data, err)
	}
}

func TestLiveOpusProviderCancelsStalledProducerAndConsumer(t *testing.T) {
	p, err := NewLiveOpusProvider(1)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if err := p.WriteSample(context.Background(), []byte{0xfc}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := p.WriteSample(ctx, []byte{0xfc, 1}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stalled producer error = %v, want context deadline", err)
	}

	consumer, stop := context.WithCancel(context.Background())
	stop()
	if _, err := p.NextSample(consumer); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled consumer error = %v, want context canceled", err)
	}
}

func TestLiveOpusProviderCloseDrainsThenReturnsEOF(t *testing.T) {
	p, err := NewLiveOpusProvider(1)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.WriteSample(context.Background(), []byte{0xfc}); err != nil {
		t.Fatal(err)
	}
	blockedWrite := make(chan error, 1)
	go func() { blockedWrite <- p.WriteSample(context.Background(), []byte{0xfc, 1}) }()
	select {
	case err := <-blockedWrite:
		t.Fatalf("writer returned while queue was full: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	if err := p.CloseInput(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-blockedWrite:
		if !errors.Is(err, ErrLiveOpusClosed) {
			t.Fatalf("blocked write after close = %v, want ErrLiveOpusClosed", err)
		}
	case <-time.After(time.Second):
		t.Fatal("close did not wake blocked producer")
	}
	if err := p.WriteSample(context.Background(), []byte{0xfc}); !errors.Is(err, ErrLiveOpusClosed) {
		t.Fatalf("write after close = %v, want ErrLiveOpusClosed", err)
	}
	sample, err := p.NextSample(context.Background())
	if err != nil || len(sample.Data) != 1 {
		t.Fatalf("queued sample = %v, %v", sample.Data, err)
	}
	if _, err := p.NextSample(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatalf("after drain = %v, want EOF", err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	empty, err := NewLiveOpusProvider(1)
	if err != nil {
		t.Fatal(err)
	}
	blockedRead := make(chan error, 1)
	go func() {
		_, err := empty.NextSample(context.Background())
		blockedRead <- err
	}()
	if err := empty.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-blockedRead:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("blocked read after close = %v, want EOF", err)
		}
	case <-time.After(time.Second):
		t.Fatal("close did not wake blocked consumer")
	}
}

func TestLiveOpusProviderCanBeReboundAfterUnbind(t *testing.T) {
	p, err := NewLiveOpusProvider(2)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	if err := p.OnBind(); err != nil {
		t.Fatal(err)
	}
	if err := p.WriteSample(context.Background(), []byte{0xfc, 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.NextSample(context.Background()); err != nil {
		t.Fatal(err)
	}

	if err := p.OnUnbind(); err != nil {
		t.Fatal(err)
	}
	if err := p.OnBind(); err != nil {
		t.Fatal(err)
	}
	if err := p.WriteSample(context.Background(), []byte{0xfc, 2}); err != nil {
		t.Fatalf("write after rebind = %v", err)
	}
	sample, err := p.NextSample(context.Background())
	if err != nil || len(sample.Data) != 2 || sample.Data[1] != 2 {
		t.Fatalf("sample after rebind = %v, %v; want second packet", sample.Data, err)
	}

	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if err := p.WriteSample(context.Background(), []byte{0xfc, 3}); !errors.Is(err, ErrLiveOpusClosed) {
		t.Fatalf("write after close = %v, want ErrLiveOpusClosed", err)
	}
	if _, err := p.NextSample(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatalf("read after close = %v, want EOF", err)
	}
}

func TestLiveOpusProviderRejectsInvalidSamplesAndUses20msFrames(t *testing.T) {
	p, err := NewLiveOpusProvider(1)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	for _, data := range [][]byte{nil, {}, make([]byte, MaxLiveOpusSampleBytes+1)} {
		if err := p.WriteSample(context.Background(), data); !errors.Is(err, ErrInvalidLiveOpusSample) {
			t.Fatalf("write of %d bytes = %v, want invalid sample", len(data), err)
		}
	}
	data := []byte{0xfc, 1, 2}
	if err := p.WriteSample(context.Background(), data); err != nil {
		t.Fatal(err)
	}
	data[1] = 9 // the provider owns a snapshot, not the producer's mutable buffer
	sample, err := p.NextSample(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sample.Duration != 20*time.Millisecond || sample.Data[1] != 1 {
		t.Fatalf("sample = %x, %s; want copied data and 20ms", sample.Data, sample.Duration)
	}
}

func TestPausedProviderSendsSilenceAndNoSourceSamples(t *testing.T) {
	p, err := NewLiveOpusProvider(1)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if err = p.WriteSample(t.Context(), []byte{0xfc, 42}); err != nil {
		t.Fatal(err)
	}
	p.Pause()
	for range 3 {
		sample, err := p.NextSample(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(sample.Data, []byte{0xf8, 0xff, 0xfe}) || sample.Duration != 20*time.Millisecond {
			t.Fatalf("paused sample=%+v", sample)
		}
	}
	if p.PositionMS() != 0 {
		t.Fatalf("silence advanced source position: %d", p.PositionMS())
	}
	if err = p.Resume(); err != nil {
		t.Fatal(err)
	}
	sample, err := p.NextSample(t.Context())
	if err != nil || !bytes.Equal(sample.Data, []byte{0xfc, 42}) || p.PositionMS() != 20 {
		t.Fatalf("resume sample=%v position=%d err=%v", sample.Data, p.PositionMS(), err)
	}
}
