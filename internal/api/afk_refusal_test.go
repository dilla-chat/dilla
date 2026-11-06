package api_test

import (
	"log/slog"
	"net/http"
	"sync"
	"testing"

	"github.com/fxamacker/cbor/v2"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

// refusalEnv is a community created through the route (so with its built-in AFK channel), the
// community, channel and role routes, an ordinary voice channel lounge, and a member.
type refusalEnv struct {
	e                *env
	cid, afk, lounge id.ID
	owner            id.ID
	ownerTok         string
	member           id.ID
	memberTok        string
	everyone         id.ID
}

func newRefusalEnv(t *testing.T) *refusalEnv {
	t.Helper()
	e, cid, tok := channelEnv(t)
	api.NewRoles(e.Repo, e.Clk, "dilla.example", slog.New(slog.DiscardHandler)).WithDS(e.DS).Register(e.Mux)
	afk, _ := afkPointer(t, e, cid)
	lounge, _, status := newChannel(t, e, cid, tok, uint64(api.ChannelVoice), 0, 0, "lounge")
	if status != http.StatusCreated {
		t.Fatalf("lounge = %d", status)
	}
	member, memberTok := e.NewUser("member")
	joinCommunity(t, e, cid, memberTok)
	roles, err := e.Repo.ListRoles(t.Context(), cid)
	if err != nil || len(roles) != 1 {
		t.Fatalf("ListRoles = %v, %v", roles, err)
	}
	return &refusalEnv{e: e, cid: cid, afk: afk, lounge: lounge, owner: e.sess[tok].UserID, ownerTok: tok,
		member: member, memberTok: memberTok, everyone: roles[0].ID}
}

func (r *refusalEnv) refused(t *testing.T, what string, status int, body []byte, wantStatus int, wantCode string) {
	t.Helper()
	if status != wantStatus {
		t.Fatalf("%s = %d (%x), want %d", what, status, body, wantStatus)
	}
	if wantCode != "" {
		if code := r.e.ErrCode(body); code != wantCode {
			t.Fatalf("%s code = %s, want %s", what, code, wantCode)
		}
	}
}

func TestAFKDeleteIsRefused(t *testing.T) {
	r := newRefusalEnv(t)
	status, body := r.e.Do(http.MethodDelete, "/v1/channels/"+r.afk.String(), r.ownerTok, nil)
	r.refused(t, "delete AFK", status, body, http.StatusForbidden, "E_FORBIDDEN")
	if _, err := r.e.Repo.GetChannel(t.Context(), r.afk); err != nil {
		t.Fatalf("AFK channel after a refused delete: %v", err)
	}
	if ptr, _ := afkPointer(t, r.e, r.cid); ptr != r.afk {
		t.Fatal("a refused delete moved the pointer")
	}
	// Control: an ordinary voice channel deletes.
	if status, body := r.e.Do(http.MethodDelete, "/v1/channels/"+r.lounge.String(), r.ownerTok, nil); status != http.StatusNoContent {
		t.Fatalf("delete lounge = %d (%x)", status, body)
	}
}

func TestAFKDeleteRaceStaysRefused(t *testing.T) {
	r := newRefusalEnv(t)
	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, err := http.NewRequestWithContext(t.Context(), http.MethodDelete, r.e.Srv.URL+"/v1/channels/"+r.afk.String(), nil)
			if err != nil {
				return
			}
			req.Header.Set("Authorization", "Bearer "+r.ownerTok)
			res, err := r.e.Srv.Client().Do(req)
			if err != nil {
				return
			}
			_ = res.Body.Close()
			codes[i] = res.StatusCode
		}()
	}
	wg.Wait()
	for i, c := range codes {
		if c != http.StatusForbidden {
			t.Fatalf("concurrent delete %d = %d, want 403", i, c)
		}
	}
	if _, err := r.e.Repo.GetChannel(t.Context(), r.afk); err != nil {
		t.Fatalf("AFK channel after the race: %v", err)
	}
}

