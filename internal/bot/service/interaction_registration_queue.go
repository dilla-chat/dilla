package service

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/native"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
)

const (
	interactionRegistrationQueueKey       = "dilla.bot.interaction-registration.queue.v1"
	maxInteractionRegistrationQueue       = 256
	maxInteractionRegistrationJobsPerPass = 4
	maxInteractionRegistrationDevicesPass = 8

	interactionRegistrationBackoffBase = 30 * time.Second
	interactionRegistrationBackoffMax  = time.Hour
	// The persisted wire version that carries per-device backoff. Version 1
	// rows have no backoff and still decode.
	interactionRegistrationQueueVersion = 2
)

var ErrInteractionRegistrationQueueFull = errors.New("bot interaction registration queue is full")

type interactionRegistrationEpisodeResolver interface {
	ResolveCurrentEpisode(context.Context, id.ID) (id.ID, bool, error)
}

type interactionRegistrationCandidateProvider interface {
	DeviceCandidates(context.Context, id.ID) ([]id.ID, error)
}

type interactionRegistrationJob struct {
	User, Episode id.ID
	Processed     []id.ID
	Backoff       []interactionRegistrationBackoff
}

// interactionRegistrationBackoff holds a device that has no KeyPackage to take
// out of the passes until NotBefore. It lives in the job so a restart neither
// forgets it nor repeats the entry warning.
type interactionRegistrationBackoff struct {
	Device    id.ID
	Failures  uint64
	NotBefore time.Time
}

func interactionRegistrationBackoffDelay(failures uint64) time.Duration {
	delay := interactionRegistrationBackoffBase
	for ; failures > 1 && delay < interactionRegistrationBackoffMax; failures-- {
		delay *= 2
	}
	return min(delay, interactionRegistrationBackoffMax)
}

func (j *interactionRegistrationJob) backoffIndex(device id.ID) int {
	for i, b := range j.Backoff {
		if b.Device == device {
			return i
		}
	}
	return -1
}

func (j *interactionRegistrationJob) clearBackoff(device id.ID) {
	if i := j.backoffIndex(device); i >= 0 {
		j.Backoff = append(j.Backoff[:i:i], j.Backoff[i+1:]...)
	}
}

// QueueMemberJoin durably records a member-episode trigger. It performs no
// network work, allowing the structural event cursor to advance independently
// of device enumeration and KeyPackage availability.
func (c *InteractionRegistrationCoordinator) QueueMemberJoin(ctx context.Context, user, episode id.ID) error {
	if c == nil || user.IsZero() || episode.IsZero() {
		return errors.New("invalid interaction registration member episode")
	}
	c.jobsMu.Lock()
	defer c.jobsMu.Unlock()
	jobs, revision, err := c.loadRegistrationJobs(ctx)
	if err != nil {
		return err
	}
	for _, job := range jobs {
		if job.User == user && job.Episode == episode {
			return nil
		}
	}
	if len(jobs) >= maxInteractionRegistrationQueue {
		return ErrInteractionRegistrationQueueFull
	}
	jobs = append(jobs, interactionRegistrationJob{User: user, Episode: episode})
	return c.saveRegistrationJobs(ctx, revision, jobs)
}

