// webex.go implements Webex OAuth2 authentication and transcript retrieval
// for webex-scribe.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"
)

const (
	webexAuthURL  = "https://webexapis.com/v1/authorize"
	webexTokenURL = "https://webexapis.com/v1/access_token"
	// webexScope lists the OAuth2 scopes requested during authorization.
	// - meeting:schedules_read      read scheduled meeting metadata
	// - meeting:transcripts_read    read the authenticated user's own transcripts
	// - meeting:summaries_read      read AI-generated meeting summaries
	// - spark:rooms_read            list Webex Spaces and look up room IDs
	//
	// Note: meeting:admin_transcript_read (org-wide transcripts) is not included
	// by default because it must be explicitly enabled in the Webex app
	// integration and requires admin privileges. Add it to the integration scopes
	// on developer.webex.com and pass --admin to include it at runtime.
	webexScope      = "meeting:schedules_read meeting:transcripts_read meeting:summaries_read spark:rooms_read spark:memberships_read"
	webexAdminScope = "meeting:admin_transcript_read meeting:admin_summaries_read"
	redirectURI = "http://localhost:47823/callback"
)

// webexAPIBase is the base URL for all Webex REST API calls. It is a var so
// tests can point it at a local httptest server.
var webexAPIBase = "https://webexapis.com/v1"

// RoomMember represents a single member of a Webex Space (room).
type RoomMember struct {
	DisplayName string
	Email       string
	IsModerator bool
}

// RoomInfo holds the title, type, and ID of a Webex Space.
// Type values from the API: "direct" (1:1), "group" (shared team space).
type RoomInfo struct {
	ID    string
	Title string
	Type  string // "direct" or "group"
}

// MeetingDetails holds the room and scheduling type of a Webex meeting instance
// returned by GET /meetings/{meetingId}.
type MeetingDetails struct {
	RoomID        string `json:"roomId"`
	ScheduledType string `json:"scheduledType"` // "personalRoomMeeting", "meeting", "webinar"
}

// WebexClient wraps an authenticated HTTP client for the Webex REST API.
type WebexClient struct {
	httpClient *http.Client

	// roomInfoCache caches roomId -> RoomInfo (title + type).
	roomInfoMu    sync.Mutex
	roomInfoCache map[string]RoomInfo

	// allRoomsCache caches all the user's rooms keyed by lowercase title,
	// used to match meeting topics to spaces when the API returns no roomId.
	allRoomsMu    sync.Mutex
	allRoomsCache map[string]RoomInfo // key: strings.ToLower(title)

	// roomMembersCache caches roomId -> member list.
	roomMembersMu    sync.Mutex
	roomMembersCache map[string][]RoomMember

	// meetingDetailsCache caches meetingId -> MeetingDetails (roomId + scheduledType).
	meetingDetailsMu    sync.Mutex
	meetingDetailsCache map[string]MeetingDetails
}

// Transcript holds the metadata, raw VTT content, and optional AI summary of
// a single Webex meeting transcript.
type Transcript struct {
	ID           string
	MeetingID    string
	MeetingTitle string
	// SpaceName is the human-readable Webex Space (room) title resolved from
	// RoomID. It is empty for scheduled meetings that are not tied to a Space.
	SpaceName string
	// SpaceType is the Webex room type: "direct" (1:1) or "group" (shared).
	// Empty when the transcript is not linked to a Space.
	SpaceType string
	// RoomID is the Webex Space roomId, used to fetch members.
	RoomID    string
	StartTime time.Time
	Content   string
	// AISummary is the AI-generated meeting summary. It is empty when the
	// meeting had no AI summary or when the summary API returned an error.
	AISummary string
	// RoomMembers is the member list of the Webex Space. Empty for meetings
	// not tied to a Space or when the memberships API returns an error.
	RoomMembers []RoomMember
}