func TestAFKPatchRefusals(t *testing.T) {
	r := newRefusalEnv(t)
	// The patcher is a member with manage_channels who cannot view one category (R44's control).
	mod := createRole(t, r.e, r.cid, r.ownerTok, "mod", 5, api.PermManageChannels)
	grant(t, r.e, r.cid, r.ownerTok, r.member, mod, http.StatusNoContent)
	open, _, status := newChannel(t, r.e, r.cid, r.ownerTok, uint64(api.ChannelCategory), 0, 0, "open")
	if status != http.StatusCreated {
		t.Fatalf("open category = %d", status)
	}
	hidden, _, status := newChannel(t, r.e, r.cid, r.ownerTok, uint64(api.ChannelCategory), 0, 0, "hidden")
	if status != http.StatusCreated {
		t.Fatalf("hidden category = %d", status)
	}
	denyInChannel(t, r.e, hidden, r.member, api.PermViewChannel)
	name, topic := "away", "a topic"
	one, pos := uint64(1), uint64(api.AFKBuiltinPosition)
	zero := uint64(0)
	path := "/v1/channels/" + r.afk.String()
	// [name, topic, mode, visibility, parent_id, position, slowmode_seconds]
	for _, tc := range []struct {
		name     string
		body     []any
		want     int
		wantCode string
	}{
		{"topic", []any{nil, topic, nil, nil, nil, nil, nil}, http.StatusForbidden, "E_FORBIDDEN"},
		{"mode", []any{nil, nil, one, nil, nil, nil, nil}, http.StatusForbidden, "E_FORBIDDEN"},
		{"visibility", []any{nil, nil, nil, one, nil, nil, nil}, http.StatusForbidden, "E_FORBIDDEN"},
		{"slow mode", []any{nil, nil, nil, nil, nil, nil, one}, http.StatusForbidden, "E_FORBIDDEN"},
		{"mode unchanged is still refused", []any{nil, nil, zero, nil, nil, nil, nil}, http.StatusForbidden, "E_FORBIDDEN"},
		{"name (control)", []any{name, nil, nil, nil, nil, nil, nil}, http.StatusNoContent, ""},
		{"position 2^30, unchanged (control)", []any{nil, nil, nil, nil, nil, pos, nil}, http.StatusNoContent, ""},
		{"position 7 (R-AFK8)", []any{nil, nil, nil, nil, nil, uint64(7), nil}, http.StatusForbidden, "E_FORBIDDEN"},
		{"parent to a visible category (control)", []any{nil, nil, nil, nil, open[:], nil, nil}, http.StatusNoContent, ""},
		{"parent to a hidden category (R44)", []any{nil, nil, nil, nil, hidden[:], nil, nil}, http.StatusNotFound, "E_NOT_FOUND"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, body := r.e.Do(http.MethodPatch, path, r.memberTok, tc.body)
			r.refused(t, "PATCH "+tc.name, status, body, tc.want, tc.wantCode)
		})
	}
	row, err := r.e.Repo.GetChannel(t.Context(), r.afk)
	if err != nil {
		t.Fatal(err)
	}
	if row.Name != name || row.Position != pos || row.ParentID == nil || *row.ParentID != open ||
		row.Topic != "" || row.Mode != api.ModeE2EE || row.Visibility != api.VisPrivate || row.SlowmodeSeconds != 0 ||
		row.Kind != api.ChannelVoice {
		t.Fatalf("AFK after the patches = %+v", row)
	}
	// Control for the refused fields: the same topic on lounge lands.
	if status, body := r.e.Do(http.MethodPatch, "/v1/channels/"+r.lounge.String(), r.memberTok,
		[]any{nil, topic, nil, nil, nil, nil, nil}); status != http.StatusNoContent {
		t.Fatalf("topic on lounge = %d (%x)", status, body)
	}
}

