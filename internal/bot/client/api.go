package client

import (
	"context"
	"crypto/sha256"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	botprotocol "github.com/jonasthim/dilla/internal/bot/protocol"
	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/id"
)

const maxStoredSequence uint64 = 1<<63 - 1

const maxBotGroupRegistrationBody = 65505

var ErrRegistrationReceiptNotFound = errors.New("bot client: group registration receipt not found")

// BotGroupRegistrationReceipt is the immutable acceptance result for one
// installation-scoped native group-registration operation.
type BotGroupRegistrationReceipt struct {
	OperationID        id.ID
	GroupID            id.ID
	RegistrationSHA256 [32]byte
	AcceptedGeneration uint64
	InitialEpoch       uint64
	NextSequence       uint64
}

func (c *Client) Discover(ctx context.Context) (Discovery, error) {
	d, _, err := c.DiscoverObserved(ctx)
	return d, err
}

// DiscoverObserved returns Date and generation observed on the exact response
// that supplied the discovery document. An invalid or absent Date is marked
// unavailable; callers must not substitute their local clock for server time.
func (c *Client) DiscoverObserved(ctx context.Context) (Discovery, ResponseObservation, error) {
	raw, _, observation, err := c.doObserved(ctx, "", http.MethodGet, "instance", nil, nil, MaxSmallBody, false)
	if err != nil {
		return Discovery{}, observation, err
	}
	a, err := decode(raw, 11)
	if err != nil {
		return Discovery{}, observation, err
	}
	var d Discovery
	d.WireVersions, err = uints(a[0])
	if err != nil {
		return d, observation, err
	}
	if !distinctVersions(d.WireVersions) {
		return d, observation, ErrProtocol
	}
	d.E2EEVersions, err = uints(a[1])
	if err != nil {
		return d, observation, err
	}
	if !distinctVersions(d.E2EEVersions) {
		return d, observation, ErrProtocol
	}
	d.MediaVersions, err = uints(a[2])
	if err != nil {
		return d, observation, err
	}
	if !distinctVersions(d.MediaVersions) {
		return d, observation, ErrProtocol
	}
	if d.InstanceID, _ = asID(a[3]); d.InstanceID.IsZero() {
		return d, observation, ErrProtocol
	}
	if d.Generation, _ = asUint(a[4]); d.Generation == 0 {
		return d, observation, ErrProtocol
	}
	var ok bool
	d.Name, ok = a[5].(string)
	if !ok || d.Name == "" {
		return d, observation, ErrProtocol
	}
	if d.RegistrationMode, ok = asUint(a[6]); !ok || d.RegistrationMode > 2 {
		return d, observation, ErrProtocol
	}
	d.AuthMethods, err = uints(a[7])
	if err != nil {
		return d, observation, err
	}
	if !strictlyIncreasing(d.AuthMethods) {
		return d, observation, ErrProtocol
	}
	if d.PolicyVersion, ok = asUint(a[8]); !ok {
		return d, observation, ErrProtocol
	}
	if d.ExternalSenderKeyID, ok = asID(a[9]); !ok {
		return d, observation, ErrProtocol
	}
	if d.ExternalSenderPub, ok = asBytes(a[10], 32); !ok {
		return d, observation, ErrProtocol
	}
	d.ExternalSenderPub = append([]byte(nil), d.ExternalSenderPub...)
	if !equalID(d.InstanceID, c.instance) {
		return Discovery{}, observation, ErrInstanceChanged
	}
	if !slices.Contains(d.WireVersions, uint64(3)) || !slices.Contains(d.E2EEVersions, uint64(1)) || !slices.Contains(d.MediaVersions, uint64(1)) {
		return Discovery{}, observation, ErrProtocol
	}
	c.discovered.Store(true)
	return d, observation, nil
}
func strictlyIncreasing(a []uint64) bool {
	for i := 1; i < len(a); i++ {
		if a[i] <= a[i-1] {
			return false
		}
	}
	return true
}

func (c *Client) InstanceLimits(ctx context.Context) (Limits, error) {
	var out Limits
	raw, _, err := c.do(ctx, "", http.MethodGet, "instance/limits", nil, nil, MaxSmallBody, false)
	if err != nil {
		return out, err
	}
	a, err := decode(raw, 11)
	if err != nil {
		return out, err
	}
	for i, v := range a {
		n, ok := asUint(v)
		if !ok {
			return Limits{}, ErrProtocol
		}
		out.Values[i] = n
	}
	return out, nil
}
func uints(v any) ([]uint64, error) {
	a, ok := v.([]any)
	if !ok {
		return nil, ErrProtocol
	}
	out := make([]uint64, len(a))
	for i, x := range a {
		n, ok := asUint(x)
		if !ok {
			return nil, ErrProtocol
		}
		out[i] = n
	}
	return out, nil
}

func (r *Runtime) ListInstallationsPage(ctx context.Context, after *id.ID) ([]Installation, error) {
	q := url.Values{}
	if after != nil {
		s, e := pathID(*after)
		if e != nil {
			return nil, e
		}
		q.Set("after", s)
	}
	// v=2 (Task 10c): the widened installation with the full capability mask, the restart
	// counters, the reported version and the music defaults. A server that predates it ignores
	// the parameter and answers the eight base fields, which still parse.
	q.Set("v", "2")
	raw, _, err := r.c.do(ctx, r.token, http.MethodGet, "bot-runtime/installations", q, nil, MaxSmallBody, false)
	if err != nil {
		return nil, err
	}
	a, err := decode(raw, -1)
	if err != nil {
		return nil, err
	}
	if len(a) > 100 {
		return nil, ErrProtocol
	}
	out := make([]Installation, len(a))
	var prev id.ID
	for i, v := range a {
		row, ok := v.([]any)
		if !ok || len(row) != 8 && len(row) != 13 {
			return nil, ErrProtocol
		}
		out[i], err = parseInstallation(row)
		if err != nil {
			return nil, err
		}
		if !out[i].Widened {
			// An eight-field row answers v=2 only from a server that predates Polls and Reminders
			// (Task 10c fix round 1, M2): it has no switch for them, so both are on.
			out[i].Capabilities |= LegacyServerCapabilities
		}
		if i > 0 && strings.Compare(prev.String(), out[i].ID.String()) >= 0 {
			return nil, ErrProtocol
		}
		if after != nil && i == 0 && strings.Compare(after.String(), out[i].ID.String()) >= 0 {
			return nil, ErrProtocol
		}
		prev = out[i].ID
	}
	return out, nil
}
func (r *Runtime) ListInstallations(ctx context.Context) ([]Installation, error) {
	ctx, cancel := r.c.aggregateContext(ctx)
	defer cancel()
	var out []Installation
	var cursor *id.ID
	seen := map[id.ID]struct{}{}
	for {
		page, err := r.ListInstallationsPage(ctx, cursor)
		if err != nil {
			return nil, err
		}
		if len(out)+len(page) > MaxAggregateRows {
			return nil, ErrPageLimit
		}
		for _, x := range page {
			if _, ok := seen[x.ID]; ok {
				return nil, ErrProtocol
			}
			seen[x.ID] = struct{}{}
			out = append(out, x)
		}
		if len(page) < 100 {
			return out, nil
		}
		last := page[len(page)-1].ID
		cursor = &last
	}
}
func parseInstallation(a []any) (Installation, error) {
	var x Installation
	var ok bool
	if x.ID, ok = asID(a[0]); !ok {
		return x, ErrProtocol
	}
	if x.CommunityID, ok = asID(a[1]); !ok {
		return x, ErrProtocol
	}
	if x.State, ok = asUint(a[2]); !ok {
		return x, ErrProtocol
	}
	if x.Revision, ok = asUint(a[3]); !ok {
		return x, ErrProtocol
	}
	parseNullableID := func(v any) (*id.ID, bool) {
		if v == nil {
			return nil, true
		}
		i, ok := asID(v)
		return &i, ok
	}
	if x.BotUser, ok = parseNullableID(a[4]); !ok {
		return x, ErrProtocol
	}
	if x.BotDevice, ok = parseNullableID(a[5]); !ok {
		return x, ErrProtocol
	}
	if x.Capabilities, ok = asUint(a[6]); !ok {
		return x, ErrProtocol
	}
	if x.UpdatedAt, ok = asUint(a[7]); !ok {
		return x, ErrProtocol
	}
	if len(a) == 8 {
		return x, nil
	}
	if len(a) != 13 {
		return x, ErrProtocol
	}
	x.Widened = true
	if x.RestartRequested, ok = asUint(a[8]); !ok {
		return x, ErrProtocol
	}
	if x.RestartHandled, ok = asUint(a[9]); !ok || x.RestartHandled > x.RestartRequested {
		return x, ErrProtocol
	}
	if a[10] != nil {
		version, ok := a[10].(string)
		if !ok || version == "" || len(version) > 32 {
			return x, ErrProtocol
		}
		x.RuntimeVersion = &version
	}
	if x.MusicVolume, ok = asUint(a[11]); !ok {
		return x, ErrProtocol
	}
	if x.MusicIdleMinutes, ok = asUint(a[12]); !ok {
		return x, ErrProtocol
	}
	return x, nil
}

