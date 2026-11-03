package api_test

import (
	"net/http"
	"testing"

	"github.com/fxamacker/cbor/v2"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/id"
)

// R44 (web3.9, SRV-CAT): a channel's parent must be a category its author can see, and the
// listing never reveals a category the caller cannot view.

// parentFixture is privateEnv plus two categories made by the owner: hidden, whose @everyone
// overwrite denies view_channel and which grants nothing to the moderator's role, and open,
// which everyone may view.
type parentFixture struct {
	privateFixture
	hidden, open id.ID
}

func parentEnv(t *testing.T) parentFixture {
	t.Helper()
	f := parentFixture{privateFixture: privateEnv(t)}
	f.hidden = createUnder(t, f.e, f.cid, f.ownerTok, uint64(api.ChannelCategory), nil, "hidden", http.StatusCreated)
	f.open = createUnder(t, f.e, f.cid, f.ownerTok, uint64(api.ChannelCategory), nil, "open", http.StatusCreated)
	if status, body := f.e.Do(http.MethodPut, "/v1/channels/"+f.hidden.String()+"/overwrites/0/"+f.everyone.String(),
		f.ownerTok, []any{uint64(0), uint64(api.PermViewChannel)}); status != http.StatusNoContent {
		t.Fatalf("PUT @everyone deny view on the category = %d (%x)", status, body)
	}
	return f
}

// createUnder posts a channel of kind named name under parent (nil for the top level) and
// requires want; on 201 it returns the new channel's id.
func createUnder(t *testing.T, e *env, cid id.ID, tok string, kind uint64, parent *id.ID, name string, want int) id.ID {
	t.Helper()
	var p any
	if parent != nil {
		p = *parent
	}
	status, body := e.Do(http.MethodPost, "/v1/communities/"+cid.String()+"/channels", tok,
		[]any{kind, uint64(0), uint64(0), p, name, "", uint64(0), uint64(0)})
	if status != want {
		t.Fatalf("create %q under %v = %d, want %d (%x)", name, parent, status, want, body)
	}
	if status == http.StatusNotFound && e.ErrCode(body) != "E_NOT_FOUND" {
		t.Fatalf("create %q: code %q, want E_NOT_FOUND", name, e.ErrCode(body))
	}
	if status != http.StatusCreated {
		return id.ID{}
	}
	return createdID(t, body)
}

// listed is one row of GET /v1/communities/{id}/channels: the channel and its parent_id.
type listed struct {
	ch     id.ID
	parent *id.ID
}

func listParents(t *testing.T, e *env, cid id.ID, tok string) map[id.ID]listed {
	t.Helper()
	status, body := e.Do(http.MethodGet, "/v1/communities/"+cid.String()+"/channels", tok, nil)
	if status != http.StatusOK {
		t.Fatalf("GET channels = %d (%x)", status, body)
	}
	var rows [][]cbor.RawMessage
	mustUnmarshal(t, body, &rows)
	out := make(map[id.ID]listed, len(rows))
	for _, r := range rows {
		var l listed
		mustUnmarshal(t, r[0], &l.ch)
		mustUnmarshal(t, r[4], &l.parent)
		out[l.ch] = l
	}
	return out
}

func TestCreateUnderHiddenCategoryIsNotFound(t *testing.T) {
	f := parentEnv(t)
	createUnder(t, f.e, f.cid, f.mTok, 0, &f.hidden, "inside", http.StatusNotFound)
	createUnder(t, f.e, f.cid, f.mTok, uint64(api.ChannelVoice), &f.hidden, "inside-voice", http.StatusNotFound)
	// Control: the same create under a category the moderator can view.
	createUnder(t, f.e, f.cid, f.mTok, 0, &f.open, "inside", http.StatusCreated)
}

func TestPatchIntoHiddenCategoryIsNotFound(t *testing.T) {
	f := parentEnv(t)
	ch := createUnder(t, f.e, f.cid, f.mTok, 0, nil, "movable", http.StatusCreated)
	status, body := f.e.Do(http.MethodPatch, "/v1/channels/"+ch.String(), f.mTok,
		[]any{nil, nil, nil, nil, f.hidden, nil, nil})
	if status != http.StatusNotFound {
		t.Fatalf("PATCH into the hidden category = %d (%x), want 404", status, body)
	}
	if code := f.e.ErrCode(body); code != "E_NOT_FOUND" {
		t.Fatalf("PATCH into the hidden category: code %q, want E_NOT_FOUND", code)
	}
	if row := mustChannel(t, f.e, ch); row.ParentID != nil {
		t.Fatalf("the refused move left parent %v", *row.ParentID)
	}
	// Control: into a visible category.
	if status, body := f.e.Do(http.MethodPatch, "/v1/channels/"+ch.String(), f.mTok,
		[]any{nil, nil, nil, nil, f.open, nil, nil}); status != http.StatusNoContent {
		t.Fatalf("PATCH into the visible category = %d (%x), want 204", status, body)
	}
	if row := mustChannel(t, f.e, ch); row.ParentID == nil || *row.ParentID != f.open {
		t.Fatalf("after the move: parent = %v, want %s", row.ParentID, f.open)
	}
}

