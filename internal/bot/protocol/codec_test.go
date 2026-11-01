package protocol

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"

	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/id"
)

func fixtureID(n byte) id.ID {
	var v id.ID
	v[15] = n
	return v
}

func mustCBOR(t *testing.T, value any) []byte {
	t.Helper()
	body, err := cborx.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

type vectorCorpus struct {
	Version int          `json:"version"`
	Cases   []vectorCase `json:"cases"`
	Rejects []vectorCase `json:"rejects"`
}
type vectorCase struct {
	Name                string         `json:"name"`
	Kind                string         `json:"kind"`
	Hex                 string         `json:"hex"`
	Body                string         `json:"body"`
	PreimageHex         string         `json:"preimage_hex"`
	RegistrationBodyHex string         `json:"registration_body_hex"`
	RegistrationSHA256  string         `json:"registration_sha256"`
	Context             *vectorContext `json:"context"`
}
type vectorContext struct {
	Community   string `json:"community"`
	Group       string `json:"group"`
	ActorUser   string `json:"actor_user"`
	ActorDevice string `json:"actor_device"`
	Now         string `json:"now"`
}

func TestSharedBotVectors(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "protocol", "vectors", "bot.json"))
	if err != nil {
		t.Fatal(err)
	}
	var corpus vectorCorpus
	if err := json.Unmarshal(data, &corpus); err != nil {
		t.Fatal(err)
	}
	if corpus.Version != 1 || len(corpus.Cases) == 0 || len(corpus.Rejects) == 0 {
		t.Fatalf("invalid vector corpus metadata: version=%d cases=%d rejects=%d", corpus.Version, len(corpus.Cases), len(corpus.Rejects))
	}
	for _, tc := range corpus.Cases {
		t.Run("accept/"+tc.Name, func(t *testing.T) {
			raw, err := hex.DecodeString(tc.Hex)
			if err != nil {
				t.Fatal(err)
			}
			canonical := roundTripVector(t, tc, raw)
			if !bytes.Equal(canonical, raw) {
				t.Fatalf("roundtrip bytes differ: %x != %x", canonical, raw)
			}
			if tc.PreimageHex != "" {
				proof, err := DecodeRemovalProof(raw)
				if err != nil {
					t.Fatal(err)
				}
				pre, err := RemovalPreimage(proof)
				if err != nil {
					t.Fatal(err)
				}
				want, _ := hex.DecodeString(tc.PreimageHex)
				if !bytes.Equal(pre, want) {
					t.Fatalf("preimage differs: %x != %x", pre, want)
				}
			}
		})
	}
	for _, tc := range corpus.Rejects {
		t.Run("reject/"+tc.Name, func(t *testing.T) {
			raw, err := hex.DecodeString(tc.Hex)
			if err != nil {
				t.Fatal(err)
			}
			if tc.Kind == "helper_request" && tc.Name == "helper malformed length prefix" {
				if _, err := DecodeHelperRequestFrame(raw); err == nil {
					t.Fatal("accepted malformed frame")
				}
				return
			}
			if tc.Body != "" {
				_, err = decodeBody(tc.Body)
			} else {
				_, err = roundTripVectorErr(tc, raw)
			}
			if err == nil && tc.Context != nil {
				err = validateVectorContext(tc, raw)
			}
			if err == nil {
				t.Fatal("accepted reject vector")
			}
		})
	}
}

func TestAnnouncementDeleteRequiresScheduleRevision(t *testing.T) {
	revision, expiry := uint64(120), uint64(120)
	req := Request{RequestID: fixtureID(53), CommunityID: fixtureID(34), Operation: OpAnnouncementsDelete,
		Revision: &revision, Expires: &expiry, Args: []any{fixtureID(102), uint64(1)}}
	raw, err := EncodeRequestCBOR(req)
	if err != nil {
		t.Fatalf("encode delete: %v", err)
	}
	if _, err := DecodeRequestCBOR(raw); err != nil {
		t.Fatalf("decode valid delete %x: %v", raw, err)
	}
	vector, err := hex.DecodeString("880150353535353535353535353535353535355022222222222222222222222222222222f618341878187882506666666666666666666666666666666601")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeRequestCBOR(vector); err != nil {
		t.Fatalf("decode shared delete vector: %v", err)
	}
	req.Args = []any{fixtureID(102)}
	if _, err := EncodeRequestCBOR(req); err == nil {
		t.Fatal("delete without schedule revision accepted")
	}
}