// ReportStatus tells the server, for one installation this runtime owns, the runtime's build
// version and the last restart request it has finished (Task 10c). version nil reports none.
func (r *Runtime) ReportStatus(ctx context.Context, installation id.ID, version *string, handled uint64) error {
	i, err := pathID(installation)
	if err != nil {
		return err
	}
	var v any
	if version != nil {
		v = *version
	}
	body, err := encode([]any{v, handled})
	if err != nil {
		return err
	}
	_, _, err = r.c.do(ctx, r.token, http.MethodPut, "bot-runtime/installations/"+i+"/status", nil, body, MaxSmallBody, true)
	return err
}
func (r *Runtime) Claim(ctx context.Context, installation, request id.ID) (Claim, error) {
	c, _, err := r.ClaimObserved(ctx, installation, request)
	return c, err
}

// ClaimObserved returns the authority headers from the response that carries
// the claim receipt.
func (r *Runtime) ClaimObserved(ctx context.Context, installation, request id.ID) (Claim, ResponseObservation, error) {
	var out Claim
	i, e := pathID(installation)
	if e != nil {
		return out, ResponseObservation{}, e
	}
	if request.IsZero() {
		return out, ResponseObservation{}, ErrInvalidConfig
	}
	b, e := encode([]any{request})
	if e != nil {
		return out, ResponseObservation{}, e
	}
	raw, _, observation, e := r.c.doObserved(ctx, r.token, http.MethodPost, "bot-runtime/installations/"+i+"/claim", nil, b, MaxSmallBody, true)
	if e != nil {
		return out, observation, e
	}
	a, e := decode(raw, 3)
	if e != nil {
		return out, observation, &APIError{Code: "E_OUTCOME_UNKNOWN", Outcome: OutcomeUnknown}
	}
	var ok bool
	if out.InstallationID, ok = asID(a[0]); !ok {
		return out, observation, unknown()
	}
	if out.ClaimID, ok = asID(a[1]); !ok {
		return out, observation, unknown()
	}
	if out.Expires, ok = asUint(a[2]); !ok || !equalID(out.InstallationID, installation) {
		return out, observation, unknown()
	}
	return out, observation, nil
}
func (r *Runtime) Enroll(ctx context.Context, installation, request, claim id.ID, umk, ssk, sig []byte, device DeviceRegistration) (Enrollment, error) {
	e, _, err := r.EnrollObserved(ctx, installation, request, claim, umk, ssk, sig, device)
	return e, err
}

// EnrollObserved returns the authenticated server time and generation from the
// same runtime response as the enrollment result.
func (r *Runtime) EnrollObserved(ctx context.Context, installation, request, claim id.ID, umk, ssk, sig []byte, device DeviceRegistration) (Enrollment, ResponseObservation, error) {
	var out Enrollment
	i, e := pathID(installation)
	if e != nil {
		return out, ResponseObservation{}, e
	}
	if request.IsZero() || claim.IsZero() || len(umk) != 32 || len(ssk) != 32 || len(sig) != 64 || device.DeviceID.IsZero() || len(device.DSKPub) != 32 || device.Tier != 0 || device.SignerTier != 0 || len(device.Credential) == 0 || len(device.Credential) > 2048 {
		return out, ResponseObservation{}, ErrInvalidConfig
	}
	b, e := encode([]any{request, claim, umk, ssk, sig, []any{device.DeviceID, device.DSKPub, device.Tier, device.SignerTier, device.Credential}})
	if e != nil {
		return out, ResponseObservation{}, e
	}
	raw, _, observation, e := r.c.doObserved(ctx, r.token, http.MethodPost, "bot-runtime/installations/"+i+"/enroll", nil, b, MaxRuntimeBody, true)
	if e != nil {
		return out, observation, e
	}
	a, e := decode(raw, 3)
	if e != nil {
		return out, observation, unknown()
	}
	var ok bool
	if out.BotUser, ok = asID(a[0]); !ok {
		return out, observation, unknown()
	}
	if out.BotDevice, ok = asID(a[1]); !ok || !equalID(out.BotDevice, device.DeviceID) {
		return out, observation, unknown()
	}
	out.Username, ok = a[2].(string)
	if !ok || out.Username == "" {
		return Enrollment{}, observation, unknown()
	}
	return out, observation, nil
}

func (c *Client) Challenge(ctx context.Context, device id.ID) (SessionChallenge, error) {
	var out SessionChallenge
	s, e := pathID(device)
	if e != nil {
		return out, e
	}
	b, e := encode([]any{})
	if e != nil {
		return out, e
	}
	raw, _, e := c.do(ctx, "", http.MethodPost, "devices/"+s+"/sessions/challenge", nil, b, MaxSmallBody, true)
	if e != nil {
		return out, e
	}
	a, e := decode(raw, 2)
	if e != nil {
		return out, unknown()
	}
	var ok bool
	if out.Nonce, ok = asBytes(a[0], 32); !ok {
		return out, unknown()
	}
	out.Nonce = append([]byte(nil), out.Nonce...)
	if out.Expires, ok = asUint(a[1]); !ok {
		return SessionChallenge{}, unknown()
	}
	return out, nil
}