// newWebexClient returns a WebexClient authenticated with the Webex API.
//
// Authentication is attempted in the following order:
//  1. WEBEX_TOKEN environment variable (personal access token or bot token) —
//     no browser flow required; ideal when the OAuth2 integration is blocked
//     by org policy.
//  2. Saved OAuth2 token on disk (from a previous successful authorization).
//  3. Interactive OAuth2 authorization code flow via a local callback server
//     on port 47823.
//
// clientID and clientSecret are only required for the OAuth2 path; they are
// ignored when WEBEX_TOKEN is set.
//
// When adminMode is true, meeting:admin_transcript_read is appended to the
// requested scope. This requires the scope to be enabled in the Webex app
// integration on developer.webex.com and invalidates any previously saved
// token, so the interactive OAuth2 flow will run again.
func newWebexClient(ctx context.Context, clientID, clientSecret string, adminMode bool) (*WebexClient, error) {
	// Path 1: static personal access token — skip OAuth2 entirely.
	if pat := os.Getenv("WEBEX_TOKEN"); pat != "" {
		fmt.Println("Using WEBEX_TOKEN (personal access token).")
		ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: pat})
		return &WebexClient{httpClient: oauth2.NewClient(ctx, ts)}, nil
	}
	// Path 2 & 3: OAuth2 flow — requires client credentials.
	if clientID == "" || clientSecret == "" {
		return nil, fmt.Errorf(
			"no WEBEX_TOKEN found and OAuth2 credentials are missing\n\n" +
				"Either set WEBEX_TOKEN (personal access token from developer.webex.com)\n" +
				"or provide -client-id and -client-secret for the OAuth2 flow.",
		)
	}

	scope := webexScope
	if adminMode {
		scope += " " + webexAdminScope
	}

	cfg := &oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		RedirectURL:  redirectURI,
		Scopes:       strings.Fields(scope),
		Endpoint: oauth2.Endpoint{
			AuthURL:  webexAuthURL,
			TokenURL: webexTokenURL,
		},
	}

	tok, err := loadWebexToken()
	if err == nil && adminMode {
		// A saved token predates the admin scope request; discard it so the
		// interactive flow issues a fresh token with the expanded scope.
		fmt.Println("Admin mode: discarding saved token to re-authorize with admin scope.")
		tok = nil
		err = fmt.Errorf("admin re-auth required")
	}
	if err != nil {
		// No valid saved token — run the interactive OAuth2 flow.
		tok, err = doOAuthFlow(ctx, cfg)
		if err != nil {
			return nil, err
		}
		if saveErr := saveWebexToken(tok); saveErr != nil {
			fmt.Fprintf(os.Stderr, "warning: could not save token: %v\n", saveErr)
		}
	}

	// Wrap the token source so refreshed tokens are persisted automatically.
	ts := &persistingTokenSource{
		base: cfg.TokenSource(ctx, tok),
		last: tok.AccessToken,
	}
	return &WebexClient{httpClient: oauth2.NewClient(ctx, ts)}, nil
}

// persistingTokenSource wraps an oauth2.TokenSource and persists the token to
// disk whenever the underlying source issues a refreshed access token.
type persistingTokenSource struct {
	base oauth2.TokenSource
	last string // access token from the previous call
}

// Token returns the current OAuth2 token, refreshing it via the wrapped source
// when necessary, and persists the new token to disk on refresh.
func (p *persistingTokenSource) Token() (*oauth2.Token, error) {
	tok, err := p.base.Token()
	if err != nil {
		return nil, err
	}
	if tok.AccessToken != p.last {
		p.last = tok.AccessToken
		if saveErr := saveWebexToken(tok); saveErr != nil {
			fmt.Fprintf(os.Stderr, "warning: could not persist refreshed token: %v\n", saveErr)
		}
	}
	return tok, nil
}

// webexTokenPath returns the absolute path of the persisted OAuth2 token file.
func webexTokenPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "webex-scribe", "token.json")
}

// loadWebexToken reads and unmarshals a previously saved oauth2.Token from
// webexTokenPath. It returns an error when the file is absent or malformed,
// which the caller treats as a signal to start a fresh OAuth2 flow.
func loadWebexToken() (*oauth2.Token, error) {
	data, err := os.ReadFile(webexTokenPath())
	if err != nil {
		return nil, err
	}
	var tok oauth2.Token
	if err := json.Unmarshal(data, &tok); err != nil {
		return nil, err
	}
	return &tok, nil
}

