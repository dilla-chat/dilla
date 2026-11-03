package mlswasi

import (
	"context"
	"fmt"

	"github.com/fxamacker/cbor/v2"

	"github.com/dilla-chat/dilla/internal/cborx"
)

// ABIError is the guest's error frame [1, code, detail]. The ABI never traps to
// report an error and never lets a Rust panic or an OpenMLS error type cross
// the boundary, so an ABIError is an ordinary, recoverable outcome.
type ABIError struct {
	Code   string
	Detail string
}

func (e *ABIError) Error() string { return "mlswasi: " + e.Code + ": " + e.Detail }

// encodeRequestVersion builds the CBOR request array [version, args...].
func encodeRequestVersion(version uint64, args ...any) ([]byte, error) {
	items := make([]any, 0, len(args)+1)
	items = append(items, version)
	items = append(items, args...)
	return cborx.Marshal(items)
}

// encodeRequest builds [ABIVersion, args...].
func encodeRequest(args ...any) ([]byte, error) {
	return encodeRequestVersion(ABIVersion, args...)
}

// responseElements decodes a response array, turning the error frame into an
// *ABIError and returning the success elements otherwise (element 0 is the
// status).
func responseElements(resp []byte) ([]cbor.RawMessage, error) {
	var elems []cbor.RawMessage
	if err := cborx.Unmarshal(resp, &elems); err != nil {
		return nil, fmt.Errorf("mlswasi: response: %w", err)
	}
	if len(elems) == 0 {
		return nil, fmt.Errorf("mlswasi: empty response array")
	}
	status, err := rawUint(elems[0])
	if err != nil {
		return nil, fmt.Errorf("mlswasi: response status: %w", err)
	}
	switch status {
	case 0:
		return elems, nil
	case 1:
		if len(elems) != 3 {
			return nil, fmt.Errorf("mlswasi: error frame has %d elements, want 3", len(elems))
		}
		code, err := rawText(elems[1])
		if err != nil {
			return nil, fmt.Errorf("mlswasi: error code: %w", err)
		}
		detail, err := rawText(elems[2])
		if err != nil {
			return nil, fmt.Errorf("mlswasi: error detail: %w", err)
		}
		return nil, &ABIError{Code: code, Detail: detail}
	default:
		return nil, fmt.Errorf("mlswasi: response status %d is neither 0 nor 1", status)
	}
}

// call encodes a request, runs it and decodes the response frame in one step.
func (i *Instance) call(ctx context.Context, export string, args ...any) ([]cbor.RawMessage, error) {
	req, err := encodeRequest(args...)
	if err != nil {
		return nil, fmt.Errorf("mlswasi: %s request: %w", export, err)
	}
	resp, err := i.Call(ctx, export, req)
	if err != nil {
		return nil, err
	}
	elems, err := responseElements(resp)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", export, err)
	}
	return elems, nil
}

func expectLen(elems []cbor.RawMessage, n int, what string) error {
	if len(elems) != n {
		return fmt.Errorf("mlswasi: %s response has %d elements, want %d", what, len(elems), n)
	}
	return nil
}

func isNull(raw cbor.RawMessage) bool { return len(raw) == 1 && raw[0] == 0xf6 }

func rawUint(raw cbor.RawMessage) (uint64, error) {
	if err := cborx.ExpectMajor(raw, cborx.MajorUint); err != nil {
		return 0, err
	}
	var v uint64
	if err := cborx.Unmarshal(raw, &v); err != nil {
		return 0, err
	}
	return v, nil
}

func rawUint32(raw cbor.RawMessage) (uint32, error) {
	v, err := rawUint(raw)
	if err != nil {
		return 0, err
	}
	if v > 0xffffffff {
		return 0, fmt.Errorf("mlswasi: %d does not fit in a uint32", v)
	}
	return uint32(v), nil
}

func rawOptUint32(raw cbor.RawMessage) (*uint32, error) {
	if isNull(raw) {
		return nil, nil
	}
	v, err := rawUint32(raw)
	if err != nil {
		return nil, err
	}
	return &v, nil
}

func rawBool(raw cbor.RawMessage) (bool, error) {
	// The deterministic-CBOR subset has no boolean, so the ABI carries flags as
	// uint 0 or 1.
	v, err := rawUint(raw)
	if err != nil {
		return false, err
	}
	if v > 1 {
		return false, fmt.Errorf("mlswasi: boolean field carries %d, want 0 or 1", v)
	}
	return v == 1, nil
}

func rawBytes(raw cbor.RawMessage) ([]byte, error) {
	if err := cborx.ExpectMajor(raw, cborx.MajorBytes); err != nil {
		return nil, err
	}
	var v []byte
	if err := cborx.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return v, nil
}

func rawOptBytes(raw cbor.RawMessage) ([]byte, error) {
	if isNull(raw) {
		return nil, nil
	}
	return rawBytes(raw)
}