// R-AFK8 (server half): the built-in AFK channel stays at 2^30, so a PATCH naming any other position is
// refused with the other AFK refusals' code, and the same request on an ordinary channel lands.
func TestAFKPositionPatchIsRefused(t *testing.T) {
	r := newRefusalEnv(t)
	mod := createRole(t, r.e, r.cid, r.ownerTok, "mod", 5, api.PermManageChannels)
	grant(t, r.e, r.cid, r.ownerTok, r.member, mod, http.StatusNoContent)
	builtin := uint64(api.AFKBuiltinPosition)
	// [name, topic, mode, visibility, parent_id, position, slowmode_seconds]
	for _, tc := range []struct {
		name string
		ch   id.ID
		pos  uint64
		want int
		code string
	}{
		{"AFK to 7", r.afk, 7, http.StatusForbidden, "E_FORBIDDEN"},
		{"AFK to 0", r.afk, 0, http.StatusForbidden, "E_FORBIDDEN"},
		{"AFK to 2^30-1", r.afk, builtin - 1, http.StatusForbidden, "E_FORBIDDEN"},
		{"AFK to 2^30+1", r.afk, builtin + 1, http.StatusForbidden, "E_FORBIDDEN"},
		{"AFK at 2^30 (control)", r.afk, builtin, http.StatusNoContent, ""},
		{"lounge to 7 (control)", r.lounge, 7, http.StatusNoContent, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The owner and a member with manage_channels are refused alike.
			for _, tok := range []string{r.memberTok, r.ownerTok} {
				status, body := r.e.Do(http.MethodPatch, "/v1/channels/"+tc.ch.String(), tok,
					[]any{nil, nil, nil, nil, nil, tc.pos, nil})
				r.refused(t, "PATCH position "+tc.name, status, body, tc.want, tc.code)
			}
		})
	}
	// A rename that also names a position is refused whole; the name alone still lands.
	status, body := r.e.Do(http.MethodPatch, "/v1/channels/"+r.afk.String(), r.ownerTok,
		[]any{"away", nil, nil, nil, nil, uint64(7), nil})
	r.refused(t, "PATCH name and position", status, body, http.StatusForbidden, "E_FORBIDDEN")
	if row, err := r.e.Repo.GetChannel(t.Context(), r.afk); err != nil || row.Name != "AFK" {
		t.Fatalf("AFK after a refused rename = %+v, %v; want it unchanged", row, err)
	}
	status, body = r.e.Do(http.MethodPatch, "/v1/channels/"+r.afk.String(), r.ownerTok,
		[]any{"away", nil, nil, nil, nil, nil, nil})
	r.refused(t, "PATCH name (control)", status, body, http.StatusNoContent, "")
	row, err := r.e.Repo.GetChannel(t.Context(), r.afk)
	if err != nil || row.Position != builtin || row.Name != "away" {
		t.Fatalf("AFK after the patches = %+v, %v; want away at 2^30", row, err)
	}
	if row, err := r.e.Repo.GetChannel(t.Context(), r.lounge); err != nil || row.Position != 7 {
		t.Fatalf("lounge after the control = %+v, %v; want position 7", row, err)
	}
}

// R-AFK8 (server half): 2^30 and above is the built-in AFK channel's, so an ordinary channel's PATCH
// there is a validation error; 2^30-1 is the control.
func TestAFKReservedPositionRefusedOnAnOrdinaryChannel(t *testing.T) {
	r := newRefusalEnv(t)
	builtin := uint64(api.AFKBuiltinPosition)
	for _, tc := range []struct {
		name string
		pos  uint64
		want int
		code string
	}{
		{"2^30", builtin, http.StatusBadRequest, "E_INVALID_REQUEST"},
		{"2^30+1", builtin + 1, http.StatusBadRequest, "E_INVALID_REQUEST"},
		{"2^31-1", 1<<31 - 1, http.StatusBadRequest, "E_INVALID_REQUEST"},
		{"2^30-1 (control)", builtin - 1, http.StatusNoContent, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, body := r.e.Do(http.MethodPatch, "/v1/channels/"+r.lounge.String(), r.ownerTok,
				[]any{nil, nil, nil, nil, nil, tc.pos, nil})
			r.refused(t, "PATCH lounge to "+tc.name, status, body, tc.want, tc.code)
		})
	}
	if row, err := r.e.Repo.GetChannel(t.Context(), r.lounge); err != nil || row.Position != builtin-1 {
		t.Fatalf("lounge = %+v, %v; want position 2^30-1 from the control", row, err)
	}
}