// saveWebexToken serializes tok as JSON and writes it to webexTokenPath with
// permissions 0600. It creates the parent directory if it does not exist.
func saveWebexToken(tok *oauth2.Token) error {
	path := webexTokenPath()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	data, err := json.Marshal(tok)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

// doOAuthFlow runs the interactive OAuth2 authorization code flow for Webex.
//
// It prints the authorization URL to stdout for the user to open in a browser,
// then listens on localhost:47823 for the redirect callback. The state parameter
// is validated to protect against CSRF attacks. The function blocks until the
// browser completes the flow, ctx is cancelled, or an error occurs.
func doOAuthFlow(ctx context.Context, cfg *oauth2.Config) (*oauth2.Token, error) {
	// Use a unique state value to guard against CSRF.
	state := fmt.Sprintf("wts-%d", time.Now().UnixNano())
	authURL := cfg.AuthCodeURL(state, oauth2.AccessTypeOffline)

	fmt.Printf("Open the following URL in your browser to authorize the app:\n\n  %s\n\n", authURL)
	fmt.Println("Waiting for callback on http://localhost:47823/callback ...")

	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("state") != state {
			http.Error(w, "state mismatch", http.StatusBadRequest)
			errCh <- fmt.Errorf("OAuth2 state mismatch — possible CSRF attack")
			return
		}
		code := r.URL.Query().Get("code")
		if code == "" {
			desc := r.URL.Query().Get("error_description")
			if desc == "" {
				desc = r.URL.Query().Get("error")
			}
			http.Error(w, "missing authorization code", http.StatusBadRequest)
			errCh <- fmt.Errorf("no authorization code in callback: %s", desc)
			return
		}
		fmt.Fprintln(w, "Authorization successful! You can close this window.")
		codeCh <- code
	})

	ln, err := net.Listen("tcp", "localhost:47823")
	if err != nil {
		return nil, fmt.Errorf("could not listen on localhost:47823: %w\n\nEnsure the port is free and that your Webex app's redirect URI is set to http://localhost:47823/callback", err)
	}
	srv := &http.Server{Handler: mux}
	go func() {
		if serveErr := srv.Serve(ln); serveErr != nil && serveErr != http.ErrServerClosed {
			errCh <- serveErr
		}
	}()
	defer srv.Shutdown(ctx) //nolint:errcheck

	var code string
	select {
	case code = <-codeCh:
	case err := <-errCh:
		return nil, err
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	tok, err := cfg.Exchange(ctx, code)
	if err != nil {
		return nil, fmt.Errorf("exchanging authorization code for token: %w", err)
	}
	return tok, nil
}

// listTranscriptItems returns the metadata for all Webex meeting transcripts
// whose start time falls within [from, to] (inclusive), without downloading
// any content. Filtering by content (e.g. deduplication) should be applied
// to the returned items before calling downloadTranscriptItems.
//
// from and to must be in "YYYY-MM-DD" format. When both are empty the window
// is [days] days ago → today. days is ignored when from/to are provided.
// The total window is split into 30-day chunks automatically because the Webex
// API truncates results for windows wider than ~30 days.
// When spaceID is non-empty only transcripts from that Webex Space are returned.
func listTranscriptItems(ctx context.Context, client *WebexClient, from, to string, days int, spaceID string) ([]transcriptItem, error) {
	now := time.Now().UTC()

	var fromTime, toTime time.Time
	switch {
	case from != "" && to != "":
		ft, err := time.Parse("2006-01-02", from)
		if err != nil {
			return nil, fmt.Errorf("invalid --from date %q: %w", from, err)
		}
		tt, err := time.Parse("2006-01-02", to)
		if err != nil {
			return nil, fmt.Errorf("invalid --to date %q: %w", to, err)
		}
		fromTime = ft
		toTime = tt.Add(24*time.Hour - time.Second)
	case from != "":
		ft, err := time.Parse("2006-01-02", from)
		if err != nil {
			return nil, fmt.Errorf("invalid --from date %q: %w", from, err)
		}
		fromTime = ft
		toTime = now
	case to != "":
		tt, err := time.Parse("2006-01-02", to)
		if err != nil {
			return nil, fmt.Errorf("invalid --to date %q: %w", to, err)
		}
		toTime = tt.Add(24*time.Hour - time.Second)
		fromTime = toTime.AddDate(0, 0, -days)
	default:
		if days <= 0 {
			days = 30
		}
		fromTime = now.AddDate(0, 0, -days)
		toTime = now
	}

	// Split the total window into 30-day chunks to avoid Webex API truncation.
	const chunkDays = 30
	seen := make(map[string]bool)
	var allItems []transcriptItem

	chunkStart := fromTime
	for chunkStart.Before(toTime) {
		chunkEnd := chunkStart.Add(chunkDays * 24 * time.Hour)
		if chunkEnd.After(toTime) {
			chunkEnd = toTime
		}

		firstPage := fmt.Sprintf(
			"%s/meetingTranscripts?from=%s&to=%s&max=100",
			webexAPIBase,
			url.QueryEscape(chunkStart.Format(time.RFC3339)),
			url.QueryEscape(chunkEnd.Format(time.RFC3339)),
		)
		if spaceID != "" {
			firstPage += "&roomId=" + url.QueryEscape(spaceID)
		}

		for pageURL := firstPage; pageURL != ""; {
			items, nextURL, err := listTranscriptPage(client, pageURL)
			if err != nil {
				return nil, err
			}
			for _, item := range items {
				if !seen[item.ID] {
					seen[item.ID] = true
					allItems = append(allItems, item)
				}
			}
			pageURL = nextURL
		}

		chunkStart = chunkEnd
	}
	return allItems, nil
}

