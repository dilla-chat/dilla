// Package encode bridges bounded signed 16-bit PCM input to the live Opus transport.
package encode

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"time"

	"github.com/livekit/server-sdk-go/v2/pkg/oggreader"

	"github.com/dilla-chat/dilla/internal/media"
)

const (
	outputRate        = 48000
	outputChannels    = 2
	outputBitrate     = "96k"
	maxInputRate      = 192000
	maxPCMReadBytes   = 32 * 1024
	maxActiveEncoders = 4
	maxOpusTagsBytes  = 32 * 1024
)

var (
	ErrInvalidPCMFormat       = errors.New("music encode: invalid PCM format")
	ErrStalled                = errors.New("music encode: PCM or encoder stalled")
	ErrEncoderFailed          = errors.New("music encode: ffmpeg failed")
	ErrInvalidOpus            = errors.New("music encode: ffmpeg emitted invalid Opus stream")
	ErrSourceNotInterruptible = errors.New("music encode: PCM source must implement io.ReadCloser")
	encoderSlots              = make(chan struct{}, maxActiveEncoders)
)

// Options controls liveness detection. FFmpegPath defaults to "ffmpeg". A nonpositive
// StallTimeout selects the 10-second default. The cap of four bounds child processes and pipes.
type Options struct {
	FFmpegPath   string
	StallTimeout time.Duration
}

// Bridge takes ownership of a closable PCM source and closes it before returning. Its Close
// method must unblock a concurrent Read; cancellation and stall handling depend on that contract.
// Bridge encodes interleaved signed 16-bit little-endian PCM into live 48 kHz stereo Opus.
// sampleRate and channels describe the PCM source itself; callers must pass the actual format
// reported by their source because source output rates can vary by track/device.
func Bridge(ctx context.Context, pcm io.Reader, sampleRate, channels int, provider *media.LiveOpusProvider) error {
	return BridgeWithOptions(ctx, pcm, sampleRate, channels, provider, Options{})
}