// ProcessPending advances a bounded number of durable member-episode jobs and
// per-device registrations. Current episode is rechecked immediately before
// each device registration. A transient device failure stays queued while
// completed or ineligible devices are remembered for idempotent retry.
func (c *InteractionRegistrationCoordinator) ProcessPending(ctx context.Context, jobBudget, deviceBudget uint64, episodes interactionRegistrationEpisodeResolver, candidates interactionRegistrationCandidateProvider) error {
	if c == nil || episodes == nil || candidates == nil {
		return errors.New("invalid interaction registration worker")
	}
	if jobBudget == 0 {
		jobBudget = maxInteractionRegistrationJobsPerPass
	}
	if jobBudget > maxInteractionRegistrationJobsPerPass {
		jobBudget = maxInteractionRegistrationJobsPerPass
	}
	if deviceBudget == 0 {
		deviceBudget = maxInteractionRegistrationDevicesPass
	}
	if deviceBudget > maxInteractionRegistrationDevicesPass {
		deviceBudget = maxInteractionRegistrationDevicesPass
	}
	c.jobsMu.Lock()
	defer c.jobsMu.Unlock()
	now := c.now()
	jobs, revision, err := c.loadRegistrationJobs(ctx)
	if err != nil || len(jobs) == 0 {
		return err
	}
	var failures []error
	// Jobs that made no progress this pass go behind everything else, so stuck
	// members cannot hold the head of the queue and starve later ones.
	var rotated []interactionRegistrationJob
	jobsVisited, devicesVisited := uint64(0), uint64(0)
	// keep leaves the job pending; one that processed no new device rotates.
	keep := func(i int, job interactionRegistrationJob, progressed bool) int {
		if progressed {
			jobs[i] = job
			return i + 1
		}
		rotated = append(rotated, job)
		jobs = removeRegistrationJob(jobs, i)
		return i
	}
	for i := 0; i < len(jobs) && jobsVisited < jobBudget && devicesVisited < deviceBudget; {
		jobsVisited++
		job := jobs[i]
		current, present, err := episodes.ResolveCurrentEpisode(ctx, job.User)
		if err != nil {
			failures = append(failures, fmt.Errorf("resolve member episode: %w", err))
			i = keep(i, job, false)
			continue
		}
		if !present || current != job.Episode {
			jobs = removeRegistrationJob(jobs, i)
			continue
		}
		devices, err := candidates.DeviceCandidates(ctx, job.User)
		if err != nil {
			failures = append(failures, fmt.Errorf("read signed device candidates: %w", err))
			i = keep(i, job, false)
			continue
		}
		if len(devices) == 0 {
			// A signed empty list can become populated later without a new
			// membership episode. Keep the durable job for a future pass.
			i = keep(i, job, false)
			continue
		}
		processed := make(map[id.ID]struct{}, len(job.Processed))
		for _, device := range job.Processed {
			processed[device] = struct{}{}
		}
		stale := false
		processedBefore := len(job.Processed)
		for _, device := range devices {
			if device.IsZero() {
				failures = append(failures, errors.New("device candidate has zero identifier"))
				continue
			}
			if _, done := processed[device]; done {
				continue
			}
			if b := job.backoffIndex(device); b >= 0 && now.Before(job.Backoff[b].NotBefore) {
				continue
			}
			if devicesVisited >= deviceBudget {
				break
			}
			current, present, err = episodes.ResolveCurrentEpisode(ctx, job.User)
			if err != nil {
				failures = append(failures, fmt.Errorf("recheck member episode: %w", err))
				break
			}
			if !present || current != job.Episode {
				stale = true
				break
			}
			devicesVisited++
			group := interactionRegistrationGroupID(c.installation, c.community, job.Episode, job.User, device)
			_, err = c.Register(ctx, InteractionRegistrationRequest{Group: group, ParticipantUser: job.User, ParticipantDevice: device})
			if errors.Is(err, ErrInteractionRegistrationManualRecovery) {
				failures = append(failures, fmt.Errorf("manual recovery required for device registration: %w", err))
				processed[device] = struct{}{}
				job.Processed = append(job.Processed, device)
				continue
			}
			if code := keyPackageUnavailableCode(err); code != "" {
				c.backOffDevice(&job, device, code, now)
				continue
			}
			job.clearBackoff(device)
			if err != nil {
				// Eligibility can change while the member episode remains current.
				// Keep rejected candidates pending so a later bounded pass can
				// recheck them; Register derives a stable group ID and resumes its
				// durable intent, so this does not create duplicate groups.
				failures = append(failures, fmt.Errorf("register eligible device: %w", err))
				continue
			}
			processed[device] = struct{}{}
			job.Processed = append(job.Processed, device)
		}
		job.pruneBackoff(devices)
		if stale {
			jobs = removeRegistrationJob(jobs, i)
			continue
		}
		allDone := true
		for _, device := range devices {
			if _, done := processed[device]; !done {
				allDone = false
				break
			}
		}
		if allDone {
			jobs = removeRegistrationJob(jobs, i)
			continue
		}
		i = keep(i, job, len(job.Processed) > processedBefore)
	}
	jobs = append(jobs, rotated...)
	if err := c.saveRegistrationJobs(ctx, revision, jobs); err != nil {
		return errors.Join(err, errors.Join(failures...))
	}
	return errors.Join(failures...)
}