// downloadTranscriptItems fetches the VTT content and AI summary for each item
// in items and returns the fully populated Transcript slice. Items whose content
// cannot be downloaded are logged as warnings and omitted from the result.
func downloadTranscriptItems(client *WebexClient, items []transcriptItem) []Transcript {
	var transcripts []Transcript
	for i, item := range items {
		startTime, _ := time.Parse(time.RFC3339, item.StartTime)
		fmt.Printf("  [%d/%d] downloading %s (%s)\n", i+1, len(items), item.MeetingTopic, startTime.Format("Jan 2, 2006"))
		content, err := downloadTranscript(client, item.ID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "        warning: skipping — %v\n", err)
			continue
		}
		summary, err := fetchMeetingSummary(client, item.MeetingID)
		if err != nil {
			// Summary is optional — log and continue without it.
			fmt.Fprintf(os.Stderr, "        note: no AI summary available — %v\n", err)
		}
		// Resolve the Space name and member list.
		// Resolution order:
		//  1. GET /meetings/{meetingId} — authoritative; gives roomId and scheduledType.
		//     scheduledType=="personalRoomMeeting" → route to personal-room regardless.
		//  2. roomId from the transcript item (fallback when meetings API fails).
		//  3. Title match against the user's room list (last resort).
		var spaceName, spaceType, resolvedRoomID string
		var members []RoomMember

		resolvedRoomID = item.RoomID
		skipTitleMatch := false

		if details, err := client.fetchMeetingDetails(item.MeetingID); err == nil {
			if details.ScheduledType == "personalRoomMeeting" {
				// Definitively a Personal Meeting Room — clear any roomId and
				// skip title matching so it routes to personal-room.
				resolvedRoomID = ""
				skipTitleMatch = true
			} else if details.RoomID != "" {
				resolvedRoomID = details.RoomID
				skipTitleMatch = true
			}
		} else {
			fmt.Fprintf(os.Stderr, "        note: meeting details lookup failed for %s — %v\n", item.MeetingID, err)
		}

		if resolvedRoomID != "" {
			if info, err := client.fetchRoomInfo(resolvedRoomID); err == nil {
				spaceName = info.Title
				spaceType = info.Type
			} else {
				fmt.Fprintf(os.Stderr, "        note: room name lookup failed for %s — using meeting topic as folder name (%v)\n", resolvedRoomID, err)
			}
		} else if !skipTitleMatch {
			// No roomId resolved and not a confirmed PMR — try title match.
			if allRooms, err := client.fetchAllRooms(); err == nil {
				key := strings.ToLower(item.MeetingTopic)
				if matched, ok := allRooms[key]; ok {
					resolvedRoomID = matched.ID
					spaceName = matched.Title
					spaceType = matched.Type
					fmt.Fprintf(os.Stderr, "        note: matched %q to Space %q (type=%s) via title lookup\n", item.MeetingTopic, matched.Title, matched.Type)
				}
			}
		}

		if resolvedRoomID != "" {
			if m, err := client.fetchRoomMembers(resolvedRoomID); err == nil {
				members = m
			} else {
				fmt.Fprintf(os.Stderr, "        note: could not fetch room members — %v\n", err)
			}
		}
		transcripts = append(transcripts, Transcript{
			ID:           item.ID,
			MeetingID:    item.MeetingID,
			MeetingTitle: item.MeetingTopic,
			SpaceName:    spaceName,
			SpaceType:    spaceType,
			RoomID:       resolvedRoomID,
			StartTime:    startTime,
			Content:      content,
			AISummary:    summary,
			RoomMembers:  members,
		})
	}
	return transcripts
}

