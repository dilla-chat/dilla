package media

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/bits"
	"os"
	"time"

	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/pion/webrtc/v4/pkg/media"
	"github.com/pion/webrtc/v4/pkg/media/h264reader"

	"github.com/dilla-chat/dilla/internal/sframe"
)

var errNotSPS = errors.New("media: not an SPS NAL")

// h264AUProvider turns an Annex-B file into dilla-sframe/1 access units (G5): SPS and PPS before
// every IDR, every slice of one picture in one sample, no AUD, SEI or filler, and every SPS
// rewritten by canonicalSPS so a libwebrtc receiver leaves it — and the AAD — alone. The SDK's own
// file reader hands an encryptor one bare NAL per sample (SP-15), from which no prefix can be formed.
// It loops the file.
type h264AUProvider struct {
	lksdk.BaseSampleProvider
	path     string
	f        *os.File
	r        *h264reader.H264Reader
	sps, pps []byte
	held     *h264reader.NAL // the NAL that opened the next access unit
	frame    time.Duration
}

func newH264AUProvider(path string) (*h264AUProvider, error) {
	p := &h264AUProvider{path: path, frame: time.Second / 30}
	if err := p.open(); err != nil {
		return nil, err
	}
	return p, nil
}

func (p *h264AUProvider) open() error {
	if p.f != nil {
		_ = p.f.Close()
	}
	f, err := os.Open(p.path)
	if err != nil {
		return err
	}
	r, err := h264reader.NewReader(f)
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("media: %s: %w", p.path, err)
	}
	p.f, p.r = f, r
	return nil
}

func (p *h264AUProvider) nextNAL() (*h264reader.NAL, error) {
	if n := p.held; n != nil {
		p.held = nil
		return n, nil
	}
	nal, err := p.r.NextNAL()
	if errors.Is(err, io.EOF) {
		if err := p.open(); err != nil {
			return nil, err
		}
		nal, err = p.r.NextNAL()
	}
	return nal, err
}

// NextSample returns the next access unit. A parameter set or a slice with first_mb_in_slice = 0
// after a slice closes the picture in hand; that NAL is held for the next call.
func (p *h264AUProvider) NextSample(ctx context.Context) (media.Sample, error) {
	var pic [][]byte
	idr := false
	for {
		if ctx.Err() != nil {
			return media.Sample{}, io.EOF // the SDK's write loop ends quietly on EOF
		}
		nal, err := p.nextNAL()
		if err != nil {
			return media.Sample{}, err
		}
		switch nal.UnitType {
		case h264reader.NalUnitTypeSPS, h264reader.NalUnitTypePPS:
			if len(pic) > 0 {
				p.held = nal
				return p.sample(pic, idr), nil
			}
			if nal.UnitType == h264reader.NalUnitTypePPS {
				p.pps = bytes.Clone(nal.Data)
				continue
			}
			sps, err := canonicalSPS(nal.Data)
			if err != nil {
				return media.Sample{}, err
			}
			p.sps = sps
		case h264reader.NalUnitTypeCodedSliceIdr, h264reader.NalUnitTypeCodedSliceNonIdr:
			first := firstMB(nal.Data) == 0
			if first && len(pic) > 0 {
				p.held = nal
				return p.sample(pic, idr), nil
			}
			if (len(pic) == 0 && !first) || p.sps == nil || p.pps == nil {
				continue // a slice without its picture's first slice, or before the parameter sets
			}
			idr = idr || nal.UnitType == h264reader.NalUnitTypeCodedSliceIdr
			pic = append(pic, bytes.Clone(nal.Data))
		default:
			// AUD (9), SEI (6), filler (12), end of sequence or stream: never sent.
		}
	}
}

func (p *h264AUProvider) sample(pic [][]byte, idr bool) media.Sample {
	var out []byte
	if idr {
		out = appendNAL(appendNAL(out, p.sps), p.pps)
	}
	for _, s := range pic {
		out = appendNAL(out, s)
	}
	return media.Sample{Data: out, Duration: p.frame}
}