// EstablishSession submits a normal device proof. The signature is produced by
// the native helper over the protocol/02 session preimage and is never logged.
func (c *Client) EstablishSession(ctx context.Context, device id.ID, nonce []byte, purpose uint64, signature []byte, registration any, login []byte) (DeviceSession, error) {
	var out DeviceSession
	s, e := pathID(device)
	if e != nil {
		return out, e
	}
	if len(nonce) != 32 || len(signature) != 64 || purpose > 2 {
		return out, ErrInvalidConfig
	}
	b, e := encode([]any{nonce, purpose, signature, registration, login})
	if e != nil {
		return out, e
	}
	raw, _, e := c.do(ctx, "", http.MethodPost, "devices/"+s+"/sessions", nil, b, MaxSessionBody, true)
	if e != nil {
		return out, e
	}
	a, e := decode(raw, 7)
	if e != nil {
		return out, unknown()
	}
	var ok bool
	if out.Token, ok = a[0].(string); !ok || out.Token == "" {
		return out, unknown()
	}
	if out.Scope, ok = asUint(a[1]); !ok || out.Scope != 0 {
		return out, unknown()
	}
	if out.UserID, ok = asID(a[2]); !ok {
		return out, unknown()
	}
	if out.DeviceID, ok = asID(a[3]); !ok || !equalID(out.DeviceID, device) {
		return out, unknown()
	}
	if out.Expires, ok = asUint(a[4]); !ok {
		return out, unknown()
	}
	if out.IdleExpires, ok = asUint(a[5]); !ok {
		return out, unknown()
	}
	if out.Generation, ok = asUint(a[6]); !ok {
		return out, unknown()
	}
	return out, nil
}
func (s *Session) Negotiate(ctx context.Context, wire, e2ee, media []uint64) (Negotiated, error) {
	var out Negotiated
	if !distinctVersions(wire) || !distinctVersions(e2ee) || !distinctVersions(media) {
		return out, ErrInvalidConfig
	}
	b, e := encode([]any{wire, e2ee, media})
	if e != nil {
		return out, e
	}
	raw, _, e := s.do(ctx, http.MethodPost, "sessions/negotiate", nil, b, MaxSmallBody, true)
	if e != nil {
		return out, e
	}
	a, e := decode(raw, 3)
	if e != nil {
		return out, unknown()
	}
	var ok bool
	if out.Wire, ok = asUint(a[0]); !ok || out.Wire != 3 {
		return Negotiated{}, unknown()
	}
	if out.E2EE, ok = asUint(a[1]); !ok || out.E2EE != 1 {
		return Negotiated{}, unknown()
	}
	if out.Media, ok = asUint(a[2]); !ok || out.Media != 1 {
		return Negotiated{}, unknown()
	}
	return out, nil
}
func distinctVersions(a []uint64) bool {
	if len(a) == 0 || len(a) > 8 {
		return false
	}
	m := map[uint64]bool{}
	for _, n := range a {
		if n == 0 || m[n] {
			return false
		}
		m[n] = true
	}
	return true
}

func (s *Session) FinalizeCredential(ctx context.Context, installation, request id.ID, revision uint64, credential []byte) (Installation, error) {
	return s.installationMutation(ctx, installation, "credential", request, revision, credential, true)
}
func (s *Session) MarkReady(ctx context.Context, installation, request id.ID, revision uint64) (Installation, error) {
	return s.installationMutation(ctx, installation, "ready", request, revision, nil, false)
}

// PublishKeyPackages sends the exact canonical upload body with a caller-owned operation ID. It
// does not retry uncertain mutations; callers resolve that same ID through
// KeyPackagePublicationReceipt before deciding what to do next.
func (s *Session) PublishKeyPackages(ctx context.Context, installation, operation id.ID, generation uint64, upload []byte) (uint64, error) {
	var count uint64
	request, err := botprotocol.EncodeKeyPackagePublication(botprotocol.KeyPackagePublication{
		OperationID: operation, ExpectedGeneration: generation, UploadBody: upload,
	})
	if err != nil {
		return 0, ErrInvalidConfig
	}
	inst, err := pathID(installation)
	if err != nil {
		return 0, err
	}
	if operation.IsZero() {
		return 0, ErrInvalidConfig
	}
	raw, _, err := s.do(ctx, http.MethodPost, "bot-runtime/installations/"+inst+"/keypackages", nil, request, MaxDSBody, true)
	if err != nil {
		return 0, err
	}
	a, err := decode(raw, 1)
	if err != nil {
		return 0, unknown()
	}
	if count, _ = asUint(a[0]); count < 1 || count > 33 {
		return 0, unknown()
	}
	return count, nil
}

func (s *Session) KeyPackagePublicationReceipt(ctx context.Context, installation, operation id.ID) (KeyPackagePublicationReceipt, error) {
	r, _, err := s.KeyPackagePublicationReceiptObserved(ctx, installation, operation)
	return r, err
}

// KeyPackagePublicationReceiptObserved returns the authenticated historical
// receipt and authority headers from that same pinned response.
func (s *Session) KeyPackagePublicationReceiptObserved(ctx context.Context, installation, operation id.ID) (KeyPackagePublicationReceipt, ResponseObservation, error) {
	var out KeyPackagePublicationReceipt
	inst, err := pathID(installation)
	if err != nil {
		return out, ResponseObservation{}, err
	}
	op, err := pathID(operation)
	if err != nil {
		return out, ResponseObservation{}, err
	}
	raw, _, observation, err := s.doObserved(ctx, http.MethodGet, "bot-runtime/installations/"+inst+"/keypackages/"+op, nil, nil, MaxSmallBody, false)
	if err != nil {
		return out, observation, err
	}
	receipt, err := botprotocol.DecodeKeyPackagePublicationReceipt(raw)
	if err != nil {
		return out, observation, ErrProtocol
	}
	out.Generation, out.Count, out.UploadSHA256 = receipt.ExpectedGeneration, receipt.Count, receipt.UploadSHA256
	return out, observation, nil
}

func (s *Session) installationMutation(ctx context.Context, installation id.ID, action string, request id.ID, revision uint64, credential []byte, put bool) (Installation, error) {
	var out Installation
	i, e := pathID(installation)
	if e != nil {
		return out, e
	}
	if request.IsZero() {
		return out, ErrInvalidConfig
	}
	var body []byte
	if put {
		if len(credential) == 0 || len(credential) > 8192 {
			return out, ErrInvalidConfig
		}
		body, e = encode([]any{request, revision, credential})
	} else {
		body, e = encode([]any{request, revision})
	}
	if e != nil {
		return out, e
	}
	method := http.MethodPost
	if put {
		method = http.MethodPut
	}
	path := "bot-runtime/installations/" + i + "/" + action
	raw, _, e := s.do(ctx, method, path, nil, body, MaxRuntimeBody, true)
	if e != nil {
		return out, e
	}
	a, e := decode(raw, 8)
	if e != nil {
		return out, unknown()
	}
	out, e = parseInstallation(a)
	if e != nil || !equalID(out.ID, installation) {
		return Installation{}, unknown()
	}
	return out, nil
}

func unknown() error { return &APIError{Code: "E_OUTCOME_UNKNOWN", Outcome: OutcomeUnknown} }