func TestAutoModerationConfigOperationShapes(t *testing.T) {
	community := fixtureID(34)
	requestID := fixtureID(80)
	document, err := cborx.Marshal([]any{uint64(1), uint64(0), "log", "log", "log", uint64(600000), []any{}})
	if err != nil {
		t.Fatal(err)
	}
	get := Request{RequestID: requestID, CommunityID: community, Operation: OpAutoModerationGet}
	if _, err := EncodeRequestCBOR(get); err != nil {
		t.Fatalf("encode get: %v", err)
	}
	revision, expires := uint64(4), uint64(120)
	put := Request{RequestID: requestID, CommunityID: community, Operation: OpAutoModerationPut, Revision: &revision, Expires: &expires, Args: []any{"grant-token", document}}
	if _, err := EncodeRequestCBOR(put); err != nil {
		t.Fatalf("encode put: %v", err)
	}
	for name, bad := range map[string]Request{
		"get args":             {RequestID: requestID, CommunityID: community, Operation: OpAutoModerationGet, Args: []any{uint64(0)}},
		"put without revision": {RequestID: requestID, CommunityID: community, Operation: OpAutoModerationPut, Args: []any{"grant-token", document}},
		"missing grant":        {RequestID: requestID, CommunityID: community, Operation: OpAutoModerationPut, Revision: &revision, Expires: &expires, Args: []any{"", document}},
		"oversized document":   {RequestID: requestID, CommunityID: community, Operation: OpAutoModerationPut, Revision: &revision, Expires: &expires, Args: []any{"grant-token", make([]byte, 2401)}},
		"non-array document":   {RequestID: requestID, CommunityID: community, Operation: OpAutoModerationPut, Revision: &revision, Expires: &expires, Args: []any{"grant-token", []byte{0x01}}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := EncodeRequestCBOR(bad); err == nil {
				t.Fatal("accepted malformed request")
			}
		})
	}
}