func (p *h264AUProvider) Close() error {
	if p.f == nil {
		return nil
	}
	return p.f.Close()
}

func appendNAL(out, nal []byte) []byte { return append(append(out, 0, 0, 0, 1), nal...) }

// firstMB is a slice NAL's first_mb_in_slice; an unreadable one answers 1 (never a picture start).
func firstMB(nal []byte) uint64 {
	if len(nal) < 2 {
		return 1
	}
	r := &bitReader{data: sframe.RBSPUnescape(0, nal[1:min(len(nal), 9)])}
	v, err := r.ue()
	if err != nil {
		return 1
	}
	return v
}

var errShortSPS = errors.New("media: the SPS ends early")

// bitReader reads an RBSP MSB-first.
type bitReader struct {
	data []byte
	bit  int
}

func (r *bitReader) u(n int) (uint64, error) {
	var v uint64
	for range n {
		if r.bit/8 >= len(r.data) {
			return 0, errShortSPS
		}
		v = v<<1 | uint64(r.data[r.bit/8]>>(7-r.bit%8)&1)
		r.bit++
	}
	return v, nil
}

func (r *bitReader) ue() (uint64, error) {
	zeros := 0
	for {
		b, err := r.u(1)
		if err != nil {
			return 0, err
		}
		if b == 1 {
			break
		}
		if zeros++; zeros > 31 {
			return 0, errShortSPS
		}
	}
	v, err := r.u(zeros)
	if err != nil {
		return 0, err
	}
	return 1<<zeros - 1 + v, nil
}

func (r *bitReader) se() (int64, error) {
	k, err := r.ue()
	if err != nil {
		return 0, err
	}
	mag := int64((k + 1) / 2) //nolint:gosec // G115: k < 2^32 (ue stops at 31 zeros)
	if k%2 == 1 {
		return mag, nil
	}
	return -mag, nil
}

// bitWriter writes an RBSP MSB-first.
type bitWriter struct {
	out []byte
	bit int
}

func (w *bitWriter) put(one bool) {
	if w.bit%8 == 0 {
		w.out = append(w.out, 0)
	}
	if one {
		w.out[len(w.out)-1] |= 0x80 >> (w.bit % 8)
	}
	w.bit++
}

func (w *bitWriter) u(n int, v uint64) {
	for i := n - 1; i >= 0; i-- {
		w.put(v>>i&1 == 1)
	}
}

func (w *bitWriter) ue(v uint64) {
	x := v + 1
	n := bits.Len64(x)
	w.u(n-1, 0)
	w.u(n, x)
}

func (w *bitWriter) se(v int64) {
	if v > 0 {
		w.ue(uint64(2*v - 1))
		return
	}
	w.ue(uint64(-2 * v))
}

func (w *bitWriter) trailing() {
	w.put(true)
	for w.bit%8 != 0 {
		w.put(false)
	}
}

// spsCopier reads an SPS RBSP and writes the same fields back.
type spsCopier struct {
	r *bitReader
	w *bitWriter
}

func (c spsCopier) u(n int) (uint64, error) {
	v, err := c.r.u(n)
	if err == nil {
		c.w.u(n, v)
	}
	return v, err
}

func (c spsCopier) ue() (uint64, error) {
	v, err := c.r.ue()
	if err == nil {
		c.w.ue(v)
	}
	return v, err
}

func (c spsCopier) se() (int64, error) {
	v, err := c.r.se()
	if err == nil {
		c.w.se(v)
	}
	return v, err
}

func (c spsCopier) hrd() error {
	cpb, err := c.ue()
	if err != nil {
		return err
	}
	if cpb > 31 {
		return errShortSPS
	}
	if _, err := c.u(8); err != nil {
		return err
	}
	for range cpb + 1 {
		if _, err := c.ue(); err != nil {
			return err
		}
		if _, err := c.ue(); err != nil {
			return err
		}
		if _, err := c.u(1); err != nil {
			return err
		}
	}
	_, err = c.u(20)
	return err
}

