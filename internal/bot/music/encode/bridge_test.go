package encode

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"os/exec"
	"testing"
	"time"

	"github.com/livekit/server-sdk-go/v2/pkg/oggreader"

	"github.com/dilla-chat/dilla/internal/media"
)

func TestBridgeEncodesPCMToBoundedOpusPackets(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is not installed")
	}
	provider, err := media.NewLiveOpusProvider(100)
	if err != nil {
		t.Fatal(err)
	}
	pcm := make([]byte, 44100*2*2/4) // 250ms stereo PCM at the probe's common 44.1kHz rate.
	for frame := 0; frame < len(pcm)/4; frame++ {
		v := int16(math.Sin(2*math.Pi*440*float64(frame)/44100) * 9000)
		binary.LittleEndian.PutUint16(pcm[frame*4:], uint16(v))
		binary.LittleEndian.PutUint16(pcm[frame*4+2:], uint16(v))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := Bridge(ctx, io.NopCloser(bytes.NewReader(pcm)), 44100, 2, provider); err != nil {
		t.Fatal(err)
	}
	count := 0
	for {
		sample, err := provider.NextSample(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if len(sample.Data) == 0 || len(sample.Data) > media.MaxLiveOpusSampleBytes {
			t.Fatalf("packet size %d outside bounds", len(sample.Data))
		}
		if sample.Duration != 20*time.Millisecond {
			t.Fatalf("packet duration %s, want 20ms", sample.Duration)
		}
		count++
	}
	if count < 8 || count > 20 {
		t.Fatalf("got %d packets for 250ms PCM", count)
	}
}

func TestPumpPCMDoesNotOverwriteFragmentedSampleFrames(t *testing.T) {
	pcm := []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}
	var written bytes.Buffer
	activity := make(chan struct{}, 1)
	err := pumpPCM(&singleByteReader{source: bytes.NewReader(pcm)}, &written, 2, activity)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(written.Bytes(), pcm) {
		t.Fatalf("fragmented PCM changed: got %v, want %v", written.Bytes(), pcm)
	}
}

type singleByteReader struct{ source *bytes.Reader }

func (r *singleByteReader) Read(p []byte) (int, error) {
	if len(p) > 1 {
		p = p[:1]
	}
	return r.source.Read(p)
}

func TestBridgeRejectsNonInterruptibleSource(t *testing.T) {
	provider, _ := media.NewLiveOpusProvider(1)
	err := Bridge(context.Background(), bytes.NewReader(nil), 44100, 2, provider)
	if !errors.Is(err, ErrSourceNotInterruptible) {
		t.Fatalf("Bridge error = %v, want ErrSourceNotInterruptible", err)
	}
}

func TestBridgeStartupFailureClosesSourceAndProvider(t *testing.T) {
	provider, _ := media.NewLiveOpusProvider(1)
	reader := &trackedReader{Reader: bytes.NewReader(nil)}
	err := BridgeWithOptions(context.Background(), reader, 44100, 2, provider, Options{FFmpegPath: "/no/such/ffmpeg"})
	if !errors.Is(err, ErrEncoderFailed) {
		t.Fatalf("Bridge error = %v, want ErrEncoderFailed", err)
	}
	if !reader.closed {
		t.Fatal("source reader was not closed")
	}
	if _, err := provider.NextSample(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatalf("NextSample error = %v, want EOF after startup failure", err)
	}
}

func TestBridgeTimesOutWhenProviderBackpressureStopsProgress(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is not installed")
	}
	provider, _ := media.NewLiveOpusProvider(1)
	pcm := make([]byte, 48000*4/2) // 500ms stereo PCM.
	err := BridgeWithOptions(context.Background(), io.NopCloser(bytes.NewReader(pcm)), 48000, 2, provider,
		Options{StallTimeout: 100 * time.Millisecond})
	if !errors.Is(err, ErrStalled) {
		t.Fatalf("Bridge error = %v, want ErrStalled", err)
	}
}

func TestReadPacketSafelyConvertsParserPanicToError(t *testing.T) {
	_, err := readPacketSafely(panickingPacketReader{})
	if !errors.Is(err, ErrInvalidOpus) {
		t.Fatalf("readPacketSafely error = %v, want ErrInvalidOpus", err)
	}
}

func TestCheckedOggReaderRejectsBadPageChecksum(t *testing.T) {
	valid := testOpusHeaderPages(1)
	valid = append(valid, testOggPageWithGranule(1, 2, 0, 960, []byte{0x42})...)
	bad := testOggPageWithGranule(1, 3, 0, 1920, []byte{0x43})
	bad[len(bad)-1] ^= 0xff // Corrupt only payload, preserving the version and stale CRC.
	stream := append(valid, bad...)
	reader := &checkedOggReader{source: bytes.NewReader(stream)}
	if _, err := io.ReadAll(reader); !errors.Is(err, ErrInvalidOpus) {
		t.Fatalf("reading payload-corrupt Ogg page returned %v, want ErrInvalidOpus", err)
	}
}