// fetchMeetingDetails fetches the roomId and scheduledType for a Webex meeting
// instance from GET /meetings/{meetingId}. The scheduledType field definitively
// identifies personal room meetings ("personalRoomMeeting") vs. ad-hoc or
// space meetings ("meeting"/"webinar"). Results are cached.
func (c *WebexClient) fetchMeetingDetails(meetingID string) (MeetingDetails, error) {
	c.meetingDetailsMu.Lock()
	if c.meetingDetailsCache == nil {
		c.meetingDetailsCache = make(map[string]MeetingDetails)
	}
	if d, ok := c.meetingDetailsCache[meetingID]; ok {
		c.meetingDetailsMu.Unlock()
		return d, nil
	}
	c.meetingDetailsMu.Unlock()

	apiURL := webexAPIBase + "/meetings/" + url.PathEscape(meetingID)
	resp, err := c.httpClient.Get(apiURL)
	if err != nil {
		return MeetingDetails{}, fmt.Errorf("fetching meeting %s: %w", meetingID, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return MeetingDetails{}, fmt.Errorf("meetings API returned %d for %s", resp.StatusCode, meetingID)
	}
	var d MeetingDetails
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		return MeetingDetails{}, fmt.Errorf("decoding meeting %s: %w", meetingID, err)
	}

	c.meetingDetailsMu.Lock()
	c.meetingDetailsCache[meetingID] = d
	c.meetingDetailsMu.Unlock()
	return d, nil
}

// fetchRoomInfo resolves a Webex roomId to its title and type ("direct" or
// "group"). Results are cached. Returns an error when the room cannot be found
// or the API fails.
func (c *WebexClient) fetchRoomInfo(roomID string) (RoomInfo, error) {
	c.roomInfoMu.Lock()
	if c.roomInfoCache == nil {
		c.roomInfoCache = make(map[string]RoomInfo)
	}
	if info, ok := c.roomInfoCache[roomID]; ok {
		c.roomInfoMu.Unlock()
		return info, nil
	}
	c.roomInfoMu.Unlock()

	apiURL := webexAPIBase + "/rooms/" + url.PathEscape(roomID)
	resp, err := c.httpClient.Get(apiURL)
	if err != nil {
		return RoomInfo{}, fmt.Errorf("fetching room %s: %w", roomID, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return RoomInfo{}, fmt.Errorf("rooms API returned %d for %s", resp.StatusCode, roomID)
	}
	var room struct {
		ID    string `json:"id"`
		Title string `json:"title"`
		Type  string `json:"type"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&room); err != nil {
		return RoomInfo{}, fmt.Errorf("decoding room %s: %w", roomID, err)
	}
	info := RoomInfo{ID: room.ID, Title: room.Title, Type: room.Type}

	c.roomInfoMu.Lock()
	c.roomInfoCache[roomID] = info
	c.roomInfoMu.Unlock()
	return info, nil
}

// fetchRoomName is a convenience wrapper around fetchRoomInfo that returns
// only the room title. Kept for callers that don't need the type.
func (c *WebexClient) fetchRoomName(roomID string) (string, error) {
	info, err := c.fetchRoomInfo(roomID)
	return info.Title, err
}

// fetchAllRooms returns a map of all Webex Spaces the authenticated user is a
// member of, keyed by lowercase title. The result is cached after the first
// call. It is used to resolve meeting-topic-to-room matches when the
// /meetingTranscripts API returns no roomId (common for PMR-hosted meetings).
func (c *WebexClient) fetchAllRooms() (map[string]RoomInfo, error) {
	c.allRoomsMu.Lock()
	if c.allRoomsCache != nil {
		m := c.allRoomsCache
		c.allRoomsMu.Unlock()
		return m, nil
	}
	c.allRoomsMu.Unlock()

	byTitle := make(map[string]RoomInfo)
	pageURL := fmt.Sprintf("%s/rooms?max=1000", webexAPIBase)
	for pageURL != "" {
		resp, err := c.httpClient.Get(pageURL)
		if err != nil {
			return nil, fmt.Errorf("fetching room list: %w", err)
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return nil, fmt.Errorf("rooms list API returned %d", resp.StatusCode)
		}
		var result struct {
			Items []struct {
				ID    string `json:"id"`
				Title string `json:"title"`
				Type  string `json:"type"`
			} `json:"items"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			resp.Body.Close()
			return nil, fmt.Errorf("decoding room list: %w", err)
		}
		resp.Body.Close()
		for _, item := range result.Items {
			info := RoomInfo{ID: item.ID, Title: item.Title, Type: item.Type}
			byTitle[strings.ToLower(item.Title)] = info
			// Also seed the per-room cache so fetchRoomInfo skips the API call.
			c.roomInfoMu.Lock()
			if c.roomInfoCache == nil {
				c.roomInfoCache = make(map[string]RoomInfo)
			}
			c.roomInfoCache[item.ID] = info
			c.roomInfoMu.Unlock()
		}
		pageURL = parseLinkNext(resp.Header.Get("Link"))
	}

	c.allRoomsMu.Lock()
	c.allRoomsCache = byTitle
	c.allRoomsMu.Unlock()
	return byTitle, nil
}

