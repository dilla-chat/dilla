//go:build linux

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/jonasthim/dilla/internal/bot/client"
	"github.com/jonasthim/dilla/internal/bot/helper"
	"github.com/jonasthim/dilla/internal/bot/music"
	"github.com/jonasthim/dilla/internal/bot/music/persistence"
	"github.com/jonasthim/dilla/internal/bot/native"
	"github.com/jonasthim/dilla/internal/bot/protocol"
	"github.com/jonasthim/dilla/internal/bot/service"
	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/permbits"
)

// The transport replaces the server and MLS boundary, not command intake. Source
// sync still fetches admission/context/info/pages, applies messages, flushes its
// cursor, refreshes trust and drains the real durable-event poller. Queue state
// uses the real native helper and its encrypted SQLite store. Events use a
// separate on-disk SQLite transport journal; requests never enter the handler
// directly. The TS entry point issues the shipping client-core operation.
type controlE2ENative struct {
	*native.Client                         // real helper IPC, priority gate and encrypted SQLite state
	db                                     *sql.DB
	binding                                native.Binding
	descriptor                             native.InteractionGroupDescriptor
	session                                *client.Session
	next                                   atomic.Uint64
	loads, trustReads, eventReads, applies atomic.Int32
}

func (n *controlE2ENative) Binding() native.Binding                            { return n.binding }
func (n *controlE2ENative) Groups(context.Context) ([]native.GroupInfo, error) { return nil, nil }
func (n *controlE2ENative) InteractionGroups(context.Context) ([]native.InteractionGroupDescriptor, error) {
	return []native.InteractionGroupDescriptor{n.descriptor}, nil
}
func (n *controlE2ENative) InspectGroup(context.Context, id.ID) (native.GroupInfo, error) {
	return native.GroupInfo{}, &helperStatusForbidden
}

var helperStatusForbidden = helper.StatusError{Status: 2}

