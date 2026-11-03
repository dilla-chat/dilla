package sfu

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/livekit/livekit-server/pkg/rtc/types"
	lkprom "github.com/livekit/livekit-server/pkg/telemetry/prometheus"
	"github.com/livekit/protocol/livekit"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/dilla-chat/dilla/internal/sfu/sfutest"
)

// series counts the series of family in g's output, and the distinct values its label takes.
func series(t *testing.T, g prometheus.Gatherer, family, label string) (int, map[string]bool) {
	t.Helper()
	mfs, err := g.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	values := map[string]bool{}
	for _, mf := range mfs {
		if mf.GetName() != family {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, lp := range m.GetLabel() {
				if lp.GetName() == label {
					values[lp.GetValue()] = true
				}
			}
		}
		return len(mf.GetMetric()), values
	}
	return 0, values
}

// sums is what a family's series add up to: a counter's value, or a histogram's count, sum and
// cumulative buckets by index.
type sums struct {
	value   float64
	count   uint64
	sum     float64
	buckets []uint64
}

// totals adds up family's series in g, only those whose state label is state when state is set.
func totals(t *testing.T, g prometheus.Gatherer, family, state string) sums {
	t.Helper()
	mfs, err := g.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	var s sums
	for _, mf := range mfs {
		if mf.GetName() != family {
			continue
		}
		for _, m := range mf.GetMetric() {
			if state != "" {
				match := false
				for _, lp := range m.GetLabel() {
					match = match || (lp.GetName() == "state" && lp.GetValue() == state)
				}
				if !match {
					continue
				}
			}
			s.value += m.GetCounter().GetValue()
			h := m.GetHistogram()
			s.count += h.GetSampleCount()
			s.sum += h.GetSampleSum()
			for i, b := range h.GetBucket() {
				if i == len(s.buckets) {
					s.buckets = append(s.buckets, 0)
				}
				s.buckets[i] += b.GetCumulativeCount()
			}
		}
	}
	return s
}

// The Go SDK the media worker and the load rig join with, and the LiveKit server dillad runs, speak
// the one protocol the /rtc gate admits. A bump of either fails here before any join is refused.
func TestTheGoSDKSpeaksTheAdmittedProtocol(t *testing.T) {
	if lksdk.PROTOCOL != ClientProtocol || types.CurrentProtocol != ClientProtocol {
		t.Fatalf("server-sdk-go PROTOCOL %d, livekit-server CurrentProtocol %d, gate admits %d",
			lksdk.PROTOCOL, types.CurrentProtocol, ClientProtocol)
	}
}