// fetchRoomMembers returns the members of the Webex Space identified by
// roomID. Results are cached. Pagination is followed automatically.
func (c *WebexClient) fetchRoomMembers(roomID string) ([]RoomMember, error) {
	c.roomMembersMu.Lock()
	if c.roomMembersCache == nil {
		c.roomMembersCache = make(map[string][]RoomMember)
	}
	if members, ok := c.roomMembersCache[roomID]; ok {
		c.roomMembersMu.Unlock()
		return members, nil
	}
	c.roomMembersMu.Unlock()

	var all []RoomMember
	pageURL := fmt.Sprintf("%s/memberships?roomId=%s&max=1000", webexAPIBase, url.QueryEscape(roomID))
	for pageURL != "" {
		resp, err := c.httpClient.Get(pageURL)
		if err != nil {
			return nil, fmt.Errorf("fetching memberships for room %s: %w", roomID, err)
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return nil, fmt.Errorf("memberships API returned %d for room %s", resp.StatusCode, roomID)
		}
		var result struct {
			Items []struct {
				PersonDisplayName string `json:"personDisplayName"`
				PersonEmail       string `json:"personEmail"`
				IsModerator       bool   `json:"isModerator"`
			} `json:"items"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			resp.Body.Close()
			return nil, fmt.Errorf("decoding memberships for room %s: %w", roomID, err)
		}
		resp.Body.Close()
		for _, item := range result.Items {
			all = append(all, RoomMember{
				DisplayName: item.PersonDisplayName,
				Email:       item.PersonEmail,
				IsModerator: item.IsModerator,
			})
		}
		pageURL = parseLinkNext(resp.Header.Get("Link"))
	}

	c.roomMembersMu.Lock()
	c.roomMembersCache[roomID] = all
	c.roomMembersMu.Unlock()
	return all, nil
}

// transcriptItem is the JSON shape returned by the Webex
// GET /meetingTranscripts list endpoint.
type transcriptItem struct {
	ID           string `json:"id"`
	MeetingID    string `json:"meetingId"`
	MeetingTopic string `json:"meetingTopic"`
	RoomID       string `json:"roomId"`
	StartTime    string `json:"startTime"`
}

// listTranscriptPage fetches a single page of transcript metadata from apiURL
// and returns the items together with the URL of the next page, if any.
func listTranscriptPage(client *WebexClient, apiURL string) ([]transcriptItem, string, error) {
	resp, err := client.httpClient.Get(apiURL)
	if err != nil {
		return nil, "", fmt.Errorf("listing transcripts: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, "", fmt.Errorf("Webex API returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var result struct {
		Items []transcriptItem `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, "", fmt.Errorf("decoding transcript list: %w", err)
	}

	nextURL := parseLinkNext(resp.Header.Get("Link"))
	return result.Items, nextURL, nil
}

// parseLinkNext extracts the URL from a Link response header of the form
//
//	<url>; rel="next"
//
// It returns an empty string when no "next" relation is present.
func parseLinkNext(link string) string {
	if link == "" {
		return ""
	}
	for _, part := range strings.Split(link, ",") {
		segs := strings.SplitN(strings.TrimSpace(part), ";", 2)
		if len(segs) == 2 && strings.TrimSpace(segs[1]) == `rel="next"` {
			return strings.Trim(strings.TrimSpace(segs[0]), "<>")
		}
	}
	return ""
}

// fetchMeetingSummary fetches the AI-generated summary for meetingID from the
// Webex Meeting Summaries API. It returns an empty string when no summary
// exists and a non-nil error for any other failure.
//
// The returned string is a plain-text block containing the meeting notes
// followed by a numbered list of action items, suitable for direct insertion
// into a Google Doc.
func fetchMeetingSummary(client *WebexClient, meetingID string) (string, error) {
	apiURL := fmt.Sprintf("%s/meetingSummaries?meetingId=%s", webexAPIBase, url.QueryEscape(meetingID))

	resp, err := client.httpClient.Get(apiURL)
	if err != nil {
		return "", fmt.Errorf("summary request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return "", fmt.Errorf("no AI summary for meeting %s", meetingID)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("summary HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var result struct {
		Items []struct {
			Notes struct {
				Content string `json:"content"`
			} `json:"notes"`
			ActionItems []struct {
				Content string `json:"content"`
			} `json:"actionItems"`
		} `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decoding summary: %w", err)
	}

	if len(result.Items) == 0 {
		return "", fmt.Errorf("no AI summary for meeting %s", meetingID)
	}

	item := result.Items[0]
	notesText := stripHTML(item.Notes.Content)

	if notesText == "" && len(item.ActionItems) == 0 {
		return "", fmt.Errorf("empty AI summary for meeting %s", meetingID)
	}

	var sb strings.Builder
	if notesText != "" {
		sb.WriteString(notesText)
		sb.WriteString("\n")
	}
	if len(item.ActionItems) > 0 {
		sb.WriteString("\nAction Items:\n")
		for i, ai := range item.ActionItems {
			fmt.Fprintf(&sb, "%d. %s\n", i+1, ai.Content)
		}
	}
	return sb.String(), nil
}

// htmlTagRE matches any HTML tag for use in stripHTML.
var htmlTagRE = regexp.MustCompile(`<[^>]+>`)

// stripHTML converts an HTML string to readable plain text. Block-level closing
// tags are replaced with newlines before all remaining tags are stripped, so
// the output retains paragraph structure.
func stripHTML(html string) string {
	r := strings.NewReplacer(
		"</h2>", "\n\n",
		"</h3>", "\n",
		"</p>", "\n\n",
		"</li>", "\n",
		"<br>", "\n",
		"<br/>", "\n",
		"<br />", "\n",
	)
	s := r.Replace(html)
	s = htmlTagRE.ReplaceAllString(s, "")
	for strings.Contains(s, "\n\n\n") {
		s = strings.ReplaceAll(s, "\n\n\n", "\n\n")
	}
	return strings.TrimSpace(s)
}

// downloadTranscript downloads the raw VTT content for the transcript
// identified by transcriptID from the Webex API.
func downloadTranscript(client *WebexClient, transcriptID string) (string, error) {
	apiURL := fmt.Sprintf("%s/meetingTranscripts/%s/download", webexAPIBase, transcriptID)

	resp, err := client.httpClient.Get(apiURL)
	if err != nil {
		return "", fmt.Errorf("download request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("download HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	content, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("reading transcript body: %w", err)
	}
	return string(content), nil
}

// botSpace represents a Webex Space (room) the bot is a member of.
type botSpace struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Type  string `json:"type"` // "direct" or "group"
}

// userConfigPath returns the path to the user-level config file where the
// Webex personal access token is persisted between runs.
//
//	~/.webex-meeting-sync/.env
func userConfigPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".webex-meeting-sync", ".env")
}

// saveUserToken writes WEBEX_TOKEN=<token> to the user config file, creating
// the directory if needed. Existing content is replaced.
func saveUserToken(token string) error {
	path := userConfigPath()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("creating config dir: %w", err)
	}
	content := fmt.Sprintf("WEBEX_TOKEN=%q\n", token)
	return os.WriteFile(path, []byte(content), 0600)
}

// validateWebexToken calls GET /people/me with the given token and returns the
// display name of the authenticated user, or a non-nil error when the token is
// absent, expired, or invalid.
func validateWebexToken(token string) (string, error) {
	if token == "" {
		return "", fmt.Errorf("token is empty")
	}
	req, _ := http.NewRequest(http.MethodGet, webexAPIBase+"/people/me", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("validating token: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return "", fmt.Errorf("token is invalid or expired (401)")
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("token validation HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var me struct {
		DisplayName string `json:"displayName"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&me); err != nil {
		return "", fmt.Errorf("decoding /people/me: %w", err)
	}
	return me.DisplayName, nil
}

// ensureWebexToken checks that WEBEX_TOKEN is set and valid. When it is
// missing or expired, the user is prompted to paste a new token from
// https://developer.webex.com. The new token is validated, saved to the user
// config file (~/.webex-meeting-sync/.env), and set in the process environment
// so subsequent code sees it immediately.
//
// This function is a no-op when the existing token is valid.
func ensureWebexToken() error {
	token := os.Getenv("WEBEX_TOKEN")

	// Fast path: existing token is valid.
	if name, err := validateWebexToken(token); err == nil {
		fmt.Printf("Webex token valid — signed in as %s.\n", name)
		return nil
	}

	if token != "" {
		fmt.Println("Webex token is expired or invalid — a new token is required.")
	} else {
		fmt.Println("No Webex token found.")
	}
	fmt.Println()
	fmt.Println("  1. Open https://developer.webex.com/docs/getting-started")
	fmt.Println("  2. Sign in and copy the Personal Access Token (valid for 12 hours).")
	fmt.Println()

	reader := bufio.NewReader(os.Stdin)
	for {
		fmt.Print("Paste your Webex Personal Access Token: ")
		input, err := reader.ReadString('\n')
		if err != nil {
			return fmt.Errorf("reading token: %w", err)
		}
		newToken := strings.TrimSpace(input)
		if newToken == "" {
			fmt.Println("Token cannot be empty, please try again.")
			continue
		}

		name, err := validateWebexToken(newToken)
		if err != nil {
			fmt.Printf("Invalid token (%v), please try again.\n", err)
			continue
		}

		// Token is valid — persist and inject into the current process.
		if saveErr := saveUserToken(newToken); saveErr != nil {
			fmt.Fprintf(os.Stderr, "warning: could not save token to %s: %v\n", userConfigPath(), saveErr)
		} else {
			fmt.Printf("Token saved to %s\n", userConfigPath())
		}
		os.Setenv("WEBEX_TOKEN", newToken)
		fmt.Printf("Signed in as %s.\n", name)
		return nil
	}
}

// newBotClient creates a WebexClient from WEBEX_BOT_TOKEN.
// when the variable is not set.
func newBotClient(ctx context.Context) (*WebexClient, error) {
	tok := os.Getenv("WEBEX_BOT_TOKEN")
	if tok == "" {
		return nil, fmt.Errorf("WEBEX_BOT_TOKEN is not set — add it to .env or the environment")
	}
	fmt.Println("Using WEBEX_BOT_TOKEN.")
	ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: tok})
	return &WebexClient{httpClient: oauth2.NewClient(ctx, ts)}, nil
}

// listBotSpaces returns all Webex Spaces the bot is a member of. It follows
// pagination so it returns the complete list regardless of room count.
func listBotSpaces(client *WebexClient) ([]botSpace, error) {
	var all []botSpace
	apiURL := fmt.Sprintf("%s/rooms?max=1000&type=group", webexAPIBase)

	for apiURL != "" {
		resp, err := client.httpClient.Get(apiURL)
		if err != nil {
			return nil, fmt.Errorf("listing bot spaces: %w", err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("listing bot spaces HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
		}

		var result struct {
			Items []botSpace `json:"items"`
		}
		if err := json.Unmarshal(body, &result); err != nil {
			return nil, fmt.Errorf("decoding bot spaces: %w", err)
		}
		all = append(all, result.Items...)
		apiURL = parseLinkNext(resp.Header.Get("Link"))
	}
	return all, nil
}
