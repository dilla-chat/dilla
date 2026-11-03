package protocol

// Typed codecs for managed-bot KeyPackage publication and native helper intents.
// They deliberately sit beside, rather than inside, the generic helper envelope.
import (
	"bytes"
	"math"

	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
)

const MaxKeyPackageUploadBody = 1_047_296 // 1 MiB - 1280 bytes

type KeyPackagePublication struct {
	OperationID        id.ID
	ExpectedGeneration uint64
	UploadBody         []byte
}
type KeyPackagePublicationReceipt struct {
	ExpectedGeneration uint64
	UploadSHA256       [32]byte
	Count              uint64
}
type KeyPackageIntent struct {
	OperationID        id.ID
	Count              uint64
	LastResort         uint64
	ExpectedGeneration uint64
}
type KeyPackageIntentResult struct {
	UploadBody     []byte
	PackageRefs    [][32]byte
	GreatestExpiry uint64
}
type KeyPackageResolution struct {
	OperationID       id.ID
	PackageRefs       [][32]byte
	Outcome           uint64
	Now               uint64
	CurrentGeneration uint64
}

func kpUint(v any) (uint64, bool)  { n, ok := v.(uint64); return n, ok }
func kpBytes(v any) ([]byte, bool) { b, ok := v.([]byte); return b, ok }
func kpID(v any) (id.ID, bool) {
	var x id.ID
	b, ok := kpBytes(v)
	if !ok || len(b) != 16 {
		return x, false
	}
	copy(x[:], b)
	return x, !x.IsZero()
}
func kpRefs(v any, minimum, maximum int) ([][32]byte, bool) {
	a, ok := v.([]any)
	if !ok || len(a) < minimum || len(a) > maximum {
		return nil, false
	}
	out := make([][32]byte, 0, len(a))
	seen := map[[32]byte]bool{}
	for _, x := range a {
		b, ok := kpBytes(x)
		if !ok || len(b) != 32 {
			return nil, false
		}
		var r [32]byte
		copy(r[:], b)
		if seen[r] {
			return nil, false
		}
		seen[r] = true
		out = append(out, r)
	}
	return out, true
}
func parseKP(raw []byte, n int) ([]any, error) {
	if len(raw) == 0 || len(raw) > MaxHelperPayload || cborx.ExpectMajor(raw, cborx.MajorArray) != nil {
		return nil, ErrInvalid
	}
	var a []any
	if err := cborx.Unmarshal(raw, &a); err != nil || len(a) != n {
		return nil, ErrInvalid
	}
	return a, nil
}
func canonicalUpload(raw []byte) (int, error) {
	if len(raw) > MaxKeyPackageUploadBody {
		return 0, ErrLimit
	}
	a, err := parseKP(raw, 2)
	if err != nil {
		return 0, err
	}
	pkgs, ok := a[0].([]any)
	if !ok || len(pkgs) < 1 || len(pkgs) > 32 {
		return 0, ErrInvalid
	}
	for _, v := range pkgs {
		b, ok := kpBytes(v)
		if !ok || len(b) == 0 {
			return 0, ErrInvalid
		}
	}
	count := len(pkgs)
	if a[1] != nil {
		b, ok := kpBytes(a[1])
		if !ok || len(b) == 0 {
			return 0, ErrInvalid
		}
		count++
	}
	encoded, e := cborx.Marshal(a)
	if e != nil || !bytes.Equal(encoded, raw) {
		return 0, ErrInvalid
	}
	return count, nil
}
func DecodeKeyPackagePublication(raw []byte) (KeyPackagePublication, error) {
	var z KeyPackagePublication
	a, e := parseKP(raw, 3)
	if e != nil {
		return z, e
	}
	idv, ok := kpID(a[0])
	gen, gok := kpUint(a[1])
	body, bok := kpBytes(a[2])
	if !ok || !gok || gen > math.MaxInt64 || !bok {
		return z, ErrInvalid
	}
	if _, e = canonicalUpload(body); e != nil {
		return z, e
	}
	return KeyPackagePublication{idv, gen, append([]byte(nil), body...)}, nil
}
func EncodeKeyPackagePublication(v KeyPackagePublication) ([]byte, error) {
	if v.OperationID.IsZero() || v.ExpectedGeneration > math.MaxInt64 {
		return nil, ErrInvalid
	}
	if _, e := canonicalUpload(v.UploadBody); e != nil {
		return nil, e
	}
	return cborx.Marshal([]any{v.OperationID, v.ExpectedGeneration, v.UploadBody})
}
func DecodeKeyPackagePublicationReceipt(raw []byte) (KeyPackagePublicationReceipt, error) {
	var z KeyPackagePublicationReceipt
	a, e := parseKP(raw, 3)
	if e != nil {
		return z, e
	}
	gen, gok := kpUint(a[0])
	h, hok := kpBytes(a[1])
	count, cok := kpUint(a[2])
	if !gok || gen > math.MaxInt64 || !hok || len(h) != 32 || !cok || count < 1 || count > 33 {
		return z, ErrInvalid
	}
	z.ExpectedGeneration = gen
	copy(z.UploadSHA256[:], h)
	z.Count = count
	return z, nil
}
func EncodeKeyPackagePublicationReceipt(v KeyPackagePublicationReceipt) ([]byte, error) {
	if v.ExpectedGeneration > math.MaxInt64 || v.Count < 1 || v.Count > 33 {
		return nil, ErrInvalid
	}
	return cborx.Marshal([]any{v.ExpectedGeneration, v.UploadSHA256[:], v.Count})
}
func DecodeKeyPackageIntent(raw []byte) (KeyPackageIntent, error) {
	var z KeyPackageIntent
	a, e := parseKP(raw, 4)
	if e != nil {
		return z, e
	}
	op, ok := kpID(a[0])
	count, cok := kpUint(a[1])
	last, lok := kpUint(a[2])
	gen, gok := kpUint(a[3])
	if !ok || !cok || count < 1 || count > 32 || !lok || last > 1 || !gok || gen > math.MaxInt64 {
		return z, ErrInvalid
	}
	return KeyPackageIntent{op, count, last, gen}, nil
}
func EncodeKeyPackageIntent(v KeyPackageIntent) ([]byte, error) {
	if v.OperationID.IsZero() || v.Count < 1 || v.Count > 32 || v.LastResort > 1 || v.ExpectedGeneration > math.MaxInt64 {
		return nil, ErrInvalid
	}
	return cborx.Marshal([]any{v.OperationID, v.Count, v.LastResort, v.ExpectedGeneration})
}
func DecodeKeyPackageIntentResult(raw []byte) (KeyPackageIntentResult, error) {
	var z KeyPackageIntentResult
	a, e := parseKP(raw, 3)
	if e != nil {
		return z, e
	}
	body, bok := kpBytes(a[0])
	if !bok {
		return z, ErrInvalid
	}
	count, e := canonicalUpload(body)
	if e != nil {
		return z, e
	}
	refs, rok := kpRefs(a[1], 1, 33)
	expiry, xok := kpUint(a[2])
	if !rok || len(refs) != count || !xok || expiry > math.MaxInt64 {
		return z, ErrInvalid
	}
	return KeyPackageIntentResult{append([]byte(nil), body...), refs, expiry}, nil
}
func EncodeKeyPackageIntentResult(v KeyPackageIntentResult) ([]byte, error) {
	count, e := canonicalUpload(v.UploadBody)
	if e != nil {
		return nil, e
	}
	if len(v.PackageRefs) != count || !validKPRefs(v.PackageRefs, 1, 33) || v.GreatestExpiry > math.MaxInt64 {
		return nil, ErrInvalid
	}
	refs := make([]any, len(v.PackageRefs))
	for i := range v.PackageRefs {
		refs[i] = v.PackageRefs[i][:]
	}
	b, err := cborx.Marshal([]any{v.UploadBody, refs, v.GreatestExpiry})
	if err != nil || len(b) > MaxHelperPayload {
		return nil, ErrLimit
	}
	return b, nil
}
func DecodeKeyPackageResolution(raw []byte) (KeyPackageResolution, error) {
	var z KeyPackageResolution
	a, e := parseKP(raw, 5)
	if e != nil {
		return z, e
	}
	op, ok := kpID(a[0])
	refs, rok := kpRefs(a[1], 1, 33)
	out, o := kpUint(a[2])
	now, n := kpUint(a[3])
	gen, g := kpUint(a[4])
	if !ok || !rok || !o || out > 1 || !n || now > math.MaxInt64 || !g || gen > math.MaxInt64 {
		return z, ErrInvalid
	}
	return KeyPackageResolution{op, refs, out, now, gen}, nil
}
func EncodeKeyPackageResolution(v KeyPackageResolution) ([]byte, error) {
	if v.OperationID.IsZero() || !validKPRefs(v.PackageRefs, 1, 33) || v.Outcome > 1 || v.Now > math.MaxInt64 || v.CurrentGeneration > math.MaxInt64 {
		return nil, ErrInvalid
	}
	refs := make([]any, len(v.PackageRefs))
	for i := range v.PackageRefs {
		refs[i] = v.PackageRefs[i][:]
	}
	return cborx.Marshal([]any{v.OperationID, refs, v.Outcome, v.Now, v.CurrentGeneration})
}
func validKPRefs(a [][32]byte, minimum, maximum int) bool {
	if len(a) < minimum || len(a) > maximum {
		return false
	}
	seen := map[[32]byte]bool{}
	for _, x := range a {
		if seen[x] {
			return false
		}
		seen[x] = true
	}
	return true
}