// Branch review SFU-1, at the merge: every LiveKit series whose label a client writes — the protocol
// number of the three session histograms, the SDK, track source, participant kind and subscriber
// count of the pub/sub histogram, a subscription failure's error text, the country of the packet
// counters — is fed through LiveKit's own recording functions with 40 distinct values each. LiveKit's
// registry grows by a series per value; what dillad serves does not grow at all.
func TestClientChosenLabelsCannotGrowTheServedMetrics(t *testing.T) {
	if err := lkprom.Init("ND_metrics_test", livekit.NodeType_SERVER); err != nil {
		t.Fatalf("Init: %v", err)
	}
	served := LiveKitGatherer(prometheus.DefaultGatherer)
	feed := func(n int) {
		lkprom.RecordSessionJoinLatency(n, time.Millisecond)
		lkprom.RecordSessionStartTime(n, false, time.Millisecond)
		lkprom.RecordSessionDuration(n, time.Second)
		lkprom.RecordPublishTime(fmt.Sprintf("C%d", n), livekit.TrackSource(1000+n), livekit.TrackType(1000+n),
			time.Millisecond, livekit.ClientInfo_SDK(1000+n), livekit.ParticipantInfo_Kind(1000+n))
		lkprom.RecordSubscribeTime("", livekit.TrackSource_MICROPHONE, livekit.TrackType_AUDIO, time.Millisecond,
			livekit.ClientInfo_JS, livekit.ParticipantInfo_STANDARD, 1000+n)
		lkprom.RecordTrackSubscribeFailure(fmt.Errorf("could not subscribe to TR_%d", n), false)
		lkprom.IncrementPackets(fmt.Sprintf("C%d", n), lkprom.Incoming, 1, false)
	}
	families := []string{
		"livekit_session_join_latency_ms", "livekit_session_start_time_ms", "livekit_session_duration_ms",
		"livekit_pubsubtime_ms", "livekit_track_subscribe_counter", "livekit_packet_total",
	}
	feed(ClientProtocol)
	before := map[string]int{}
	rawBefore := map[string]int{}
	for _, f := range families {
		before[f], _ = series(t, served, f, "")
		rawBefore[f], _ = series(t, prometheus.DefaultGatherer, f, "")
		if before[f] == 0 {
			t.Fatalf("%s is not served at all", f)
		}
	}
	for n := range 40 {
		feed(2000 + n)
	}
	for _, f := range families {
		raw, _ := series(t, prometheus.DefaultGatherer, f, "")
		got, _ := series(t, served, f, "")
		if raw < rawBefore[f]+40 {
			t.Errorf("%s: LiveKit's registry went from %d to %d series; the feed did not reach the label", f, rawBefore[f], raw)
		}
		if got != before[f] {
			t.Errorf("%s: dillad serves %d series after 40 client-chosen values, %d before", f, got, before[f])
		}
	}
	for _, label := range []string{"protocol_version", "country", "count", "error"} {
		for _, f := range families {
			if _, vs := series(t, served, f, label); len(vs) > 0 {
				t.Errorf("%s still carries %s %v", f, label, vs)
			}
		}
	}
	// A closed label keeps its known values and folds the rest into "other".
	_, sdks := series(t, served, "livekit_pubsubtime_ms", "sdk")
	if !sdks["JS"] || !sdks["other"] {
		t.Errorf("pubsubtime sdk values %v, want JS and other among them", sdks)
	}
	for v := range sdks {
		if _, known := livekit.ClientInfo_SDK_value[v]; !known && v != "other" {
			t.Errorf("pubsubtime serves sdk %q", v)
		}
	}
	// Re-review N3: the merge sums what it folds. The served histogram's count, sum and every bucket,
	// and the served failure counter, equal the sums over LiveKit's raw series; the top finite bucket
	// never exceeds the count, so the +Inf bucket promhttp renders from the count is consistent.
	rawJoin := totals(t, prometheus.DefaultGatherer, "livekit_session_join_latency_ms", "")
	servedJoin := totals(t, served, "livekit_session_join_latency_ms", "")
	if fmt.Sprint(rawJoin) != fmt.Sprint(servedJoin) {
		t.Errorf("join histogram served %+v, raw sums %+v", servedJoin, rawJoin)
	}
	if n := len(servedJoin.buckets); n == 0 || servedJoin.buckets[n-1] > servedJoin.count {
		t.Errorf("served join buckets %v exceed its count %d", servedJoin.buckets, servedJoin.count)
	}
	rawFail := totals(t, prometheus.DefaultGatherer, "livekit_track_subscribe_counter", "failure")
	servedFail := totals(t, served, "livekit_track_subscribe_counter", "failure")
	if rawFail.value < 41 || rawFail.value != servedFail.value {
		t.Errorf("subscribe failures served %v, raw sum %v (want at least 41)", servedFail.value, rawFail.value)
	}
	// The aggregation keeps the totals: the join histogram counts every observation.
	mfs, _ := served.Gather()
	for _, mf := range mfs {
		if mf.GetName() == "livekit_session_join_latency_ms" && mf.GetMetric()[0].GetHistogram().GetSampleCount() < 41 {
			t.Errorf("the merged join histogram counts %d samples, want at least 41", mf.GetMetric()[0].GetHistogram().GetSampleCount())
		}
	}
}