// BridgeWithOptions is Bridge with an explicit ffmpeg path and stall deadline.
func BridgeWithOptions(ctx context.Context, pcm io.Reader, sampleRate, channels int, provider *media.LiveOpusProvider, options Options) error {
	if provider != nil {
		defer func() { _ = provider.CloseInput() }()
	}
	closer, closable := pcm.(io.ReadCloser)
	if closable {
		defer func() { _ = closer.Close() }()
	}
	if pcm == nil || !closable {
		return ErrSourceNotInterruptible
	}
	if ctx == nil || pcm == nil || provider == nil || sampleRate < 8000 || sampleRate > maxInputRate || (channels != 1 && channels != 2) {
		return ErrInvalidPCMFormat
	}
	if options.FFmpegPath == "" {
		options.FFmpegPath = "ffmpeg"
	}
	if options.StallTimeout <= 0 {
		options.StallTimeout = 10 * time.Second
	}
	select {
	case encoderSlots <- struct{}{}:
		defer func() { <-encoderSlots }()
	case <-ctx.Done():
		_ = provider.CloseInput()
		return ctx.Err()
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	activity := make(chan struct{}, 1)
	result := make(chan error, 1)
	go func() { result <- run(runCtx, pcm, sampleRate, channels, provider, options.FFmpegPath, activity) }()
	timer := time.NewTimer(options.StallTimeout)
	defer timer.Stop()
	resetTimer := func() {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(options.StallTimeout)
	}
	for {
		select {
		case err := <-result:
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		case <-ctx.Done():
			cancel()
			closeReader(pcm)
			<-result
			_ = provider.CloseInput()
			return ctx.Err()
		case <-timer.C:
			if provider.Paused() {
				resetTimer()
				continue
			}
			cancel()
			closeReader(pcm)
			<-result
			_ = provider.CloseInput()
			return ErrStalled
		case <-activity:
			resetTimer()
		}
	}
}

func run(ctx context.Context, pcm io.Reader, sampleRate, channels int, provider *media.LiveOpusProvider, ffmpeg string, activity chan<- struct{}) error {
	cmd := exec.CommandContext(ctx, ffmpeg, // #nosec G204 -- FFmpegPath is operator-configured; all arguments are fixed.
		"-hide_banner", "-loglevel", "error", "-nostdin",
		"-f", "s16le", "-ar", fmt.Sprint(sampleRate), "-ac", fmt.Sprint(channels), "-i", "pipe:0",
		"-map", "0:a:0", "-ar", fmt.Sprint(outputRate), "-ac", fmt.Sprint(outputChannels),
		"-c:a", "libopus", "-application", "audio", "-b:a", outputBitrate, "-vbr", "off", "-dtx", "0", "-frame_duration", "20",
		"-flush_packets", "1", "-page_duration", "20000", "-f", "ogg", "pipe:1")
	cmd.Stderr = io.Discard // Never expose provider bytes or source-specific diagnostics in logs.
	configureProcessGroup(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("%w: stdin pipe: %w", ErrEncoderFailed, err)
	}
	defer func() { _ = stdin.Close() }()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("%w: stdout pipe: %w", ErrEncoderFailed, err)
	}
	defer func() { _ = stdout.Close() }()
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("%w: start: %w", ErrEncoderFailed, err)
	}
	pumpErr := make(chan error, 1)
	go func() {
		pumpErr <- pumpPCM(pcm, stdin, channels, activity)
		_ = stdin.Close()
	}()

	checked := &checkedOggReader{source: stdout}
	reader, header, err := oggreader.NewOggReader(checked)
	if err != nil {
		_ = cmd.Cancel()
		_ = stdin.Close()
		<-pumpErr
		_ = cmd.Wait()
		return fmt.Errorf("%w: Ogg headers: %w", ErrInvalidOpus, err)
	}
	if !validOpusHeader(header) {
		_ = cmd.Cancel()
		_ = stdin.Close()
		<-pumpErr
		_ = cmd.Wait()
		return fmt.Errorf("%w: unexpected output format", ErrInvalidOpus)
	}
	for {
		packet, readErr := readPacketSafely(reader)
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				_ = cmd.Cancel()
				_ = stdin.Close()
				<-pumpErr
				_ = cmd.Wait()
				return fmt.Errorf("%w: Ogg packet: %w", ErrInvalidOpus, readErr)
			}
			break
		}
		duration, durationErr := oggreader.ParsePacketDuration(packet)
		if durationErr != nil || len(packet) == 0 || len(packet) > media.MaxLiveOpusSampleBytes || duration != media.LiveOpusSampleDuration {
			_ = cmd.Cancel()
			_ = stdin.Close()
			<-pumpErr
			_ = cmd.Wait()
			return ErrInvalidOpus
		}
		signal(activity)
		if err := provider.WriteSample(ctx, packet); err != nil {
			_ = cmd.Cancel()
			_ = stdin.Close()
			<-pumpErr
			_ = cmd.Wait()
			return err
		}
	}
	if err := <-pumpErr; err != nil {
		_ = cmd.Cancel()
		_ = cmd.Wait()
		return fmt.Errorf("music encode: read PCM: %w", err)
	}
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("%w: process exited unsuccessfully", ErrEncoderFailed)
	}
	_ = provider.CloseInput()
	return nil
}

// pumpPCM preserves incomplete interleaved sample frames across short reads.
// Write the complete prefix before moving the trailing bytes to the front;
// moving them first corrupts the prefix that is about to be encoded.
func pumpPCM(source io.Reader, target io.Writer, channels int, activity chan<- struct{}) error {
	buf := make([]byte, maxPCMReadBytes)
	frameBytes := channels * 2
	pending := 0
	for {
		read, readErr := source.Read(buf[pending:])
		total := pending + read
		complete := total - total%frameBytes
		if complete > 0 {
			if err := writeAll(target, buf[:complete]); err != nil {
				return err
			}
			signal(activity)
		}
		pending = total - complete
		copy(buf[:pending], buf[complete:total])
		if readErr != nil {
			if readErr != io.EOF {
				return readErr
			}
			if pending != 0 {
				return io.ErrUnexpectedEOF
			}
			return nil
		}
		if read == 0 {
			return io.ErrNoProgress
		}
	}
}

