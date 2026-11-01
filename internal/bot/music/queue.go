// Package music contains deterministic, provider-independent music queue state.
package music

import (
	"errors"
	"math"
	"unicode/utf8"
)

const (
	// QueueLimit is the maximum number of upcoming tracks in one channel.
	QueueLimit = 500
	// MaxMetadataBytes bounds each human-readable metadata field.
	MaxMetadataBytes    = 512
	MaxIDBytes          = 128
	MaxCanonicalIDBytes = 512
	MaxTrackDurationMS  = int64(24 * 60 * 60 * 1000)
)

var (
	ErrRevisionConflict = errors.New("music queue revision conflict")
	ErrQueueFull        = errors.New("music queue limit exceeded")
	ErrInvalidTrack     = errors.New("invalid music track")
	ErrInvalidOperation = errors.New("invalid music queue operation")
)

type Provider string

const (
	ProviderYouTube Provider = "youtube"
	ProviderSpotify Provider = "spotify"
)

// Track is the stable, bounded descriptor stored in the encrypted queue.
// DurationMS is zero when the provider has not supplied a duration.
type Track struct {
	ID          string   `json:"id"`
	Provider    Provider `json:"provider"`
	CanonicalID string   `json:"canonical_id"`
	Title       string   `json:"title"`
	Artist      string   `json:"artist"`
	Album       string   `json:"album"`
	DurationMS  int64    `json:"duration_ms"`
	RequesterID string   `json:"requester_id"`
}

type RepeatMode uint8

const (
	RepeatOff RepeatMode = iota
	RepeatCurrent
	RepeatQueue
)

// State is scoped to one voice channel. Current is nil when nothing is playing.
// Paused starts true so restoring this value can never start audio by itself.
type State struct {
	Revision   uint64  `json:"revision"`
	Current    *Track  `json:"current,omitempty"`
	Queue      []Track `json:"queue"`
	PositionMS int64   `json:"position_ms"`
	// PositionAtMS is the Bot's Unix time in milliseconds at which PositionMS was the playhead (Ruling AI). While a
	// track plays, the playhead is PositionMS + (now - PositionAtMS); 0 means unknown (a state from an older runtime).
	PositionAtMS int64      `json:"position_at_ms,omitempty"`
	Paused       bool       `json:"paused"`
	Volume       uint8      `json:"volume"`
	Shuffle      bool       `json:"shuffle"`
	Repeat       RepeatMode `json:"repeat"`
}

func InitialState() State {
	return State{Queue: []Track{}, Paused: true, Volume: 100, Repeat: RepeatOff}
}

type OperationKind uint8

const (
	OpEnqueue OperationKind = iota + 1
	OpImportPlaylist
	OpRemove
	OpReorder
	OpSkip
	OpPause
	OpResume
	OpStop
	OpClear
	OpSeek
	OpSetVolume
	OpSetShuffle
	OpSetRepeat
	OpComplete
)

// Operation contains only fields used by its Kind. Seed is explicit so shuffle
// order is reproducible and can be persisted with the resulting queue state.
type Operation struct {
	Kind       OperationKind
	Tracks     []Track
	ItemID     string
	BeforeID   string // empty moves the item to the end
	PositionMS int64
	Volume     uint8
	Enabled    bool
	Seed       uint64
	Repeat     RepeatMode
	// ExpectedCurrentItemID makes an immediate skip conditional on the item the
	// caller saw. Nil preserves legacy unconditional skips; empty means no item.
	ExpectedCurrentItemID *string
	// NowMS is the Bot's Unix time in milliseconds when the operation applies. With it, Apply first settles the
	// playhead a playing track has reached, then stamps PositionAtMS; 0 leaves the playhead fields as they are.
	NowMS int64
}

