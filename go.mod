module github.com/jonasthim/dilla

go 1.27.3

require (
	github.com/caddyserver/certmagic v0.25.4
	github.com/coder/websocket v1.8.15
	github.com/coreos/go-oidc/v3 v3.21.0
	github.com/fxamacker/cbor/v2 v2.9.4
	github.com/go-jose/go-jose/v4 v4.1.6
	github.com/go-logr/logr v1.4.4
	github.com/go-webauthn/webauthn v0.18.2
	github.com/gorilla/websocket v1.5.4-0.20250319132907-e064f32e3674
	github.com/jackc/pgx/v5 v5.11.0
	github.com/libdns/libdns v1.1.1
	github.com/livekit/livekit-server v1.13.7
	// B13: interfaces.md §3.5 pins protocol v1.51.1-0.20260905133529-a4f4b5c0c23f
	// and server-sdk-go v2.18.1. That pair does not compile — v2.18.1 requires
	// protocol v1.49.0, MVS lifts protocol to livekit-server v1.13.7's v1.51.1,
	// and server-sdk-go's sipclient.go then returns the wrong type. The two
	// pseudo-versions below are the pair verified to build on 2026-09-23.
	github.com/livekit/protocol v1.51.1-0.20260910121219-271d9cde3897
	github.com/livekit/server-sdk-go/v2 v2.18.2-0.20260922130803-2088dabd3442
	github.com/pelletier/go-toml/v2 v2.4.3
	github.com/pion/logging v0.2.4
	github.com/pion/rtp v1.10.5
	github.com/pion/stun/v3 v3.1.7
	github.com/pion/transport/v4 v4.1.0
	github.com/pion/turn/v5 v5.0.13
	github.com/pion/webrtc/v4 v4.2.19
	github.com/pires/go-proxyproto v0.15.0
	github.com/pquerna/otp v1.5.0
	github.com/pressly/goose/v3 v3.28.0
	github.com/prometheus/client_golang v1.24.1
	github.com/prometheus/client_model v0.6.3
	github.com/tetratelabs/wazero v1.12.0
	github.com/twitchtv/twirp v8.1.3+incompatible
	golang.org/x/crypto v0.57.0
	golang.org/x/oauth2 v0.37.0
	golang.org/x/sys v0.48.0
	golang.org/x/text v0.42.0
	golang.org/x/time v0.16.0
	google.golang.org/protobuf v1.36.12
	modernc.org/sqlite v1.59.0
)