// PublishDeviceList sends the helper-produced signed outer list unchanged.
func (s *Session) PublishDeviceList(ctx context.Context, user id.ID, version uint64, blob, signature, previousHash []byte) error {
	u, e := pathID(user)
	if e != nil {
		return e
	}
	if version == 0 || len(blob) == 0 || len(blob) > MaxSmallBody || len(signature) != 64 || len(previousHash) != 32 {
		return ErrInvalidConfig
	}
	b, e := encode([]any{version, blob, signature, previousHash})
	if e != nil {
		return e
	}
	_, _, e = s.do(ctx, http.MethodPut, "users/"+u+"/device-list", nil, b, MaxSmallBody, true)
	return e
}
func (s *Session) OwnDeviceList(ctx context.Context, user id.ID) ([]byte, error) {
	u, e := pathID(user)
	if e != nil {
		return nil, e
	}
	raw, _, e := s.do(ctx, http.MethodGet, "users/"+u+"/device-list", nil, nil, MaxSmallBody, false)
	if e != nil {
		return nil, e
	}
	a, decodeErr := decode(raw, 4)
	if decodeErr != nil {
		return nil, decodeErr
	}
	if _, ok := asUint(a[0]); !ok {
		return nil, ErrProtocol
	}
	if _, ok := asBytes(a[1], -1); !ok {
		return nil, ErrProtocol
	}
	if _, ok := asBytes(a[2], 64); !ok {
		return nil, ErrProtocol
	}
	if _, ok := asBytes(a[3], 32); !ok {
		return nil, ErrProtocol
	}
	return raw, nil
}

func (s *Session) DeviceListHistory(ctx context.Context, user id.ID, after uint64) ([][]any, error) {
	u, e := pathID(user)
	if e != nil {
		return nil, e
	}
	if after > maxStoredSequence {
		return nil, ErrInvalidConfig
	}
	q := url.Values{}
	qnum(q, "after", after)
	raw, _, e := s.do(ctx, http.MethodGet, "users/"+u+"/device-list", q, nil, MaxBody, false)
	if e != nil {
		return nil, e
	}
	a, e := decode(raw, -1)
	if e != nil || len(a) > 64 {
		return nil, ErrProtocol
	}
	rows := make([][]any, len(a))
	var last uint64
	for i, value := range a {
		row, ok := value.([]any)
		if !ok || len(row) != 4 {
			return nil, ErrProtocol
		}
		version, ok := asUint(row[0])
		if !ok || version <= after || i > 0 && version <= last {
			return nil, ErrProtocol
		}
		if _, ok := asBytes(row[1], -1); !ok {
			return nil, ErrProtocol
		}
		if _, ok := asBytes(row[2], 64); !ok {
			return nil, ErrProtocol
		}
		if _, ok := asBytes(row[3], 32); !ok {
			return nil, ErrProtocol
		}
		last, rows[i] = version, row
	}
	return rows, nil
}

func (s *Session) KeyPackage(ctx context.Context, device id.ID) ([]byte, error) {
	d, e := pathID(device)
	if e != nil {
		return nil, e
	}
	raw, _, e := s.do(ctx, http.MethodGet, "devices/"+d+"/keypackage", nil, nil, MaxDSBody, false)
	if e != nil {
		return nil, e
	}
	a, e := decode(raw, 3)
	if e != nil {
		return nil, e
	}
	keyPackage, ok := asBytes(a[0], -1)
	if !ok || len(keyPackage) == 0 || len(keyPackage) > 65505 {
		return nil, ErrProtocol
	}
	if n, ok := asUint(a[1]); !ok || n > 1 {
		return nil, ErrProtocol
	}
	if _, ok := asBytes(a[2], -1); !ok {
		return nil, ErrProtocol
	}
	return append([]byte(nil), keyPackage...), nil
}
func (s *Session) Welcomes(ctx context.Context, after uint64, limit uint64) ([]Welcome, error) {
	if limit == 0 || limit > 64 {
		return nil, ErrInvalidConfig
	}
	if after > maxStoredSequence {
		return nil, ErrInvalidConfig
	}
	q := url.Values{}
	qnum(q, "after", after)
	qnum(q, "limit", limit)
	raw, _, e := s.do(ctx, http.MethodGet, "welcomes", q, nil, MaxBody, false)
	if e != nil {
		return nil, e
	}
	a, e := decode(raw, -1)
	if e != nil {
		return nil, e
	}
	if uint64(len(a)) > limit {
		return nil, ErrProtocol
	}
	out := make([]Welcome, len(a))
	last := after
	for i, row := range a {
		v, ok := row.([]any)
		if !ok || len(v) != 7 {
			return nil, ErrProtocol
		}
		if out[i].ID, ok = asUint(v[0]); !ok || out[i].ID == 0 || out[i].ID <= last {
			return nil, ErrProtocol
		}
		last = out[i].ID
		if out[i].GroupID, ok = asID(v[1]); !ok {
			return nil, ErrProtocol
		}
		if out[i].Epoch, ok = asUint(v[2]); !ok {
			return nil, ErrProtocol
		}
		if out[i].CommitSequence, ok = asUint(v[3]); !ok {
			return nil, ErrProtocol
		}
		if out[i].Blob, ok = asBytes(v[4], -1); !ok {
			return nil, ErrProtocol
		}
		if out[i].Tree, ok = asBytes(v[5], -1); !ok {
			return nil, ErrProtocol
		}
		if out[i].TreeHash, ok = asBytes(v[6], 32); !ok {
			return nil, ErrProtocol
		}
	}
	return out, nil
}
func (s *Session) AllWelcomes(ctx context.Context, limit uint64) ([]Welcome, error) {
	if limit == 0 || limit > 64 {
		return nil, ErrInvalidConfig
	}
	ctx, cancel := s.c.aggregateContext(ctx)
	defer cancel()
	var all []Welcome
	var after uint64
	for {
		page, err := s.Welcomes(ctx, after, limit)
		if err != nil {
			return nil, err
		}
		if len(all)+len(page) > MaxAggregateRows {
			return nil, ErrPageLimit
		}
		all = append(all, page...)
		if uint64(len(page)) < limit {
			return all, nil
		}
		if len(page) == 0 || page[len(page)-1].ID <= after {
			return nil, ErrProtocol
		}
		after = page[len(page)-1].ID
	}
}
func (s *Session) AcknowledgeWelcome(ctx context.Context, welcome uint64) error {
	if welcome == 0 || welcome > maxStoredSequence {
		return ErrInvalidConfig
	}
	_, _, e := s.do(ctx, http.MethodDelete, "welcomes/"+strconv.FormatUint(welcome, 10), nil, nil, MaxSmallBody, true)
	return e
}

func (s *Session) GroupInfo(ctx context.Context, group id.ID) (GroupInfo, error) {
	return s.groupInfoAt(ctx, "groups/", group)
}

func (s *Session) BotInteractionGroupInfo(ctx context.Context, installation, group id.ID) (GroupInfo, error) {
	inst, err := pathID(installation)
	if err != nil {
		return GroupInfo{}, err
	}
	return s.groupInfoAt(ctx, "bot-runtime/installations/"+inst+"/interaction-groups/", group)
}