func TestListingHidesAHiddenParent(t *testing.T) {
	f := parentEnv(t)
	// The owner puts a channel the member can view under a category the member cannot view.
	child := createUnder(t, f.e, f.cid, f.ownerTok, 0, &f.hidden, "child", http.StatusCreated)

	member := listParents(t, f.e, f.cid, f.bTok)
	got, ok := member[child]
	if !ok {
		t.Fatal("the member does not list the visible child")
	}
	if got.parent != nil {
		t.Fatalf("the member's row for the child names parent %s, want null", *got.parent)
	}
	if _, ok := member[f.hidden]; ok {
		t.Fatal("the member lists the category they cannot view")
	}
	if _, ok := member[f.open]; !ok {
		t.Fatal("the member does not list the visible category")
	}

	// Control: the owner's listing has both, with the parent set.
	owner := listParents(t, f.e, f.cid, f.ownerTok)
	if _, ok := owner[f.hidden]; !ok {
		t.Fatal("the owner does not list the hidden category")
	}
	if got, ok := owner[child]; !ok || got.parent == nil || *got.parent != f.hidden {
		t.Fatalf("the owner's row for the child = %+v, want parent %s", got, f.hidden)
	}
}

func TestTheOwnerMayParentAnywhere(t *testing.T) {
	f := parentEnv(t)
	createUnder(t, f.e, f.cid, f.ownerTok, 0, &f.hidden, "owners", http.StatusCreated)
}

// A PATCH that leaves parent_id alone is not a move: a moderator may still rename a channel that
// sits under a category they cannot view, and its parent stays. R44 checks only a destination.
func TestPatchWithoutParentLeavesAHiddenCategoryAlone(t *testing.T) {
	f := parentEnv(t)
	child := createUnder(t, f.e, f.cid, f.ownerTok, 0, &f.hidden, "child", http.StatusCreated)
	if status, body := f.e.Do(http.MethodPatch, "/v1/channels/"+child.String(), f.mTok,
		[]any{"renamed", nil, nil, nil, nil, nil, nil}); status != http.StatusNoContent {
		t.Fatalf("the moderator's rename of a child of the hidden category = %d (%x), want 204", status, body)
	}
	row := mustChannel(t, f.e, child)
	if row.Name != "renamed" || row.ParentID == nil || *row.ParentID != f.hidden {
		t.Fatalf("after the rename: name %q, parent %v, want \"renamed\" under %s", row.Name, row.ParentID, f.hidden)
	}
}

// getParent is element 5, parent_id, of GET /v1/channels/{id} as tok reads it.
func getParent(t *testing.T, e *env, ch id.ID, tok string) *id.ID {
	t.Helper()
	status, body := e.Do(http.MethodGet, "/v1/channels/"+ch.String(), tok, nil)
	if status != http.StatusOK {
		t.Fatalf("GET channel = %d (%x)", status, body)
	}
	var row []cbor.RawMessage
	mustUnmarshal(t, body, &row)
	var parent *id.ID
	mustUnmarshal(t, row[5], &parent)
	return parent
}

func TestGetHidesAHiddenParent(t *testing.T) {
	f := parentEnv(t)
	child := createUnder(t, f.e, f.cid, f.ownerTok, 0, &f.hidden, "child", http.StatusCreated)
	if got := getParent(t, f.e, child, f.bTok); got != nil {
		t.Fatalf("the member's GET of the child names parent %s, want null", *got)
	}
	// Control: the owner reads the parent.
	if got := getParent(t, f.e, child, f.ownerTok); got == nil || *got != f.hidden {
		t.Fatalf("the owner's GET of the child = parent %v, want %s", got, f.hidden)
	}
	// Control: the member reads the parent of a child under a category they can view.
	visible := createUnder(t, f.e, f.cid, f.ownerTok, 0, &f.open, "visible", http.StatusCreated)
	if got := getParent(t, f.e, visible, f.bTok); got == nil || *got != f.open {
		t.Fatalf("the member's GET under the visible category = parent %v, want %s", got, f.open)
	}
}