require (
	buf.build/gen/go/bufbuild/protovalidate/protocolbuffers/go v1.36.12-20260825204119-511051f7f437.1 // indirect
	buf.build/go/protovalidate v1.4.0 // indirect
	buf.build/go/protoyaml v0.7.0 // indirect
	cel.dev/cel-go v0.32.0 // indirect
	cel.dev/expr v0.25.3 // indirect
	github.com/antlr4-go/antlr/v4 v4.13.1 // indirect
	github.com/benbjohnson/clock v1.3.5 // indirect
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/bep/debounce v1.2.1 // indirect
	github.com/boombuler/barcode v1.0.1-0.20190219062509-6c824513bacc // indirect
	github.com/caddyserver/zerossl v0.1.5 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/d5/tengo/v2 v2.17.0 // indirect
	github.com/dennwc/iters v1.2.2 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/elliotchance/orderedmap/v3 v3.1.1 // indirect
	github.com/florianl/go-tc v0.4.8 // indirect
	github.com/frostbyte73/core v0.1.1 // indirect
	github.com/fsnotify/fsnotify v1.10.1 // indirect
	github.com/gammazero/deque v1.2.1 // indirect
	github.com/gammazero/workerpool v1.2.1 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/go-viper/mapstructure/v2 v2.5.0 // indirect
	github.com/go-webauthn/x v0.3.1 // indirect
	github.com/golang-jwt/jwt/v5 v5.3.1 // indirect
	github.com/google/go-cmp v0.7.0 // indirect
	github.com/google/go-tpm v0.9.8 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/google/wire v0.7.0 // indirect
	github.com/hashicorp/go-cleanhttp v0.5.2 // indirect
	github.com/hashicorp/go-retryablehttp v0.7.8 // indirect
	github.com/hashicorp/go-version v1.9.0 // indirect
	github.com/hashicorp/golang-lru v1.0.2 // indirect
	github.com/hashicorp/golang-lru/v2 v2.0.7 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/jellydator/ttlcache/v3 v3.4.1 // indirect
	github.com/josharian/native v1.1.0 // indirect
	github.com/jxskiss/base62 v1.1.0 // indirect
	github.com/klauspost/compress v1.20.0 // indirect
	github.com/klauspost/cpuid/v2 v2.4.0 // indirect
	github.com/lithammer/shortuuid/v4 v4.3.0 // indirect
	github.com/livekit/mageutil v0.0.0-20250511045019-0f1ff63f7731 // indirect
	github.com/livekit/mediatransportutil v0.0.0-20260821083140-f234b534b095 // indirect
	github.com/livekit/psrpc v0.7.7 // indirect
	github.com/mackerelio/go-osstat v0.2.8 // indirect
	github.com/magefile/mage v1.17.2 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/mdlayher/netlink v1.11.2 // indirect
	github.com/mdlayher/socket v0.6.1 // indirect
	github.com/mfridman/interpolate v0.0.2 // indirect
	github.com/mholt/acmez/v3 v3.1.6 // indirect
	github.com/miekg/dns v1.1.72 // indirect
	github.com/mitchellh/go-homedir v1.1.0 // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/nats-io/nats.go v1.53.1 // indirect
	github.com/nats-io/nkeys v0.4.16 // indirect
	github.com/nats-io/nuid v1.0.1 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/nyaruka/phonenumbers v1.8.1 // indirect
	github.com/petermattis/goid v0.0.0-20260820044319-269ab09b5261 // indirect
	github.com/philhofer/fwd v1.2.0 // indirect
	github.com/pion/datachannel v1.6.2 // indirect
	github.com/pion/dtls/v3 v3.1.8 // indirect
	github.com/pion/ice/v4 v4.4.0 // indirect
	github.com/pion/interceptor v0.1.47 // indirect
	github.com/pion/mdns/v2 v2.2.0 // indirect
	github.com/pion/randutil v0.1.0 // indirect
	github.com/pion/rtcp v1.2.17 // indirect
	github.com/pion/sctp v1.11.1 // indirect
	github.com/pion/sdp/v3 v3.0.19 // indirect
	github.com/pion/srtp/v3 v3.0.13 // indirect
	github.com/pkg/errors v0.9.1 // indirect
	github.com/prometheus/common v0.71.0 // indirect
	github.com/prometheus/procfs v0.22.0 // indirect
	github.com/puzpuzpuz/xsync/v4 v4.5.0 // indirect
	github.com/redis/go-redis/v9 v9.22.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	github.com/rs/cors v1.11.1 // indirect
	github.com/sethvargo/go-retry v0.4.0 // indirect
	github.com/thoas/go-funk v0.9.3 // indirect
	github.com/tinylib/msgp v1.6.4 // indirect
	github.com/tomnomnom/linkheader v0.0.0-20250811210735-e5fe3b51442e // indirect
	github.com/ua-parser/uap-go v0.0.0-20260529044130-17c35e68e58c // indirect
	github.com/urfave/cli/v3 v3.10.1 // indirect
	github.com/urfave/negroni/v3 v3.1.1 // indirect
	github.com/wlynxg/anet v0.0.5 // indirect
	github.com/x448/float16 v0.8.4 // indirect
	github.com/zeebo/blake3 v0.2.4 // indirect
	github.com/zeebo/xxh3 v1.1.0 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/otel v1.46.0 // indirect
	go.opentelemetry.io/otel/metric v1.46.0 // indirect
	go.opentelemetry.io/otel/trace v1.46.0 // indirect
	go.uber.org/atomic v1.11.0 // indirect
	go.uber.org/multierr v1.11.0 // indirect
	go.uber.org/zap v1.28.0 // indirect
	go.uber.org/zap/exp v0.3.0 // indirect
	go.yaml.in/yaml/v3 v3.0.5 // indirect
	golang.org/x/exp v0.0.0-20260824195058-e88cd73687aa // indirect
	golang.org/x/mod v0.41.0 // indirect
	golang.org/x/net v0.60.0 // indirect
	golang.org/x/sync v0.23.0 // indirect
	golang.org/x/tools v0.49.0 // indirect
	google.golang.org/genproto/googleapis/api v0.0.0-20260831171406-18b4a7587f8a // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260831171406-18b4a7587f8a // indirect
	google.golang.org/grpc v1.83.2 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
	modernc.org/libc v1.75.7 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.12.1 // indirect
)

// Copied verbatim from livekit-server v1.13.7's own go.mod (lines 167, 169,
// 171). Go ignores `replace` in dependencies, so without these three
// github.com/livekit/livekit-server/pkg/rtc does not compile.
replace github.com/pion/webrtc/v4 => github.com/livekit/webrtc-pion/v4 v4.2.18-warp.1

replace github.com/pion/dtls/v3 => github.com/livekit/dtls/v3 v3.1.5-warp.1

replace github.com/pion/ice/v4 => github.com/livekit/ice/v4 v4.4.0-warp.2

// Pion's published permission timer race fix, backported without the transport/v5 API migration.
replace github.com/pion/turn/v5 => ./third_party/pion-turn