func TestKeyPackageResultWorstCaseFitsHelperPayload(t *testing.T) {
	resultBytes := 1 + 5 + MaxKeyPackageUploadBody + 2 + 33*34 + 9
	if resultBytes != 1_048_435 || MaxHelperPayload-resultBytes != 45 {
		t.Fatalf("max result arithmetic=%d spare=%d", resultBytes, MaxHelperPayload-resultBytes)
	}
	maxUpload := keyPackageUploadAtLength(t, MaxKeyPackageUploadBody)
	refs := keyPackageTestRefs()
	result := KeyPackageIntentResult{UploadBody: maxUpload, PackageRefs: refs, GreatestExpiry: math.MaxInt64}
	encodedResult, err := EncodeKeyPackageIntentResult(result)
	if err != nil {
		t.Fatalf("encode maximum result: %v", err)
	}
	if len(encodedResult) != resultBytes || len(encodedResult) > MaxHelperPayload {
		t.Fatalf("maximum result length=%d, want %d (cap %d)", len(encodedResult), resultBytes, MaxHelperPayload)
	}
	decodedResult, err := DecodeKeyPackageIntentResult(encodedResult)
	if err != nil || !bytes.Equal(decodedResult.UploadBody, maxUpload) || !reflect.DeepEqual(decodedResult.PackageRefs, refs) || decodedResult.GreatestExpiry != math.MaxInt64 {
		t.Fatalf("maximum result roundtrip failed: %v", err)
	}
	reencodedResult, err := EncodeKeyPackageIntentResult(decodedResult)
	if err != nil || !bytes.Equal(reencodedResult, encodedResult) {
		t.Fatalf("maximum result did not re-encode byte-exactly: %v", err)
	}

	publication := KeyPackagePublication{OperationID: fixtureID(0x96), ExpectedGeneration: math.MaxInt64, UploadBody: maxUpload}
	encodedPublication, err := EncodeKeyPackagePublication(publication)
	if err != nil {
		t.Fatalf("encode maximum publication: %v", err)
	}
	decodedPublication, err := DecodeKeyPackagePublication(encodedPublication)
	if err != nil || !bytes.Equal(decodedPublication.UploadBody, maxUpload) || decodedPublication.ExpectedGeneration != math.MaxInt64 {
		t.Fatalf("maximum publication roundtrip failed: %v", err)
	}
	reencodedPublication, err := EncodeKeyPackagePublication(decodedPublication)
	if err != nil || !bytes.Equal(reencodedPublication, encodedPublication) {
		t.Fatalf("maximum publication did not re-encode byte-exactly: %v", err)
	}

	helper, err := EncodeHelperResponse(HelperResponse{RequestID: fixtureID(0x97), Status: 0, Payload: []any{maxUpload, refsAsAny(refs), uint64(math.MaxInt64)}})
	if err != nil {
		t.Fatalf("encode maximum helper response: %v", err)
	}
	if len(helper)-4 != 1_048_460 || len(helper)-4 > MaxHelperBytes {
		t.Fatalf("helper response body length %d, want 1048460 and at most %d", len(helper)-4, MaxHelperBytes)
	}
	if _, err := DecodeHelperResponseFrame(helper); err != nil {
		t.Fatalf("decode maximum helper response: %v", err)
	}

	oversizedUpload := keyPackageUploadAtLength(t, MaxKeyPackageUploadBody+1)
	if _, err := EncodeKeyPackagePublication(KeyPackagePublication{OperationID: fixtureID(0x96), ExpectedGeneration: 0, UploadBody: oversizedUpload}); err == nil {
		t.Fatal("publication encoder accepted body over cap")
	}
	oversizedPublication, err := cborx.Marshal([]any{fixtureID(0x96), uint64(0), oversizedUpload})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeKeyPackagePublication(oversizedPublication); err == nil {
		t.Fatal("publication decoder accepted body over cap")
	}
	if _, err := EncodeKeyPackageIntentResult(KeyPackageIntentResult{UploadBody: oversizedUpload, PackageRefs: refs, GreatestExpiry: math.MaxInt64}); err == nil {
		t.Fatal("result encoder accepted body over cap")
	}
	oversizedResult, err := cborx.Marshal([]any{oversizedUpload, refsAsAny(refs), uint64(math.MaxInt64)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeKeyPackageIntentResult(oversizedResult); err == nil {
		t.Fatal("result decoder accepted body over cap")
	}
	if _, err := EncodeKeyPackageIntent(KeyPackageIntent{OperationID: fixtureID(1), Count: 33}); err == nil {
		t.Fatal("encoder accepted count above 32")
	}
}

func TestGroupRegistrationCodecBoundsAndHash(t *testing.T) {
	var group id.ID
	for i := range group {
		group[i] = 0x33
	}
	body, err := cborx.Marshal([]any{group, []byte{1}, []byte{2}, []byte{3}})
	if err != nil {
		t.Fatal(err)
	}
	hash, err := GroupRegistrationHash(body)
	if err != nil || hex.EncodeToString(hash[:]) != "b4dfefdcb5ea9714fdfc57b25030aaeae6d876d78e9ba007f0dea64b846e1870" {
		t.Fatalf("registration hash=%x err=%v", hash, err)
	}
	request := GroupRegistrationRequest{OperationID: fixtureID(1), RegistrationBody: body}
	encoded, err := EncodeGroupRegistrationRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeGroupRegistrationRequest(encoded)
	if err != nil || !bytes.Equal(decoded.RegistrationBody, body) {
		t.Fatalf("request roundtrip: %#v %v", decoded, err)
	}
	request.ExpectedGeneration = uint64(math.MaxInt64) + 1
	if _, err := EncodeGroupRegistrationRequest(request); err == nil {
		t.Fatal("accepted generation above int64")
	}

	receipt := GroupRegistrationReceipt{OperationID: fixtureID(1), GroupID: group, RegistrationSHA256: hash, AcceptedGeneration: math.MaxInt64, InitialEpoch: math.MaxInt64, NextSequence: 1}
	encoded, err = EncodeGroupRegistrationReceipt(receipt)
	if err != nil {
		t.Fatal(err)
	}
	decodedReceipt, err := DecodeGroupRegistrationReceipt(encoded)
	if err != nil || decodedReceipt != receipt {
		t.Fatalf("receipt roundtrip: %#v %v", decodedReceipt, err)
	}
	receipt.NextSequence = 2
	if _, err := EncodeGroupRegistrationReceipt(receipt); err == nil {
		t.Fatal("accepted non-initial next sequence")
	}
}

func keyPackageUploadAtLength(t *testing.T, target int) []byte {
	t.Helper()
	bigLen := target - 72 // outer array + 2-byte 32-item array head + 5-byte large bstr head + 31 small packages + fallback
	packages := make([]any, 0, 32)
	packages = append(packages, make([]byte, bigLen))
	for range 31 {
		packages = append(packages, []byte{0x22})
	}
	raw, err := cborx.Marshal([]any{packages, []byte{0x33}})
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) != target {
		t.Fatalf("constructed upload length=%d, want %d", len(raw), target)
	}
	return raw
}

func keyPackageTestRefs() [][32]byte {
	refs := make([][32]byte, 33)
	for i := range refs {
		refs[i][31] = byte(i + 1)
	}
	return refs
}

func refsAsAny(refs [][32]byte) []any {
	out := make([]any, len(refs))
	for i := range refs {
		out[i] = append([]byte(nil), refs[i][:]...)
	}
	return out
}

func roundTripVector(t *testing.T, tc vectorCase, raw []byte) []byte {
	t.Helper()
	var got []byte
	var err error
	switch tc.Kind {
	case "request":
		var v Request
		if tc.Body != "" {
			v, err = DecodeRequest(tc.Body)
		} else {
			v, err = DecodeRequestCBOR(raw)
		}
		if err == nil {
			got, err = EncodeRequestCBOR(v)
			if err == nil && tc.Body != "" {
				body, e := EncodeRequest(v)
				if e != nil || body != tc.Body {
					t.Fatalf("body roundtrip mismatch: %q %v", body, e)
				}
			}
		}
	case "response":
		var v Response
		if tc.Body != "" {
			v, err = DecodeResponse(tc.Body)
		} else {
			v, err = DecodeResponseCBOR(raw)
		}
		if err == nil {
			got, err = EncodeResponseCBOR(v)
		}
	case "removal":
		var v RemovalProof
		v, err = DecodeRemovalProof(raw)
		if err == nil {
			got, err = encodeRemoval(v)
		}
	case "event":
		var v Event
		v, err = DecodeEvent(raw)
		if err == nil {
			got, err = EncodeEvent(v)
		}
	case "receipt":
		var v SendReceipt
		v, err = DecodeReceipt(raw)
		if err == nil {
			got, err = EncodeReceipt(v)
		}
	case "helper_request":
		var v HelperRequest
		v, err = DecodeHelperRequestCBOR(raw)
		if err == nil {
			got, err = EncodeHelperRequestCBOR(v)
		}
	case "helper_response":
		var v HelperResponse
		v, err = DecodeHelperResponseCBOR(raw)
		if err == nil {
			got, err = EncodeHelperResponseCBOR(v)
		}
	case "keypackage_publication":
		var v KeyPackagePublication
		v, err = DecodeKeyPackagePublication(raw)
		if err == nil {
			got, err = EncodeKeyPackagePublication(v)
		}
	case "keypackage_publication_receipt":
		var v KeyPackagePublicationReceipt
		v, err = DecodeKeyPackagePublicationReceipt(raw)
		if err == nil {
			got, err = EncodeKeyPackagePublicationReceipt(v)
		}
	case "keypackage_intent":
		var v KeyPackageIntent
		v, err = DecodeKeyPackageIntent(raw)
		if err == nil {
			got, err = EncodeKeyPackageIntent(v)
		}
	case "keypackage_intent_result":
		var v KeyPackageIntentResult
		v, err = DecodeKeyPackageIntentResult(raw)
		if err == nil {
			got, err = EncodeKeyPackageIntentResult(v)
		}
	case "keypackage_resolution":
		var v KeyPackageResolution
		v, err = DecodeKeyPackageResolution(raw)
		if err == nil {
			got, err = EncodeKeyPackageResolution(v)
		}
	case "group_registration_request":
		var v GroupRegistrationRequest
		v, err = DecodeGroupRegistrationRequest(raw)
		if err == nil {
			got, err = EncodeGroupRegistrationRequest(v)
		}
		if err == nil && tc.RegistrationBodyHex != "" {
			body, e := hex.DecodeString(tc.RegistrationBodyHex)
			if e != nil || !bytes.Equal(v.RegistrationBody, body) {
				t.Fatalf("registration body mismatch: %x %v", v.RegistrationBody, e)
			}
			hash, e := GroupRegistrationHash(v.RegistrationBody)
			want, _ := hex.DecodeString(tc.RegistrationSHA256)
			if e != nil || !bytes.Equal(hash[:], want) {
				t.Fatalf("registration hash mismatch: %x %v", hash, e)
			}
		}
	case "group_registration_receipt":
		var v GroupRegistrationReceipt
		v, err = DecodeGroupRegistrationReceipt(raw)
		if err == nil {
			got, err = EncodeGroupRegistrationReceipt(v)
		}
	default:
		t.Fatalf("unknown vector kind %q", tc.Kind)
	}
	if err != nil {
		t.Fatal(err)
	}
	if tc.Context != nil {
		if err := validateVectorContext(tc, raw); err != nil {
			t.Fatal(err)
		}
	}
	return got
}

func roundTripVectorErr(tc vectorCase, raw []byte) ([]byte, error) {
	switch tc.Kind {
	case "request":
		if tc.Body != "" {
			v, e := DecodeRequest(tc.Body)
			if e != nil {
				return nil, e
			}
			return EncodeRequestCBOR(v)
		}
		v, e := DecodeRequestCBOR(raw)
		if e != nil {
			return nil, e
		}
		return EncodeRequestCBOR(v)
	case "response":
		v, e := DecodeResponseCBOR(raw)
		if e != nil {
			return nil, e
		}
		return EncodeResponseCBOR(v)
	case "removal":
		v, e := DecodeRemovalProof(raw)
		if e != nil {
			return nil, e
		}
		return encodeRemoval(v)
	case "event":
		v, e := DecodeEvent(raw)
		if e != nil {
			return nil, e
		}
		return EncodeEvent(v)
	case "receipt":
		v, e := DecodeReceipt(raw)
		if e != nil {
			return nil, e
		}
		return EncodeReceipt(v)
	case "helper_request":
		v, e := DecodeHelperRequestCBOR(raw)
		if e != nil {
			return nil, e
		}
		return EncodeHelperRequestCBOR(v)
	case "helper_response":
		v, e := DecodeHelperResponseCBOR(raw)
		if e != nil {
			return nil, e
		}
		return EncodeHelperResponseCBOR(v)
	case "keypackage_publication":
		v, e := DecodeKeyPackagePublication(raw)
		if e != nil {
			return nil, e
		}
		return EncodeKeyPackagePublication(v)
	case "keypackage_publication_receipt":
		v, e := DecodeKeyPackagePublicationReceipt(raw)
		if e != nil {
			return nil, e
		}
		return EncodeKeyPackagePublicationReceipt(v)
	case "keypackage_intent":
		v, e := DecodeKeyPackageIntent(raw)
		if e != nil {
			return nil, e
		}
		return EncodeKeyPackageIntent(v)
	case "keypackage_intent_result":
		v, e := DecodeKeyPackageIntentResult(raw)
		if e != nil {
			return nil, e
		}
		return EncodeKeyPackageIntentResult(v)
	case "keypackage_resolution":
		v, e := DecodeKeyPackageResolution(raw)
		if e != nil {
			return nil, e
		}
		return EncodeKeyPackageResolution(v)
	case "group_registration_request":
		v, e := DecodeGroupRegistrationRequest(raw)
		if e != nil {
			return nil, e
		}
		return EncodeGroupRegistrationRequest(v)
	case "group_registration_receipt":
		v, e := DecodeGroupRegistrationReceipt(raw)
		if e != nil {
			return nil, e
		}
		return EncodeGroupRegistrationReceipt(v)
	default:
		return nil, invalid()
	}
}

func validateVectorContext(tc vectorCase, raw []byte) error {
	c := tc.Context
	community, e := id.Parse(c.Community)
	if e != nil {
		return e
	}
	now, e := strconv.ParseUint(c.Now, 10, 64)
	if e != nil {
		return e
	}
	switch tc.Kind {
	case "request":
		v, e := DecodeRequestCBOR(raw)
		if e != nil {
			return e
		}
		return ValidateRequestContext(v, community, now)
	case "response":
		v, e := DecodeResponseCBOR(raw)
		if e != nil {
			return e
		}
		return ValidateResponseContext(v, community)
	case "event":
		v, e := DecodeEvent(raw)
		if e != nil {
			return e
		}
		return ValidateEventContext(v, community)
	case "receipt":
		v, e := DecodeReceipt(raw)
		if e != nil {
			return e
		}
		g, e := id.Parse(c.Group)
		if e != nil {
			return e
		}
		return ValidateReceiptContext(v, community, g)
	case "removal":
		v, e := DecodeRemovalProof(raw)
		if e != nil {
			return e
		}
		g, e := id.Parse(c.Group)
		if e != nil {
			return e
		}
		u, e := id.Parse(c.ActorUser)
		if e != nil {
			return e
		}
		d, e := id.Parse(c.ActorDevice)
		if e != nil {
			return e
		}
		return ValidateRemovalContext(v, community, g, u, d, now)
	default:
		return nil
	}
}

func TestRequestRoundTripAndContext(t *testing.T) {
	req := Request{RequestID: fixtureID(1), CommunityID: fixtureID(2), ChannelID: ptrID(fixtureID(3)), Operation: OpMusicSeek, Revision: u64(8), Expires: u64(30), Args: []any{uint64(math.MaxUint64)}}
	body, err := EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if got.Operation != req.Operation || got.Args[0] != uint64(math.MaxUint64) {
		t.Fatalf("round trip mismatch: %#v", got)
	}
	if err := ValidateRequestContext(got, fixtureID(2), 10); err != nil {
		t.Fatal(err)
	}
	if err := ValidateRequestContext(got, fixtureID(3), 10); !errors.Is(err, ErrContext) {
		t.Fatalf("wrong community: %v", err)
	}
	if err := ValidateRequestContext(got, fixtureID(2), 30); !errors.Is(err, ErrExpired) {
		t.Fatalf("expiry boundary: %v", err)
	}
}

func TestRequestRejectsArrayForByteStringAndNoncanonicalBody(t *testing.T) {
	// [1, request, community, null, settings.put, 1, 20, [section, bstr]]
	// document is deliberately an array rather than a byte string.
	raw, _ := hex.DecodeString("880150000000000000000000000000000000015000000000000000000000000000000002f60201f614820180")
	if _, err := DecodeRequestCBOR(raw); err == nil {
		t.Fatal("accepted array where document bstr is required")
	}
	if _, err := DecodeRequest("dilla.bot/1:AA=="); err == nil {
		t.Fatal("accepted padded base64url")
	}
}

func TestRemovalPreimage(t *testing.T) {
	p := RemovalProof{RequestID: fixtureID(1), ActorUser: fixtureID(2), ActorDevice: fixtureID(3), CommunityID: fixtureID(4), GroupID: fixtureID(5), TargetSequence: 6, Expires: 7}
	preimage, err := RemovalPreimage(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(preimage[:len(removalDomain)]) != removalDomain {
		t.Fatalf("wrong domain prefix: %x", preimage)
	}
	got, err := DecodeRemovalProof(preimage[len(removalDomain):])
	if err != nil || got.TargetSequence != 6 {
		t.Fatalf("decode: %#v %v", got, err)
	}
}

func TestEventReceiptAndHelperRoundTrips(t *testing.T) {
	event := Event{ID: fixtureID(1), Sequence: 2, CommunityID: fixtureID(3), Timestamp: 4, Kind: EventMemberJoin, Data: []any{fixtureID(4), fixtureID(5)}}
	eb, err := EncodeEvent(event)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeEvent(eb); err != nil {
		t.Fatal(err)
	}
	receipt := SendReceipt{OperationID: fixtureID(1), AttemptID: fixtureID(4), CommunityID: fixtureID(2), GroupID: fixtureID(3), CiphertextHash: make([]byte, 32), Outcome: ReceiptClosedNotAccepted}
	rb, err := EncodeReceipt(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeReceipt(rb); err != nil {
		t.Fatal(err)
	}
	request := HelperRequest{RequestID: fixtureID(1), Operation: HelperOpen, Payload: []any{}}
	frame, err := EncodeHelperRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeHelperRequestFrame(frame)
	if err != nil || got.Operation != HelperOpen {
		t.Fatalf("helper: %#v %v", got, err)
	}
	frame[len(frame)-1] ^= 1
	if _, err := DecodeHelperRequestFrame(frame); err == nil {
		t.Fatal("accepted mismatched helper frame")
	}
}

func TestNewCallJoinHelperOperationsRoundTrip(t *testing.T) {
	for _, operation := range []HelperOperation{HelperCallJoinCheck, HelperCallGroupForget} {
		frame, err := EncodeHelperRequest(HelperRequest{RequestID: fixtureID(2), Operation: operation, Payload: []any{fixtureID(3)}})
		if err != nil {
			t.Fatalf("encode helper operation %d: %v", operation, err)
		}
		got, err := DecodeHelperRequestFrame(frame)
		if err != nil || got.Operation != operation {
			t.Fatalf("decode helper operation %d: %#v %v", operation, got, err)
		}
	}
	if _, err := EncodeHelperRequest(HelperRequest{RequestID: fixtureID(2), Operation: HelperCallGroupForget + 1, Payload: []any{}}); err == nil {
		t.Fatal("helper operation after the frozen range accepted")
	}
}

func TestSendReceiptEncoderRejectsInvalidTypedValues(t *testing.T) {
	base := SendReceipt{OperationID: fixtureID(1), AttemptID: fixtureID(2), CommunityID: fixtureID(3), GroupID: fixtureID(4), CiphertextHash: make([]byte, 32), Outcome: ReceiptClosedNotAccepted}
	validBody, err := cborx.Marshal([]any{uint64(1), make([]byte, 32), uint64(0)})
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := decodeOriginalSendResponse(validBody)
	if err != nil {
		t.Fatal(err)
	}
	good := base
	good.Outcome = ReceiptAccepted
	good.SendResponse = accepted
	if _, err := EncodeReceipt(good); err != nil {
		t.Fatalf("valid accepted receipt: %v", err)
	}

	bad := []SendReceipt{}
	x := base
	x.AttemptID = id.ID{}
	bad = append(bad, x)
	x = base
	x.ExpectedGeneration = math.MaxInt64 + 1
	bad = append(bad, x)
	x = base
	x.Class = 2
	bad = append(bad, x)
	x = base
	x.Outcome = ReceiptAccepted
	bad = append(bad, x)
	x = base
	x.SendResponse = accepted
	bad = append(bad, x)
	x = good
	x.SendResponse = &OriginalSendResponse{Sequence: 2, FrankingTag: make([]byte, 32), ReceiveTimestamp: 0, Body: validBody}
	bad = append(bad, x)
	for i, receipt := range bad {
		if _, err := EncodeReceipt(receipt); err == nil {
			t.Fatalf("invalid receipt %d encoded", i)
		}
	}
}

func TestEveryRequestOperationShape(t *testing.T) {
	tests := []struct {
		name         string
		op           Operation
		args         []any
		mut, channel bool
	}{
		{"overview", OpOverviewGet, nil, false, false},
		{"settings_get", OpSettingsGet, []any{uint64(5)}, false, false},
		{"settings_put", OpSettingsPut, []any{uint64(0), []byte{0x80}}, true, false},
		{"music_state", OpMusicState, nil, false, true},
		{"music_search", OpMusicSearch, []any{"query"}, false, true},
		{"music_enqueue", OpMusicEnqueue, []any{"input"}, true, true},
		{"music_pause", OpMusicPause, nil, true, true},
		{"music_resume", OpMusicResume, nil, true, true},
		{"music_skip", OpMusicSkip, nil, true, true},
		{"music_stop", OpMusicStop, nil, true, true},
		{"music_seek", OpMusicSeek, []any{uint64(math.MaxUint64)}, true, true},
		{"music_volume", OpMusicVolume, []any{uint64(100)}, true, true},
		{"music_shuffle", OpMusicShuffle, nil, true, true},
		{"music_repeat", OpMusicRepeat, []any{uint64(2)}, true, true},
		{"music_remove", OpMusicRemove, []any{fixtureID(7)}, true, true},
		{"music_move", OpMusicMove, []any{fixtureID(7), nil}, true, true},
		{"music_clear", OpMusicClear, nil, true, true},
		{"music_providers_status", OpMusicProvidersStatus, nil, false, false},
		{"welcome_preview", OpWelcomePreview, []any{"hello"}, false, false},
		{"role_panel_publish", OpRolePanelPublish, []any{fixtureID(10), mustCBOR(t, []any{[]any{"🎮", fixtureID(11), "Gamers"}})}, true, false},
		{"roles_list", OpRolesList, []any{uint64(0)}, false, false},
		{"announcements_list", OpAnnouncementsList, []any{uint64(0)}, false, false},
		{"announcements_put", OpAnnouncementsPut, []any{nil, []byte{0x80}}, true, false},
		{"announcements_delete", OpAnnouncementsDelete, []any{fixtureID(8), uint64(2)}, true, false},
		{"moderation_cases_first_page", OpModerationCases, []any{nil}, false, false},
		{"moderation_cases_next_page", OpModerationCases, []any{[]byte{0x01, 0x02}}, false, false},
		{"moderation_evidence_list_first_page", OpModerationEvidenceList, []any{nil}, false, false},
		{"moderation_evidence_list_next_page", OpModerationEvidenceList, []any{[]byte{0x01, 0x02}}, false, false},
		{"moderation_evidence", OpModerationEvidence, []any{fixtureID(9), "", "excerpt"}, true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := Request{RequestID: fixtureID(1), CommunityID: fixtureID(2), Operation: tt.op, Args: tt.args}
			if tt.channel {
				r.ChannelID = ptrID(fixtureID(3))
			}
			if tt.mut {
				r.Revision, r.Expires = u64(4), u64(30)
			}
			if tt.op == OpRolePanelPublish {
				r.Revision = u64(0)
			}
			body, err := EncodeRequest(r)
			if err != nil {
				t.Fatal(err)
			}
			got, err := DecodeRequest(body)
			if err != nil {
				t.Fatal(err)
			}
			if got.Operation != tt.op {
				t.Fatalf("operation = %d", got.Operation)
			}
		})
	}
}

func TestRolePanelPublishRejectsInvalidMappings(t *testing.T) {
	validRole := fixtureID(11)
	for name, options := range map[string]any{
		"empty":              []any{},
		"zero role":          []any{[]any{"🎮", id.ID{}, "Gamers"}},
		"duplicate emoji":    []any{[]any{"✅", fixtureID(11), "One"}, []any{"✅", fixtureID(12), "Two"}},
		"duplicate role":     []any{[]any{"✅", validRole, "One"}, []any{"🎮", validRole, "Two"}},
		"control emoji":      []any{[]any{"\n", fixtureID(12), "Two"}},
		"invalid name":       []any{[]any{"✅", fixtureID(12), "\n"}},
		"wrong option shape": []any{[]any{"✅", fixtureID(12)}},
	} {
		t.Run(name, func(t *testing.T) {
			raw := mustCBOR(t, options)
			r := Request{RequestID: fixtureID(1), CommunityID: fixtureID(2), Operation: OpRolePanelPublish,
				Revision: u64(1), Expires: u64(30), Args: []any{fixtureID(10), raw}}
			if _, err := EncodeRequest(r); err == nil {
				t.Fatal("invalid role-panel mapping was accepted")
			}
		})
	}
	if _, err := EncodeRequest(Request{RequestID: fixtureID(1), CommunityID: fixtureID(2), Operation: OpRolePanelPublish,
		Revision: u64(1), Expires: u64(30), Args: []any{fixtureID(10), mustCBOR(t, []any{[]any{"✅", fixtureID(11), "Readers"}})}}); err == nil {
		t.Fatal("nonzero reserved panel revision was accepted")
	}
}

func TestModerationCasesCursorBounds(t *testing.T) {
	base := Request{RequestID: fixtureID(1), CommunityID: fixtureID(2), Operation: OpModerationCases}
	for _, tc := range []struct {
		name   string
		cursor any
		valid  bool
	}{
		{name: "first page", cursor: nil, valid: true},
		{name: "opaque cursor", cursor: []byte{0xff}, valid: true},
		{name: "empty cursor", cursor: []byte{}, valid: false},
		{name: "legacy numeric offset", cursor: uint64(0), valid: false},
		{name: "oversized cursor", cursor: make([]byte, MaxModerationCursorBytes+1), valid: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := base
			request.Args = []any{tc.cursor}
			_, err := EncodeRequest(request)
			if (err == nil) != tc.valid {
				t.Fatalf("EncodeRequest error = %v, valid=%t", err, tc.valid)
			}
		})
	}
}

func TestEveryEventKindRoundTrips(t *testing.T) {
	tests := []Event{
		{ID: fixtureID(1), Sequence: 1, CommunityID: fixtureID(2), Kind: EventMemberJoin, Data: []any{fixtureID(3), fixtureID(4)}},
		{ID: fixtureID(1), Sequence: 1, CommunityID: fixtureID(2), Kind: EventMemberLeave, Data: []any{fixtureID(3), fixtureID(4)}},
		{ID: fixtureID(1), Sequence: 1, CommunityID: fixtureID(2), Kind: EventInstallation, Data: []any{fixtureID(3), uint64(5), uint64(math.MaxUint64)}},
		{ID: fixtureID(1), Sequence: 1, CommunityID: fixtureID(2), Kind: EventPermission, Data: []any{uint64(3), fixtureID(3), uint64(1)}},
		{ID: fixtureID(1), Sequence: 1, CommunityID: fixtureID(2), Kind: EventChannel, Data: []any{fixtureID(3), uint64(2), uint64(1)}},
		{ID: fixtureID(1), Sequence: 1, CommunityID: fixtureID(2), Kind: EventModeration, Data: []any{fixtureID(3), fixtureID(4), fixtureID(5), fixtureID(6), nil, fixtureID(7), uint64(8), uint64(4), nil}},
		{ID: fixtureID(1), Sequence: 1, CommunityID: fixtureID(2), Kind: EventRolePanel, Data: []any{fixtureID(3), uint64(1), uint64(1)}},
		{ID: fixtureID(1), Sequence: 1, CommunityID: fixtureID(2), Kind: EventDeviceList, Data: []any{fixtureID(3), uint64(2)}},
	}
	for _, ev := range tests {
		raw, err := EncodeEvent(ev)
		if err != nil {
			t.Fatalf("kind %d encode: %v", ev.Kind, err)
		}
		got, err := DecodeEvent(raw)
		if err != nil || got.Kind != ev.Kind {
			t.Fatalf("kind %d decode: %#v %v", ev.Kind, got, err)
		}
	}
}

func TestContextRejectsFarFutureAndOverflow(t *testing.T) {
	r := Request{CommunityID: fixtureID(2), RequestID: fixtureID(1), Operation: OpMusicPause, ChannelID: ptrID(fixtureID(3)), Revision: u64(1), Expires: u64(41)}
	if err := ValidateRequestContext(r, fixtureID(2), 10); !errors.Is(err, ErrExpired) {
		t.Fatalf("far future: %v", err)
	}
	r.Expires = u64(math.MaxUint64)
	if err := ValidateRequestContext(r, fixtureID(2), math.MaxUint64-10); !errors.Is(err, ErrExpired) {
		t.Fatalf("overflow: %v", err)
	}
}

func TestRawDecodersRejectOversizeBeforeAllocating(t *testing.T) {
	// A valid, canonical top-level bstr is deliberately the wrong envelope shape.
	// Without an entrypoint size guard, parse copies its entire payload before the
	// envelope decoder can reject it.
	n := MaxHelperBytes + 1
	raw := make([]byte, 5+n)
	raw[0] = 0x5a
	raw[1] = byte(uint32(n) >> 24)
	raw[2] = byte(uint32(n) >> 16)
	raw[3] = byte(uint32(n) >> 8)
	raw[4] = byte(n)
	decoders := []struct {
		name   string
		decode func([]byte) error
	}{
		{"request", func(b []byte) error { _, e := DecodeRequestCBOR(b); return e }},
		{"response", func(b []byte) error { _, e := DecodeResponseCBOR(b); return e }},
		{"event", func(b []byte) error { _, e := DecodeEvent(b); return e }},
		{"removal", func(b []byte) error { _, e := DecodeRemovalProof(b); return e }},
		{"receipt", func(b []byte) error { _, e := DecodeReceipt(b); return e }},
		{"helper_request", func(b []byte) error { _, e := DecodeHelperRequestCBOR(b); return e }},
		{"helper_response", func(b []byte) error { _, e := DecodeHelperResponseCBOR(b); return e }},
	}
	for _, tc := range decoders {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.decode(raw); !errors.Is(err, ErrLimit) {
				t.Fatalf("oversized input error = %v, want ErrLimit", err)
			}
			allocs := testing.AllocsPerRun(5, func() { _ = tc.decode(raw) })
			if allocs != 0 {
				t.Fatalf("oversized input allocated %.1f objects per call", allocs)
			}
		})
	}
}