// fakeGatherer answers fixed families.
type fakeGatherer []*dto.MetricFamily

func (f fakeGatherer) Gather() ([]*dto.MetricFamily, error) { return f, errors.New("partial") }

// Only LiveKit's known families, and the Go runtime's and process's, are served: a family a LiveKit
// upgrade adds, or any other package's on the default registry, is not, and a Gather error passes on.
func TestOnlyKnownFamiliesAreServed(t *testing.T) {
	counter := dto.MetricType_COUNTER
	fam := func(name string) *dto.MetricFamily {
		v := 1.0
		return &dto.MetricFamily{Name: &name, Type: &counter, Metric: []*dto.Metric{{Counter: &dto.Counter{Value: &v}}}}
	}
	mfs, err := LiveKitGatherer(fakeGatherer{
		fam("livekit_room_total"), fam("livekit_brand_new_total"), fam("livekit_config_reload_total"),
		fam("go_goroutines"), fam("process_open_fds"), fam("pion_something_total"),
	}).Gather()
	if err == nil {
		t.Error("the underlying Gather error was dropped")
	}
	var names []string
	for _, mf := range mfs {
		names = append(names, mf.GetName())
	}
	if got := strings.Join(names, ","); got != "livekit_room_total,go_goroutines,process_open_fds" {
		t.Errorf("served families %s", got)
	}
}

// The same through LiveKit's real join path: 20 WebSocket joins straight to the in-process SFU, each
// with a protocol number of its own (the attack the /rtc gate now refuses), mint 20
// livekit_session_join_latency_ms series in LiveKit's registry and one in what dillad serves.
func TestDistinctProtocolJoinsLeaveTheServedSeriesUnchanged(t *testing.T) {
	c := testConfig()
	c.Port, c.UDPPort = sfutest.FreePorts(t)
	srv, err := Start(t.Context(), c)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = srv.Stop(t.Context()) }()
	const room = "0123456789abcdef0123456789abcdef-1790000002"
	if err := srv.CreateRoom(t.Context(), room); err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}
	const family = "livekit_session_join_latency_ms"
	served := LiveKitGatherer(prometheus.DefaultGatherer)
	join := func(i, protocol int) {
		tok, err := srv.Token(room, fmt.Sprintf("p%d", i), PublishGrant(true, false, false), nil)
		if err != nil {
			t.Fatal(err)
		}
		conn, resp, err := websocket.Dial(t.Context(), fmt.Sprintf("%s/rtc?access_token=%s&protocol=%d",
			srv.URL(), url.QueryEscape(tok), protocol), nil)
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		if err != nil {
			t.Fatalf("join %d: %v", i, err)
		}
		defer func() { _ = conn.CloseNow() }()
		if _, _, err := conn.Read(t.Context()); err != nil {
			t.Fatalf("join %d: no join response: %v", i, err)
		}
	}
	waitFor := func(protocol string) {
		deadline := time.Now().Add(10 * time.Second)
		for {
			if _, vs := series(t, prometheus.DefaultGatherer, family, "protocol_version"); vs[protocol] {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("LiveKit never recorded a join with protocol %s", protocol)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	join(0, ClientProtocol)
	waitFor(fmt.Sprint(ClientProtocol))
	before, _ := series(t, served, family, "")
	for i := range 20 {
		join(i+1, 3000+i)
	}
	waitFor(fmt.Sprint(3019))
	_, raw := series(t, prometheus.DefaultGatherer, family, "protocol_version")
	after, _ := series(t, served, family, "")
	t.Logf("LiveKit holds %d protocol_version values; dillad serves %d series, %d before", len(raw), after, before)
	if after != before || after != 1 {
		t.Fatalf("dillad serves %d %s series after 20 distinct protocols, %d before; want 1 throughout", after, family, before)
	}
}
