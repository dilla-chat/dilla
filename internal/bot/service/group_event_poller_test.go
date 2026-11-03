package service

import (
	"context"
	"errors"
	"testing"

	"github.com/dilla-chat/dilla/internal/bot/native"
	"github.com/dilla-chat/dilla/internal/id"
)

type fakeGroupEventNative struct {
	binding native.Binding
	page    native.GroupEventPage
	acked   []uint64
	ackErr  error
}

func (f *fakeGroupEventNative) Binding() native.Binding { return f.binding }
func (f *fakeGroupEventNative) ReadGroupEvents(_ context.Context, after, limit uint64) (native.GroupEventPage, error) {
	if after != 0 || limit != 100 {
		return native.GroupEventPage{}, errors.New("unexpected page request")
	}
	return f.page, nil
}
func (f *fakeGroupEventNative) AcknowledgeGroupEvents(_ context.Context, through uint64) error {
	if f.ackErr != nil {
		return f.ackErr
	}
	f.acked = append(f.acked, through)
	return nil
}

type groupEventHandlerFunc func(context.Context, native.GroupEvent) error

func (f groupEventHandlerFunc) HandleGroupEvent(ctx context.Context, event native.GroupEvent) error {
	return f(ctx, event)
}

func TestGroupEventPollerAcknowledgesOnlyAfterHandlerAndAdvancesInvisibleRows(t *testing.T) {
	community := id.New()
	f := &fakeGroupEventNative{binding: native.Binding{Instance: id.New(), Community: community, Installation: id.New()}, page: native.GroupEventPage{
		Events: []native.GroupEvent{{DeliveryID: 2, Community: community}, {DeliveryID: 4, Community: community}}, Next: 5, HighWater: 5,
	}}
	var handled []uint64
	p, err := newGroupEventPoller(f)
	if err != nil {
		t.Fatal(err)
	}
	result, err := p.PollOnce(context.Background(), groupEventHandlerFunc(func(_ context.Context, event native.GroupEvent) error {
		handled = append(handled, event.DeliveryID)
		if len(f.acked) != len(handled)-1 {
			t.Fatal("event acknowledged before its handler completed")
		}
		return nil
	}))
	if err != nil || result.Handled != 2 || result.AcknowledgedThrough != 5 {
		t.Fatalf("PollOnce = (%+v, %v)", result, err)
	}
	if len(f.acked) != 3 || f.acked[0] != 2 || f.acked[1] != 4 || f.acked[2] != 5 {
		t.Fatalf("acks = %v", f.acked)
	}
}

func TestGroupEventPollerDoesNotAcknowledgeFailedHandler(t *testing.T) {
	community := id.New()
	f := &fakeGroupEventNative{binding: native.Binding{Instance: id.New(), Community: community, Installation: id.New()}, page: native.GroupEventPage{
		Events: []native.GroupEvent{{DeliveryID: 1, Community: community}, {DeliveryID: 2, Community: community}}, Next: 2, HighWater: 2,
	}}
	p, _ := newGroupEventPoller(f)
	_, err := p.PollOnce(context.Background(), groupEventHandlerFunc(func(_ context.Context, event native.GroupEvent) error {
		if event.DeliveryID == 2 {
			return errors.New("delivery unavailable")
		}
		return nil
	}))
	if err == nil || len(f.acked) != 1 || f.acked[0] != 1 {
		t.Fatalf("failed event acknowledgement = %v, %v", f.acked, err)
	}
}