// backOffDevice records one more KeyPackage-less attempt. The warning fires
// only when the device first enters backoff; later doublings are silent.
func (c *InteractionRegistrationCoordinator) backOffDevice(job *interactionRegistrationJob, device id.ID, code string, now time.Time) {
	i := job.backoffIndex(device)
	if i < 0 {
		job.Backoff = append(job.Backoff, interactionRegistrationBackoff{Device: device})
		i = len(job.Backoff) - 1
		c.logger.Warn("bot interaction registration backing off a device with no key packages", "installation_id", c.installation.String(), "device_id", device.String(), "reason", code)
	}
	b := &job.Backoff[i]
	b.Failures++
	b.NotBefore = now.Add(interactionRegistrationBackoffDelay(b.Failures))
}

// pruneBackoff drops entries for devices that are done or no longer candidates.
func (j *interactionRegistrationJob) pruneBackoff(devices []id.ID) {
	kept := j.Backoff[:0:0]
	for _, b := range j.Backoff {
		for _, d := range devices {
			if d == b.Device {
				kept = append(kept, b)
				break
			}
		}
	}
	for _, done := range j.Processed {
		kept = slicesDeleteDevice(kept, done)
	}
	j.Backoff = kept
}

func slicesDeleteDevice(rows []interactionRegistrationBackoff, device id.ID) []interactionRegistrationBackoff {
	out := rows[:0]
	for _, b := range rows {
		if b.Device != device {
			out = append(out, b)
		}
	}
	return out
}

func interactionRegistrationGroupID(installation, community, episode, user, device id.ID) id.ID {
	hash := sha256.New()
	_, _ = hash.Write([]byte("dilla bot interaction group v1\x00"))
	_, _ = hash.Write(installation[:])
	_, _ = hash.Write(community[:])
	_, _ = hash.Write(episode[:])
	_, _ = hash.Write(user[:])
	_, _ = hash.Write(device[:])
	digest := hash.Sum(nil)
	var group id.ID
	copy(group[:], digest[:len(group)])
	group[6] = (group[6] & 0x0f) | 0x50
	group[8] = (group[8] & 0x3f) | 0x80
	return group
}

func (c *InteractionRegistrationCoordinator) loadRegistrationJobs(ctx context.Context) ([]interactionRegistrationJob, uint64, error) {
	value, err := c.native.StateGet(ctx, interactionRegistrationStateArea, []byte(interactionRegistrationQueueKey))
	if err != nil {
		return nil, 0, err
	}
	if value.Tombstone {
		return nil, value.Revision, ErrInteractionRegistrationRecovery
	}
	if value.Revision == 0 {
		return nil, 0, nil
	}
	jobs, err := decodeInteractionRegistrationJobs(value.Value)
	return jobs, value.Revision, err
}

func (c *InteractionRegistrationCoordinator) saveRegistrationJobs(ctx context.Context, revision uint64, jobs []interactionRegistrationJob) error {
	value, err := encodeInteractionRegistrationJobs(jobs)
	if err != nil {
		return err
	}
	revisions, err := c.native.StateTransaction(ctx, id.New(), []native.StateChange{{Area: interactionRegistrationStateArea, Key: []byte(interactionRegistrationQueueKey), ExpectedRevision: revision, Value: value}})
	if err != nil {
		return err
	}
	if len(revisions) != 1 || revisions[0] != revision+1 {
		return ErrInteractionRegistrationRecovery
	}
	return nil
}

