// Package spotify runs the pinned Spotify helper as a bounded child process.
package spotify

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jonasthim/dilla/internal/bot/client"
	musicruntime "github.com/jonasthim/dilla/internal/bot/music/runtime"
)

const (
	MaxStatusLine = 256
	// The helper resamples librespot to 48 kHz stereo s16le: 960 samples
	// per channel every 20 ms, four bytes per stereo sample pair.
	PCMBytesPerFrame = 3840
	maxCredential    = 64 * 1024
	startupTimeout   = 40 * time.Second
	operationTimeout = 2 * time.Second
	stopTimeout      = 2 * time.Second
	pcmIdleTimeout   = 10 * time.Second
)

var (
	ErrInvalidConfig             = errors.New("spotify: invalid adapter configuration")
	ErrHelperContractUnsupported = errors.New("spotify: helper protocol unavailable")
	ErrPlaybackFailed            = errors.New("spotify: playback failed")
	ErrStopped                   = errors.New("spotify: playback stopped")
	ErrControlInProgress         = errors.New("spotify: another control is in progress")
)

type Config struct {
	HelperPath, StateDir string
	Credential           *os.File
}
type Adapter struct{ config Config }

func New(config Config) (*Adapter, error) {
	if !filepath.IsAbs(config.HelperPath) || !filepath.IsAbs(config.StateDir) || config.Credential == nil {
		return nil, ErrInvalidConfig
	}
	return &Adapter{config: config}, nil
}

type Status struct {
	OK    bool    `json:"ok"`
	Phase string  `json:"phase"`
	Code  *string `json:"code"`
}

// ParseStatus reads one length-prefixed helper status frame. Unknown fields and
// status values are rejected so child diagnostics never become service output.
func ParseStatus(r io.Reader) (Status, error) {
	var out Status
	if r == nil {
		return out, errors.New("spotify: invalid status")
	}
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return out, errors.New("spotify: invalid status")
	}
	n := binary.BigEndian.Uint32(header[:])
	if n == 0 || n > MaxStatusLine {
		return out, errors.New("spotify: invalid status")
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return out, errors.New("spotify: invalid status")
	}
	d := json.NewDecoder(strings.NewReader(string(b)))
	d.DisallowUnknownFields()
	if d.Decode(&out) != nil || (out.OK == (out.Code != nil)) || !validPhase(out.Phase) || (out.Code != nil && !validCode(*out.Code)) {
		return Status{}, errors.New("spotify: invalid status")
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return Status{}, errors.New("spotify: invalid status")
	}
	return out, nil
}
func validPhase(v string) bool {
	switch v {
	case "startup", "authorization", "playback", "control", "shutdown":
		return true
	}
	return false
}
func validCode(v string) bool {
	switch v {
	case "invalid_request", "credential_unavailable", "provider_unavailable", "provider_connect", "provider_connect_timeout", "build_configuration", "playback_timeout", "cancelled", "internal":
		return true
	}
	return false
}

