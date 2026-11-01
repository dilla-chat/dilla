package client

import (
	"context"
	"net/http"

	"github.com/jonasthim/dilla/internal/id"
)

// KeyPackageInventory is the server's current directory snapshot for one
// managed installation. It is distinct from the historical publication
// receipt, which proves acceptance but says nothing about package consumption.
type KeyPackageInventory struct {
	Generation         uint64
	ServerTime         uint64
	OrdinaryActive     uint64
	HasCurrentFallback bool
}

func (s *Session) KeyPackageInventory(ctx context.Context, installation id.ID) (KeyPackageInventory, error) {
	var out KeyPackageInventory
	i, err := pathID(installation)
	if err != nil {
		return out, err
	}
	raw, _, _, err := s.doObserved(ctx, http.MethodGet, "bot-runtime/installations/"+i+"/keypackages", nil, nil, MaxSmallBody, false)
	if err != nil {
		return out, err
	}
	a, err := decode(raw, 4)
	if err != nil {
		return out, ErrProtocol
	}
	var ok bool
	if out.Generation, ok = asUint(a[0]); !ok || out.Generation > 1<<63-1 {
		return KeyPackageInventory{}, ErrProtocol
	}
	if out.ServerTime, ok = asUint(a[1]); !ok || out.ServerTime > 1<<63-1 {
		return KeyPackageInventory{}, ErrProtocol
	}
	if out.OrdinaryActive, ok = asUint(a[2]); !ok || out.OrdinaryActive > 1<<63-1 {
		return KeyPackageInventory{}, ErrProtocol
	}
	flag, ok := asUint(a[3])
	if !ok || flag > 1 {
		return KeyPackageInventory{}, ErrProtocol
	}
	out.HasCurrentFallback = flag == 1
	return out, nil
}