// R-AFK8 (server half): the same reserved range on create; 2^30-1 is the control.
func TestAFKReservedPositionRefusedOnCreate(t *testing.T) {
	r := newRefusalEnv(t)
	builtin := uint64(api.AFKBuiltinPosition)
	path := "/v1/communities/" + r.cid.String() + "/channels"
	for _, tc := range []struct {
		name string
		pos  uint64
		want int
		code string
	}{
		{"2^30", builtin, http.StatusBadRequest, "E_INVALID_REQUEST"},
		{"2^30+1", builtin + 1, http.StatusBadRequest, "E_INVALID_REQUEST"},
		{"2^31-1", 1<<31 - 1, http.StatusBadRequest, "E_INVALID_REQUEST"},
		{"2^30-1 (control)", builtin - 1, http.StatusCreated, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// [kind, mode, visibility, parent_id, name, topic, position, slowmode_seconds]
			status, body := r.e.Do(http.MethodPost, path, r.ownerTok,
				[]any{uint64(api.ChannelVoice), uint64(0), uint64(0), nil, "at " + tc.name, "", tc.pos, uint64(0)})
			r.refused(t, "POST at "+tc.name, status, body, tc.want, tc.code)
		})
	}
	all, err := r.e.Repo.ListChannels(t.Context(), r.cid)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range all {
		if c.ID != r.afk && c.Position >= builtin {
			t.Fatalf("an ordinary channel at %d after the refused creates: %+v", c.Position, c)
		}
	}
}

func TestAFKOverwriteRefused(t *testing.T) {
	r := newRefusalEnv(t)
	for _, ch := range []struct {
		name string
		id   id.ID
		want int
	}{{"AFK", r.afk, http.StatusForbidden}, {"lounge (control)", r.lounge, http.StatusNoContent}} {
		for _, target := range []struct {
			kind string
			id   id.ID
		}{{"0", r.everyone}, {"1", r.member}} {
			path := "/v1/channels/" + ch.id.String() + "/overwrites/" + target.kind + "/" + target.id.String()
			status, body := r.e.Do(http.MethodPut, path, r.ownerTok, []any{uint64(0), uint64(api.PermSpeak)})
			if status != ch.want {
				t.Fatalf("PUT %s kind %s = %d (%x), want %d", ch.name, target.kind, status, body, ch.want)
			}
			if ch.want == http.StatusForbidden && r.e.ErrCode(body) != "E_FORBIDDEN" {
				t.Fatalf("PUT %s code = %s", ch.name, r.e.ErrCode(body))
			}
			status, body = r.e.Do(http.MethodDelete, path, r.ownerTok, nil)
			if status != ch.want {
				t.Fatalf("DELETE %s kind %s = %d (%x), want %d", ch.name, target.kind, status, body, ch.want)
			}
		}
	}
	if ow, err := r.e.Repo.ListOverwrites(t.Context(), r.afk); err != nil || len(ow) != 0 {
		t.Fatalf("AFK overwrites = %v, %v", ow, err)
	}
	// A DELETE of an overwrite written under the store (a web3.8 row) is refused too.
	if err := r.e.Repo.PutOverwrite(t.Context(), store.OverwriteRow{ChannelID: r.afk, TargetKind: 1, TargetID: r.member,
		Deny: uint64(api.PermSpeak)}); err != nil {
		t.Fatal(err)
	}
	path := "/v1/channels/" + r.afk.String() + "/overwrites/1/" + r.member.String()
	if status, _ := r.e.Do(http.MethodDelete, path, r.ownerTok, nil); status != http.StatusForbidden {
		t.Fatalf("DELETE of a stored AFK overwrite = %d, want 403", status)
	}
}

func TestAFKSurvivesTransferAndCategoryDelete(t *testing.T) {
	r := newRefusalEnv(t)
	cat, _, status := newChannel(t, r.e, r.cid, r.ownerTok, uint64(api.ChannelCategory), 0, 0, "voice")
	if status != http.StatusCreated {
		t.Fatalf("category = %d", status)
	}
	if status, body := r.e.Do(http.MethodPatch, "/v1/channels/"+r.afk.String(), r.ownerTok,
		[]any{nil, nil, nil, nil, cat[:], nil, nil}); status != http.StatusNoContent {
		t.Fatalf("move AFK into the category = %d (%x)", status, body)
	}
	if status, body := r.e.Do(http.MethodDelete, "/v1/channels/"+cat.String(), r.ownerTok, nil); status != http.StatusNoContent {
		t.Fatalf("delete category = %d (%x)", status, body)
	}
	row, err := r.e.Repo.GetChannel(t.Context(), r.afk)
	if err != nil || row.ParentID != nil {
		t.Fatalf("AFK after its category's delete = %+v, %v; want live at the top level", row, err)
	}
	if ptr, _ := afkPointer(t, r.e, r.cid); ptr != r.afk {
		t.Fatal("a category delete moved the pointer")
	}
	if status, body := r.e.Do(http.MethodPost, "/v1/communities/"+r.cid.String()+"/owner", r.ownerTok,
		[]any{r.member[:]}); status != http.StatusNoContent {
		t.Fatalf("transfer = %d (%x)", status, body)
	}
	if ptr, _ := afkPointer(t, r.e, r.cid); ptr != r.afk {
		t.Fatal("a transfer moved the pointer")
	}
	if status, _ := r.e.Do(http.MethodDelete, "/v1/channels/"+r.afk.String(), r.memberTok, nil); status != http.StatusForbidden {
		t.Fatalf("the new owner's delete of AFK = %d, want 403", status)
	}
}

