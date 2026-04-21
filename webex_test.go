package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// --------------------------------------------------------------------------- #
// parseLinkNext
// --------------------------------------------------------------------------- #

func TestParseLinkNext(t *testing.T) {
	tests := []struct {
		name   string
		header string
		want   string
	}{
		{
			name:   "empty header",
			header: "",
			want:   "",
		},
		{
			name:   "next link present",
			header: `<https://api.example.com/v1/items?page=2>; rel="next"`,
			want:   "https://api.example.com/v1/items?page=2",
		},
		{
			name:   "multiple relations — next first",
			header: `<https://api.example.com/v1/items?page=2>; rel="next", <https://api.example.com/v1/items?page=1>; rel="prev"`,
			want:   "https://api.example.com/v1/items?page=2",
		},
		{
			name:   "multiple relations — next last",
			header: `<https://api.example.com/v1/items?page=1>; rel="prev", <https://api.example.com/v1/items?page=3>; rel="next"`,
			want:   "https://api.example.com/v1/items?page=3",
		},
		{
			name:   "no next relation",
			header: `<https://api.example.com/v1/items?page=1>; rel="prev"`,
			want:   "",
		},
		{
			name:   "only next",
			header: `<https://webexapis.com/v1/meetingTranscripts?cursor=abc>; rel="next"`,
			want:   "https://webexapis.com/v1/meetingTranscripts?cursor=abc",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseLinkNext(tc.header)
			if got != tc.want {
				t.Errorf("parseLinkNext(%q) = %q; want %q", tc.header, got, tc.want)
			}
		})
	}
}

// --------------------------------------------------------------------------- #
// fetchRoomInfo — via httptest server
// --------------------------------------------------------------------------- #

func newTestClient(handler http.Handler, baseOverride *string) *WebexClient {
	srv := httptest.NewServer(handler)
	// We capture the base URL so callers can close the server if desired.
	if baseOverride != nil {
		*baseOverride = srv.URL
	}
	return &WebexClient{httpClient: srv.Client()}
}

func TestFetchRoomInfo_Success(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/rooms/room-1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"title": "Team Sync", "type": "group"}) //nolint:errcheck
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// Temporarily point webexAPIBase at the test server.
	origBase := webexAPIBase
	webexAPIBase = srv.URL
	defer func() { webexAPIBase = origBase }()

	wc := &WebexClient{httpClient: srv.Client()}

	info, err := wc.fetchRoomInfo("room-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if info.Title != "Team Sync" {
		t.Errorf("Title = %q; want %q", info.Title, "Team Sync")
	}
	if info.Type != "group" {
		t.Errorf("Type = %q; want %q", info.Type, "group")
	}
}

func TestFetchRoomInfo_Caching(t *testing.T) {
	calls := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/rooms/room-2", func(w http.ResponseWriter, r *http.Request) {
		calls++
		json.NewEncoder(w).Encode(map[string]string{"title": "Cached Room", "type": "direct"}) //nolint:errcheck
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	origBase := webexAPIBase
	webexAPIBase = srv.URL
	defer func() { webexAPIBase = origBase }()

	wc := &WebexClient{httpClient: srv.Client()}

	for i := range 5 {
		info, err := wc.fetchRoomInfo("room-2")
		if err != nil {
			t.Fatalf("call %d: unexpected error: %v", i, err)
		}
		if info.Title != "Cached Room" {
			t.Errorf("call %d: Title = %q", i, info.Title)
		}
	}
	if calls != 1 {
		t.Errorf("expected 1 API call, got %d", calls)
	}
}

func TestFetchRoomInfo_NotFound(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/rooms/missing", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	origBase := webexAPIBase
	webexAPIBase = srv.URL
	defer func() { webexAPIBase = origBase }()

	wc := &WebexClient{httpClient: srv.Client()}

	_, err := wc.fetchRoomInfo("missing")
	if err == nil {
		t.Fatal("expected error for 404, got nil")
	}
}

func TestFetchRoomName_DelegatesToFetchRoomInfo(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/rooms/room-3", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"title": "1:1 with Alice", "type": "direct"}) //nolint:errcheck
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	origBase := webexAPIBase
	webexAPIBase = srv.URL
	defer func() { webexAPIBase = origBase }()

	wc := &WebexClient{httpClient: srv.Client()}

	name, err := wc.fetchRoomName("room-3")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if name != "1:1 with Alice" {
		t.Errorf("name = %q; want %q", name, "1:1 with Alice")
	}
}