// Apply applies one revision-checked mutation. It never mutates state or the
// operation's track slice, and returns the original state on every error.
func Apply(state State, expectedRevision uint64, op Operation) (State, error) {
	if expectedRevision != state.Revision {
		return state, ErrRevisionConflict
	}
	if state.Revision == math.MaxUint64 || !validState(state) {
		return state, ErrInvalidOperation
	}
	next := cloneState(state)
	if op.NowMS < 0 {
		return state, ErrInvalidOperation
	}
	if op.NowMS > 0 {
		settlePosition(&next, op.NowMS)
	}
	var err error
	switch op.Kind {
	case OpEnqueue, OpImportPlaylist:
		err = appendTracks(&next, op.Tracks, op.Seed)
	case OpRemove:
		err = removeTrack(&next, op.ItemID)
	case OpReorder:
		err = reorderTrack(&next, op.ItemID, op.BeforeID)
	case OpSkip:
		skip(&next)
	case OpPause:
		next.Paused = true
	case OpResume:
		if next.Current == nil {
			startNext(&next)
		}
		if next.Current != nil {
			// Fix round 2 (Ruling AI, the truth): a provider starts a track from its beginning; it has no offset.
			if next.Paused {
				next.PositionMS = 0
			}
			next.Paused = false
		}
	case OpStop:
		next.Current = nil
		next.PositionMS = 0
		next.Paused = true
	case OpClear:
		next.Current = nil
		next.Queue = []Track{}
		next.PositionMS = 0
		next.Paused = true
	case OpSeek:
		if next.Current == nil || op.PositionMS < 0 || (next.Current.DurationMS > 0 && op.PositionMS > next.Current.DurationMS) {
			err = ErrInvalidOperation
		}
		// Fix round 2: no provider applies a seek to the audio yet, so the playhead does not move; the request is
		// validated and acknowledged (a provider that seeks will move PositionMS here).
	case OpSetVolume:
		if op.Volume > 100 {
			err = ErrInvalidOperation
		} else {
			next.Volume = op.Volume
		}
	case OpSetShuffle:
		next.Shuffle = op.Enabled
		if op.Enabled {
			shuffle(next.Queue, op.Seed)
		}
	case OpSetRepeat:
		if op.Repeat > RepeatQueue {
			err = ErrInvalidOperation
		} else {
			next.Repeat = op.Repeat
		}
	case OpComplete:
		err = complete(&next)
	default:
		err = ErrInvalidOperation
	}
	if err != nil {
		return state, err
	}
	if op.NowMS > 0 {
		next.PositionAtMS = op.NowMS
	}
	next.Revision++
	return next, nil
}

// settlePosition folds the time a playing track has played since PositionAtMS into PositionMS (clamped to its
// duration), so a pause, seek or skip starts from the playhead it actually reached.
func settlePosition(state *State, nowMS int64) {
	if state.Current == nil || state.Paused || state.PositionAtMS <= 0 || nowMS <= state.PositionAtMS {
		return
	}
	position := state.PositionMS + (nowMS - state.PositionAtMS)
	if state.Current.DurationMS > 0 && position > state.Current.DurationMS {
		position = state.Current.DurationMS
	}
	state.PositionMS = position
	state.PositionAtMS = nowMS
}

// PlayheadMS is the position a client should show at nowMS: the stamped position plus the time played since, clamped.
func PlayheadMS(state State, nowMS int64) int64 {
	settled := cloneState(state)
	settlePosition(&settled, nowMS)
	return settled.PositionMS
}

func appendTracks(state *State, tracks []Track, seed uint64) error {
	if len(tracks) == 0 {
		return ErrInvalidOperation
	}
	queued := len(state.Queue)
	if state.Current != nil {
		queued++
	}
	if queued+len(tracks) > QueueLimit {
		return ErrQueueFull
	}
	seen := make(map[string]struct{}, len(state.Queue)+len(tracks)+1)
	if state.Current != nil {
		seen[state.Current.ID] = struct{}{}
	}
	for _, queued := range state.Queue {
		seen[queued.ID] = struct{}{}
	}
	for _, track := range tracks {
		if !validTrack(track) {
			return ErrInvalidTrack
		}
		if _, ok := seen[track.ID]; ok {
			return ErrInvalidTrack
		}
		seen[track.ID] = struct{}{}
	}
	added := append([]Track(nil), tracks...)
	if state.Shuffle {
		shuffle(added, seed)
	}
	state.Queue = append(state.Queue, added...)
	return nil
}

func removeTrack(state *State, id string) error {
	for i := range state.Queue {
		if state.Queue[i].ID == id {
			state.Queue = append(state.Queue[:i], state.Queue[i+1:]...)
			return nil
		}
	}
	return ErrInvalidOperation
}

func reorderTrack(state *State, id, beforeID string) error {
	from := indexTrack(state.Queue, id)
	if from < 0 {
		return ErrInvalidOperation
	}
	to := len(state.Queue)
	if beforeID != "" {
		to = indexTrack(state.Queue, beforeID)
		if to < 0 {
			return ErrInvalidOperation
		}
	}
	item := state.Queue[from]
	state.Queue = append(state.Queue[:from], state.Queue[from+1:]...)
	if from < to {
		to--
	}
	state.Queue = append(state.Queue, Track{})
	copy(state.Queue[to+1:], state.Queue[to:])
	state.Queue[to] = item
	return nil
}