func (s *Session) groupInfoAt(ctx context.Context, base string, group id.ID) (GroupInfo, error) {
	var out GroupInfo
	g, e := pathID(group)
	if e != nil {
		return out, e
	}
	raw, _, e := s.do(ctx, http.MethodGet, base+g+"/info", nil, nil, MaxBody, false)
	if e != nil {
		return out, e
	}
	a, e := decode(raw, 4)
	if e != nil {
		return out, e
	}
	var ok bool
	if out.Epoch, ok = asUint(a[0]); !ok {
		return out, ErrProtocol
	}
	if out.Info, ok = asBytes(a[1], -1); !ok {
		return out, ErrProtocol
	}
	if out.TreeHash, ok = asBytes(a[2], 32); !ok {
		return out, ErrProtocol
	}
	if out.NextSequence, ok = asUint(a[3]); !ok {
		return out, ErrProtocol
	}
	return out, nil
}
func (s *Session) GroupTree(ctx context.Context, group id.ID) (GroupTree, error) {
	var out GroupTree
	g, e := pathID(group)
	if e != nil {
		return out, e
	}
	raw, _, e := s.do(ctx, http.MethodGet, "groups/"+g+"/tree", nil, nil, MaxBody, false)
	if e != nil {
		return out, e
	}
	a, e := decode(raw, 3)
	if e != nil {
		return out, e
	}
	var ok bool
	if out.Epoch, ok = asUint(a[0]); !ok {
		return out, ErrProtocol
	}
	if out.Tree, ok = asBytes(a[1], -1); !ok {
		return out, ErrProtocol
	}
	if out.TreeHash, ok = asBytes(a[2], 32); !ok {
		return out, ErrProtocol
	}
	return out, nil
}
func (s *Session) RegisterGroup(ctx context.Context, group id.ID, binding, info, tree []byte) (uint64, error) {
	_, e := pathID(group)
	if e != nil {
		return 0, e
	}
	if len(binding) == 0 || len(info) == 0 || len(tree) == 0 {
		return 0, ErrInvalidConfig
	}
	b, e := encode([]any{group, binding, info, tree})
	if e != nil {
		return 0, e
	}
	raw, _, e := s.do(ctx, http.MethodPost, "groups", nil, b, MaxDSBody, true)
	if e != nil {
		return 0, e
	}
	a, e := decode(raw, 2)
	if e != nil {
		return 0, unknown()
	}
	got, ok := asID(a[0])
	if !ok || !equalID(got, group) {
		return 0, unknown()
	}
	n, ok := asUint(a[1])
	if !ok {
		return 0, unknown()
	}
	return n, nil
}

// RegisterBotGroup submits the caller's exact pending native registration body
// through the installation-scoped durable operation endpoint. It never
// reconstructs, retries or replaces that body; an uncertain mutation remains
// unknown until the caller checks its receipt.
func (s *Session) RegisterBotGroup(ctx context.Context, installation, operation id.ID, expectedGeneration uint64, registrationBody []byte) (BotGroupRegistrationReceipt, error) {
	return s.registerBotGroup(ctx, installation, operation, expectedGeneration, registrationBody, nil)
}

// RegisterBotGroupWithAdmissionRequest submits a kind-3 registration intent linked to its
// durable admission request.
func (s *Session) RegisterBotGroupWithAdmissionRequest(ctx context.Context, installation, operation id.ID, expectedGeneration uint64, registrationBody []byte, admissionRequestID id.ID) (BotGroupRegistrationReceipt, error) {
	if admissionRequestID.IsZero() {
		return BotGroupRegistrationReceipt{}, ErrInvalidConfig
	}
	return s.registerBotGroup(ctx, installation, operation, expectedGeneration, registrationBody, &admissionRequestID)
}

func (s *Session) registerBotGroup(ctx context.Context, installation, operation id.ID, expectedGeneration uint64, registrationBody []byte, admissionRequestID *id.ID) (BotGroupRegistrationReceipt, error) {
	if operation.IsZero() || expectedGeneration > maxStoredSequence || len(registrationBody) == 0 || len(registrationBody) > maxBotGroupRegistrationBody {
		return BotGroupRegistrationReceipt{}, ErrInvalidConfig
	}
	inst, err := pathID(installation)
	if err != nil {
		return BotGroupRegistrationReceipt{}, err
	}
	group, err := registrationBodyGroup(registrationBody)
	if err != nil {
		return BotGroupRegistrationReceipt{}, ErrInvalidConfig
	}
	if admissionRequestID != nil && group.bodyFields != 5 || admissionRequestID == nil && group.bodyFields != 4 {
		return BotGroupRegistrationReceipt{}, ErrInvalidConfig
	}
	var requestTuple []any
	if admissionRequestID == nil {
		requestTuple = []any{operation, expectedGeneration, registrationBody}
	} else {
		requestTuple = []any{operation, expectedGeneration, registrationBody, *admissionRequestID}
	}
	request, err := encode(requestTuple)
	if err != nil {
		return BotGroupRegistrationReceipt{}, ErrInvalidConfig
	}
	raw, _, err := s.do(ctx, http.MethodPost, "bot-runtime/installations/"+inst+"/groups", nil, request, MaxSmallBody, true)
	if err != nil {
		return BotGroupRegistrationReceipt{}, err
	}
	return decodeBotGroupRegistrationReceipt(raw, operation, &group.id, registrationBody, &expectedGeneration)
}

// BotGroupRegistrationReceipt reads the durable receipt scoped to this
// installation and operation. ErrRegistrationReceiptNotFound is an absence
// result only; callers must preserve the native intent when its outcome is
// otherwise ambiguous.
func (s *Session) BotGroupRegistrationReceipt(ctx context.Context, installation, operation id.ID) (BotGroupRegistrationReceipt, error) {
	inst, err := pathID(installation)
	if err != nil {
		return BotGroupRegistrationReceipt{}, err
	}
	op, err := pathID(operation)
	if err != nil {
		return BotGroupRegistrationReceipt{}, err
	}
	raw, _, err := s.do(ctx, http.MethodGet, "bot-runtime/installations/"+inst+"/groups/"+op, nil, nil, MaxSmallBody, false)
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound && apiErr.Code == "E_NOT_FOUND" {
			return BotGroupRegistrationReceipt{}, ErrRegistrationReceiptNotFound
		}
		return BotGroupRegistrationReceipt{}, err
	}
	return decodeBotGroupRegistrationReceipt(raw, operation, nil, nil, nil)
}

type registrationGroup struct {
	id         id.ID
	bodyFields int
}

func registrationBodyGroup(body []byte) (registrationGroup, error) {
	var fields []any
	if len(body) > maxBotGroupRegistrationBody || cborx.Unmarshal(body, &fields) != nil || len(fields) != 4 && len(fields) != 5 {
		return registrationGroup{}, ErrProtocol
	}
	group, ok := asID(fields[0])
	if !ok {
		return registrationGroup{}, ErrProtocol
	}
	for _, field := range fields[1:] {
		if _, ok := asBytes(field, -1); !ok {
			return registrationGroup{}, ErrProtocol
		}
	}
	if len(fields) == 5 {
		welcome, ok := asBytes(fields[4], -1)
		if !ok || len(welcome) == 0 {
			return registrationGroup{}, ErrProtocol
		}
	}
	return registrationGroup{id: group, bodyFields: len(fields)}, nil
}