// writeRestriction is libwebrtc's AddBitstreamRestriction: no reordering, a decoded-picture buffer
// of max_num_ref_frames.
func writeRestriction(w *bitWriter, maxRef uint64) {
	w.u(1, 1) // bitstream_restriction_flag
	w.u(1, 1) // motion_vectors_over_pic_boundaries_flag
	w.ue(2)   // max_bytes_per_pic_denom
	w.ue(1)   // max_bits_per_mb_denom
	w.ue(16)  // log2_max_mv_length_horizontal
	w.ue(16)  // log2_max_mv_length_vertical
	w.ue(0)   // max_num_reorder_frames
	w.ue(maxRef)
}

// canonicalSPS is what libwebrtc's outgoing SpsVuiRewriter would emit for nal (one SPS NAL with its
// header byte): every field copied, a VUI added when there is none, and the bitstream restriction
// set to max_num_reorder_frames = 0, max_dec_frame_buffering = max_num_ref_frames. The result passes
// sframe.CheckSPSVUI, so a receiver's incoming rewrite leaves it unchanged.
func canonicalSPS(nal []byte) ([]byte, error) {
	if len(nal) < 4 || nal[0]&0x1f != 7 {
		return nil, errNotSPS
	}
	c := spsCopier{r: &bitReader{data: sframe.RBSPUnescape(0, nal[1:])}, w: &bitWriter{}}
	profile, err := c.u(8)
	if err != nil {
		return nil, err
	}
	if _, err := c.u(16); err != nil { // constraint flags, reserved bits, level_idc
		return nil, err
	}
	if _, err := c.ue(); err != nil { // seq_parameter_set_id
		return nil, err
	}
	switch profile {
	case 100, 110, 122, 244, 44, 83, 86, 118, 128, 138, 139, 134, 135:
		if err := c.highProfileFields(); err != nil {
			return nil, err
		}
	}
	if _, err := c.ue(); err != nil { // log2_max_frame_num_minus4
		return nil, err
	}
	poc, err := c.ue()
	if err != nil {
		return nil, err
	}
	switch poc {
	case 0:
		if _, err := c.ue(); err != nil {
			return nil, err
		}
	case 1:
		if _, err := c.u(1); err != nil {
			return nil, err
		}
		if _, err := c.se(); err != nil {
			return nil, err
		}
		if _, err := c.se(); err != nil {
			return nil, err
		}
		cycle, err := c.ue()
		if err != nil {
			return nil, err
		}
		if cycle > 255 {
			return nil, errShortSPS
		}
		for range cycle {
			if _, err := c.se(); err != nil {
				return nil, err
			}
		}
	}
	maxRef, err := c.ue()
	if err != nil {
		return nil, err
	}
	if _, err := c.u(1); err != nil { // gaps_in_frame_num_value_allowed_flag
		return nil, err
	}
	if _, err := c.ue(); err != nil { // pic_width_in_mbs_minus1
		return nil, err
	}
	if _, err := c.ue(); err != nil { // pic_height_in_map_units_minus1
		return nil, err
	}
	frameMbsOnly, err := c.u(1)
	if err != nil {
		return nil, err
	}
	if frameMbsOnly == 0 {
		if _, err := c.u(1); err != nil {
			return nil, err
		}
	}
	if _, err := c.u(1); err != nil { // direct_8x8_inference_flag
		return nil, err
	}
	crop, err := c.u(1)
	if err != nil {
		return nil, err
	}
	if crop == 1 {
		for range 4 {
			if _, err := c.ue(); err != nil {
				return nil, err
			}
		}
	}
	vui, err := c.r.u(1)
	if err != nil {
		return nil, err
	}
	c.w.u(1, 1) // vui_parameters_present_flag
	if vui == 0 {
		c.w.u(8, 0) // aspect, overscan, video signal, chroma loc, timing, nal hrd, vcl hrd, pic_struct: all absent
		writeRestriction(c.w, maxRef)
	} else if err := c.vui(maxRef); err != nil {
		return nil, err
	}
	c.w.trailing()
	return append([]byte{nal[0]}, sframe.RBSPEscape(0, c.w.out)...), nil
}