func skip(state *State) {
	wasPaused := state.Paused
	hadCurrent := state.Current != nil
	if state.Current != nil && state.Repeat == RepeatQueue {
		state.Queue = append(state.Queue, *state.Current)
	}
	state.Current = nil
	state.PositionMS = 0
	if hadCurrent {
		startNext(state)
	}
	if wasPaused {
		state.Paused = true
	}
}

func complete(state *State) error {
	if state.Current == nil || state.Paused {
		return ErrInvalidOperation
	}
	if state.Repeat == RepeatCurrent {
		state.PositionMS = 0
		return nil
	}
	if state.Repeat == RepeatQueue {
		state.Queue = append(state.Queue, *state.Current)
	}
	state.Current = nil
	state.PositionMS = 0
	startNext(state)
	return nil
}

func startNext(state *State) {
	if state.Current != nil || len(state.Queue) == 0 {
		if state.Current == nil {
			state.Paused = true
		}
		return
	}
	track := state.Queue[0]
	state.Queue = state.Queue[1:]
	state.Current = &track
	state.PositionMS = 0
	state.Paused = false
}

func shuffle(tracks []Track, seed uint64) {
	// SplitMix64 gives deterministic per-operation output without shared random state.
	next := func() uint64 {
		seed += 0x9e3779b97f4a7c15
		z := seed
		z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
		z = (z ^ (z >> 27)) * 0x94d049bb133111eb
		return z ^ (z >> 31)
	}
	for i := len(tracks) - 1; i > 0; i-- {
		j := int(next() % uint64(i+1)) // #nosec G115 -- modulo bounds the result below i+1, already an int.
		tracks[i], tracks[j] = tracks[j], tracks[i]
	}
}

func validTrack(track Track) bool {
	if !boundedString(track.ID, MaxIDBytes, false) || !validCanonicalID(track.Provider, track.CanonicalID) || !boundedString(track.RequesterID, MaxIDBytes, false) {
		return false
	}
	if !boundedString(track.Title, MaxMetadataBytes, true) || !boundedString(track.Artist, MaxMetadataBytes, true) || !boundedString(track.Album, MaxMetadataBytes, true) {
		return false
	}
	return track.DurationMS >= 0 && track.DurationMS <= MaxTrackDurationMS
}

// CanonicalID is a provider-native track identifier. Playlist expansion must
// store one descriptor per track, never a URL, path, or playlist-level ID.
func validCanonicalID(provider Provider, id string) bool {
	if !boundedString(id, MaxCanonicalIDBytes, false) {
		return false
	}
	switch provider {
	case ProviderYouTube:
		if len(id) != 11 {
			return false
		}
		for i := 0; i < len(id); i++ {
			c := id[i]
			if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '_' && c != '-' {
				return false
			}
		}
		return true
	case ProviderSpotify:
		const prefix = "spotify:track:"
		if len(id) != len(prefix)+22 || id[:len(prefix)] != prefix {
			return false
		}
		for i := len(prefix); i < len(id); i++ {
			c := id[i]
			if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func boundedString(value string, maxBytes int, allowEmpty bool) bool {
	if !utf8.ValidString(value) || len(value) > maxBytes || (!allowEmpty && value == "") {
		return false
	}
	return true
}

func validState(state State) bool {
	trackCount := len(state.Queue)
	if state.Current != nil {
		trackCount++
	}
	if state.Volume > 100 || state.Repeat > RepeatQueue || state.PositionMS < 0 || state.PositionAtMS < 0 || trackCount > QueueLimit {
		return false
	}
	seen := make(map[string]struct{}, len(state.Queue)+1)
	if state.Current != nil {
		if !validTrack(*state.Current) || (state.Current.DurationMS > 0 && state.PositionMS > state.Current.DurationMS) {
			return false
		}
		seen[state.Current.ID] = struct{}{}
	} else if state.PositionMS != 0 || !state.Paused {
		return false
	}
	for _, track := range state.Queue {
		if !validTrack(track) {
			return false
		}
		if _, exists := seen[track.ID]; exists {
			return false
		}
		seen[track.ID] = struct{}{}
	}
	return true
}

func cloneState(state State) State {
	state.Queue = append([]Track(nil), state.Queue...)
	if state.Queue == nil {
		state.Queue = []Track{}
	}
	if state.Current != nil {
		current := *state.Current
		state.Current = &current
	}
	return state
}

func indexTrack(tracks []Track, id string) int {
	for i := range tracks {
		if tracks[i].ID == id {
			return i
		}
	}
	return -1
}
