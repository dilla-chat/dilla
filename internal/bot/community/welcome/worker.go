// Package welcome processes bounded, community-scoped membership welcome episodes.
package welcome

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/dilla-chat/dilla/internal/bot/native"
	"github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
)

const (
	stateArea        = 8
	statePrefix      = "dilla.bot.welcome.v1/"
	maxTemplateBytes = 2000
	maxRenderedBytes = 4000
	maxCatchupAge    = 24 * time.Hour
)

var (
	ErrConflict             = errors.New("welcome episode conflicts with persisted event")
	ErrRoleGrantUnavailable = errors.New("welcome role grants are unavailable")
	ErrReceiptUnresolved    = errors.New("welcome send outcome must be resolved before expiry")
	// ErrRetryLater is returned (errors.Is) when the welcome could not be sent yet and the episode stays pending: the
	// Bot has not joined the destination group, the group has pending proposals or a pending commit, or the server
	// closed the attempt without accepting it (a successor attempt is stored). The caller runs the join again later.
	ErrRetryLater = errors.New("welcome deferred: retry later")
	// ErrGaveUp is returned once, when the maxAttempts-th attempt closed: the episode is stored as StatusFailed.
	ErrGaveUp = errors.New("welcome given up")
)

// maxAttempts bounds the send attempts of one membership episode. Deferrals that send nothing do not count; the
// 24 h catch-up cutoff bounds those.
const maxAttempts = 8

type Native interface {
	Binding() native.Binding
	StateGet(context.Context, uint64, []byte) (native.StateValue, error)
	StateTransaction(context.Context, id.ID, []native.StateChange) ([]uint64, error)
}

type MemberResolver interface {
	ResolveCurrentMember(context.Context, id.ID) (present bool, name string, err error)
}

// DestinationResolver verifies that the configured channel is still a valid
// text destination and returns its authorized MLS group.
type DestinationResolver interface {
	ResolveTextGroup(context.Context, id.ID, id.ID) (id.ID, error)
}

// Sender must reuse the supplied operation and attempt IDs on retries and return
// the server's decoded receipt. An error leaves the episode pending.
type Sender interface {
	SendWelcome(context.Context, SendRequest) (protocol.SendReceipt, error)
	ResolveWelcome(context.Context, id.ID, id.ID) (protocol.SendReceipt, bool, error)
}

// ResolveWelcome is required for a pending send that crosses the catch-up
// cutoff. It must query the authoritative operation receipt without sending.
// found=false must mean a definitive, current absence; uncertain recovery is an error.

type SendRequest struct {
	OperationID, AttemptID                                    id.ID
	PredecessorAttemptID                                      *id.ID
	CommunityID, InstallationID, MemberID, EpisodeID, GroupID id.ID
	Text                                                      string
	OriginalNow                                               uint64
}

type Options struct {
	ChannelID                         id.ID
	ExpectedGroupID                   id.ID
	Template, ServerName, ChannelName string
	Members                           MemberResolver
	Destination                       DestinationResolver
	Sender                            Sender
	Now                               func() time.Time
}

type Status uint64

const (
	StatusNone Status = iota
	StatusPending
	StatusPosted
	StatusMissed
	StatusLeft
	// StatusFailed is terminal: every one of maxAttempts attempts was closed without acceptance.
	StatusFailed
)

type episode struct {
	Status      Status
	EventID     id.ID
	Timestamp   uint64
	OperationID id.ID
	// Attempt is the attempt the next send uses: the count of attempts the server closed without accepting. Rows stored
	// before attempts existed read as 0.
	Attempt uint32
}

type Processor struct {
	mu                                sync.Mutex
	native                            Native
	binding                           native.Binding
	channel                           id.ID
	expectedGroup                     id.ID
	template, serverName, channelName string
	members                           MemberResolver
	destination                       DestinationResolver
	sender                            Sender
	now                               func() time.Time
}

