package store_test

import (
	"errors"
	"reflect"
	"testing"

	"github.com/dilla-chat/dilla/internal/store"
)

func TestParseQueryRenderers(t *testing.T) {
	cases := []struct {
		raw  string
		fts5 string
		tsq  string
	}{
		{`kryptering`, `"kryptering"`, `'kryptering'`},
		{`c++`, `"c"`, `'c'`},
		{`AND OR NOT`, `"and" AND "or" AND "not"`, `'and' & 'or' & 'not'`},
		{`krypt*`, `"krypt"*`, `'krypt':*`},
		{`"never sees"`, `"never sees"`, `'never' <-> 'sees'`},
		// A negation-only query is legal: `-spam` is a real thing a moderator
		// types. FTS5 has no match-all token, so the empty phrase carries it;
		// Postgres needs no placeholder. See TestANegationOnlyQueryIsLegal.
		{`-raids`, `"" NOT "raids"`, `!'raids'`},
		{`hej -raids`, `"hej" NOT "raids"`, `'hej' & !'raids'`},
		// A negated PHRASE is parenthesised on the Postgres side: `!` binds
		// tighter than `<->`, so `!'never' <-> 'sees'` would mean "a word that is
		// not 'never', followed by 'sees'" rather than "not the phrase".
		{`hej -"never sees"`, `"hej" NOT "never sees"`, `'hej' & !('never' <-> 'sees')`},
		{`unbalanced " quote`, `"unbalanced" AND "quote"`, `'unbalanced' & 'quote'`},
		{`a & | ! ( " :*`, `"a"`, `'a'`},
		{`haller`, `"haller"`, `'haller'`},
		// Terms and phrases are emitted in INPUT order, not terms-then-phrases:
		// a renderer that bucketed them would make the rendering depend on how
		// the parser happened to classify each token.
		{`"a b" c*`, `"a b" AND "c"*`, `'a' <-> 'b' & 'c':*`},
		// normalizeWords strips every non-word rune, so the doubled inner quotes
		// are gone before either renderer sees the phrase. The expectation is the
		// post-normalisation spelling, which is what is actually searched.
		{`say "he said ""hi"""`, `"say" AND "he said hi"`, `'say' & 'he' <-> 'said' <-> 'hi'`},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			q, err := store.ParseQuery(tc.raw)
			if err != nil {
				t.Fatalf("ParseQuery(%q): %v", tc.raw, err)
			}
			if got := q.FTS5(); got != tc.fts5 {
				t.Errorf("FTS5() = %q, want %q", got, tc.fts5)
			}
			if got := q.TSQuery(); got != tc.tsq {
				t.Errorf("TSQuery() = %q, want %q", got, tc.tsq)
			}
		})
	}
}

func TestParseQueryRejectsAnEmptySearch(t *testing.T) {
	// A bare '-' is NOT in this list: it produces no term, no phrase and no
	// negation, so it is empty for the same reason "" is. It is listed as its own
	// case below so the boundary between "empty" and "negation only" is explicit.
	for _, raw := range []string{"", "   ", `+ & | ! ( ) " :`, `-`, `- -`} {
		if _, err := store.ParseQuery(raw); !errors.Is(err, store.ErrEmptyQuery) {
			t.Fatalf("ParseQuery(%q) = %v, want ErrEmptyQuery", raw, err)
		}
	}
}

func TestANegationOnlyQueryIsLegal(t *testing.T) {
	q, err := store.ParseQuery(`-raids`)
	if err != nil {
		t.Fatalf("ParseQuery(-raids): %v", err)
	}
	if len(q.Terms) != 0 || len(q.Phrases) != 0 || len(q.Not) != 1 || q.Not[0] != "raids" {
		t.Fatalf("parsed = %+v", q)
	}
}

func TestParseQueryIgnoresSurroundingWhitespace(t *testing.T) {
	// The structural round trip that replaces the old idempotence test. FTS5()
	// output is deliberately NOT in the input language — its bare AND and NOT
	// become terms and its quoted terms become phrases on a re-parse — so
	// re-parsing a rendering asserts nothing about the parser.
	a, err := store.ParseQuery(`hej "never sees" krypt* -raids`)
	if err != nil {
		t.Fatalf("ParseQuery: %v", err)
	}
	b, err := store.ParseQuery("  hej   \t \"never sees\"  krypt*   -raids  ")
	if err != nil {
		t.Fatalf("ParseQuery with whitespace: %v", err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("whitespace changed the parse: %+v vs %+v", a, b)
	}
}

// FTS5 has no match-all token and its empty phrase `""` matches NOTHING, so
// FTS5()'s `"" NOT "raids"` alone would make every negation-only search empty on
// SQLite while Postgres's `!'raids'` answers every other message: the engines
// would disagree on the one query a moderator types most. FTS5Match puts the
// channel scope on the positive side instead, which is what `-raids` means.
func TestFTS5MatchKeepsANegationOnlyQueryPositive(t *testing.T) {
	scope := []string{"00aa", "00bb"}
	cases := []struct{ raw, want string }{
		{`-raids`, `{channel_hex} : ("00aa" OR "00bb") NOT body : ("raids")`},
		{`-raids -"never sees"`, `{channel_hex} : ("00aa" OR "00bb") NOT body : ("raids") NOT body : ("never sees")`},
		{`hej -raids`, `{channel_hex} : ("00aa" OR "00bb") AND body : ("hej" NOT "raids")`},
		{`krypt*`, `{channel_hex} : ("00aa" OR "00bb") AND body : ("krypt"*)`},
	}
	for _, tc := range cases {
		q, err := store.ParseQuery(tc.raw)
		if err != nil {
			t.Fatalf("ParseQuery(%q): %v", tc.raw, err)
		}
		if got := q.FTS5Match(scope); got != tc.want {
			t.Errorf("FTS5Match(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}