func (a *Adapter) Start(ctx context.Context, source string) (*Playback, error) {
	if a == nil || ctx == nil || a.config.Credential == nil || !validTrackSource(source) {
		return nil, ErrInvalidConfig
	}
	ctx = client.WithoutCooperativeStop(client.WithBackground(ctx))
	credentialInfo, err := a.config.Credential.Stat()
	if err != nil || !credentialInfo.Mode().IsRegular() || credentialInfo.Size() < 1 || credentialInfo.Size() > maxCredential {
		return nil, ErrInvalidConfig
	}
	credentialRead, credentialWrite, err := os.Pipe()
	if err != nil {
		return nil, ErrPlaybackFailed
	}
	cmd := exec.CommandContext(ctx, a.config.HelperPath, source) // #nosec G204 -- helper is absolute validated config; source is restricted by validTrackSource.
	cmd.Env = []string{}
	cmd.ExtraFiles = []*os.File{credentialRead} // child descriptor 3
	stdin, err := cmd.StdinPipe()
	if err != nil {
		_ = credentialRead.Close()
		_ = credentialWrite.Close()
		return nil, ErrPlaybackFailed
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		_ = credentialRead.Close()
		_ = credentialWrite.Close()
		return nil, ErrPlaybackFailed
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		_ = credentialRead.Close()
		_ = credentialWrite.Close()
		return nil, ErrPlaybackFailed
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		_ = stderr.Close()
		_ = credentialRead.Close()
		_ = credentialWrite.Close()
		return nil, ErrPlaybackFailed
	}
	_ = credentialRead.Close()
	p := &Playback{ctx: ctx, cmd: cmd, stdin: stdin, stdout: stdout, stderr: stderr, frames: make(chan []byte, 4), statuses: make(chan Status, 4), readErr: make(chan error, 1), done: make(chan struct{}), discard: make(chan struct{})}
	p.pipeReaders.Add(3)
	go p.wait()
	go p.readPCM(stdout)
	go p.readStatuses(stderr)
	go p.writeCredential(a.config.Credential, credentialWrite, credentialInfo.Size())
	go func() {
		select {
		case <-ctx.Done():
			p.explicitStop.Store(true)
			p.killAndWait()
		case <-p.done:
		}
	}()
	timer := time.NewTimer(startupTimeout)
	defer timer.Stop()
	ready, receivedPCM := false, false
	readinessPhases := [...]string{"startup", "authorization", "playback"}
	phaseIndex := 0
	statusFrames, pcmFrames, processDone := (<-chan Status)(p.statuses), (<-chan []byte)(p.frames), p.done
	for !ready || !receivedPCM {
		if statusFrames == nil && pcmFrames == nil {
			select {
			case err := <-p.readErr:
				return nil, safePlaybackError(err)
			default:
				return nil, ErrPlaybackFailed
			}
		}
		select {
		case status, ok := <-statusFrames:
			if !ok {
				statusFrames = nil
				continue
			}
			if !status.OK {
				p.killAndWait()
				return nil, ErrPlaybackFailed
			}
			if phaseIndex == len(readinessPhases) || status.Phase != readinessPhases[phaseIndex] {
				p.killAndWait()
				return nil, ErrHelperContractUnsupported
			}
			phaseIndex++
			if status.Phase == "playback" {
				ready = true
			}
		case frame, ok := <-pcmFrames:
			if !ok {
				pcmFrames = nil
				continue
			}
			if !receivedPCM {
				p.pending = frame
				receivedPCM = true
			}
		case err := <-p.readErr:
			p.killAndWait()
			return nil, safePlaybackError(err)
		case <-processDone:
			processDone = nil
		case <-ctx.Done():
			p.killAndWait()
			return nil, ErrPlaybackFailed
		case <-timer.C:
			p.killAndWait()
			return nil, ErrPlaybackFailed
		}
	}
	p.startPCMWatchdog()
	return p, nil
}

// Only errors built from validated helper enums may carry extra diagnostic detail.
func safePlaybackError(err error) error {
	if errors.Is(err, ErrPlaybackFailed) {
		return err
	}
	return ErrPlaybackFailed
}

type Playback struct {
	ctx          context.Context
	cmd          *exec.Cmd
	stdin        io.WriteCloser
	stdout       io.ReadCloser
	stderr       io.ReadCloser
	frames       chan []byte
	statuses     chan Status
	readErr      chan error
	done         chan struct{}
	mu           sync.Mutex
	stopOnce     sync.Once
	stopErr      error
	waitErr      error
	pending      []byte
	frame        []byte
	lastPCM      atomic.Int64
	paused       atomic.Bool
	explicitStop atomic.Bool
	pipeReaders  sync.WaitGroup
	discard      chan struct{}
	discardOnce  sync.Once
}

// PCMFormat describes the helper output, after its 44.1-to-48 kHz conversion.
func (*Playback) PCMFormat() musicruntime.PCMFormat {
	return musicruntime.PCMFormat{SampleRate: 48000, Channels: 2}
}

// Wait only after both StdoutPipe and StderrPipe readers have consumed EOF.
// Calling Cmd.Wait earlier can close those pipes before their buffered frames drain.
func (p *Playback) wait() { p.pipeReaders.Wait(); p.waitErr = p.cmd.Wait(); close(p.done) }

// writeCredential copies from offset zero into this child's private pipe. ReadAt
// leaves the configured source offset untouched, and the transient chunk is cleared.
func (p *Playback) writeCredential(source *os.File, target *os.File, size int64) {
	defer p.pipeReaders.Done()
	defer func() { _ = target.Close() }()
	buf := make([]byte, 4096)
	defer clear(buf)
	for offset := int64(0); offset < size; {
		want := len(buf)
		if remaining := size - offset; remaining < int64(want) {
			want = int(remaining)
		}
		n, err := source.ReadAt(buf[:want], offset)
		if n != want || (err != nil && (err != io.EOF || n != want)) {
			return
		}
		for written := 0; written < n; {
			m, writeErr := target.Write(buf[written:n])
			if writeErr != nil || m == 0 {
				return
			}
			written += m
		}
		offset += int64(n)
		clear(buf[:n])
	}
}