func decodeBotGroupRegistrationReceipt(raw []byte, operation id.ID, group *id.ID, body []byte, expectedGeneration *uint64) (BotGroupRegistrationReceipt, error) {
	var out BotGroupRegistrationReceipt
	a, err := decode(raw, 6)
	if err != nil {
		return out, unknown()
	}
	if out.OperationID, _ = asID(a[0]); out.OperationID.IsZero() || !equalID(out.OperationID, operation) {
		return BotGroupRegistrationReceipt{}, unknown()
	}
	if out.GroupID, _ = asID(a[1]); out.GroupID.IsZero() || group != nil && !equalID(out.GroupID, *group) {
		return BotGroupRegistrationReceipt{}, unknown()
	}
	hash, ok := asBytes(a[2], sha256.Size)
	if !ok {
		return BotGroupRegistrationReceipt{}, unknown()
	}
	copy(out.RegistrationSHA256[:], hash)
	if body != nil && out.RegistrationSHA256 != botGroupRegistrationHash(body) {
		return BotGroupRegistrationReceipt{}, unknown()
	}
	if out.AcceptedGeneration, ok = asUint(a[3]); !ok || out.AcceptedGeneration > maxStoredSequence || expectedGeneration != nil && out.AcceptedGeneration != *expectedGeneration {
		return BotGroupRegistrationReceipt{}, unknown()
	}
	if out.InitialEpoch, ok = asUint(a[4]); !ok || out.InitialEpoch > maxStoredSequence {
		return BotGroupRegistrationReceipt{}, unknown()
	}
	if out.NextSequence, ok = asUint(a[5]); !ok || out.NextSequence != 1 {
		return BotGroupRegistrationReceipt{}, unknown()
	}
	return out, nil
}