func TestHelperPayloadExactMaximum(t *testing.T) {
	id := fixtureID(1)
	maxValue := make([]byte, MaxHelperPayload-6) // array head + five-byte bstr head
	encoded, err := cborx.Marshal([]any{maxValue})
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) != MaxHelperPayload {
		t.Fatalf("test payload has %d bytes, want %d", len(encoded), MaxHelperPayload)
	}
	if _, err := EncodeHelperResponse(HelperResponse{RequestID: id, Payload: []any{maxValue}}); err != nil {
		t.Fatalf("exact cap rejected: %v", err)
	}
	tooLarge := append(append([]byte(nil), maxValue...), 0)
	if _, err := EncodeHelperResponse(HelperResponse{RequestID: id, Payload: []any{tooLarge}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("one over cap error = %v, want ErrInvalid", err)
	}
	tooLargePayload, err := cborx.Marshal([]any{tooLarge})
	if err != nil {
		t.Fatal(err)
	}
	outer, err := cborx.Marshal([]any{uint64(1), id, uint64(0), tooLargePayload})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeHelperResponseCBOR(outer); !errors.Is(err, ErrLimit) {
		t.Fatalf("decoded payload one over cap error = %v, want ErrLimit", err)
	}
}

func TestMusicSkipOptionalCurrentItemGuard(t *testing.T) {
	for _, args := range [][]any{nil, {fixtureID(9).String()}, {""}} {
		request := Request{RequestID: fixtureID(1), CommunityID: fixtureID(2), ChannelID: ptrID(fixtureID(3)),
			Operation: OpMusicSkip, Revision: u64(0), Expires: u64(120), Args: args}
		body, err := EncodeRequest(request)
		if err != nil {
			t.Fatalf("encode args=%v: %v", args, err)
		}
		decoded, err := DecodeRequest(body)
		if err != nil || len(decoded.Args) != len(args) || len(args) == 1 && decoded.Args[0] != args[0] {
			t.Fatalf("roundtrip args=%v got=%v err=%v", args, decoded.Args, err)
		}
	}
	for _, args := range [][]any{{uint64(1)}, {nil}, {"bad-id"}, {fixtureID(9).String(), fixtureID(10).String()}} {
		_, err := EncodeRequest(Request{RequestID: fixtureID(1), CommunityID: fixtureID(2), ChannelID: ptrID(fixtureID(3)),
			Operation: OpMusicSkip, Revision: u64(0), Expires: u64(120), Args: args})
		if err == nil {
			t.Errorf("accepted malformed skip args=%v", args)
		}
	}
}