func TestAFKSettingsBody(t *testing.T) {
	r := newRefusalEnv(t)
	path := "/v1/communities/" + r.cid.String() + "/afk/settings"
	for _, tc := range []struct {
		name    string
		body    []any
		want    int
		timeout uint64 // the stored timeout afterwards
	}{
		{"[300]", []any{uint64(300)}, http.StatusNoContent, 300},
		{"[built-in, 900]", []any{r.afk[:], uint64(900)}, http.StatusNoContent, 900},
		{"[lounge, 300]", []any{r.lounge[:], uint64(300)}, http.StatusBadRequest, 900},
		{"[null, 300]", []any{nil, uint64(300)}, http.StatusBadRequest, 900},
		{"[120]", []any{uint64(120)}, http.StatusBadRequest, 900},
		{"[]", []any{}, http.StatusBadRequest, 900},
		{"[60] (control)", []any{uint64(60)}, http.StatusNoContent, 60},
	} {
		status, body := r.e.Do(http.MethodPut, path, r.ownerTok, tc.body)
		if status != tc.want {
			t.Fatalf("PUT %s = %d (%x), want %d", tc.name, status, body, tc.want)
		}
		if tc.want == http.StatusBadRequest && r.e.ErrCode(body) != "E_INVALID_REQUEST" {
			t.Fatalf("PUT %s code = %s", tc.name, r.e.ErrCode(body))
		}
		ptr, timeout := afkPointer(t, r.e, r.cid)
		if ptr != r.afk || timeout != tc.timeout {
			t.Fatalf("after PUT %s the pointer is %s %d, want %s %d", tc.name, ptr, timeout, r.afk, tc.timeout)
		}
	}
	// A member without manage_community is refused, and the GET names the built-in channel.
	if status, _ := r.e.Do(http.MethodPut, path, r.memberTok, []any{uint64(300)}); status != http.StatusForbidden {
		t.Fatalf("member PUT = %d, want 403", status)
	}
	status, body := r.e.Do(http.MethodGet, path, r.ownerTok, nil)
	if status != http.StatusOK {
		t.Fatalf("GET settings = %d", status)
	}
	var got []cbor.RawMessage
	mustUnmarshal(t, body, &got)
	var ch id.ID
	var timeout uint64
	mustUnmarshal(t, got[0], &ch)
	mustUnmarshal(t, got[1], &timeout)
	if len(got) != 2 || ch != r.afk || timeout != 60 {
		t.Fatalf("GET settings = %s %d", ch, timeout)
	}
}

func TestRetiredAFKRoutesAreGone(t *testing.T) {
	r := newRefusalEnv(t)
	api.NewCalls(r.e.Repo, api.NewResolver(r.e.Repo), &stubSFU{}, api.CallsConfig{LiveKitURL: testLiveKitURL}, r.e.Clk,
		slog.New(slog.DiscardHandler)).Register(r.e.Mux)
	path := "/v1/communities/" + r.cid.String() + "/afk"
	for _, m := range []struct {
		method string
		body   any
	}{{http.MethodPost, []any{r.lounge[:]}}, {http.MethodDelete, nil}} {
		if status, _ := r.e.Do(m.method, path, r.memberTok, m.body); status != http.StatusNotFound && status != http.StatusMethodNotAllowed {
			t.Fatalf("%s /afk = %d, want 404 or 405 (retired in web3.9, no longer registered)", m.method, status)
		}
	}
	// Control: the snapshot route next to them still answers.
	if status, _ := r.e.Do(http.MethodGet, path, r.memberTok, nil); status != http.StatusOK {
		t.Fatalf("GET /afk = %d", status)
	}
}
