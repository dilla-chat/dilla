package service

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"

	"github.com/dilla-chat/dilla/internal/bot/native"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
)

const (
	interactionRegistrationQueueKey       = "dilla.bot.interaction-registration.queue.v1"
	maxInteractionRegistrationQueue       = 256
	maxInteractionRegistrationJobsPerPass = 4
	maxInteractionRegistrationDevicesPass = 8
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

// HasPendingWork reports whether a durable member-episode job is still queued: a member whose devices are not all
// registered yet (one may be waiting for KeyPackages, or for a device list). The Bot polls nothing (Bot v2 task 14),
// so its loop runs the pass again, on a backoff, while this holds.
func (c *InteractionRegistrationCoordinator) HasPendingWork(ctx context.Context) (bool, error) {
	n, err := c.PendingJobs(ctx)
	return n > 0, err
}

// PendingJobs is how many durable member-episode jobs are queued.
func (c *InteractionRegistrationCoordinator) PendingJobs(ctx context.Context) (int, error) {
	if c == nil {
		return 0, errors.New("invalid interaction registration worker")
	}
	c.jobsMu.Lock()
	defer c.jobsMu.Unlock()
	jobs, _, err := c.loadRegistrationJobs(ctx)
	return len(jobs), err
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
			c.backoff.dropJob(c.installation, job.User, job.Episode)
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
			slot := interactionRegistrationBackoffKey{installation: c.installation, user: job.User, episode: job.Episode, device: device}
			if !c.backoff.due(slot, now) {
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
				c.backoff.clear(slot)
				processed[device] = struct{}{}
				job.Processed = append(job.Processed, device)
				continue
			}
			if code := keyPackageUnavailableCode(err); code != "" {
				if c.backoff.fail(slot, now) {
					c.logger.Warn("bot interaction registration backing off a device with no key packages", "installation_id", c.installation.String(), "device_id", device.String(), "reason", code)
				}
				continue
			}
			if errors.Is(err, errInteractionCreateRefused) {
				// Register already started the intent over, which costs a KeyPackage
				// each time. Back the device off so a permanently refused one takes
				// one package per step, and warn once per step instead of routing it
				// into the work-pending report that repeats only every 30 minutes.
				c.backoff.fail(slot, now)
				c.logger.Warn("bot interaction registration backing off a device whose create was refused", "installation_id", c.installation.String(), "device_id", device.String(), "error", err.Error())
				continue
			}
			if err != nil {
				// Eligibility can change while the member episode remains current.
				// Keep rejected candidates pending so a later bounded pass can
				// recheck them; Register derives a stable group ID and resumes its
				// durable intent, so this does not create duplicate groups.
				failures = append(failures, fmt.Errorf("register eligible device: %w", err))
				continue
			}
			c.backoff.clear(slot)
			processed[device] = struct{}{}
			job.Processed = append(job.Processed, device)
		}
		if stale {
			c.backoff.dropJob(c.installation, job.User, job.Episode)
			jobs = removeRegistrationJob(jobs, i)
			continue
		}
		c.backoff.retain(c.installation, job.User, job.Episode, devices)
		allDone := true
		for _, device := range devices {
			if _, done := processed[device]; !done {
				allDone = false
				break
			}
		}
		if allDone {
			c.backoff.dropJob(c.installation, job.User, job.Episode)
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
		rows[i] = []any{job.User, job.Episode, processed}
	}
	return cborx.Marshal([]any{uint64(1), rows})
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
	if !ok || version != 1 || !rok || len(rows) > maxInteractionRegistrationQueue {
		return nil, ErrInteractionRegistrationRecovery
	}
	jobs := make([]interactionRegistrationJob, len(rows))
	for i, rawRow := range rows {
		row, ok := rawRow.([]any)
		if !ok || len(row) != 3 {
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
	}
	return jobs, nil
}

func removeRegistrationJob(jobs []interactionRegistrationJob, i int) []interactionRegistrationJob {
	copy(jobs[i:], jobs[i+1:])
	return jobs[:len(jobs)-1]
}
