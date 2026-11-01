# Pion TURN compatibility backport

Source: github.com/pion/turn/v5 v5.0.13, module checksum
`h1:erHOsJyxuV6QK54+PjWJhe8u1O7BM3a/US0zYJJsnx4=`.

This directory contains the upstream non-test Go sources, module manifests and
MIT license. The only source change is `internal/allocation/allocation.go`:
`AddPermission` initializes its timer under the permission lock before publishing
the permission. It also keeps duplicate lookup and insertion under that lock.

The fix is published in Pion TURN v5.1.3, upstream commit
`70eb89e` / https://github.com/pion/turn/pull/619. It resolves the allocation-close
race reported by Dilla's Go race CI. The v5.1 line requires transport/v5, while
LiveKit's current ICE fork requires transport/v4, so this backport retains the
compatible API. Remove this replacement when LiveKit's supported stack can use
the fixed release. No changes are made to TURN authentication or relay policy.

Validation uses Dilla's real UDP/TCP relay tests under `go test -race`.