func botGroupRegistrationHash(body []byte) [32]byte {
	h := sha256.New()
	_, _ = h.Write([]byte("dilla bot group registration v1\x00"))
	_, _ = h.Write(body)
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

func (s *Session) GroupPage(ctx context.Context, group id.ID, kind string, from, limit uint64) (Page, error) {
	return s.groupPageAt(ctx, "groups/", group, kind, from, limit)
}

func (s *Session) BotInteractionGroupPage(ctx context.Context, installation, group id.ID, kind string, from, limit uint64) (Page, error) {
	inst, err := pathID(installation)
	if err != nil {
		return Page{}, err
	}
	return s.groupPageAt(ctx, "bot-runtime/installations/"+inst+"/interaction-groups/", group, kind, from, limit)
}

func (s *Session) groupPageAt(ctx context.Context, base string, group id.ID, kind string, from, limit uint64) (Page, error) {
	var out Page
	if kind != "messages" && kind != "handshakes" {
		return out, ErrInvalidConfig
	}
	g, e := pathID(group)
	if e != nil {
		return out, e
	}
	if limit == 0 || limit > 256 || from > maxStoredSequence {
		return out, ErrInvalidConfig
	}
	q := url.Values{}
	qnum(q, "from", from)
	qnum(q, "limit", limit)
	raw, _, e := s.do(ctx, http.MethodGet, base+g+"/"+kind, q, nil, MaxBody, false)
	if e != nil {
		return out, e
	}
	a, e := decode(raw, -1)
	if e != nil {
		return out, e
	}
	if uint64(len(a)) > limit {
		return out, ErrProtocol
	}
	out.Rows = make([][]any, len(a))
	var prev uint64
	for i, row := range a {
		v, ok := row.([]any)
		want := 9
		if kind == "handshakes" {
			want = 5
		}
		if !ok || len(v) != want {
			return Page{}, ErrProtocol
		}
		seq, ok := asUint(v[0])
		if !ok || seq <= prev || seq < from || seq > maxStoredSequence {
			return Page{}, ErrProtocol
		}
		if kind == "messages" {
			if _, ok = asUint(v[1]); !ok {
				return Page{}, ErrProtocol
			}
			if _, ok = asID(v[2]); !ok {
				return Page{}, ErrProtocol
			}
			if v[3] != nil {
				if _, ok = asBytes(v[3], -1); !ok {
					return Page{}, ErrProtocol
				}
			}
			if v[4] != nil {
				if _, ok = asBytes(v[4], -1); !ok {
					return Page{}, ErrProtocol
				}
			}
			if _, ok = asBytes(v[5], 32); !ok {
				return Page{}, ErrProtocol
			}
			if _, ok = asUint(v[6]); !ok {
				return Page{}, ErrProtocol
			}
			if n, yes := asUint(v[7]); !yes || n > 1 {
				return Page{}, ErrProtocol
			}
			if v[8] != nil {
				if n, yes := asUint(v[8]); !yes || n > 1 {
					return Page{}, ErrProtocol
				}
			}
		} else {
			if _, ok = asUint(v[1]); !ok {
				return Page{}, ErrProtocol
			}
			if n, yes := asUint(v[2]); !yes || n > 2 {
				return Page{}, ErrProtocol
			}
			if v[3] != nil {
				if n, yes := asUint(v[3]); !yes || n > 1<<32-1 {
					return Page{}, ErrProtocol
				}
			}
			if _, ok = asBytes(v[4], -1); !ok {
				return Page{}, ErrProtocol
			}
		}
		prev = seq
		out.Rows[i] = v
	}
	out.Next = from
	if len(a) > 0 {
		if prev == ^uint64(0) {
			return Page{}, ErrProtocol
		}
		out.Next = prev + 1
	}
	return out, nil
}

func (s *Session) AllGroupPages(ctx context.Context, group id.ID, kind string, from, limit uint64) ([][]any, error) {
	if limit == 0 {
		return nil, ErrInvalidConfig
	}
	ctx, cancel := s.c.aggregateContext(ctx)
	defer cancel()
	var all [][]any
	cursor := from
	for {
		page, err := s.GroupPage(ctx, group, kind, cursor, limit)
		if err != nil {
			return nil, err
		}
		if len(all)+len(page.Rows) > MaxAggregateRows {
			return nil, ErrPageLimit
		}
		all = append(all, page.Rows...)
		if uint64(len(page.Rows)) < limit {
			return all, nil
		}
		if len(page.Rows) == 0 || page.Next <= cursor {
			return nil, ErrProtocol
		}
		if page.Next > maxStoredSequence {
			return all, nil
		}
		cursor = page.Next
	}
}
func (s *Session) Proposals(ctx context.Context, group id.ID) ([][]any, error) {
	g, e := pathID(group)
	if e != nil {
		return nil, e
	}
	raw, _, e := s.do(ctx, http.MethodGet, "groups/"+g+"/proposals", nil, nil, MaxBody, false)
	if e != nil {
		return nil, e
	}
	a, e := decode(raw, -1)
	if e != nil {
		return nil, e
	}
	out := make([][]any, len(a))
	refs := map[string]bool{}
	for i, row := range a {
		v, ok := row.([]any)
		if !ok || len(v) != 5 {
			return nil, ErrProtocol
		}
		ref, ok := asBytes(v[0], -1)
		if !ok || len(ref) == 0 || refs[string(ref)] {
			return nil, ErrProtocol
		}
		refs[string(ref)] = true
		out[i] = v
	}
	return out, nil
}

// HealStatus reports whether this server generation needs the client's post-restore snapshot.
func (s *Session) HealStatus(ctx context.Context, group id.ID) (HealStatus, error) {
	g, err := pathID(group)
	if err != nil {
		return HealStatus{}, err
	}
	return s.healStatusAt(ctx, "groups/"+g+"/heal")
}

func (s *Session) healStatusAt(ctx context.Context, path string) (HealStatus, error) {
	var out HealStatus
	raw, _, err := s.do(ctx, http.MethodGet, path, nil, nil, MaxSmallBody, false)
	if err != nil {
		return out, err
	}
	a, err := decode(raw, 4)
	if err != nil {
		return out, err
	}
	values := []*uint64{&out.Epoch, &out.NextSequence, &out.Generation, &out.NeedFromSequence}
	for i, v := range values {
		n, ok := asUint(a[i])
		if !ok {
			return HealStatus{}, ErrProtocol
		}
		*v = n
	}
	return out, nil
}

// BotInteractionHealStatus reports whether a restored private interaction group
// needs a member-authenticated MLS heal. The scoped route returns the ordinary
// heal cursor plus an explicit epoch-unknown flag.
func (s *Session) BotInteractionHealStatus(ctx context.Context, installation, group id.ID) (InteractionHealStatus, error) {
	path, err := botInteractionHealPath(installation, group)
	if err != nil {
		return InteractionHealStatus{}, err
	}
	raw, _, err := s.do(ctx, http.MethodGet, path, nil, nil, MaxSmallBody, false)
	if err != nil {
		return InteractionHealStatus{}, err
	}
	a, err := decode(raw, 5)
	if err != nil {
		return InteractionHealStatus{}, err
	}
	var out InteractionHealStatus
	values := []*uint64{&out.Epoch, &out.NextSequence, &out.Generation, &out.NeedFromSequence}
	for i, value := range values {
		n, ok := asUint(a[i])
		if !ok {
			return InteractionHealStatus{}, ErrProtocol
		}
		*value = n
	}
	needsHeal, ok := asUint(a[4])
	if !ok || needsHeal > 1 {
		return InteractionHealStatus{}, ErrProtocol
	}
	out.NeedsHeal = needsHeal == 1
	return out, nil
}

func botInteractionHealPath(installation, group id.ID) (string, error) {
	inst, err := pathID(installation)
	if err != nil {
		return "", err
	}
	g, err := pathID(group)
	if err != nil {
		return "", err
	}
	return "bot-runtime/installations/" + inst + "/interaction-groups/" + g + "/heal", nil
}

// Heal uploads a client-authenticated snapshot and its bounded handshake tail after a restore.
// Each tail row is [sequence, epoch, kind, sender-or-null, blob].
func (s *Session) Heal(ctx context.Context, group id.ID, groupInfo []byte, tail [][]any, ratchetTree []byte) (HealResult, error) {
	g, err := pathID(group)
	if err != nil {
		return HealResult{}, err
	}
	return s.healAt(ctx, "groups/"+g+"/heal", groupInfo, tail, ratchetTree)
}

func (s *Session) BotInteractionHeal(ctx context.Context, installation, group id.ID, groupInfo []byte, tail [][]any, ratchetTree []byte) (HealResult, error) {
	path, err := botInteractionHealPath(installation, group)
	if err != nil {
		return HealResult{}, err
	}
	return s.healAt(ctx, path, groupInfo, tail, ratchetTree)
}

func (s *Session) healAt(ctx context.Context, path string, groupInfo []byte, tail [][]any, ratchetTree []byte) (HealResult, error) {
	var out HealResult
	if len(groupInfo) == 0 || len(tail) > 64 {
		return out, ErrInvalidConfig
	}
	if tail == nil {
		tail = make([][]any, 0)
	}
	for _, row := range tail {
		if len(row) != 5 {
			return out, ErrInvalidConfig
		}
		if _, ok := asUint(row[0]); !ok {
			return out, ErrInvalidConfig
		}
		if _, ok := asUint(row[1]); !ok {
			return out, ErrInvalidConfig
		}
		kind, ok := asUint(row[2])
		if !ok || kind > 255 {
			return out, ErrInvalidConfig
		}
		if row[3] != nil {
			sender, ok := asUint(row[3])
			if !ok || sender > 1<<32-1 {
				return out, ErrInvalidConfig
			}
		}
		blob, ok := asBytes(row[4], -1)
		if !ok || len(blob) == 0 {
			return out, ErrInvalidConfig
		}
	}
	b, err := encode([]any{groupInfo, tail, nullableBytes(ratchetTree)})
	if err != nil {
		return out, err
	}
	raw, _, err := s.do(ctx, http.MethodPost, path, nil, b, MaxDSBody, true)
	if err != nil {
		return out, err
	}
	a, err := decode(raw, 2)
	if err != nil {
		return out, unknown()
	}
	if out.Epoch, _ = asUint(a[0]); a[0] == nil {
		return HealResult{}, unknown()
	}
	if out.NextSequence, _ = asUint(a[1]); a[1] == nil {
		return HealResult{}, unknown()
	}
	if _, ok := asUint(a[0]); !ok {
		return HealResult{}, unknown()
	}
	if _, ok := asUint(a[1]); !ok {
		return HealResult{}, unknown()
	}
	return out, nil
}

// ForkReport records an authenticated divergence report. It is accepted with HTTP 202 and [];
// callers must not retry when the outcome is unknown.
func (s *Session) ForkReport(ctx context.Context, group id.ID, epoch, sequence uint64, reason string) error {
	if reason == "" {
		return ErrInvalidConfig
	}
	g, err := pathID(group)
	if err != nil {
		return err
	}
	b, err := encode([]any{epoch, sequence, reason})
	if err != nil {
		return err
	}
	raw, _, err := s.do(ctx, http.MethodPost, "groups/"+g+"/fork-report", nil, b, MaxSmallBody, true)
	if err != nil {
		return err
	}
	a, err := decode(raw, 0)
	if err != nil {
		return unknown()
	}
	_ = a
	return nil
}

func nullableBytes(b []byte) any {
	if b == nil {
		return nil
	}
	return b
}
func (s *Session) Commit(ctx context.Context, group id.ID, epoch uint64, commit, info []byte, welcomes [][]any, tree []byte) (SequenceResult, error) {
	if len(commit) == 0 || len(info) == 0 || len(welcomes) > 256 {
		return SequenceResult{}, ErrInvalidConfig
	}
	seen := make(map[id.ID]bool, len(welcomes))
	for _, w := range welcomes {
		if len(w) != 2 {
			return SequenceResult{}, ErrInvalidConfig
		}
		device, ok := w[0].(id.ID)
		if !ok || device.IsZero() {
			return SequenceResult{}, ErrInvalidConfig
		}
		if seen[device] {
			return SequenceResult{}, ErrInvalidConfig
		}
		seen[device] = true
		if _, ok := w[1].([]byte); !ok {
			return SequenceResult{}, ErrInvalidConfig
		}
	}
	return s.groupMutation(ctx, group, "commit", []any{epoch, commit, info, welcomes, tree}, true)
}
func (s *Session) Proposal(ctx context.Context, group id.ID, epoch uint64, proposal []byte) (uint64, error) {
	if len(proposal) == 0 {
		return 0, ErrInvalidConfig
	}
	res, e := s.groupMutation(ctx, group, "proposal", []any{epoch, proposal}, false)
	return res.Sequence, e
}
func (s *Session) Resync(ctx context.Context, group id.ID, commit, info []byte) (SequenceResult, error) {
	if len(commit) == 0 || len(info) == 0 {
		return SequenceResult{}, ErrInvalidConfig
	}
	return s.groupMutation(ctx, group, "resync", []any{commit, info}, true)
}

func (s *Session) SendMessage(ctx context.Context, group id.ID, epoch uint64, ciphertext []byte, class uint64) (MessageResult, error) {
	var out MessageResult
	if len(ciphertext) == 0 || class > 1 {
		return out, ErrInvalidConfig
	}
	g, e := pathID(group)
	if e != nil {
		return out, e
	}
	b, e := encode([]any{epoch, ciphertext, class})
	if e != nil {
		return out, e
	}
	raw, _, e := s.do(ctx, http.MethodPost, "groups/"+g+"/message", nil, b, MaxDSBody, true)
	if e != nil {
		return out, e
	}
	a, e := decode(raw, 3)
	if e != nil {
		return out, unknown()
	}
	var ok bool
	if out.Sequence, ok = asUint(a[0]); !ok {
		return MessageResult{}, unknown()
	}
	if out.FrankingTag, ok = asBytes(a[1], 32); !ok {
		return MessageResult{}, unknown()
	}
	if out.ReceivedAt, ok = asUint(a[2]); !ok {
		return MessageResult{}, unknown()
	}
	return out, nil
}
func (s *Session) groupMutation(ctx context.Context, group id.ID, action string, payload []any, two bool) (SequenceResult, error) {
	var out SequenceResult
	g, e := pathID(group)
	if e != nil {
		return out, e
	}
	b, e := encode(payload)
	if e != nil {
		return out, e
	}
	bodyLimit := int64(MaxDSBody)
	if action == "commit" {
		bodyLimit = MaxCommitBody
	}
	raw, _, e := s.do(ctx, http.MethodPost, "groups/"+g+"/"+action, nil, b, bodyLimit, true)
	if e != nil {
		return out, e
	}
	n := 2
	if !two {
		n = 1
	}
	a, e := decode(raw, n)
	if e != nil {
		return out, unknown()
	}
	var ok bool
	if out.Sequence, ok = asUint(a[0]); !ok {
		return SequenceResult{}, unknown()
	}
	if two {
		if out.Epoch, ok = asUint(a[1]); !ok {
			return SequenceResult{}, unknown()
		}
	}
	return out, nil
}
func (s *Session) AdvanceCursor(ctx context.Context, group id.ID, sequence, epoch uint64) error {
	return s.advanceCursorAt(ctx, "groups/", group, sequence, epoch)
}

func (s *Session) AdvanceBotInteractionGroupCursor(ctx context.Context, installation, group id.ID, sequence, epoch uint64) error {
	inst, e := pathID(installation)
	if e != nil {
		return e
	}
	return s.advanceCursorAt(ctx, "bot-runtime/installations/"+inst+"/interaction-groups/", group, sequence, epoch)
}

func (s *Session) advanceCursorAt(ctx context.Context, base string, group id.ID, sequence, epoch uint64) error {
	g, e := pathID(group)
	if e != nil {
		return e
	}
	if sequence > maxStoredSequence {
		return ErrInvalidConfig
	}
	b, e := encode([]any{sequence, epoch})
	if e != nil {
		return e
	}
	_, _, e = s.do(ctx, http.MethodPost, base+g+"/cursor", nil, b, MaxSmallBody, true)
	return e
}

func (s *Session) Communities(ctx context.Context) ([]Community, error) {
	raw, _, e := s.do(ctx, http.MethodGet, "communities", nil, nil, MaxBody, false)
	if e != nil {
		return nil, e
	}
	a, e := decode(raw, -1)
	if e != nil {
		return nil, e
	}
	out := make([]Community, len(a))
	seen := map[id.ID]bool{}
	for i, row := range a {
		v, ok := row.([]any)
		if !ok || len(v) != 4 {
			return nil, ErrProtocol
		}
		cid, ok := asID(v[0])
		if !ok || seen[cid] {
			return nil, ErrProtocol
		}
		name, ok := v[1].(string)
		if !ok || name == "" {
			return nil, ErrProtocol
		}
		owner, ok := asID(v[2])
		if !ok {
			return nil, ErrProtocol
		}
		policy, ok := asUint(v[3])
		if !ok {
			return nil, ErrProtocol
		}
		seen[cid] = true
		out[i] = Community{ID: cid, Name: name, OwnerID: owner, PolicyVersion: policy}
	}
	return out, nil
}
func (s *Session) Channels(ctx context.Context, community id.ID) ([]Channel, error) {
	c, e := pathID(community)
	if e != nil {
		return nil, e
	}
	raw, _, e := s.do(ctx, http.MethodGet, "communities/"+c+"/channels", nil, nil, MaxBody, false)
	if e != nil {
		return nil, e
	}
	a, e := decode(raw, -1)
	if e != nil {
		return nil, e
	}
	out := make([]Channel, len(a))
	seen := map[id.ID]bool{}
	for i, row := range a {
		v, ok := row.([]any)
		if !ok || len(v) != 11 {
			return nil, ErrProtocol
		}
		cid, ok := asID(v[0])
		if !ok || seen[cid] {
			return nil, ErrProtocol
		}
		ch := Channel{ID: cid, CommunityID: community}
		if ch.Kind, ok = asUint(v[1]); !ok {
			return nil, ErrProtocol
		}
		if ch.Mode, ok = asUint(v[2]); !ok {
			return nil, ErrProtocol
		}
		if ch.Visibility, ok = asUint(v[3]); !ok {
			return nil, ErrProtocol
		}
		if ch.ParentID, ok = nullableID(v[4]); !ok {
			return nil, ErrProtocol
		}
		if ch.Name, ok = v[5].(string); !ok {
			return nil, ErrProtocol
		}
		if ch.Topic, ok = v[6].(string); !ok {
			return nil, ErrProtocol
		}
		if ch.Position, ok = asUint(v[7]); !ok {
			return nil, ErrProtocol
		}
		if ch.Slowmode, ok = asUint(v[8]); !ok {
			return nil, ErrProtocol
		}
		if ch.Sequence, ok = asUint(v[9]); !ok {
			return nil, ErrProtocol
		}
		if ch.TextGroupID, ok = nullableID(v[10]); !ok {
			return nil, ErrProtocol
		}
		seen[cid] = true
		out[i] = ch
	}
	return out, nil
}

// ChannelPermissions returns the session user's effective permission bits in
// one channel, after community roles and channel overwrites are resolved.
func (s *Session) ChannelPermissions(ctx context.Context, channelID id.ID) (uint64, error) {
	c, err := pathID(channelID)
	if err != nil {
		return 0, err
	}
	raw, _, err := s.do(ctx, http.MethodGet, "channels/"+c+"/permissions", nil, nil, MaxSmallBody, false)
	if err != nil {
		return 0, err
	}
	var bits uint64
	if err := cborx.Unmarshal(raw, &bits); err != nil {
		return 0, ErrProtocol
	}
	return bits, nil
}

func nullableID(v any) (*id.ID, bool) {
	if v == nil {
		return nil, true
	}
	x, ok := asID(v)
	return &x, ok
}