func (n *controlE2ENative) InspectInteractionGroup(context.Context, native.InteractionGroupExpected, []byte) (native.InteractionGroupInspectResult, error) {
	return native.InteractionGroupInspectResult{Binding: n.descriptor.Binding, BotDevice: n.descriptor.BotDevice, State: 2, Epoch: 1, NextSequence: n.next.Load()}, nil
}
func (n *controlE2ENative) TryClaimSource(id.ID) (func(), bool) { return func() {}, true }
func (n *controlE2ENative) PendingLeaves(context.Context) ([]native.GroupPendingLeaf, error) {
	n.trustReads.Add(1)
	return []native.GroupPendingLeaf{{Group: n.descriptor.Group, User: n.descriptor.Binding.ParticipantUser, CachedVersion: 0}}, nil
}
func (n *controlE2ENative) TryClaimTrust(id.ID) (func(), bool) { return func() {}, true }
func (n *controlE2ENative) TrustSender(context.Context, id.ID, uint64, id.ID, []byte, []byte, uint64) (native.GroupTrustVerdict, error) {
	return native.GroupTrustVerdict{}, nil
}
func (n *controlE2ENative) RevalidateGroup(context.Context, id.ID, uint64, uint64) (uint32, error) {
	return 0, nil
}
func (n *controlE2ENative) InteractionGroupCursor(context.Context, native.InteractionGroupExpected, []byte) (*native.GroupCursor, error) {
	if next := n.next.Load(); next > 1 {
		return &native.GroupCursor{Sequence: next - 1, Epoch: 1}, nil
	}
	return nil, nil
}
func (n *controlE2ENative) AcknowledgeInteractionGroupCursor(context.Context, native.InteractionGroupExpected, []byte, native.GroupCursor) error {
	return nil
}
func (n *controlE2ENative) ApplyInteractionGroup(ctx context.Context, _ native.InteractionGroupExpected, _, _, messages []byte, through uint64) (native.GroupApplyResult, error) {
	var rows [][]any
	if err := cborx.Unmarshal(messages, &rows); err != nil {
		return native.GroupApplyResult{}, err
	}
	for _, row := range rows {
		if _, err := n.db.ExecContext(ctx, "INSERT OR IGNORE INTO events(seq,body) VALUES(?,?)", row[0], string(row[3].([]byte))); err != nil {
			return native.GroupApplyResult{}, err
		}
	}
	n.applies.Add(1)
	n.next.Store(through + 1)
	return native.GroupApplyResult{State: 2, NextSequence: through + 1}, nil
}
func (n *controlE2ENative) ReadGroupEvents(ctx context.Context, _, _ uint64) (native.GroupEventPage, error) {
	n.eventReads.Add(1)
	var high uint64
	if err := n.db.QueryRowContext(ctx, "SELECT COALESCE(MAX(seq),0) FROM events").Scan(&high); err != nil {
		return native.GroupEventPage{}, err
	}
	rows, err := n.db.QueryContext(ctx, "SELECT seq,body FROM events WHERE ack=0 ORDER BY seq")
	if err != nil {
		return native.GroupEventPage{}, err
	}
	defer rows.Close()
	page := native.GroupEventPage{HighWater: high, Next: high}
	for rows.Next() {
		var seq uint64
		var body string
		if err = rows.Scan(&seq, &body); err != nil {
			return page, err
		}
		b := n.descriptor.Binding
		page.Events = append(page.Events, native.GroupEvent{DeliveryID: seq, Group: n.descriptor.Group, Community: b.Community, Channel: b.ParticipantUser, SourceSequence: seq, Epoch: 1, ReceivedAt: uint64(time.Now().Unix()), SenderUser: b.ParticipantUser, SenderDevice: b.ParticipantDevice, Message: id.New(), Body: &body})
	}
	return page, rows.Err()
}
func (n *controlE2ENative) AcknowledgeGroupEvents(ctx context.Context, through uint64) error {
	_, err := n.db.ExecContext(ctx, "UPDATE events SET ack=1 WHERE seq<=?", through)
	return err
}
func (n *controlE2ENative) StateGet(ctx context.Context, area uint64, key []byte) (native.StateValue, error) {
	if strings.HasPrefix(string(key), "dilla.bot.music.v1/") {
		n.loads.Add(1)
	}
	return n.Client.StateGet(ctx, area, key)
}
func (n *controlE2ENative) InteractionSnapshot(ctx context.Context, _ native.GroupEvent) (native.InteractionGroupExpected, []byte, error) {
	b := n.descriptor.Binding
	request := id.New()
	if _, err := n.session.IssueBotInteractionAdmission(ctx, b.Installation, request, n.descriptor.Group, b.ParticipantUser, b.ParticipantDevice); err != nil {
		return native.InteractionGroupExpected{}, nil, err
	}
	wire, err := n.session.BotInteractionContextSnapshot(ctx, b.Installation, request, n.descriptor.Group, b.ParticipantUser, b.ParticipantDevice)
	return native.InteractionGroupExpected{Instance: b.Instance, Installation: b.Installation, Community: b.Community, BotUser: b.BotUser, BotDevice: n.descriptor.BotDevice, ParticipantUser: b.ParticipantUser, ParticipantDevice: b.ParticipantDevice, Group: n.descriptor.Group, MinimumEligibilityRevision: 1, ServerNow: uint64(time.Now().Unix()), PolicyVersion: 1}, wire, err
}

type controlE2ESender struct{ db *sql.DB }

func (s controlE2ESender) ResumeInteractionResponse(context.Context, native.GroupEvent, func(context.Context) error) (bool, error) {
	return false, nil
}
func (s controlE2ESender) SendInteractionResponse(ctx context.Context, _ native.GroupEvent, body string) (bool, error) {
	response, err := protocol.DecodeResponse(body)
	if err != nil {
		return false, err
	}
	_, err = s.db.ExecContext(ctx, "INSERT INTO responses(request,body) VALUES(?,?)", response.RequestID.String(), body)
	return err == nil, err
}

