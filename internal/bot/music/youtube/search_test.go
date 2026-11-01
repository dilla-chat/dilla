package youtube

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
)

func recordedSearch(t *testing.T) []byte {
	t.Helper()
	body, err := os.ReadFile("testdata/yt-dlp-2026.10.21-ullared-search.json")
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func recordedResolver(body []byte) MetadataResolver {
	return MetadataResolver{YTDLP: "/yt-dlp", Node: "/node", runner: runnerFunc(func(context.Context, string, []string, int) ([]byte, error) { return body, nil })}
}

func TestResolveRecordedPinnedSearch(t *testing.T) {
	tracks, err := recordedResolver(recordedSearch(t)).Resolve(t.Context(), "Ullared Morgan Tjuklad")
	if err != nil || len(tracks) != 5 {
		t.Fatalf("real ytsearch5 tracks=%d class=%s err=%v", len(tracks), FailureClass(err), err)
	}
	if tracks[0].CanonicalID != "LvH_ft4XR1Q" || tracks[0].DurationMS != 70000 {
		t.Fatalf("first search descriptor=%#v", tracks[0])
	}
}

func TestSearchRecordedPinnedSearch(t *testing.T) {
	tracks, err := recordedResolver(recordedSearch(t)).Search(t.Context(), "Ullared Morgan Tjuklad")
	if err != nil || len(tracks) != 5 {
		t.Fatalf("search tracks=%d class=%s err=%v", len(tracks), FailureClass(err), err)
	}
	if tracks[0].CanonicalID != "LvH_ft4XR1Q" {
		t.Fatalf("first search descriptor=%#v", tracks[0])
	}
}

func TestSearchMissingDurationDoesNotWeakenResolve(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(map[bool]string{false: "null", true: "absent"}[missing], func(t *testing.T) {
			var data map[string]any
			if err := json.Unmarshal(recordedSearch(t), &data); err != nil {
				t.Fatal(err)
			}
			entry := data["entries"].([]any)[0].(map[string]any)
			if missing {
				delete(entry, "duration")
			} else {
				entry["duration"] = nil
			}
			body, err := json.Marshal(data)
			if err != nil {
				t.Fatal(err)
			}
			resolver := recordedResolver(body)
			tracks, err := resolver.Search(t.Context(), "Ullared Morgan Tjuklad")
			if err != nil || len(tracks) != 5 || tracks[0].DurationMS != 0 {
				t.Fatalf("search tracks=%v err=%v", tracks, err)
			}
			tracks, err = resolver.Resolve(t.Context(), "Ullared Morgan Tjuklad")
			if !errors.Is(err, ErrMetadataInvalid) || len(tracks) != 0 || FailureClass(err) != "invalid_metadata:missing_duration" {
				t.Fatalf("strict resolve tracks=%v err=%v class=%s", tracks, err, FailureClass(err))
			}
		})
	}
}

func TestSearchKeepsMetadataValidation(t *testing.T) {
	cases := []struct {
		name, field string
		value       any
		reason      string
		root        bool
	}{
		{"root extractor", "extractor_key", "Generic", "extractor", true},
		{"root type", "_type", "url", "type", true},
		{"missing count", "playlist_count", nil, "playlist_count", true},
		{"mismatch count", "playlist_count", 4, "playlist_count", true},
		{"large count", "playlist_count", 6, "playlist_count", true},
		{"wrong extractor", "ie_key", "Generic", "extractor", false},
		{"bad id", "id", "bad", "id", false},
		{"blank title", "title", "", "title", false},
		{"private", "availability", "private", "availability", false},
		{"live", "live_status", "is_live", "live", false},
		{"upcoming", "live_status", "is_upcoming", "live", false},
		{"live flag", "is_live", true, "live", false},
		{"zero duration", "duration", 0, "invalid_duration", false},
		{"negative duration", "duration", -1, "invalid_duration", false},
		{"nonnumeric duration", "duration", "invalid", "invalid_duration", false},
		{"duration limit", "duration", 86401, "duration_limit", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var data map[string]any
			if err := json.Unmarshal(recordedSearch(t), &data); err != nil {
				t.Fatal(err)
			}
			target := data
			if !tc.root {
				target = data["entries"].([]any)[0].(map[string]any)
			}
			target[tc.field] = tc.value
			body, err := json.Marshal(data)
			if err != nil {
				t.Fatal(err)
			}
			tracks, err := recordedResolver(body).Search(t.Context(), "Ullared Morgan Tjuklad")
			if !errors.Is(err, ErrMetadataInvalid) || len(tracks) != 0 || FailureClass(err) != "invalid_metadata:"+tc.reason {
				t.Fatalf("tracks=%v err=%v class=%s", tracks, err, FailureClass(err))
			}
		})
	}
}

