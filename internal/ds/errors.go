package ds

import (
	"errors"
	"fmt"
	"net/http"
)

// ErrAlreadyMember is what ProposeAdd's refusal of a device that already holds a current leaf of
// the group wraps. The refusal itself is an *Error (E_INVALID_REQUEST, 400) like every other; the
// sentinel lets a caller that only needs the device to be IN the group (the test host's
// POST /debug/admit, which a scenario's `join … via=welcome` uses to make sure the joiner is
// admitted) tell "already done" from a real refusal with errors.Is.
var ErrAlreadyMember = errors.New("ds: the device is already a member of the group")

// Error is one refusal from the delivery service, carrying exactly what protocol/02's error table
// says the body carries. The HTTP layer turns it into the CBOR array
// [code, detail, retry_after_ms, …extras] with the status below.
type Error struct {
	Code   string
	Detail string
	Rule   string // E_COMMIT_INVALID only: the invariant-4 clause that refused it
	Status int

	WinningCommit []byte   // E_COMMIT_CONFLICT
	Proposals     [][]byte // E_COMMIT_CONFLICT, E_COMMIT_REQUIRED
	RetryAfterMS  *uint64  // E_RATE_LIMITED, E_COMMIT_REQUIRED

	cause error // a sentinel errors.Is can match (ErrAlreadyMember); never on the wire
}

func (e *Error) Error() string {
	if e.Rule != "" {
		return fmt.Sprintf("%s (%s): %s", e.Code, e.Rule, e.Detail)
	}
	return e.Code + ": " + e.Detail
}

// Unwrap answers the sentinel the refusal carries, if any.
func (e *Error) Unwrap() error { return e.cause }

func errNotFound(what string) *Error {
	return &Error{Code: "E_NOT_FOUND", Detail: what, Status: http.StatusNotFound}
}

// errKeyPackagesExhausted is a 404 with its own code: the device is live and listed but has
// nothing to serve, which a client answers by waiting for it to refill, not by dropping it.
func errKeyPackagesExhausted() *Error {
	return &Error{Code: "E_KEYPACKAGES_EXHAUSTED", Detail: "key package", Status: http.StatusNotFound}
}

func errForbidden(detail string) *Error {
	return &Error{Code: "E_FORBIDDEN", Detail: detail, Status: http.StatusForbidden}
}

func errInvalid(detail string) *Error {
	return &Error{Code: "E_INVALID_REQUEST", Detail: detail, Status: http.StatusBadRequest}
}

func errBinding(detail string) *Error {
	return &Error{Code: "E_BINDING_INVALID", Detail: detail, Status: http.StatusBadRequest}
}

// errProvisionalOutsidePairing is §2.2 point 4's refusal: a provisional session reached beyond the
// one pairing group it was issued for.
func errProvisionalOutsidePairing(detail string) *Error {
	return &Error{
		Code:   "E_PROVISIONAL_OUTSIDE_PAIRING",
		Detail: detail,
		Status: http.StatusForbidden,
	}
}

func errModeReadable() *Error {
	return &Error{
		Code:   "E_MODE_READABLE",
		Detail: "text groups are not allowed for this channel",
		Status: http.StatusForbidden,
	}
}

// errRemovePending is invariant 6's refusal of a member's own Remove of a leaf the instance is
// already removing: a state conflict over a well-formed proposal, which the member reads as "I am
// being removed" — it withdraws the proposal and does not retry. Clients branch on the code, never
// on the detail.
func errRemovePending() *Error {
	return &Error{
		Code:   "E_REMOVE_PENDING",
		Detail: "a removal of this leaf is already pending",
		Status: http.StatusConflict,
	}
}

// errRemoveTarget is guard 1's refusal (resync.go guardResyncTarget): the device is the target of an
// outstanding instance Remove. It is invariant 6's code — the device is being removed — so a client
// waits until a member commits the Remove (or the group closes) and never re-adds itself before.
func errRemoveTarget() *Error {
	return &Error{
		Code:   "E_REMOVE_PENDING",
		Detail: "this device is the target of an outstanding Remove",
		Status: http.StatusConflict,
	}
}

func errGroupExists(detail string) *Error {
	return &Error{Code: "E_GROUP_EXISTS", Detail: detail, Status: http.StatusConflict}
}

// errCommitInvalid names the clause that refused the commit. `rule` is a stable string a client
// may log but must not branch on beyond the code.
func errCommitInvalid(rule, detail string) *Error {
	return &Error{
		Code:   "E_COMMIT_INVALID",
		Detail: detail,
		Rule:   rule,
		Status: http.StatusUnprocessableEntity,
	}
}

func errCommitConflict(winning []byte, proposals [][]byte) *Error {
	return &Error{
		Code:          "E_COMMIT_CONFLICT",
		Detail:        "another commit won this epoch",
		Status:        http.StatusConflict,
		WinningCommit: winning,
		Proposals:     proposals,
	}
}

func errCommitRequired(proposals [][]byte, retryAfterMS uint64) *Error {
	return &Error{
		Code:         "E_COMMIT_REQUIRED",
		Detail:       "outstanding delivery-service proposals must be committed first",
		Status:       http.StatusTooEarly,
		Proposals:    proposals,
		RetryAfterMS: &retryAfterMS,
	}
}

func errLeafNotCurrent() *Error {
	return &Error{
		Code:   "E_LEAF_NOT_CURRENT",
		Detail: "this device's leaf is not in the current tree",
		Status: http.StatusForbidden,
	}
}

func errCommitmentInvalid(n int) *Error {
	return &Error{
		Code:   "E_COMMITMENT_INVALID",
		Detail: fmt.Sprintf("authenticated_data is %d bytes, want exactly 32", n),
		Status: http.StatusUnprocessableEntity,
	}
}

func errTooLarge(n, limit int) *Error {
	return &Error{
		Code:   "E_TOO_LARGE",
		Detail: fmt.Sprintf("%d bytes of ciphertext, limit %d", n, limit),
		Status: http.StatusRequestEntityTooLarge,
	}
}

func errPruned(from, floor uint64) *Error {
	return &Error{
		Code:   "E_PRUNED",
		Detail: fmt.Sprintf("seq %d is older than the retention floor %d", from, floor),
		Status: http.StatusGone,
	}
}

func errNotUploader() *Error {
	return &Error{
		Code:   "E_NOT_UPLOADER",
		Detail: "only the uploading user may delete this message",
		Status: http.StatusForbidden,
	}
}