func (c spsCopier) highProfileFields() error {
	chroma, err := c.ue()
	if err != nil {
		return err
	}
	if chroma == 3 {
		if _, err := c.u(1); err != nil {
			return err
		}
	}
	for range 2 { // bit_depth_luma_minus8, bit_depth_chroma_minus8
		if _, err := c.ue(); err != nil {
			return err
		}
	}
	if _, err := c.u(1); err != nil { // qpprime_y_zero_transform_bypass_flag
		return err
	}
	present, err := c.u(1)
	if err != nil || present == 0 {
		return err
	}
	lists := 8
	if chroma == 3 {
		lists = 12
	}
	for i := range lists {
		on, err := c.u(1)
		if err != nil {
			return err
		}
		if on == 0 {
			continue
		}
		size := 64
		if i < 6 {
			size = 16
		}
		last, next := int64(8), int64(8)
		for range size {
			if next != 0 {
				d, err := c.se()
				if err != nil {
					return err
				}
				next = ((last+d)%256 + 256) % 256
			}
			if next != 0 {
				last = next
			}
		}
	}
	return nil
}

// vui copies a present VUI through pic_struct_present_flag and replaces its bitstream restriction.
func (c spsCopier) vui(maxRef uint64) error {
	if on, err := c.u(1); err != nil { // aspect_ratio_info_present_flag
		return err
	} else if on == 1 {
		idc, err := c.u(8)
		if err != nil {
			return err
		}
		if idc == 255 {
			if _, err := c.u(32); err != nil {
				return err
			}
		}
	}
	if on, err := c.u(1); err != nil { // overscan_info_present_flag
		return err
	} else if on == 1 {
		if _, err := c.u(1); err != nil {
			return err
		}
	}
	if on, err := c.u(1); err != nil { // video_signal_type_present_flag
		return err
	} else if on == 1 {
		if _, err := c.u(4); err != nil {
			return err
		}
		if colour, err := c.u(1); err != nil {
			return err
		} else if colour == 1 {
			if _, err := c.u(24); err != nil {
				return err
			}
		}
	}
	if on, err := c.u(1); err != nil { // chroma_loc_info_present_flag
		return err
	} else if on == 1 {
		for range 2 {
			if _, err := c.ue(); err != nil {
				return err
			}
		}
	}
	if on, err := c.u(1); err != nil { // timing_info_present_flag
		return err
	} else if on == 1 {
		if _, err := c.u(32); err != nil { // num_units_in_tick
			return err
		}
		if _, err := c.u(33); err != nil { // time_scale, fixed_frame_rate_flag
			return err
		}
	}
	nalHRD, err := c.u(1)
	if err != nil {
		return err
	}
	if nalHRD == 1 {
		if err := c.hrd(); err != nil {
			return err
		}
	}
	vclHRD, err := c.u(1)
	if err != nil {
		return err
	}
	if vclHRD == 1 {
		if err := c.hrd(); err != nil {
			return err
		}
	}
	if nalHRD == 1 || vclHRD == 1 {
		if _, err := c.u(1); err != nil { // low_delay_hrd_flag
			return err
		}
	}
	if _, err := c.u(1); err != nil { // pic_struct_present_flag
		return err
	}
	restriction, err := c.r.u(1)
	if err != nil {
		return err
	}
	if restriction == 0 {
		writeRestriction(c.w, maxRef)
		return nil
	}
	c.w.u(1, 1)                       // bitstream_restriction_flag
	if _, err := c.u(1); err != nil { // motion_vectors_over_pic_boundaries_flag
		return err
	}
	for range 4 {
		if _, err := c.ue(); err != nil {
			return err
		}
	}
	if _, err := c.r.ue(); err != nil { // the old max_num_reorder_frames, replaced
		return err
	}
	if _, err := c.r.ue(); err != nil { // the old max_dec_frame_buffering, replaced
		return err
	}
	c.w.ue(0)
	c.w.ue(maxRef)
	return nil
}
