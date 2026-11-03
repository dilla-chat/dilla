package media

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/livekit/protocol/livekit"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/livekit/server-sdk-go/v2/pkg/oggreader"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
	"github.com/pion/webrtc/v4/pkg/media/ivfreader"

	"github.com/dilla-chat/dilla/internal/sframe"
)

// h264Fmtp is the one H.264 registration dilla negotiates (DEV-06): packetization mode 1,
// Constrained Baseline 3.1.
const h264Fmtp = "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f"

// PublishOpusOgg publishes an Ogg Opus file as the microphone, encrypted by enc, under the name
// "opus".
func PublishOpusOgg(room *lksdk.Room, path string, enc *FrameEncryptor) (*lksdk.LocalTrackPublication, error) {
	p, err := newOggProvider(path)
	if err != nil {
		return nil, err
	}
	return publish(room, p, webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2},
		enc, "opus", livekit.TrackSource_MICROPHONE)
}

// PublishLiveOpus publishes bounded live Opus packets as an encrypted microphone track named
// "music". The sender is bound here to the protocol's Opus microphone slot so callers cannot
// accidentally publish the stream with a camera or screen counter partition.
func PublishLiveOpus(room *lksdk.Room, p *LiveOpusProvider, sender *sframe.Sender) (*lksdk.LocalTrackPublication, error) {
	if room == nil || p == nil || sender == nil {
		return nil, errors.New("media: live Opus publication requires room, provider, and sender")
	}
	return publishWithCompletion(room, p, webrtc.RTPCodecCapability{
		MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2,
	}, NewFrameEncryptor(sender, sframe.Opus, sframe.Mic, 0), "music", livekit.TrackSource_MICROPHONE, nil)
}

// PublishVP8IVF publishes a VP8 IVF file as source (single layer), encrypted by enc, under the name
// "vp8".
func PublishVP8IVF(room *lksdk.Room, path string, enc *FrameEncryptor, source livekit.TrackSource) (*lksdk.LocalTrackPublication, error) {
	p, err := newIVFProvider(path)
	if err != nil {
		return nil, err
	}
	return publish(room, p, webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8, ClockRate: 90000}, enc, "vp8", source)
}

// PublishH264 publishes an Annex-B H.264 file through the access-unit builder as source (single
// layer), encrypted by enc, under the name "h264".
func PublishH264(room *lksdk.Room, path string, enc *FrameEncryptor, source livekit.TrackSource) (*lksdk.LocalTrackPublication, error) {
	p, err := newH264AUProvider(path)
	if err != nil {
		return nil, err
	}
	return publish(room, p, webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000, SDPFmtpLine: h264Fmtp},
		enc, "h264", source)
}

// publish binds p to a new track once the track is negotiated and publishes it with
// Encryption_CUSTOM — never AttachUserTimestamp/AttachFrameId, whose trailers LiveKit would strip.
func publish(room *lksdk.Room, p lksdk.SampleProvider, codec webrtc.RTPCodecCapability, enc *FrameEncryptor,
	name string, source livekit.TrackSource) (*lksdk.LocalTrackPublication, error) {
	return publishWithCompletion(room, p, codec, enc, name, source, func() { _ = p.Close() })
}

func publishWithCompletion(room *lksdk.Room, p lksdk.SampleProvider, codec webrtc.RTPCodecCapability,
	enc *FrameEncryptor, name string, source livekit.TrackSource, onWriteComplete func()) (*lksdk.LocalTrackPublication, error) {
	track, err := lksdk.NewLocalTrack(codec, lksdk.WithFrameEncryptor(enc))
	if err != nil {
		_ = p.Close()
		return nil, err
	}
	track.OnBind(func() {
		// StartWrite fails only for a provider whose OnBind fails, which these never do.
		_ = track.StartWrite(p, onWriteComplete)
	})
	pub, err := room.LocalParticipant.PublishTrack(track, &lksdk.TrackPublicationOptions{
		Name: name, Source: source, Encryption: livekit.Encryption_CUSTOM,
	})
	if err != nil {
		_ = p.Close()
		return nil, err
	}
	return pub, nil
}

func eof(err error) bool { return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) }

// oggProvider hands out one Opus packet per sample and loops the file.
type oggProvider struct {
	lksdk.BaseSampleProvider
	path string
	f    *os.File
	r    *oggreader.OggReader
}

func newOggProvider(path string) (*oggProvider, error) {
	p := &oggProvider{path: path}
	if err := p.open(); err != nil {
		return nil, err
	}
	return p, nil
}

func (p *oggProvider) open() error {
	if p.f != nil {
		_ = p.f.Close()
	}
	f, err := os.Open(p.path)
	if err != nil {
		return err
	}
	r, _, err := oggreader.NewOggReader(f)
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("media: %s: %w", p.path, err)
	}
	p.f, p.r = f, r
	return nil
}

func (p *oggProvider) NextSample(ctx context.Context) (media.Sample, error) {
	if ctx.Err() != nil {
		return media.Sample{}, io.EOF
	}
	pkt, err := p.r.ReadPacket()
	if eof(err) {
		if err := p.open(); err != nil {
			return media.Sample{}, err
		}
		pkt, err = p.r.ReadPacket()
	}
	if err != nil {
		return media.Sample{}, err
	}
	d, err := oggreader.ParsePacketDuration(pkt)
	if err != nil {
		return media.Sample{}, err
	}
	return media.Sample{Data: pkt, Duration: d}, nil
}

// CurrentAudioLevel is the RFC 6464 level the SDK attaches: 15 (−15 dBov), the SDK file reader's
// own default for a talking track.
func (p *oggProvider) CurrentAudioLevel() uint8 { return 15 }

func (p *oggProvider) Close() error {
	if p.f == nil {
		return nil
	}
	return p.f.Close()
}

// ivfProvider hands out one VP8 frame per sample at the file's frame rate and loops the file.
type ivfProvider struct {
	lksdk.BaseSampleProvider
	path  string
	f     *os.File
	r     *ivfreader.IVFReader
	frame time.Duration
}

func newIVFProvider(path string) (*ivfProvider, error) {
	p := &ivfProvider{path: path}
	if err := p.open(); err != nil {
		return nil, err
	}
	return p, nil
}

func (p *ivfProvider) open() error {
	if p.f != nil {
		_ = p.f.Close()
	}
	f, err := os.Open(p.path)
	if err != nil {
		return err
	}
	r, h, err := ivfreader.NewWith(f)
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("media: %s: %w", p.path, err)
	}
	p.frame = time.Second / 30
	if h.TimebaseDenominator != 0 && h.TimebaseNumerator != 0 {
		p.frame = time.Duration(h.TimebaseNumerator) * time.Second / time.Duration(h.TimebaseDenominator)
	}
	p.f, p.r = f, r
	return nil
}

func (p *ivfProvider) NextSample(ctx context.Context) (media.Sample, error) {
	if ctx.Err() != nil {
		return media.Sample{}, io.EOF
	}
	frame, _, err := p.r.ParseNextFrame()
	if eof(err) {
		if err := p.open(); err != nil {
			return media.Sample{}, err
		}
		frame, _, err = p.r.ParseNextFrame()
	}
	if err != nil {
		return media.Sample{}, err
	}
	return media.Sample{Data: frame, Duration: p.frame}, nil
}

func (p *ivfProvider) Close() error {
	if p.f == nil {
		return nil
	}
	return p.f.Close()
}
