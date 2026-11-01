// Package youtube contains bounded metadata and PCM adapters. Network-capable
// subprocesses run only inside bubblewrap and use a host CONNECT broker that
// permits public HTTPS destinations on the YouTube host allowlist.
package youtube

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/url"
	"os/exec"
	"regexp"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/jonasthim/dilla/internal/bot/music"
)

const (
	MaxInputBytes    = 512
	MaxSearchResults = 5
	MaxPlaylistItems = 500
	MaxTitleBytes    = music.MaxMetadataBytes
	MaxMetadataBytes = 2 << 20
	MaxDuration      = 24 * time.Hour
)

var (
	ErrInvalidInput       = errors.New("youtube: invalid or unsupported input")
	ErrMetadataTooLarge   = errors.New("youtube: metadata output exceeds limit")
	ErrMetadataInvalid    = errors.New("youtube: invalid or incomplete metadata")
	ErrRunnerRequired     = errors.New("youtube: package-approved sandbox runner required")
	ErrExtractFailed      = errors.New("youtube: metadata extraction failed")
	DefaultProcessTimeout = 2 * time.Minute
	idPattern             = regexp.MustCompile(`^[A-Za-z0-9_-]{11}$`)
	playlistPattern       = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
	schemePattern         = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*:`)
)

type inputKind uint8

const (
	kindSearch inputKind = iota + 1
	kindVideo
	kindPlaylist
)

// NormalizeInput validates the intentionally small input grammar and returns
// a canonical URL or yt-dlp search expression. All other extractor syntax is
// rejected before it can reach yt-dlp.
func NormalizeInput(value string) (kind string, canonical string, err error) {
	if value == "" || len(value) > MaxInputBytes || strings.TrimSpace(value) != value {
		return "", "", ErrInvalidInput
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return "", "", ErrInvalidInput
		}
	}

	for _, host := range []string{"youtube.com/", "www.youtube.com/", "m.youtube.com/", "music.youtube.com/", "youtu.be/"} {
		if strings.HasPrefix(value, host) {
			return normalizeURL("https://" + value)
		}
	}
	if strings.Contains(value, "://") || strings.HasPrefix(value, "//") {
		k, normalized, parseErr := normalizeURL(value)
		if parseErr != nil {
			return "", "", ErrInvalidInput
		}
		return k, normalized, nil
	}
	lower := strings.ToLower(value)
	if strings.HasPrefix(lower, "ytsearch") || strings.HasPrefix(lower, "youtube:") ||
		strings.HasPrefix(lower, "file:") || strings.HasPrefix(lower, "ftp:") ||
		(schemePattern.MatchString(value) && !strings.HasPrefix(lower, "spotify: ")) ||
		strings.Contains(lower, "youtube.com/") || strings.Contains(lower, "youtu.be/") ||
		(looksLikeHost(value) && !displayHostSearch(lower)) {
		return "", "", ErrInvalidInput
	}
	// Keep the search-text byte limit unchanged.
	if len(value) > 200 {
		return "", "", ErrInvalidInput
	}
	return "search", "ytsearch5:" + value, nil
}

func normalizeURL(raw string) (string, string, error) {
	if strings.Contains(raw, "#") {
		return "", "", ErrInvalidInput
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Opaque != "" || u.User != nil || u.Fragment != "" || u.RawFragment != "" {
		return "", "", ErrInvalidInput
	}
	host := strings.ToLower(u.Hostname())
	if host == "" || u.Port() != "" || u.Host != host {
		return "", "", ErrInvalidInput
	}
	if host != "youtube.com" && host != "www.youtube.com" && host != "m.youtube.com" && host != "youtu.be" && host != "music.youtube.com" {
		return "", "", ErrInvalidInput
	}
	values, err := url.ParseQuery(u.RawQuery)
	if err != nil || strings.HasSuffix(u.RawQuery, "&") {
		return "", "", ErrInvalidInput
	}
	if host == "youtu.be" {
		videoID := strings.TrimPrefix(u.EscapedPath(), "/")
		if strings.Count(u.EscapedPath(), "/") != 1 || !idPattern.MatchString(videoID) {
			return "", "", ErrInvalidInput
		}
		return "video", videoURL(videoID), nil
	}
	if u.Path == "/watch" && len(values["v"]) == 1 && idPattern.MatchString(values.Get("v")) {
		for key := range values {
			switch key {
			case "v", "si", "t", "pp", "feature", "list", "index", "ab_channel", "start_radio":
			default:
				return "", "", ErrInvalidInput
			}
		}
		if strings.HasSuffix(u.RawQuery, "&") {
			return "", "", ErrInvalidInput
		}
		return "video", videoURL(values.Get("v")), nil
	}
	shareQuery := func(playlist bool) bool {
		for key, vals := range values {
			if len(vals) != 1 || (key != "si" && key != "feature" && (!playlist || key != "list")) {
				return false
			}
		}
		return true
	}
	for _, prefix := range []string{"/live/", "/shorts/", "/embed/"} {
		if match := strings.TrimPrefix(u.EscapedPath(), prefix); match != u.EscapedPath() && idPattern.MatchString(match) && shareQuery(false) {
			return "video", videoURL(match), nil
		}
	}
	if u.Path == "/playlist" && shareQuery(true) && playlistPattern.MatchString(values.Get("list")) {
		return "playlist", "https://www.youtube.com/playlist?list=" + values.Get("list"), nil
	}
	return "", "", ErrInvalidInput
}

func videoURL(id string) string { return "https://www.youtube.com/watch?v=" + id }

func looksLikeHost(value string) bool {
	if strings.ContainsAny(value, "/?#@") {
		return false
	}
	if strings.Contains(value, ".") {
		return true
	}
	return false
}

// sandboxedRunner can only be implemented by this package. A future concrete
// runner must connect to the qualification harness's verified network sandbox.
type sandboxedRunner interface {
	Run(ctx context.Context, executable string, args []string, outputLimit int) ([]byte, error)
	youtubeSandboxedRunner()
}

// ExecRunner bounds process time, output, and child cleanup, but provides no
// network or memory isolation. It deliberately does not satisfy sandboxedRunner.
type ExecRunner struct {
	MaxStderr int
	Timeout   time.Duration
}

func (r ExecRunner) Run(ctx context.Context, executable string, args []string, outputLimit int) ([]byte, error) {
	if ctx == nil || executable == "" || outputLimit < 1 {
		return nil, ErrExtractFailed
	}
	timeout := r.Timeout
	if timeout <= 0 || timeout > DefaultProcessTimeout {
		timeout = DefaultProcessTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if r.MaxStderr <= 0 {
		r.MaxStderr = 64 << 10
	}
	cmd := exec.CommandContext(ctx, executable, args...) // #nosec G204 -- executable is resolved from configured dependencies; args use validated URLs.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 2 * time.Second
	stdout := &limitedBuffer{limit: outputLimit}
	stderr := &limitedBuffer{limit: r.MaxStderr}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	err := cmd.Run()
	if stdout.exceeded || stderr.exceeded {
		return nil, ErrMetadataTooLarge
	}
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, ErrExtractFailed
	}
	return bytes.Clone(stdout.Bytes()), nil
}

type limitedBuffer struct {
	buf      bytes.Buffer
	limit    int
	exceeded bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	remaining := b.limit - b.buf.Len()
	if len(p) > remaining {
		if remaining > 0 {
			_, _ = b.buf.Write(p[:remaining])
		}
		b.exceeded = true
		return remaining, errors.New("output limit exceeded")
	}
	return b.buf.Write(p)
}

func (b *limitedBuffer) Bytes() []byte { return b.buf.Bytes() }

// MetadataResolver uses configured yt-dlp and JavaScript runtime paths and
// validates metadata before returning queue descriptors.
type MetadataResolver struct {
	YTDLP   string
	Node    string
	runner  sandboxedRunner
	Timeout time.Duration
}

// NonProductionMetadataResolver remains an alias for callers of the original
// qualification-only adapter name.
type NonProductionMetadataResolver = MetadataResolver

type metadataProfile uint8

const (
	queueMetadata metadataProfile = iota
	searchMetadata
)

// Resolve returns complete, duration-bounded descriptors suitable for enqueue.
func (r MetadataResolver) Resolve(ctx context.Context, input string) ([]music.Track, error) {
	return r.resolve(ctx, input, queueMetadata)
}

// Search returns bounded suggestions. Flat search entries may omit duration;
// enqueue must resolve the selected canonical video again through Resolve.
func (r MetadataResolver) Search(ctx context.Context, input string) ([]music.Track, error) {
	return r.resolve(ctx, input, searchMetadata)
}

func (r MetadataResolver) resolve(ctx context.Context, input string, profile metadataProfile) ([]music.Track, error) {
	if ctx == nil {
		return nil, ErrExtractFailed
	}
	if r.runner == nil {
		return nil, ErrRunnerRequired
	}
	if r.YTDLP == "" || r.Node == "" {
		return nil, ErrExtractFailed
	}
	kindName, canonical, err := NormalizeInput(input)
	if err != nil {
		return nil, err
	}
	kind := kindSearch
	switch kindName {
	case "video":
		kind = kindVideo
	case "playlist":
		kind = kindPlaylist
	}
	args := []string{
		"--ignore-config", "--no-plugin-dirs", "--no-remote-components", "--flat-playlist",
		"--js-runtimes", "node:" + r.Node, "--no-warnings", "--no-progress", "--color", "never",
		"--socket-timeout", "10", "--extractor-retries", "1", "--playlist-end", "501",
		"--dump-single-json", canonical,
	}
	deadline := r.Timeout
	if deadline <= 0 || deadline > 2*time.Minute {
		deadline = 2 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	ctx = context.WithValue(ctx, diagnosticInputKey{}, input)
	output, err := r.runner.Run(ctx, r.YTDLP, args, MaxMetadataBytes)
	if err != nil {
		return nil, err
	}
	if len(output) > MaxMetadataBytes {
		return nil, ErrMetadataTooLarge
	}
	var data metadata
	if err := json.Unmarshal(output, &data); err != nil {
		return nil, invalidMetadata("json")
	}
	return parseTracks(data, kind, profile)
}

type metadata struct {
	Type          *string         `json:"_type"`
	Extractor     string          `json:"extractor_key"`
	ID            string          `json:"id"`
	Title         string          `json:"title"`
	Uploader      string          `json:"uploader"`
	Duration      json.RawMessage `json:"duration"`
	Availability  string          `json:"availability"`
	LiveStatus    string          `json:"live_status"`
	IsLive        bool            `json:"is_live"`
	PlaylistCount *int            `json:"playlist_count"`
	Entries       []metadataEntry `json:"entries"`
}

type metadataEntry struct {
	Type         *string         `json:"_type"`
	Extractor    string          `json:"ie_key"`
	Extractor2   string          `json:"extractor_key"`
	ID           string          `json:"id"`
	Title        string          `json:"title"`
	Uploader     string          `json:"uploader"`
	Duration     json.RawMessage `json:"duration"`
	Availability string          `json:"availability"`
	LiveStatus   string          `json:"live_status"`
	IsLive       bool            `json:"is_live"`
}

func parseTracks(data metadata, kind inputKind, profile metadataProfile) ([]music.Track, error) {
	if data.Type != nil && *data.Type == "" {
		return nil, invalidMetadata("type")
	}
	if kind == kindVideo {
		if data.Extractor != "Youtube" {
			return nil, invalidMetadata("extractor")
		}
		if (data.Type != nil && *data.Type != "video") || len(data.Entries) != 0 {
			return nil, invalidMetadata("type")
		}
		if liveMetadata(data.LiveStatus, data.IsLive) {
			return nil, invalidMetadata("live")
		}
		track, err := makeTrack(data.ID, data.Title, data.Uploader, data.Duration, data.Availability, data.Extractor, false)
		if err != nil {
			return nil, err
		}
		return []music.Track{track}, nil
	}
	rootIsYouTube := data.Extractor == "Youtube" || kind == kindPlaylist && data.Extractor == "YoutubeTab" || kind == kindSearch && data.Extractor == "YoutubeSearch"
	if !rootIsYouTube {
		return nil, invalidMetadata("extractor")
	}
	if data.Type == nil || *data.Type != "playlist" {
		return nil, invalidMetadata("type")
	}
	minimumCount := 1
	if kind == kindSearch && profile == searchMetadata {
		minimumCount = 0
	}
	if data.PlaylistCount == nil || *data.PlaylistCount < minimumCount || *data.PlaylistCount > MaxPlaylistItems || len(data.Entries) != *data.PlaylistCount || (kind == kindSearch && len(data.Entries) > MaxSearchResults) {
		return nil, invalidMetadata("playlist_count")
	}
	tracks := make([]music.Track, 0, len(data.Entries))
	for _, entry := range data.Entries {
		extractor := entry.Extractor
		if entry.Type == nil || *entry.Type == "video" {
			extractor = entry.Extractor2
		} else if *entry.Type != "url" {
			return nil, invalidMetadata("type")
		}
		if liveMetadata(entry.LiveStatus, entry.IsLive) {
			return nil, invalidMetadata("live")
		}
		track, err := makeTrack(entry.ID, entry.Title, entry.Uploader, entry.Duration, entry.Availability, extractor, kind == kindSearch && profile == searchMetadata)
		if err != nil {
			return nil, err
		}
		tracks = append(tracks, track)
	}
	return tracks, nil
}

func liveMetadata(status string, isLive bool) bool {
	return isLive || status == "is_live" || status == "is_upcoming" || status == "post_live"
}

func makeTrack(id, title, uploader string, rawDuration json.RawMessage, availability, extractor string, allowMissingDuration bool) (music.Track, error) {
	if extractor != "Youtube" {
		return music.Track{}, invalidMetadata("extractor")
	}
	if !idPattern.MatchString(id) {
		return music.Track{}, invalidMetadata("id")
	}
	if title == "" || len(title) > MaxTitleBytes || strings.TrimSpace(title) != title || invalidText(title) {
		return music.Track{}, invalidMetadata("title")
	}
	if len(uploader) > music.MaxMetadataBytes || invalidText(uploader) {
		return music.Track{}, invalidMetadata("uploader")
	}
	if availability != "" && availability != "public" && availability != "unlisted" {
		return music.Track{}, invalidMetadata("availability")
	}
	duration, err := parseDuration(rawDuration, allowMissingDuration)
	if err != nil {
		return music.Track{}, err
	}
	return music.Track{
		ID: "yt:" + id, Provider: music.ProviderYouTube,
		CanonicalID: id, Title: title, Artist: uploader, DurationMS: duration,
	}, nil
}

func invalidText(value string) bool {
	for _, r := range value {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}

func parseDuration(raw json.RawMessage, allowMissing bool) (int64, error) {
	if len(raw) == 0 || string(raw) == "null" {
		if allowMissing {
			return 0, nil
		}
		return 0, invalidMetadata("missing_duration")
	}
	var seconds float64
	if err := json.Unmarshal(raw, &seconds); err != nil || math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds <= 0 {
		return 0, invalidMetadata("invalid_duration")
	}
	if seconds > MaxDuration.Seconds() {
		return 0, invalidMetadata("duration_limit")
	}
	duration := int64(math.Round(seconds * 1000))
	if duration <= 0 {
		return 0, invalidMetadata("invalid_duration")
	}
	return duration, nil
}

func displayHostSearch(value string) bool {
	for _, host := range []string{"youtube.com", "www.youtube.com", "m.youtube.com", "music.youtube.com", "youtu.be", "open.spotify.com", "www.spotify.com"} {
		if strings.HasPrefix(value, host+" ") {
			return true
		}
	}
	return false
}