func TestClientMusicControlsEndToEnd(t *testing.T) { testClientMusicControlsEndToEnd(t, false) }
func TestClientMusicControlsEndToEndDuringSweep(t *testing.T) {
	testClientMusicControlsEndToEnd(t, true)
}
func testClientMusicControlsEndToEnd(t *testing.T, duringSweep bool) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	binary := os.Getenv("DILLA_BOT_HELPER_BIN")
	if binary == "" {
		binary = filepath.Join(root, "target/debug/dilla-bot-core")
	}
	binary, err = filepath.Abs(binary)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(binary); err != nil {
		t.Skip("build dilla-bot-core or set DILLA_BOT_HELPER_BIN for the SQLite helper integration")
	}
	bundle := filepath.Join(t.TempDir(), "control-client.mjs")
	build := exec.CommandContext(t.Context(), filepath.Join(root, "node_modules/.bin/esbuild"), filepath.Join(root, "cmd/dillabot/testdata/control_latency_client.ts"), "--bundle", "--platform=node", "--format=esm", "--outfile="+bundle)
	if out, buildErr := build.CombinedOutput(); buildErr != nil {
		t.Fatalf("build real client fixture: %s: %v", out, buildErr)
	}
	for _, operation := range []string{"pause", "skip"} {
		t.Run(operation, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "music.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			db.SetMaxOpenConns(1)
			for _, schema := range []string{"CREATE TABLE source(seq INTEGER PRIMARY KEY,body TEXT)", "CREATE TABLE events(seq INTEGER PRIMARY KEY,body TEXT,ack INTEGER NOT NULL DEFAULT 0)", "CREATE TABLE responses(request TEXT PRIMARY KEY,body TEXT)"} {
				if _, err = db.ExecContext(ctx, schema); err != nil {
					t.Fatal(err)
				}
			}
			binding := native.Binding{Instance: id.New(), Installation: id.New(), Community: id.New()}
			channel, group, user, device, botDevice := id.New(), id.New(), id.New(), id.New(), id.New()
			n := &controlE2ENative{db: db, binding: binding, descriptor: native.InteractionGroupDescriptor{Group: group, Binding: native.InteractionGroupBinding{Instance: binding.Instance, Installation: binding.Installation, Community: binding.Community, BotUser: id.New(), ParticipantUser: user, ParticipantDevice: device, PolicyVersion: 1, E2EEVersion: 1}, BotDevice: botDevice, Epoch: 1, State: 2}}
			databaseKey := make([]byte, 32)
			databaseKey[0] = 1
			n.Client, err = native.Start(ctx, helper.Options{Executable: binary, StateDir: filepath.Join(t.TempDir(), "native-helper")}, binding, databaseKey)
			if err != nil {
				t.Fatal(err)
			}
			defer n.Close()
			n.next.Store(1)
			gateway := newBotGatewayRuntime(ctx, nil, true, slog.New(slog.NewTextHandler(io.Discard, nil)))
			gateway.StartCommandWorkers()

			var started atomic.Int64
			var admissions, contexts, pages, cursors, uploads, authorityReads, historyReads atomic.Int32
			blockedChannel := id.New()
			sweepEntered, sweepDone := make(chan struct{}), make(chan struct{})
			jsonReply := func(w http.ResponseWriter, v any) {
				w.Header().Set("Content-Type", "application/json")
				if err := json.NewEncoder(w).Encode(v); err != nil {
					t.Error(err)
				}
			}
			cborReply := func(w http.ResponseWriter, v any) {
				b, e := cborx.Marshal(v)
				if e != nil {
					t.Error(e)
					w.WriteHeader(500)
					return
				}
				w.Header().Set("Content-Type", "application/cbor")
				_, _ = w.Write(b)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/fixture":
					jsonReply(w, map[string]any{"instance": binding.Instance.String(), "community": binding.Community.String(), "installation": binding.Installation.String(), "channel": channel.String(), "user": user.String(), "device": device.String(), "operation": operation})
				case r.URL.Path == "/arm":
					n.loads.Store(0)
					gateway.pendingMu.Lock()
					budget := gateway.budgetLocked(binding.Installation, group)
					budget.refill(time.Now())
					budget.tokens = 0
					gateway.pendingMu.Unlock()
					w.WriteHeader(204)
				case r.URL.Path == "/core":
					var input struct {
						M          string   `json:"m"`
						RequestID  string   `json:"requestId"`
						Operation  uint64   `json:"operation"`
						Args       []any    `json:"args"`
						Revision   *string  `json:"revision"`
						RequestIDs []string `json:"requestIds"`
						Operations []uint64 `json:"operations"`
						StartedAt  *int64   `json:"startedAt"`
					}
					if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
						t.Error(err)
						w.WriteHeader(400)
						return
					}
					if input.StartedAt != nil {
						started.CompareAndSwap(0, *input.StartedAt)
					}
					switch input.M {
					case "getBotCommandStatus":
						jsonReply(w, map[string]any{"installationId": binding.Installation.String(), "botUserId": n.descriptor.Binding.BotUser.String(), "state": 3, "revision": "1"})
					case "humanInteractionRequest":
						requestID, e := id.Parse(input.RequestID)
						if e != nil {
							t.Error(e)
							w.WriteHeader(400)
							return
						}
						req := protocol.Request{RequestID: requestID, CommunityID: binding.Community, ChannelID: &channel, Operation: protocol.Operation(input.Operation), Args: input.Args}
						if input.Revision != nil {
							revision, e := strconv.ParseUint(*input.Revision, 10, 64)
							if e != nil {
								t.Error(e)
								w.WriteHeader(400)
								return
							}
							expires := uint64(time.Now().Unix() + 20)
							req.Revision = &revision
							req.Expires = &expires
						}
						body, e := protocol.EncodeRequest(req)
						if e != nil {
							t.Errorf("encode client request: %v", e)
							w.WriteHeader(400)
							return
						}
						if _, e = db.ExecContext(r.Context(), "INSERT INTO source(body) VALUES(?)", body); e != nil {
							t.Error(e)
							w.WriteHeader(500)
							return
						}
						uploads.Add(1)
						gateway.notifyInteraction(binding.Installation, group)
						jsonReply(w, map[string]any{"requestId": input.RequestID, "upload": "accepted"})
					case "readBotInteractionResponses":
						responses := []any{}
						for _, request := range input.RequestIDs {
							var body string
							e := db.QueryRowContext(r.Context(), "SELECT body FROM responses WHERE request=?", request).Scan(&body)
							if errors.Is(e, sql.ErrNoRows) {
								continue
							}
							if e != nil {
								t.Error(e)
								w.WriteHeader(500)
								return
							}
							response, e := protocol.DecodeResponse(body)
							if e != nil {
								t.Error(e)
								w.WriteHeader(500)
								return
							}
							var revision any
							if response.Revision != nil {
								revision = strconv.FormatUint(*response.Revision, 10)
							}
							data := make([]int, len(response.Data))
							for i, b := range response.Data {
								data[i] = int(b)
							}
							responses = append(responses, map[string]any{"requestId": request, "operation": input.Operations[0], "status": response.Status, "revision": revision, "data": data})
						}
						jsonReply(w, map[string]any{"responses": responses, "through": nil})
					default:
						t.Errorf("unexpected client bridge call %s", input.M)
						w.WriteHeader(400)
					}
				case r.URL.Path == "/v1/channels/"+blockedChannel.String()+"/call":
					close(sweepEntered)
					select {
					case <-r.Context().Done():
					case <-time.After(10 * time.Second):
					}
					w.WriteHeader(503)
				case r.URL.Path == "/v1/bot-runtime/installations":
					authorityReads.Add(1)
					cborReply(w, []any{[]any{binding.Installation, binding.Community, uint64(3), uint64(1), n.descriptor.Binding.BotUser, botDevice, uint64(1), uint64(time.Now().Unix()), uint64(0), uint64(0), nil, uint64(100), uint64(5)}})
				case r.URL.Path == "/v1/channels/"+channel.String()+"/call":
					authorityReads.Add(1)
					cborReply(w, []any{group, channel, []any{[]any{user, device, uint64(0)}}})
				case r.URL.Path == "/v1/channels/"+channel.String()+"/permissions":
					authorityReads.Add(1)
					cborReply(w, uint64(1<<7|1<<8))
				case strings.Contains(r.URL.Path, "/bot/actors/") && strings.Contains(r.URL.Path, "/channels/"):
					authorityReads.Add(1)
					cborReply(w, []any{binding.Community, user, channel, permbits.ViewChannel | permbits.ControlMusic})
				case strings.Contains(r.URL.Path, "/bot/actors/") && strings.HasSuffix(r.URL.Path, "/authority"):
					authorityReads.Add(1)
					cborReply(w, []any{permbits.ViewChannel | permbits.ControlMusic, uint64(0), uint64(0), []any{}})
				case strings.HasSuffix(r.URL.Path, "/device-list") && r.URL.Query().Has("after"):
					historyReads.Add(1)
					cborReply(w, []any{[]any{uint64(1), []byte("signed-device-list"), make([]byte, 64), make([]byte, 32)}})
				case strings.HasSuffix(r.URL.Path, "/device-list"):
					historyReads.Add(1)
					cborReply(w, []any{uint64(1), []byte("signed-device-list"), make([]byte, 64), make([]byte, 32)})
				case r.URL.Path == "/v1/instance":
					cborReply(w, []any{[]any{uint64(3), uint64(2)}, []any{uint64(1)}, []any{uint64(1)}, binding.Instance, uint64(1), "test.local", uint64(0), []any{uint64(0)}, uint64(1), id.New(), make([]byte, 32)})
				case strings.HasSuffix(r.URL.Path, "/interaction-admissions"):
					admissions.Add(1)
					cborReply(w, []any{[]byte{1}, make([]byte, 64)})
				case strings.HasSuffix(r.URL.Path, "/interaction-admissions/context"):
					contexts.Add(1)
					var request []any
					if e := cborx.Unmarshal(readControlBody(t, r), &request); e != nil {
						t.Error(e)
						w.WriteHeader(400)
						return
					}
					b := n.descriptor.Binding
					payload, e := cborx.Marshal([]any{uint64(1), request[0], id.New(), []byte{1}, make([]byte, 64), uint64(time.Now().Unix()), uint64(1), uint64(1), uint64(1), uint64(0), id.New(), id.New(), make([]byte, 32), []any{b.Instance, b.Installation, b.Community, b.BotUser, botDevice, user, device, group}, []any{}, []any{}})
					if e != nil {
						t.Error(e)
						w.WriteHeader(500)
						return
					}
					cborReply(w, []any{payload, make([]byte, 64)})
				case strings.HasSuffix(r.URL.Path, "/info"):
					var head uint64
					if e := db.QueryRowContext(r.Context(), "SELECT COALESCE(MAX(seq),0)+1 FROM source").Scan(&head); e != nil {
						t.Error(e)
						w.WriteHeader(500)
						return
					}
					cborReply(w, []any{uint64(1), []byte{1}, make([]byte, 32), head})
				case strings.HasSuffix(r.URL.Path, "/handshakes"):
					cborReply(w, []any{})
				case strings.HasSuffix(r.URL.Path, "/messages"):
					pages.Add(1)
					from, _ := strconv.ParseUint(r.URL.Query().Get("from"), 10, 64)
					rows, e := db.QueryContext(r.Context(), "SELECT seq,body FROM source WHERE seq>=? ORDER BY seq", from)
					if e != nil {
						t.Error(e)
						w.WriteHeader(500)
						return
					}
					defer rows.Close()
					values := []any{}
					for rows.Next() {
						var seq uint64
						var body string
						if e = rows.Scan(&seq, &body); e != nil {
							t.Error(e)
							w.WriteHeader(500)
							return
						}
						values = append(values, []any{seq, uint64(1), id.New(), []byte(body), nil, make([]byte, 32), uint64(time.Now().Unix()), uint64(0), nil})
					}
					if e = rows.Err(); e != nil {
						t.Error(e)
						w.WriteHeader(500)
						return
					}
					cborReply(w, values)
				case strings.HasSuffix(r.URL.Path, "/cursor"):
					cursors.Add(1)
					w.WriteHeader(204)
				default:
					t.Errorf("unexpected HTTP %s %s", r.Method, r.URL.Path)
					w.WriteHeader(404)
				}
			}))
			defer func() { cancel(); gateway.Close(); server.Close() }()
			api, e := client.New(client.Config{BaseURL: server.URL, InstanceID: binding.Instance, AllowLoopbackHTTP: true})
			if e != nil {
				t.Fatal(e)
			}
			if _, e = api.Discover(ctx); e != nil {
				t.Fatal(e)
			}
			n.session, e = api.Session("test-token")
			if e != nil {
				t.Fatal(e)
			}
			store, e := persistence.New(n, binding.Community)
			if e != nil {
				t.Fatal(e)
			}
			starter := &controlLatencyStarter{entered: make(chan struct{}), cancelled: make(chan struct{}), release: make(chan struct{}), playback: &controlLatencyPlayback{done: make(chan struct{})}}
			supervisor, e := music.NewSupervisor(ctx, channel, persistedMusicQueue{store}, starter)
			if e != nil {
				t.Fatal(e)
			}
			defer supervisor.Close()
			tracks := []music.Track{{ID: id.New().String(), Provider: music.ProviderYouTube, CanonicalID: "abcdefghijk", Title: "First", RequesterID: user.String()}, {ID: id.New().String(), Provider: music.ProviderYouTube, CanonicalID: "bcdefghijkl", Title: "Next", RequesterID: user.String()}}
			queued, e := supervisor.Apply(ctx, 0, music.Operation{Kind: music.OpEnqueue, Tracks: tracks})
			if e != nil {
				t.Fatal(e)
			}
			if _, e = supervisor.Apply(ctx, queued.Revision, music.Operation{Kind: music.OpResume}); e != nil {
				t.Fatal(e)
			}
			queues := &musicQueueRuntime{store: store, community: binding.Community, supervisors: map[id.ID]*music.Supervisor{channel: supervisor}}
			registry := &musicQueueRegistry{runtimes: map[id.ID]*retainedMusicRuntime{binding.Installation: {community: binding.Community, queues: queues, native: n}}}
			runtimeAPI, e := api.Runtime("runtime-token")
			if e != nil {
				t.Fatal(e)
			}
			authority := &service.CommunityMusicInteractionAuthority{Session: n.session, Store: runtimeMusicAuthorityStore{runtime: runtimeAPI, session: n.session, installation: binding.Installation, community: binding.Community}, InstallationID: binding.Installation, CommunityID: binding.Community}
			h := service.MusicInteractions{Store: store, Authority: authority, InstallationID: binding.Installation, CommunityID: binding.Community, CurrentState: registry.CurrentState, OnImmediateControl: registry.applyImmediateControl}
			dispatcher, e := service.NewInteractionDispatcherWithSnapshots(n, h.Handlers(), func() uint64 { return uint64(time.Now().Unix()) }, n)
			if e != nil {
				t.Fatal(e)
			}
			poller, e := service.NewGroupEventPoller(n)
			if e != nil {
				t.Fatal(e)
			}
			events := newInteractionEventRuntime(poller, newInteractionEventHandler(n, dispatcher, controlE2ESender{db}))
			membership, e := service.NewMembershipReconciler(n, service.SourceSyncOptions{}, 0)
			if e != nil {
				t.Fatal(e)
			}
			trust, e := service.NewSourceTrust(n)
			if e != nil {
				t.Fatal(e)
			}
			failures := make(chan error, 1)
			intake := newInteractionIntakePoller(membership, trust, n.session, events, func(service.TrustRefresh) {})
			gateway.SetInteractionPoller(binding.Installation, func(turn context.Context, group id.ID) error {
				err := intake(turn, group)
				if err != nil {
					select {
					case failures <- err:
					default:
					}
				}
				return err
			})
			if duringSweep {
				gateway.SetTextPoller(binding.Installation, func(bg context.Context) error { _, err := n.session.ChannelCall(bg, blockedChannel); return err })
				go func() { defer close(sweepDone); gateway.PollText(ctx) }()
				select {
				case <-sweepEntered:
				case <-time.After(time.Second):
					t.Fatal("shared-session background sweep never blocked")
				}
			}
			runCtx, runCancel := context.WithTimeout(ctx, 10*time.Second)
			defer runCancel()
			node := exec.CommandContext(runCtx, "node", bundle, server.URL)
			completed := make(chan error, 1)
			var output strings.Builder
			node.Stdout = &output
			node.Stderr = &output
			go func() { completed <- node.Run() }()
			select {
			case <-starter.playback.done:
			case err := <-failures:
				t.Fatalf("interaction intake: %v", err)
			case e := <-completed:
				t.Fatalf("client ended before audio changed: %v %s", e, output.String())
			case <-time.After(5 * time.Second):
				t.Fatal("client control did not reach playback")
			}
			elapsed := time.Unix(0, starter.playback.closedAt.Load()).Sub(time.UnixMilli(started.Load()))
			if started.Load() == 0 || elapsed >= time.Second {
				t.Fatalf("client invocation to Playback.Close %s, must be <1s", elapsed)
			}
			if duringSweep {
				select {
				case <-sweepDone:
					t.Fatal("background sweep finished before control")
				default:
				}
			}
			select {
			case e := <-completed:
				if e != nil {
					t.Fatalf("client: %v %s", e, output.String())
				}
			case <-time.After(5 * time.Second):
				t.Fatal("control response never confirmed")
			}
			if loads := n.loads.Load(); loads != 1 {
				t.Fatalf("control used %d music Store.Load calls, want exactly one", loads)
			}
			if authorityReads.Load() < 20 || historyReads.Load() < 6 {
				t.Fatalf("authority/trust HTTP work skipped: authority=%d history=%d", authorityReads.Load(), historyReads.Load())
			}
			if admissions.Load() == 0 || contexts.Load() == 0 || pages.Load() < 2 || cursors.Load() < 2 || n.applies.Load() < 2 || n.trustReads.Load() < 2 || n.eventReads.Load() < 2 || uploads.Load() != 2 {
				t.Fatalf("intake work skipped: admissions=%d context=%d pages=%d cursors=%d apply=%d trust=%d events=%d uploads=%d", admissions.Load(), contexts.Load(), pages.Load(), cursors.Load(), n.applies.Load(), n.trustReads.Load(), n.eventReads.Load(), uploads.Load())
			}
			t.Logf("client-core %s -> HTTP upload/admission/context/source-sync/trust/durable-event intake -> handler/supervisor/SQLite -> Playback.Close: %s (blocked sweep=%v; music loads=1)", operation, elapsed, duringSweep)
		})
	}
}
func readControlBody(t *testing.T, r *http.Request) []byte {
	t.Helper()
	b, e := io.ReadAll(r.Body)
	if e != nil {
		t.Error(e)
	}
	return b
}