// --------------------------------------------------------------------------- #
// fetchRoomMembers — via httptest server
// --------------------------------------------------------------------------- #

func TestFetchRoomMembers_Success(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/memberships", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("roomId") != "room-m" {
			http.Error(w, "bad roomId", http.StatusBadRequest)
			return
		}
		resp := map[string]interface{}{
			"items": []map[string]interface{}{
				{"personDisplayName": "Alice", "personEmail": "alice@example.com", "isModerator": true},
				{"personDisplayName": "Bob", "personEmail": "bob@example.com", "isModerator": false},
			},
		}
		json.NewEncoder(w).Encode(resp) //nolint:errcheck
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	origBase := webexAPIBase
	webexAPIBase = srv.URL
	defer func() { webexAPIBase = origBase }()

	wc := &WebexClient{httpClient: srv.Client()}

	members, err := wc.fetchRoomMembers("room-m")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(members) != 2 {
		t.Fatalf("expected 2 members, got %d", len(members))
	}
	if members[0].DisplayName != "Alice" || !members[0].IsModerator {
		t.Errorf("unexpected member[0]: %+v", members[0])
	}
	if members[1].Email != "bob@example.com" || members[1].IsModerator {
		t.Errorf("unexpected member[1]: %+v", members[1])
	}
}

func TestFetchRoomMembers_Pagination(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	var srvURL string // filled after server starts

	mux := http.NewServeMux()
	mux.HandleFunc("/memberships", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		page := calls
		mu.Unlock()

		if page == 1 {
			// Return first page with a Link: next header pointing to page 2.
			w.Header().Set("Link", fmt.Sprintf(`<%s/memberships?roomId=room-p&page=2>; rel="next"`, srvURL))
			json.NewEncoder(w).Encode(map[string]interface{}{ //nolint:errcheck
				"items": []map[string]interface{}{
					{"personDisplayName": "Alice", "personEmail": "a@example.com"},
				},
			})
		} else {
			// Last page — no Link header.
			json.NewEncoder(w).Encode(map[string]interface{}{ //nolint:errcheck
				"items": []map[string]interface{}{
					{"personDisplayName": "Bob", "personEmail": "b@example.com"},
				},
			})
		}
	})
	srv := httptest.NewServer(mux)
	srvURL = srv.URL
	defer srv.Close()

	origBase := webexAPIBase
	webexAPIBase = srv.URL
	defer func() { webexAPIBase = origBase }()

	wc := &WebexClient{httpClient: srv.Client()}

	members, err := wc.fetchRoomMembers("room-p")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(members) != 2 {
		t.Fatalf("expected 2 members after pagination, got %d", len(members))
	}
}