func validOpusHeader(header *oggreader.OggHeader) bool {
	return header != nil && header.SampleRate == outputRate && header.Channels == outputChannels && header.Version <= 15 && header.ChannelMap == 0
}

type oggPacketReader interface {
	ReadPacket() ([]byte, error)
}

// checkedOggReader validates every Ogg page before exposing its bytes. The LiveKit Ogg reader
// intentionally ignores an error reading the comment page during initialization; validating at
// this layer makes malformed headers and page checksums fatal as well. At most one Ogg page is
// buffered (27-byte header + 255 lacing bytes + 255*255 payload bytes). OpusTags are restricted
// to one bounded, complete packet on page 1 and structurally validated before audio is accepted.
type checkedOggReader struct {
	source       io.Reader
	page         []byte
	offset       int
	err          error
	started      bool
	serial       uint32
	nextSequence uint32
	continued    bool
	eos          bool
	headerState  uint8
	lastGranule  uint64
}

func (r *checkedOggReader) Read(dst []byte) (int, error) {
	if len(dst) == 0 {
		return 0, nil
	}
	if r.err != nil {
		return 0, r.err
	}
	if r.offset == len(r.page) {
		if err := r.readPage(); err != nil {
			r.err = err
			return 0, err
		}
	}
	n := copy(dst, r.page[r.offset:])
	r.offset += n
	return n, nil
}

func (r *checkedOggReader) readPage() error {
	if r.eos {
		var trailing [1]byte
		n, err := r.source.Read(trailing[:])
		if n != 0 {
			return ErrInvalidOpus
		}
		if err == io.EOF {
			return io.EOF
		}
		if err != nil {
			return err
		}
		return ErrInvalidOpus
	}
	var header [27]byte
	if _, err := io.ReadFull(r.source, header[:]); err != nil {
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return ErrInvalidOpus // A finite ffmpeg Ogg stream must terminate with EOS.
		}
		return err
	}
	flags := header[5]
	serial := binary.LittleEndian.Uint32(header[14:18])
	sequence := binary.LittleEndian.Uint32(header[18:22])
	if string(header[:4]) != "OggS" || header[4] != 0 || flags&^byte(0x07) != 0 {
		return ErrInvalidOpus
	}
	continuation := flags&0x01 != 0
	beginning, ending := flags&0x02 != 0, flags&0x04 != 0
	if !r.started {
		if !beginning || continuation || ending || sequence != 0 {
			return ErrInvalidOpus
		}
		r.serial, r.nextSequence = serial, 0
	} else if beginning || serial != r.serial || sequence != r.nextSequence || continuation != r.continued {
		return ErrInvalidOpus
	}
	segments := int(header[26])
	if continuation && segments == 0 {
		return ErrInvalidOpus
	}
	page := make([]byte, len(header)+segments)
	copy(page, header[:])
	if _, err := io.ReadFull(r.source, page[len(header):]); err != nil {
		return err
	}
	payloadSize := 0
	for _, lace := range page[len(header):] {
		payloadSize += int(lace)
	}
	lastLaceContinues := segments > 0 && page[len(header)+segments-1] == 255
	if ending && lastLaceContinues {
		return ErrInvalidOpus
	}
	completePage := make([]byte, len(page)+payloadSize)
	copy(completePage, page)
	page = completePage
	if _, err := io.ReadFull(r.source, page[len(header)+segments:]); err != nil {
		return err
	}
	expected := binary.LittleEndian.Uint32(page[22:26])
	for i := 22; i < 26; i++ {
		page[i] = 0
	}
	actual := oggCRC(page)
	binary.LittleEndian.PutUint32(page[22:26], expected)
	if actual != expected {
		return ErrInvalidOpus
	}
	if err := r.validateLogicalPage(page, segments, flags, binary.LittleEndian.Uint64(header[6:14])); err != nil {
		return err
	}
	r.started = true
	r.serial = serial
	r.nextSequence = sequence + 1
	r.continued = lastLaceContinues
	r.eos = ending
	r.page, r.offset = page, 0
	return nil
}