func encodeInteractionRegistrationJobs(jobs []interactionRegistrationJob) ([]byte, error) {
	if len(jobs) > maxInteractionRegistrationQueue {
		return nil, ErrInteractionRegistrationQueueFull
	}
	rows := make([]any, len(jobs))
	for i, job := range jobs {
		if job.User.IsZero() || job.Episode.IsZero() || len(job.Processed) > 256 {
			return nil, ErrInteractionRegistrationRecovery
		}
		processed := make([]any, len(job.Processed))
		for j, device := range job.Processed {
			if device.IsZero() {
				return nil, ErrInteractionRegistrationRecovery
			}
			processed[j] = device
		}
		if len(job.Backoff) > 256 {
			return nil, ErrInteractionRegistrationRecovery
		}
		backoff := make([]any, len(job.Backoff))
		for j, b := range job.Backoff {
			if b.Device.IsZero() || b.Failures == 0 || b.NotBefore.UnixMilli() < 0 {
				return nil, ErrInteractionRegistrationRecovery
			}
			backoff[j] = []any{b.Device, b.Failures, uint64(b.NotBefore.UnixMilli())}
		}
		rows[i] = []any{job.User, job.Episode, processed, backoff}
	}
	return cborx.Marshal([]any{uint64(interactionRegistrationQueueVersion), rows})
}

func decodeInteractionRegistrationJobs(raw []byte) ([]interactionRegistrationJob, error) {
	if cborx.CheckDeterministic(raw) != nil {
		return nil, ErrInteractionRegistrationRecovery
	}
	var value []any
	if cborx.Unmarshal(raw, &value) != nil || len(value) != 2 {
		return nil, ErrInteractionRegistrationRecovery
	}
	version, ok := value[0].(uint64)
	rows, rok := value[1].([]any)
	if !ok || (version != 1 && version != interactionRegistrationQueueVersion) || !rok || len(rows) > maxInteractionRegistrationQueue {
		return nil, ErrInteractionRegistrationRecovery
	}
	jobs := make([]interactionRegistrationJob, len(rows))
	for i, rawRow := range rows {
		row, ok := rawRow.([]any)
		if !ok || (version == 1 && len(row) != 3) || (version == interactionRegistrationQueueVersion && len(row) != 4) {
			return nil, ErrInteractionRegistrationRecovery
		}
		user, uok := registrationID(row[0])
		episode, eok := registrationID(row[1])
		processed, pok := row[2].([]any)
		if !uok || user.IsZero() || !eok || episode.IsZero() || !pok || len(processed) > 256 {
			return nil, ErrInteractionRegistrationRecovery
		}
		jobs[i] = interactionRegistrationJob{User: user, Episode: episode, Processed: make([]id.ID, len(processed))}
		seen := make(map[id.ID]struct{}, len(processed))
		for j, rawDevice := range processed {
			device, ok := registrationID(rawDevice)
			if !ok || device.IsZero() {
				return nil, ErrInteractionRegistrationRecovery
			}
			if _, duplicate := seen[device]; duplicate {
				return nil, ErrInteractionRegistrationRecovery
			}
			seen[device] = struct{}{}
			jobs[i].Processed[j] = device
		}
		if version == 1 {
			continue
		}
		backoff, ok := row[3].([]any)
		if !ok || len(backoff) > 256 {
			return nil, ErrInteractionRegistrationRecovery
		}
		for _, rawBackoff := range backoff {
			entry, ok := rawBackoff.([]any)
			if !ok || len(entry) != 3 {
				return nil, ErrInteractionRegistrationRecovery
			}
			device, dok := registrationID(entry[0])
			failures, fok := entry[1].(uint64)
			notBefore, nok := entry[2].(uint64)
			if !dok || device.IsZero() || !fok || failures == 0 || !nok || notBefore > math.MaxInt64 || jobs[i].backoffIndex(device) >= 0 {
				return nil, ErrInteractionRegistrationRecovery
			}
			jobs[i].Backoff = append(jobs[i].Backoff, interactionRegistrationBackoff{Device: device, Failures: failures, NotBefore: time.UnixMilli(int64(notBefore))})
		}
	}
	return jobs, nil
}

func removeRegistrationJob(jobs []interactionRegistrationJob, i int) []interactionRegistrationJob {
	copy(jobs[i:], jobs[i+1:])
	return jobs[:len(jobs)-1]
}