func TestFetchRoomMembers_Caching(t *testing.T) {
	calls := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/memberships", func(w http.ResponseWriter, r *http.Request) {
		calls++
		json.NewEncoder(w).Encode(map[string]interface{}{ //nolint:errcheck
			"items": []map[string]interface{}{},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	origBase := webexAPIBase
	webexAPIBase = srv.URL
	defer func() { webexAPIBase = origBase }()

	wc := &WebexClient{httpClient: srv.Client()}

	for i := range 3 {
		_, err := wc.fetchRoomMembers("room-c")
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if calls != 1 {
		t.Errorf("expected 1 API call, got %d", calls)
	}
}

// --------------------------------------------------------------------------- #
// listTranscriptPage — via httptest server
// --------------------------------------------------------------------------- #

func TestListTranscriptPage_Success(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/transcripts", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{ //nolint:errcheck
			"items": []map[string]interface{}{
				{
					"id":           "t1",
					"meetingId":    "m1",
					"meetingTopic": "Weekly Sync",
					"roomId":       "r1",
					"startTime":    "2026-04-01T10:00:00Z",
				},
			},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	wc := &WebexClient{httpClient: srv.Client()}

	items, next, err := listTranscriptPage(wc, srv.URL+"/transcripts")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if next != "" {
		t.Errorf("expected no next URL, got %q", next)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	got := items[0]
	if got.ID != "t1" || got.MeetingTopic != "Weekly Sync" || got.RoomID != "r1" {
		t.Errorf("unexpected item: %+v", got)
	}
}

func TestListTranscriptPage_NextLink(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/transcripts", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Link", `<https://api.example.com/v1/transcripts?page=2>; rel="next"`)
		json.NewEncoder(w).Encode(map[string]interface{}{"items": []interface{}{}}) //nolint:errcheck
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	wc := &WebexClient{httpClient: srv.Client()}

	_, next, err := listTranscriptPage(wc, srv.URL+"/transcripts")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if next != "https://api.example.com/v1/transcripts?page=2" {
		t.Errorf("next = %q; want page=2 URL", next)
	}
}

func TestListTranscriptPage_APIError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/transcripts", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	wc := &WebexClient{httpClient: srv.Client()}

	_, _, err := listTranscriptPage(wc, srv.URL+"/transcripts")
	if err == nil {
		t.Fatal("expected error for 401, got nil")
	}
}

// --------------------------------------------------------------------------- #
// Transcript routing logic
// --------------------------------------------------------------------------- #

func TestTranscriptRouting(t *testing.T) {
	const personal = "personal-folder-id"
	const direct = "direct-folder-id"
	const shared = "shared-folder-id"

	routeTranscript := func(t Transcript) string {
		switch {
		case t.RoomID == "":
			return personal
		case t.SpaceType == "direct":
			return direct
		default:
			return shared
		}
	}

	tests := []struct {
		name      string
		t         Transcript
		wantRoute string
	}{
		{
			name:      "no roomId -> personal",
			t:         Transcript{RoomID: "", SpaceType: ""},
			wantRoute: personal,
		},
		{
			name:      "direct space -> direct-rooms",
			t:         Transcript{RoomID: "r1", SpaceType: "direct"},
			wantRoute: direct,
		},
		{
			name:      "group space -> shared-rooms",
			t:         Transcript{RoomID: "r2", SpaceType: "group"},
			wantRoute: shared,
		},
		{
			name:      "roomId set but lookup failed (SpaceType empty) -> shared-rooms",
			t:         Transcript{RoomID: "r3", SpaceType: ""},
			wantRoute: shared,
		},
		{
			name:      "unknown future type -> shared-rooms",
			t:         Transcript{RoomID: "r4", SpaceType: "team"},
			wantRoute: shared,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := routeTranscript(tc.t)
			if got != tc.wantRoute {
				t.Errorf("route = %q; want %q", got, tc.wantRoute)
			}
		})
	}
}

// --------------------------------------------------------------------------- #
// fetchMeetingSummary — via httptest server
// --------------------------------------------------------------------------- #

func TestFetchMeetingSummary_NoSummary(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/meetingSummaries", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{"items": []interface{}{}}) //nolint:errcheck
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	origBase := webexAPIBase
	webexAPIBase = srv.URL
	defer func() { webexAPIBase = origBase }()

	wc := &WebexClient{httpClient: srv.Client()}
	summary, err := fetchMeetingSummary(wc, "meeting-1")
	if err == nil {
		t.Errorf("expected error (no summary), got nil; summary=%q", summary)
	}
}

func TestFetchMeetingSummary_WithSummary(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/meetingSummaries", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{ //nolint:errcheck
			"items": []map[string]interface{}{
				{
					"meetingNotes": "Discussed Q2 plans.",
					"actionItems":  []map[string]interface{}{{"action": "Ship the feature"}},
				},
			},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	origBase := webexAPIBase
	webexAPIBase = srv.URL
	defer func() { webexAPIBase = origBase }()

	wc := &WebexClient{httpClient: srv.Client()}
	summary, err := fetchMeetingSummary(wc, "meeting-2")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if summary == "" {
		t.Error("expected non-empty summary")
	}
}

// --------------------------------------------------------------------------- #
// RoomMember struct sanity
// --------------------------------------------------------------------------- #

func TestRoomMemberFields(t *testing.T) {
	m := RoomMember{DisplayName: "Alice", Email: "alice@example.com", IsModerator: true}
	if m.DisplayName != "Alice" {
		t.Errorf("DisplayName = %q", m.DisplayName)
	}
	if !m.IsModerator {
		t.Error("IsModerator should be true")
	}
}

func TestEnsureWebexToken_FallsBackToSavedUserToken(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/people/me", func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		switch auth {
		case "Bearer good-token":
			json.NewEncoder(w).Encode(map[string]string{"displayName": "Sherif"}) //nolint:errcheck
		default:
			http.Error(w, "unauthorized", http.StatusUnauthorized)
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	origBase := webexAPIBase
	webexAPIBase = srv.URL
	defer func() { webexAPIBase = origBase }()

	home := t.TempDir()
	oldHome := os.Getenv("HOME")
	os.Setenv("HOME", home)
	t.Cleanup(func() { os.Setenv("HOME", oldHome) })

	cfgPath := filepath.Join(home, ".webex-meeting-sync", ".env")
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0700); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}
	if err := os.WriteFile(cfgPath, []byte("WEBEX_TOKEN=good-token\n"), 0600); err != nil {
		t.Fatalf("write user token: %v", err)
	}

	os.Setenv("WEBEX_TOKEN", "bad-token")
	t.Cleanup(func() { os.Unsetenv("WEBEX_TOKEN") })

	if err := ensureWebexToken(); err != nil {
		t.Fatalf("ensureWebexToken: %v", err)
	}
	if got := os.Getenv("WEBEX_TOKEN"); got != "good-token" {
		t.Fatalf("WEBEX_TOKEN = %q; want %q", got, "good-token")
	}
}