func TestSearchVideoAndPlaylistStillRequireDuration(t *testing.T) {
	cases := []struct{ input, body string }{
		{videoURL("LvH_ft4XR1Q"), `{"_type":"video","extractor_key":"Youtube","id":"LvH_ft4XR1Q","title":"video"}`},
		{"https://www.youtube.com/playlist?list=PLabc_123", `{"_type":"playlist","extractor_key":"YoutubeTab","playlist_count":1,"entries":[{"_type":"url","ie_key":"Youtube","id":"LvH_ft4XR1Q","title":"video"}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			tracks, err := recordedResolver([]byte(tc.body)).Search(t.Context(), tc.input)
			if !errors.Is(err, ErrMetadataInvalid) || len(tracks) != 0 || FailureClass(err) != "invalid_metadata:missing_duration" {
				t.Fatalf("tracks=%v err=%v class=%s", tracks, err, FailureClass(err))
			}
		})
	}
}

func TestSearchNormalizesReportedInputs(t *testing.T) {
	cases := []struct {
		input, canonical string
		invalid          bool
	}{
		{"Ullared Morgan Tjuklad", "ytsearch5:Ullared Morgan Tjuklad", false},
		{"https://www.youtube.com/watch?v=LvH_", "", true},
		{"https://www.youtube.com/watch?v=LvH_ft4XR1Q&si=shared&feature=share", videoURL("LvH_ft4XR1Q"), false},
	}
	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			calls := 0
			resolver := recordedResolver(nil)
			resolver.runner = runnerFunc(func(_ context.Context, _ string, args []string, _ int) ([]byte, error) {
				calls++
				if args[len(args)-1] != tc.canonical {
					t.Fatalf("canonical=%q want=%q", args[len(args)-1], tc.canonical)
				}
				if tc.canonical == videoURL("LvH_ft4XR1Q") {
					return os.ReadFile("testdata/yt-dlp-2026.10.21-LvH_ft4XR1Q.json")
				}
				return recordedSearch(t), nil
			})
			_, err := resolver.Search(t.Context(), tc.input)
			if tc.invalid {
				if !errors.Is(err, ErrInvalidInput) || calls != 0 {
					t.Fatalf("invalid partial url err=%v calls=%d", err, calls)
				}
			} else if err != nil || calls != 1 {
				t.Fatalf("err=%v calls=%d", err, calls)
			}
		})
	}
}

func TestSearchAcceptsEmptyResults(t *testing.T) {
	var data map[string]any
	if err := json.Unmarshal(recordedSearch(t), &data); err != nil {
		t.Fatal(err)
	}
	// A second real pinned ytsearch5 probe returned this same root/count shape.
	data["entries"] = []any{}
	data["playlist_count"] = 0
	body, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	tracks, err := recordedResolver(body).Search(t.Context(), "unique query")
	if err != nil || len(tracks) != 0 {
		t.Fatalf("empty search tracks=%v err=%v", tracks, err)
	}
	// Empty queue expansion retains the existing strict refusal.
	_, err = recordedResolver(body).Resolve(t.Context(), "unique query")
	if !errors.Is(err, ErrMetadataInvalid) {
		t.Fatalf("strict empty resolve err=%v", err)
	}
}