func New(n Native, options Options) (*Processor, error) {
	if n == nil || options.Members == nil || options.Destination == nil || options.Sender == nil || options.ChannelID.IsZero() || options.ExpectedGroupID.IsZero() {
		return nil, errors.New("missing welcome dependency")
	}
	b := n.Binding()
	if b.Instance.IsZero() || b.Community.IsZero() || b.Installation.IsZero() {
		return nil, errors.New("invalid welcome installation binding")
	}
	if _, err := Preview(options.Template, "member", options.ServerName, options.ChannelName); err != nil {
		return nil, err
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	return &Processor{native: n, binding: b, channel: options.ChannelID, expectedGroup: options.ExpectedGroupID, template: options.Template, serverName: options.ServerName, channelName: options.ChannelName, members: options.Members, destination: options.Destination, sender: options.Sender, now: options.Now}, nil
}

// ProcessLeave records a membership departure without resolving a welcome
// destination. A deleted channel must not hold the structural event cursor.
func ProcessLeave(ctx context.Context, n Native, event protocol.Event) error {
	if n == nil || event.Kind != protocol.EventMemberLeave {
		return errors.New("invalid welcome leave")
	}
	binding := n.Binding()
	if binding.Instance.IsZero() || binding.Community.IsZero() || binding.Installation.IsZero() {
		return errors.New("invalid welcome installation binding")
	}
	return (&Processor{native: n, binding: binding}).Process(ctx, event)
}

// Preview substitutes only the three supported references and bounds both the
// source and rendered UTF-8 text. It returns plain text for encrypted chat.
func Preview(template, member, server, channel string) (string, error) {
	if !utf8.ValidString(template) || len([]byte(template)) == 0 || len([]byte(template)) > maxTemplateBytes {
		return "", errors.New("invalid welcome template size or encoding")
	}
	for _, part := range []string{member, server, channel} {
		if !utf8.ValidString(part) || len([]byte(part)) > 256 {
			return "", errors.New("invalid welcome reference")
		}
	}
	var out strings.Builder
	for i := 0; i < len(template); {
		if template[i] == '}' {
			return "", errors.New("unsupported welcome template reference")
		}
		if template[i] != '{' {
			out.WriteByte(template[i])
			i++
			continue
		}
		end := strings.IndexByte(template[i:], '}')
		if end < 0 {
			return "", errors.New("unsupported welcome template reference")
		}
		token := template[i : i+end+1]
		var value string
		switch token {
		case "{member}":
			value = member
		case "{server}":
			value = server
		case "{channel}":
			value = channel
		default:
			return "", errors.New("unsupported welcome template reference")
		}
		out.WriteString(value)
		i += end + 1
	}
	result := out.String()
	if !utf8.ValidString(result) || len([]byte(result)) > maxRenderedBytes {
		return "", errors.New("rendered welcome exceeds limit")
	}
	return result, nil
}

// Process applies only join and leave events. Other valid structural events are
// intentionally ignored by this feature worker.
func (p *Processor) Process(ctx context.Context, event protocol.Event) error {
	if p == nil {
		return errors.New("missing welcome processor")
	}
	if protocol.ValidateEventContext(event, p.binding.Community) != nil {
		return protocol.ErrContext
	}
	if _, err := protocol.EncodeEvent(event); err != nil {
		return err
	}
	if event.Kind != protocol.EventMemberJoin && event.Kind != protocol.EventMemberLeave {
		return nil
	}
	user, ok := event.Data[0].(id.ID)
	if !ok || user.IsZero() {
		return protocol.ErrInvalid
	}
	episodeID, ok := event.Data[1].(id.ID)
	if !ok || episodeID.IsZero() {
		return protocol.ErrInvalid
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	key := p.stateKey(user, episodeID)
	current, revision, err := p.load(ctx, key)
	if err != nil {
		return err
	}
	if event.Kind == protocol.EventMemberLeave {
		if current.Status == StatusPosted || current.Status == StatusFailed {
			return nil
		}
		if current.Status == StatusLeft && current.EventID == event.ID {
			return nil
		}
		return p.save(ctx, key, revision, episode{Status: StatusLeft, EventID: event.ID, Timestamp: event.Timestamp, OperationID: operationID(p.binding.Installation, user, episodeID)})
	}
	op := operationID(p.binding.Installation, user, episodeID)
	if current.Status == StatusPosted || current.Status == StatusMissed || current.Status == StatusLeft || current.Status == StatusFailed {
		return nil
	}
	if current.Status == StatusPending && (current.EventID != event.ID || current.Timestamp != event.Timestamp || current.OperationID != op) {
		return ErrConflict
	}
	if current.Status == StatusNone {
		status := StatusPending
		if tooOld(event.Timestamp, p.now()) {
			status = StatusMissed
		}
		if err = p.save(ctx, key, revision, episode{Status: status, EventID: event.ID, Timestamp: event.Timestamp, OperationID: op}); err != nil {
			return err
		}
		current, revision, err = p.load(ctx, key)
		if err != nil {
			return err
		}
		if current.Status == StatusMissed {
			return nil
		}
	}
	// next is the stored episode with a new status; it keeps the attempt counter.
	next := func(status Status) episode {
		v := current
		v.Status = status
		return v
	}
	attempt := attemptID(op, current.Attempt)
	if tooOld(event.Timestamp, p.now()) {
		// Only the current attempt can be outstanding: each earlier one was closed before its successor was stored.
		receipt, found, resolveErr := p.sender.ResolveWelcome(ctx, op, attempt)
		if resolveErr != nil {
			return resolveErr
		}
		if found {
			if closedReceipt(receipt, op, attempt, p.binding.Community, p.expectedGroup) {
				return p.save(ctx, key, revision, next(StatusMissed))
			}
			if !acceptedReceipt(receipt, op, attempt, p.binding.Community, p.expectedGroup) {
				return ErrReceiptUnresolved
			}
			return p.save(ctx, key, revision, next(StatusPosted))
		}
		return p.save(ctx, key, revision, next(StatusMissed))
	}
	group, err := p.destination.ResolveTextGroup(ctx, p.binding.Community, p.channel)
	if err != nil {
		return err
	}
	if group.IsZero() || group != p.expectedGroup {
		return errors.New("welcome channel destination is missing or changed")
	}
	member, name, err := p.members.ResolveCurrentMember(ctx, user)
	if err != nil {
		return err
	}
	if !member {
		return p.save(ctx, key, revision, next(StatusLeft))
	}
	text, err := Preview(p.template, name, p.serverName, p.channelName)
	if err != nil {
		return err
	}
	if event.Timestamp > math.MaxInt64 {
		return errors.New("welcome event timestamp out of range")
	}
	req := SendRequest{OperationID: op, AttemptID: attempt, CommunityID: p.binding.Community, InstallationID: p.binding.Installation, MemberID: user, EpisodeID: episodeID, GroupID: group, Text: text, OriginalNow: event.Timestamp}
	if current.Attempt > 0 {
		// The server's attempt chain stays authoritative: a successor names the closed attempt it replaces
		// (announcements/scheduler.go does the same).
		previous := attemptID(op, current.Attempt-1)
		req.PredecessorAttemptID = &previous
	}
	receipt, sendErr := p.sender.SendWelcome(ctx, req)
	if sendErr != nil {
		return sendErr
	}
	if closedReceipt(receipt, op, attempt, p.binding.Community, group) {
		// The server refused the post (the group was frozen by pending proposals, or its epoch moved) and closed the
		// attempt. It can never be accepted later, so the episode moves on to a successor attempt, up to maxAttempts.
		closed := next(StatusPending)
		closed.Attempt++
		if closed.Attempt >= maxAttempts {
			closed.Status = StatusFailed
			if err = p.save(ctx, key, revision, closed); err != nil {
				return err
			}
			return fmt.Errorf("%w: %d attempts closed without acceptance", ErrGaveUp, closed.Attempt)
		}
		if err = p.save(ctx, key, revision, closed); err != nil {
			return err
		}
		return fmt.Errorf("welcome attempt %d closed without acceptance: %w", current.Attempt, ErrRetryLater)
	}
	if !acceptedReceipt(receipt, op, attempt, p.binding.Community, group) {
		return errors.New("welcome send has no authoritative accepted receipt")
	}
	return p.save(ctx, key, revision, next(StatusPosted))
}

// closedReceipt reports a valid receipt of exactly this attempt that the server closed without accepting it.
func closedReceipt(receipt protocol.SendReceipt, operation, attempt, community, group id.ID) bool {
	_, err := protocol.EncodeReceipt(receipt)
	return err == nil && receipt.Outcome == protocol.ReceiptClosedNotAccepted && receipt.OperationID == operation &&
		receipt.AttemptID == attempt && protocol.ValidateReceiptContext(receipt, community, group) == nil
}

func acceptedReceipt(receipt protocol.SendReceipt, operation, attempt, community, group id.ID) bool {
	_, err := protocol.EncodeReceipt(receipt)
	return err == nil && receipt.Outcome == protocol.ReceiptAccepted && receipt.OperationID == operation &&
		receipt.AttemptID == attempt && protocol.ValidateReceiptContext(receipt, community, group) == nil && receipt.SendResponse != nil
}

func (p *Processor) Status(ctx context.Context, user, episodeID id.ID) (Status, error) {
	if p == nil || user.IsZero() || episodeID.IsZero() {
		return StatusNone, errors.New("invalid welcome status key")
	}
	v, _, err := p.load(ctx, p.stateKey(user, episodeID))
	return v.Status, err
}

// GrantRole is deliberately explicit until this installation exposes an
// authoritative, hierarchy-checked Bot role mutation API.
func (p *Processor) GrantRole(context.Context, id.ID, id.ID) error { return ErrRoleGrantUnavailable }

func tooOld(ts uint64, now time.Time) bool {
	if ts > math.MaxInt64 {
		return true
	}
	eventTime := time.Unix(int64(ts), 0)
	return now.Sub(eventTime) > maxCatchupAge
}

func operationID(installation, user, episodeID id.ID) id.ID {
	h := sha256.Sum256(append(append(append([]byte("dilla.bot.welcome.operation.v1\x00"), installation[:]...), user[:]...), episodeID[:]...))
	var out id.ID
	copy(out[:], h[:16])
	if out.IsZero() {
		out[15] = 1
	}
	return out
}

// attemptID is the operation's attempt n. Attempt 0 keeps the v1 id, so episodes already in flight keep theirs; a
// successor n >= 1 hashes the v2 domain, the operation and n as a big-endian uint32.
func attemptID(op id.ID, n ...uint32) id.ID {
	h := sha256.Sum256(append([]byte("dilla.bot.welcome.attempt.v1\x00"), op[:]...))
	if len(n) > 0 && n[0] > 0 {
		var counter [4]byte
		binary.BigEndian.PutUint32(counter[:], n[0])
		h = sha256.Sum256(append(append([]byte("dilla.bot.welcome.attempt.v2\x00"), op[:]...), counter[:]...))
	}
	var out id.ID
	copy(out[:], h[:16])
	if out.IsZero() {
		out[15] = 1
	}
	return out
}
func (p *Processor) stateKey(user, episodeID id.ID) []byte {
	return []byte(statePrefix + p.binding.Community.String() + "/" + user.String() + "/" + episodeID.String())
}

func (p *Processor) load(ctx context.Context, key []byte) (episode, uint64, error) {
	v, err := p.native.StateGet(ctx, stateArea, key)
	if err != nil {
		return episode{}, 0, err
	}
	if v.Tombstone {
		return episode{}, v.Revision, errors.New("welcome episode state tombstoned")
	}
	if v.Revision == 0 {
		return episode{}, 0, nil
	}
	// Version 1 rows (five fields) predate attempts and read as attempt 0; version 2 rows add the attempt and allow
	// StatusFailed.
	var a []any
	decodeErr := cborx.Unmarshal(v.Value, &a)
	versioned := len(a) == 5 && a[0] == uint64(1) || len(a) == 6 && a[0] == uint64(2)
	if decodeErr != nil || !versioned {
		return episode{}, 0, errors.New("malformed welcome episode state")
	}
	maxStatus, attempt := uint64(StatusLeft), uint64(0)
	if len(a) == 6 {
		var aok bool
		if attempt, aok = a[5].(uint64); !aok || attempt > maxAttempts {
			return episode{}, 0, errors.New("malformed welcome episode state")
		}
		maxStatus = uint64(StatusFailed)
	}
	status, ok := a[1].(uint64)
	eventBytes, eok := a[2].([]byte)
	ts, tok := a[3].(uint64)
	opBytes, ook := a[4].([]byte)
	if !ok || status < uint64(StatusPending) || status > maxStatus || !eok || len(eventBytes) != 16 || !tok || ts > math.MaxInt64 || !ook || len(opBytes) != 16 {
		return episode{}, 0, errors.New("malformed welcome episode state")
	}
	var result episode
	result.Attempt = uint32(attempt)
	result.Status = Status(status)
	copy(result.EventID[:], eventBytes)
	copy(result.OperationID[:], opBytes)
	result.Timestamp = ts
	if result.EventID.IsZero() || result.OperationID.IsZero() {
		return episode{}, 0, errors.New("malformed welcome episode state")
	}
	return result, v.Revision, nil
}

func (p *Processor) save(ctx context.Context, key []byte, revision uint64, value episode) error {
	if revision >= math.MaxInt64 {
		return errors.New("welcome state revision exhausted")
	}
	row := []any{uint64(1), uint64(value.Status), value.EventID, value.Timestamp, value.OperationID}
	if value.Attempt > 0 || value.Status == StatusFailed {
		// Version 2 only when it carries something version 1 cannot, so an episode without retries stays readable by
		// an older Bot.
		row = []any{uint64(2), uint64(value.Status), value.EventID, value.Timestamp, value.OperationID, uint64(value.Attempt)}
	}
	body, err := cborx.Marshal(row)
	if err != nil {
		return err
	}
	// Each independent CAS attempt needs a fresh transaction ID. Reusing a
	// content-derived ID could replay an old helper receipt after a newer state
	// revision has already committed.
	tx := id.New()
	if tx.IsZero() {
		return errors.New("invalid welcome transaction id")
	}
	revisions, txErr := p.native.StateTransaction(ctx, tx, []native.StateChange{{Area: stateArea, Key: key, ExpectedRevision: revision, Value: body}})
	if txErr == nil && len(revisions) == 1 && revisions[0] == revision+1 {
		return nil
	}
	actual, readErr := p.native.StateGet(ctx, stateArea, key)
	if readErr == nil && !actual.Tombstone && actual.Revision == revision+1 && bytes.Equal(actual.Value, body) {
		return nil
	}
	if readErr != nil {
		return errors.Join(txErr, fmt.Errorf("welcome transaction readback: %w", readErr))
	}
	if txErr != nil {
		return txErr
	}
	return errors.New("invalid welcome state transaction revision")
}
