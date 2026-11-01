package gateway

import "github.com/jonasthim/dilla/internal/server"

// CallStatsReport matches the existing coarse six-element HTTP report. No addresses, input
// events, credentials or media enter it; the last two counters are deltas since the last sample.
type CallStatsReport struct {
	_                    struct{} `cbor:",toarray"`
	CandidateType        uint64
	RelayProtocol        *uint64
	RTTms                uint32
	FractionLostPermille uint64
	DecryptFailures      uint64
	FramesEncrypted      uint64
}

func (q CallStatsReport) Validate() error {
	if q.CandidateType > 3 || (q.RelayProtocol != nil && (q.CandidateType != 3 || *q.RelayProtocol > 2)) || q.RTTms > 60000 || q.FractionLostPermille > 1000 || q.DecryptFailures > 1<<20 || q.FramesEncrypted > 1<<20 {
		return server.Errorf(server.CodeInvalidRequest, "invalid coarse call report")
	}
	return nil
}