func (r *checkedOggReader) validateLogicalPage(page []byte, segments int, flags byte, granule uint64) error {
	segmentTable := page[27 : 27+segments]
	payload := page[27+segments:]
	switch r.headerState {
	case 0:
		if flags != 0x02 || granule != 0 || segments != 1 || segmentTable[0] != 19 || !validOpusHeadPacket(payload) {
			return ErrInvalidOpus
		}
		r.headerState = 1
	case 1:
		if flags != 0 || granule != 0 || segments == 0 || len(payload) > maxOpusTagsBytes || segmentTable[len(segmentTable)-1] == 255 {
			return ErrInvalidOpus
		}
		for _, lace := range segmentTable[:len(segmentTable)-1] {
			if lace != 255 {
				return ErrInvalidOpus // Restrict OpusTags to one complete packet on page 1.
			}
		}
		if !validOpusTagsPacket(payload) {
			return ErrInvalidOpus
		}
		r.headerState = 2
	case 2:
		completedPackets := 0
		for _, lace := range segmentTable {
			if lace < 255 {
				completedPackets++
			}
		}
		noGranule := ^uint64(0)
		if completedPackets == 0 {
			if flags&0x04 != 0 {
				if granule == noGranule || granule < r.lastGranule {
					return ErrInvalidOpus
				}
			} else if granule != noGranule {
				return ErrInvalidOpus
			}
		} else {
			if granule == noGranule || granule < r.lastGranule {
				return ErrInvalidOpus
			}
			r.lastGranule = granule
		}
	default:
		return ErrInvalidOpus
	}
	return nil
}

func validOpusHeadPacket(packet []byte) bool {
	return len(packet) == 19 && string(packet[:8]) == "OpusHead" && packet[8] <= 15 &&
		packet[9] == outputChannels && binary.LittleEndian.Uint32(packet[12:16]) == outputRate && packet[18] == 0
}

func validOpusTagsPacket(packet []byte) bool {
	if len(packet) < 16 || len(packet) > maxOpusTagsBytes || string(packet[:8]) != "OpusTags" {
		return false
	}
	position := 8
	vendorLength := uint64(binary.LittleEndian.Uint32(packet[position : position+4]))
	position += 4
	if vendorLength > maxOpusTagsBytes || int(vendorLength) > len(packet)-position-4 {
		return false
	}
	position += int(vendorLength)
	commentCount := binary.LittleEndian.Uint32(packet[position : position+4])
	position += 4
	if commentCount > maxOpusTagsBytes/4 || int(commentCount) > (len(packet)-position)/4 {
		return false
	}
	for i := uint32(0); i < commentCount; i++ {
		commentLength := uint64(binary.LittleEndian.Uint32(packet[position : position+4]))
		position += 4
		if commentLength > maxOpusTagsBytes || int(commentLength) > len(packet)-position {
			return false
		}
		position += int(commentLength)
	}
	for _, padding := range packet[position:] {
		if padding != 0 {
			return false
		}
	}
	return true
}

func oggCRC(page []byte) uint32 {
	var checksum uint32
	for _, value := range page {
		checksum = (checksum << 8) ^ oggCRCTable[byte(checksum>>24)^value]
	}
	return checksum
}

var oggCRCTable = makeOGGCRCTable()

func makeOGGCRCTable() [256]uint32 {
	var table [256]uint32
	for i := range table {
		value := uint32(i) << 24
		for bit := 0; bit < 8; bit++ {
			if value&0x80000000 != 0 {
				value = (value << 1) ^ 0x04c11db7
			} else {
				value <<= 1
			}
		}
		table[i] = value
	}
	return table
}

// readPacketSafely contains parser panics for malformed Ogg page/lacing state and turns them
// into ordinary stream errors. checkedOggReader verifies all Ogg page checksums before parsing.
func readPacketSafely(reader oggPacketReader) (packet []byte, err error) {
	defer func() {
		if recover() != nil {
			packet = nil
			err = ErrInvalidOpus
		}
	}()
	return reader.ReadPacket()
}

func writeAll(w io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := w.Write(data)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}

func signal(ch chan<- struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

func closeReader(r io.Reader) {
	if c, ok := r.(io.Closer); ok {
		_ = c.Close()
	}
}