func TestCheckedOggReaderRejectsReservedFlagsAndBrokenSequence(t *testing.T) {
	valid := testOpusHeaderPages(7)
	reserved := testOggPageWithGranule(7, 2, 0x80, 960, []byte{0x40})
	sequence := testOggPageWithGranule(7, 3, 0, 960, []byte{0x40})
	serial := testOggPageWithGranule(8, 2, 0, 960, []byte{0x40})
	for name, stream := range map[string][]byte{
		"reserved flags": reserved,
		"page sequence":  append(valid, sequence...),
		"serial number":  append(valid, serial...),
	} {
		t.Run(name, func(t *testing.T) {
			reader := &checkedOggReader{source: bytes.NewReader(stream)}
			if _, err := io.ReadAll(reader); !errors.Is(err, ErrInvalidOpus) {
				t.Fatalf("ReadAll error = %v, want ErrInvalidOpus", err)
			}
		})
	}
}

func TestCheckedOggReaderEnforcesContinuationAndEOS(t *testing.T) {
	headers := testOpusHeaderPages(3)
	first := testOggPageWithGranule(3, 2, 0, ^uint64(0), bytes.Repeat([]byte{0x61}, 255))
	wrongContinuation := testOggPageWithGranule(3, 3, 0, ^uint64(0), []byte{0x62})
	eos := testOggPageWithGranule(3, 2, 0x04, 960, []byte{0x42})
	trailing := testOggPageWithGranule(3, 3, 0, 1920, []byte{0x43})
	for name, stream := range map[string][]byte{
		"missing continuation flag": append(append(append([]byte(nil), headers...), first...), wrongContinuation...),
		"page after EOS":            append(append(append([]byte(nil), headers...), eos...), trailing...),
		"missing EOS":               append(append([]byte(nil), headers...), testOggPageWithGranule(3, 2, 0, 960, []byte{0x42})...),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := io.ReadAll(&checkedOggReader{source: bytes.NewReader(stream)}); !errors.Is(err, ErrInvalidOpus) {
				t.Fatalf("ReadAll error = %v, want ErrInvalidOpus", err)
			}
		})
	}
}

func TestValidOpusHeaderRejectsUnsupportedFields(t *testing.T) {
	header := &oggreader.OggHeader{SampleRate: 48000, Channels: 2, Version: 1, ChannelMap: 0}
	if !validOpusHeader(header) {
		t.Fatal("valid stereo Opus header rejected")
	}
	for name, mutate := range map[string]func(*oggreader.OggHeader){
		"rate":     func(h *oggreader.OggHeader) { h.SampleRate = 44100 },
		"channels": func(h *oggreader.OggHeader) { h.Channels = 1 },
		"version":  func(h *oggreader.OggHeader) { h.Version = 16 },
		"mapping":  func(h *oggreader.OggHeader) { h.ChannelMap = 1 },
	} {
		t.Run(name, func(t *testing.T) {
			bad := *header
			mutate(&bad)
			if validOpusHeader(&bad) {
				t.Fatal("unsupported Opus header accepted")
			}
		})
	}
}

func TestCheckedOggReaderRequiresBoundedOpusTagsPacket(t *testing.T) {
	head := testOggPageWithGranule(11, 0, 0x02, 0, testOpusHead())
	emptyTags := testOggNoSegmentPage(11, 1, 0, 0)
	eos := testOggNoSegmentPage(11, 2, 0x04, 0)
	if _, err := io.ReadAll(&checkedOggReader{source: bytes.NewReader(append(append(head, emptyTags...), eos...))}); !errors.Is(err, ErrInvalidOpus) {
		t.Fatalf("zero-segment tags page error = %v, want ErrInvalidOpus", err)
	}
}

func TestCheckedOggReaderRejectsMalformedTagsAndSpanningTags(t *testing.T) {
	head := testOggPageWithGranule(13, 0, 0x02, 0, testOpusHead())
	malformedTags := testOggPageWithGranule(13, 1, 0, 0, []byte("OpusTags"))
	spanningTags := testOggPageWithGranule(13, 1, 0, 0, bytes.Repeat([]byte{0x61}, 255))
	for name, page := range map[string][]byte{"malformed tags fields": malformedTags, "tags spanning pages": spanningTags} {
		t.Run(name, func(t *testing.T) {
			stream := append(append([]byte(nil), head...), page...)
			if _, err := io.ReadAll(&checkedOggReader{source: bytes.NewReader(stream)}); !errors.Is(err, ErrInvalidOpus) {
				t.Fatalf("ReadAll error = %v, want ErrInvalidOpus", err)
			}
		})
	}
}

func TestCheckedOggReaderRejectsMalformedHeaderGranule(t *testing.T) {
	head := testOggPageWithGranule(12, 0, 0x02, 1, testOpusHead())
	tags := testOggPageWithGranule(12, 1, 0, 0, testOpusTags())
	eos := testOggNoSegmentPage(12, 2, 0x04, 0)
	stream := append(append(head, tags...), eos...)
	if _, err := io.ReadAll(&checkedOggReader{source: bytes.NewReader(stream)}); !errors.Is(err, ErrInvalidOpus) {
		t.Fatalf("nonzero BOS granule error = %v, want ErrInvalidOpus", err)
	}
}