func (p *Playback) processError() error {
	if p.waitErr != nil {
		return ErrPlaybackFailed
	}
	return nil
}
func (p *Playback) startPCMWatchdog() {
	p.lastPCM.Store(time.Now().UnixNano())
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-p.done:
				return
			case now := <-ticker.C:
				if p.paused.Load() {
					p.lastPCM.Store(now.UnixNano())
					continue
				}
				if now.Sub(time.Unix(0, p.lastPCM.Load())) > pcmIdleTimeout {
					select {
					case p.readErr <- ErrPlaybackFailed:
					default:
					}
					p.killAndWait()
					return
				}
			}
		}
	}()
}
func (p *Playback) kill() {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return
	}
	_ = p.stdin.Close()
	_ = p.cmd.Process.Kill()
}
func (p *Playback) killAndWait() {
	p.discardOutput()
	p.kill()
	timer := time.NewTimer(stopTimeout)
	defer timer.Stop()
	select {
	case <-p.done:
	case <-timer.C:
	}
}
func (p *Playback) discardOutput() {
	p.discardOnce.Do(func() {
		close(p.discard)
		_ = p.stdout.Close()
		_ = p.stderr.Close()
	})
}

func (p *Playback) readPCM(r io.Reader) {
	defer p.pipeReaders.Done()
	defer close(p.frames)
	for {
		var h [9]byte
		if _, err := io.ReadFull(r, h[:]); err != nil {
			if err != io.EOF {
				select {
				case p.readErr <- err:
				default:
				}
			}
			return
		}
		if string(h[:4]) != "DPCM" || h[4] != 1 || binary.BigEndian.Uint32(h[5:]) != PCMBytesPerFrame {
			select {
			case p.readErr <- ErrPlaybackFailed:
			default:
			}
			return
		}
		frame := make([]byte, PCMBytesPerFrame)
		if _, err := io.ReadFull(r, frame); err != nil {
			select {
			case p.readErr <- ErrPlaybackFailed:
			default:
			}
			return
		}
		select {
		case p.frames <- frame:
			p.lastPCM.Store(time.Now().UnixNano())
		case <-p.discard:
			return
		case <-p.done:
			return
		}
	}
}
func (p *Playback) readStatuses(r io.Reader) {
	defer p.pipeReaders.Done()
	defer close(p.statuses)
	for {
		var header [4]byte
		nread, err := io.ReadFull(r, header[:])
		if errors.Is(err, io.EOF) && nread == 0 {
			return
		}
		if err != nil {
			select {
			case p.readErr <- ErrPlaybackFailed:
			default:
			}
			return
		}
		n := binary.BigEndian.Uint32(header[:])
		if n == 0 || n > MaxStatusLine {
			select {
			case p.readErr <- ErrPlaybackFailed:
			default:
			}
			return
		}
		frame := make([]byte, 4+int(n))
		copy(frame, header[:])
		if _, err := io.ReadFull(r, frame[4:]); err != nil {
			select {
			case p.readErr <- ErrPlaybackFailed:
			default:
			}
			return
		}
		st, err := ParseStatus(strings.NewReader(string(frame)))
		if err != nil {
			select {
			case p.readErr <- ErrPlaybackFailed:
			default:
			}
			return
		}
		if !st.OK {
			select {
			case p.readErr <- &helperFailure{phase: st.Phase, code: *st.Code}:
			default:
			}
			return
		}
		select {
		case p.statuses <- st:
		case <-p.done:
			return
		}
	}
}

// Read streams raw PCM. Call Read from one goroutine at a time; controls may be
// called concurrently. After the first PCM frame, read errors report helper exit.
func (p *Playback) Read(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	for len(p.frame) == 0 {
		if len(p.pending) != 0 {
			p.frame, p.pending = p.pending, nil
			break
		}
		select {
		case p.frame = <-p.frames:
			if len(p.frame) == 0 {
				return 0, p.readEndError()
			}
		case <-p.readErr:
			return 0, ErrPlaybackFailed
		case <-p.done:
			return 0, p.processErrorOrStopped()
		}
	}
	n := copy(b, p.frame)
	p.frame = p.frame[n:]
	return n, nil
}