func rawText(raw cbor.RawMessage) (string, error) {
	if err := cborx.ExpectMajor(raw, cborx.MajorText); err != nil {
		return "", err
	}
	var v string
	if err := cborx.Unmarshal(raw, &v); err != nil {
		return "", err
	}
	return v, nil
}

func rawArray(raw cbor.RawMessage) ([]cbor.RawMessage, error) {
	if err := cborx.ExpectMajor(raw, cborx.MajorArray); err != nil {
		return nil, err
	}
	var v []cbor.RawMessage
	if err := cborx.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return v, nil
}

// ABIInfo is the dilla_abi export's answer.
type ABIInfo struct {
	ABIVersion   uint64
	CoreVersion  string
	E2EEVersion  uint64
	MediaVersion uint64
	Ciphersuites []uint64
}

// ABI reports the guest's ABI, core and protocol versions.
func (r *Runtime) ABI(ctx context.Context) (ABIInfo, error) {
	inst, err := r.Acquire(ctx)
	if err != nil {
		return ABIInfo{}, err
	}
	defer inst.Release()

	elems, err := inst.call(ctx, "dilla_abi")
	if err != nil {
		return ABIInfo{}, err
	}
	if err := expectLen(elems, 6, "dilla_abi"); err != nil {
		return ABIInfo{}, err
	}
	var info ABIInfo
	if info.ABIVersion, err = rawUint(elems[1]); err != nil {
		return ABIInfo{}, err
	}
	if info.CoreVersion, err = rawText(elems[2]); err != nil {
		return ABIInfo{}, err
	}
	if info.E2EEVersion, err = rawUint(elems[3]); err != nil {
		return ABIInfo{}, err
	}
	if info.MediaVersion, err = rawUint(elems[4]); err != nil {
		return ABIInfo{}, err
	}
	suites, err := rawArray(elems[5])
	if err != nil {
		return ABIInfo{}, err
	}
	for _, s := range suites {
		v, err := rawUint(s)
		if err != nil {
			return ABIInfo{}, err
		}
		info.Ciphersuites = append(info.Ciphersuites, v)
	}
	return info, nil
}

// CaseReport is one checked field of one vector case.
type CaseReport struct {
	Case     string
	Field    string
	Expected string
	Actual   string
	OK       bool
}

// SuiteReport is one vector suite's result.
type SuiteReport struct {
	Name  string
	Cases []CaseReport
}

// VectorReport is the whole cross-target conformance run.
type VectorReport struct {
	Passed uint32
	Failed uint32
	Suites []SuiteReport
}

// OK reports whether every case passed.
func (v VectorReport) OK() bool { return v.Failed == 0 }

// VectorsCheck runs the protocol vectors inside the guest.
func (r *Runtime) VectorsCheck(ctx context.Context) (VectorReport, error) {
	inst, err := r.Acquire(ctx)
	if err != nil {
		return VectorReport{}, err
	}
	defer inst.Release()

	elems, err := inst.call(ctx, "vectors_check")
	if err != nil {
		return VectorReport{}, err
	}
	if err := expectLen(elems, 4, "vectors_check"); err != nil {
		return VectorReport{}, err
	}
	var report VectorReport
	passed, err := rawUint32(elems[1])
	if err != nil {
		return VectorReport{}, err
	}
	failed, err := rawUint32(elems[2])
	if err != nil {
		return VectorReport{}, err
	}
	report.Passed, report.Failed = passed, failed

	suites, err := rawArray(elems[3])
	if err != nil {
		return VectorReport{}, err
	}
	for _, rawSuite := range suites {
		pair, err := rawArray(rawSuite)
		if err != nil {
			return VectorReport{}, err
		}
		if err := expectLen(pair, 2, "vectors_check suite"); err != nil {
			return VectorReport{}, err
		}
		suite := SuiteReport{}
		if suite.Name, err = rawText(pair[0]); err != nil {
			return VectorReport{}, err
		}
		cases, err := rawArray(pair[1])
		if err != nil {
			return VectorReport{}, err
		}
		for _, rawCase := range cases {
			fields, err := rawArray(rawCase)
			if err != nil {
				return VectorReport{}, err
			}
			if err := expectLen(fields, 5, "vectors_check case"); err != nil {
				return VectorReport{}, err
			}
			var c CaseReport
			if c.Case, err = rawText(fields[0]); err != nil {
				return VectorReport{}, err
			}
			if c.Field, err = rawText(fields[1]); err != nil {
				return VectorReport{}, err
			}
			if c.OK, err = rawBool(fields[2]); err != nil {
				return VectorReport{}, fmt.Errorf("suite %q case %q: %w", suite.Name, c.Case, err)
			}
			if c.Expected, err = rawText(fields[3]); err != nil {
				return VectorReport{}, err
			}
			if c.Actual, err = rawText(fields[4]); err != nil {
				return VectorReport{}, err
			}
			suite.Cases = append(suite.Cases, c)
		}
		report.Suites = append(report.Suites, suite)
	}
	return report, nil
}