// --------------------------------------------------------------------------- #
// fetchMeetingDetails — via httptest server
// --------------------------------------------------------------------------- #

func TestFetchMeetingDetails_PersonalRoom(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/meetings/pmr-meeting-1", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{ //nolint:errcheck
			"scheduledType": "personalRoomMeeting",
			"roomId":        "",
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	origBase := webexAPIBase
	webexAPIBase = srv.URL
	defer func() { webexAPIBase = origBase }()

	wc := &WebexClient{httpClient: srv.Client()}

	d, err := wc.fetchMeetingDetails("pmr-meeting-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.ScheduledType != "personalRoomMeeting" {
		t.Errorf("ScheduledType = %q; want personalRoomMeeting", d.ScheduledType)
	}
	if d.RoomID != "" {
		t.Errorf("RoomID = %q; want empty", d.RoomID)
	}
}

func TestFetchMeetingDetails_SpaceMeeting(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/meetings/space-meeting-1", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{ //nolint:errcheck
			"scheduledType": "meeting",
			"roomId":        "room-xyz",
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	origBase := webexAPIBase
	webexAPIBase = srv.URL
	defer func() { webexAPIBase = origBase }()

	wc := &WebexClient{httpClient: srv.Client()}

	d, err := wc.fetchMeetingDetails("space-meeting-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.ScheduledType != "meeting" {
		t.Errorf("ScheduledType = %q; want meeting", d.ScheduledType)
	}
	if d.RoomID != "room-xyz" {
		t.Errorf("RoomID = %q; want room-xyz", d.RoomID)
	}
}

func TestFetchMeetingDetails_NotFound(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/meetings/missing-meeting", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	origBase := webexAPIBase
	webexAPIBase = srv.URL
	defer func() { webexAPIBase = origBase }()

	wc := &WebexClient{httpClient: srv.Client()}

	_, err := wc.fetchMeetingDetails("missing-meeting")
	if err == nil {
		t.Fatal("expected error for 404, got nil")
	}
}

func TestFetchMeetingDetails_Caching(t *testing.T) {
	calls := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/meetings/cached-meeting", func(w http.ResponseWriter, r *http.Request) {
		calls++
		json.NewEncoder(w).Encode(map[string]string{ //nolint:errcheck
			"scheduledType": "meeting",
			"roomId":        "room-cached",
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	origBase := webexAPIBase
	webexAPIBase = srv.URL
	defer func() { webexAPIBase = origBase }()

	wc := &WebexClient{httpClient: srv.Client()}

	for i := range 4 {
		d, err := wc.fetchMeetingDetails("cached-meeting")
		if err != nil {
			t.Fatalf("call %d: unexpected error: %v", i, err)
		}
		if d.RoomID != "room-cached" {
			t.Errorf("call %d: RoomID = %q", i, d.RoomID)
		}
	}
	if calls != 1 {
		t.Errorf("expected 1 API call, got %d", calls)
	}
}

// --------------------------------------------------------------------------- #
// fetchRoomInfo concurrency — no data races
// --------------------------------------------------------------------------- #

func TestFetchRoomInfo_ConcurrentAccess(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/rooms/concurrent", func(w http.ResponseWriter, r *http.Request) {
		// Simulate a small delay to increase race window.
		time.Sleep(2 * time.Millisecond)
		json.NewEncoder(w).Encode(map[string]string{"title": "Concurrent Room", "type": "group"}) //nolint:errcheck
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	origBase := webexAPIBase
	webexAPIBase = srv.URL
	defer func() { webexAPIBase = origBase }()

	wc := &WebexClient{httpClient: srv.Client()}

	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := wc.fetchRoomInfo("concurrent"); err != nil {
				t.Errorf("concurrent fetchRoomInfo: %v", err)
			}
		}()
	}
	wg.Wait()
}
