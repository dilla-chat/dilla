// SPDX-FileCopyrightText: 2026 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT
package allocation

import (
	"net"
	"testing"
	"time"
)

// From upstream PR #619: teardown must see a fully initialized permission.
func TestAddPermissionPublishesInitializedTimer(t *testing.T) {
	callbackStarted := make(chan struct{})
	releaseCallback := make(chan struct{})
	addDone := make(chan struct{})
	alloc := NewAllocation(nil, &FiveTuple{}, EventHandler{
		OnPermissionCreated: func(_, _ net.Addr, _, _, _ string, _ net.Addr, _ net.IP) { close(callbackStarted); <-releaseCallback },
	}, nil)
	alloc.lifetimeTimer = time.NewTimer(time.Hour)
	go func() {
		defer close(addDone)
		alloc.AddPermission(NewPermission(&net.UDPAddr{IP: net.IPv4(192, 0, 2, 1), Port: 3478}, nil, time.Hour))
	}()
	<-callbackStarted
	if err := alloc.Close(); err != nil {
		t.Error(err)
	}
	close(releaseCallback)
	<-addDone
}