func (p *Playback) readEndError() error {
	timer := time.NewTimer(stopTimeout)
	defer timer.Stop()
	select {
	case <-p.done:
		return p.processErrorOrStopped()
	case <-p.readErr:
		return ErrPlaybackFailed
	case <-timer.C:
		return ErrPlaybackFailed
	}
}
func (p *Playback) processErrorOrStopped() error {
	if p.explicitStop.Load() {
		return ErrStopped
	}
	if p.processError() != nil {
		return ErrPlaybackFailed
	}
	return io.EOF
}

func (p *Playback) control(ctx context.Context, value string) error {
	if ctx == nil {
		return ErrInvalidConfig
	}
	select {
	case <-p.done:
		return ErrStopped
	default:
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	var frame []byte
	if err := json.Unmarshal([]byte(value), new(any)); err != nil {
		return ErrInvalidConfig
	}
	b := []byte(value)
	if len(b) > 1<<20 {
		return ErrInvalidConfig
	}
	frame = make([]byte, 4+len(b))
	binary.BigEndian.PutUint32(frame, uint32(len(b))) // #nosec G115 -- control frames are capped at 1 MiB above.
	copy(frame[4:], b)
	if !p.mu.TryLock() {
		return ErrControlInProgress
	}
	result := make(chan error, 1)
	go func() { defer p.mu.Unlock(); _, err := p.stdin.Write(frame); result <- err }()
	timer := time.NewTimer(operationTimeout)
	defer timer.Stop()
	select {
	case err := <-result:
		if err != nil {
			return ErrPlaybackFailed
		}
		return nil
	case <-ctx.Done():
		p.killAndWait()
		return ctx.Err()
	case <-timer.C:
		p.killAndWait()
		return ErrPlaybackFailed
	case <-p.done:
		return ErrStopped
	}
}

// Pause submits the framed command to the helper; the protocol has no provider acknowledgment.
func (p *Playback) Pause(ctx context.Context) error {
	if err := p.control(ctx, `{"op":"pause"}`); err != nil {
		return err
	}
	p.paused.Store(true)
	return nil
}

// Resume submits the framed command to the helper; the protocol has no provider acknowledgment.
func (p *Playback) Resume(ctx context.Context) error {
	if err := p.control(ctx, `{"op":"resume"}`); err != nil {
		return err
	}
	p.paused.Store(false)
	p.lastPCM.Store(time.Now().UnixNano())
	return nil
}

// Seek submits the framed command to the helper; the protocol has no provider acknowledgment.
func (p *Playback) Seek(ctx context.Context, ms uint32) error {
	return p.control(ctx, fmt.Sprintf(`{"op":"seek","position_ms":%d}`, ms))
}
func (p *Playback) Stop(ctx context.Context) error {
	if ctx == nil {
		return ErrInvalidConfig
	}
	p.stopOnce.Do(func() {
		if p.ctx != nil && p.ctx.Err() != nil {
			p.explicitStop.Store(true)
			p.killAndWait()
			return
		}
		select {
		case <-p.done:
			p.stopErr = nil
			return
		default:
		}
		p.explicitStop.Store(true)
		p.stopErr = p.control(ctx, `{"op":"stop"}`)
		_ = p.stdin.Close()
		select {
		case <-p.done:
			p.stopErr = p.processError()
		case <-ctx.Done():
			p.killAndWait()
			p.stopErr = ctx.Err()
		case <-time.After(stopTimeout):
			p.killAndWait()
			select {
			case <-p.done:
				p.stopErr = p.processError()
			case <-time.After(stopTimeout):
				p.stopErr = ErrPlaybackFailed
			}
		}
	})
	return p.stopErr
}

// Close stops the helper and waits for its process, satisfying io.ReadCloser
// for the downstream PCM-to-Opus bridge.
func (p *Playback) Close() error { return p.Stop(context.Background()) }

var _ io.ReadCloser = (*Playback)(nil)

func validTrackSource(source string) bool {
	const prefix = "spotify:track:"
	if !strings.HasPrefix(source, prefix) {
		return false
	}
	id := strings.TrimPrefix(source, prefix)
	if len(id) != 22 {
		return false
	}
	for _, c := range id {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}