func TestCheckedOggReaderRejectsMalformedDataGranule(t *testing.T) {
	headers := testOpusHeaderPages(14)
	data := testOggPageWithGranule(14, 2, 0, ^uint64(0), []byte{0x42})
	stream := append(headers, data...)
	if _, err := io.ReadAll(&checkedOggReader{source: bytes.NewReader(stream)}); !errors.Is(err, ErrInvalidOpus) {
		t.Fatalf("completed packet with sentinel granule error = %v, want ErrInvalidOpus", err)
	}
}

func testOpusHead() []byte {
	return []byte{'O', 'p', 'u', 's', 'H', 'e', 'a', 'd', 1, 2, 0, 0, 0x80, 0xbb, 0, 0, 0, 0, 0}
}

func testOpusTags() []byte {
	return []byte{'O', 'p', 'u', 's', 'T', 'a', 'g', 's', 0, 0, 0, 0, 0, 0, 0, 0}
}

func testOpusHeaderPages(serial uint32) []byte {
	head := testOggPageWithGranule(serial, 0, 0x02, 0, testOpusHead())
	tags := testOggPageWithGranule(serial, 1, 0, 0, testOpusTags())
	return append(head, tags...)
}

func testOggPageWithGranule(serial, sequence uint32, flags byte, granule uint64, payload []byte) []byte {
	page := testOggPage(serial, sequence, flags, payload)
	binary.LittleEndian.PutUint64(page[6:14], granule)
	for i := 22; i < 26; i++ {
		page[i] = 0
	}
	binary.LittleEndian.PutUint32(page[22:26], oggCRC(page))
	return page
}

func testOggNoSegmentPage(serial, sequence uint32, flags byte, granule uint64) []byte {
	page := make([]byte, 27)
	copy(page, "OggS")
	page[5] = flags
	binary.LittleEndian.PutUint64(page[6:14], granule)
	binary.LittleEndian.PutUint32(page[14:18], serial)
	binary.LittleEndian.PutUint32(page[18:22], sequence)
	binary.LittleEndian.PutUint32(page[22:26], oggCRC(page))
	return page
}

func testOggPage(serial, sequence uint32, flags byte, payload []byte) []byte {
	page := make([]byte, 27+1+len(payload))
	copy(page, "OggS")
	page[5] = flags
	binary.LittleEndian.PutUint32(page[14:18], serial)
	binary.LittleEndian.PutUint32(page[18:22], sequence)
	page[26] = 1
	page[27] = byte(len(payload))
	copy(page[28:], payload)
	binary.LittleEndian.PutUint32(page[22:26], oggCRC(page))
	return page
}

type trackedReader struct {
	io.Reader
	closed bool
}

func (r *trackedReader) Close() error { r.closed = true; return nil }

type panickingPacketReader struct{}

func (panickingPacketReader) ReadPacket() ([]byte, error) { panic("malformed Ogg state") }

func TestBridgeRejectsInvalidFormatBeforeStarting(t *testing.T) {
	provider, _ := media.NewLiveOpusProvider(1)
	for _, tc := range []struct {
		rate, channels int
	}{
		{0, 2}, {7999, 2}, {192001, 2}, {44100, 0}, {44100, 3},
	} {
		if err := Bridge(context.Background(), io.NopCloser(bytes.NewReader(nil)), tc.rate, tc.channels, provider); !errors.Is(err, ErrInvalidPCMFormat) {
			t.Fatalf("Bridge(%d,%d) error = %v, want ErrInvalidPCMFormat", tc.rate, tc.channels, err)
		}
	}
	provider, _ = media.NewLiveOpusProvider(1)
	if err := Bridge(context.Background(), io.NopCloser(bytes.NewReader(nil)), 0, 2, provider); !errors.Is(err, ErrInvalidPCMFormat) {
		t.Fatalf("invalid format error = %v, want ErrInvalidPCMFormat", err)
	}
	if _, err := provider.NextSample(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatalf("provider NextSample = %v after invalid input, want EOF", err)
	}
}

func TestBridgeCancellationClosesReaderAndChild(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is not installed")
	}
	provider, _ := media.NewLiveOpusProvider(1)
	reader, writer := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Bridge(ctx, reader, 48000, 2, provider) }()
	time.Sleep(100 * time.Millisecond)
	cancel()
	_ = writer.Close()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Bridge error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Bridge did not stop after cancellation")
	}
}

func TestBridgeStalledProducerTimesOut(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is not installed")
	}
	provider, _ := media.NewLiveOpusProvider(1)
	reader, writer := io.Pipe()
	defer writer.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	err := BridgeWithOptions(ctx, reader, 48000, 2, provider, Options{StallTimeout: 100 * time.Millisecond})
	if !errors.Is(err, ErrStalled) {
		t.Fatalf("Bridge error = %v, want ErrStalled", err)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("stall detection took %s", time.Since(start))
	}
}
