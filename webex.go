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
	"time"

	"golang.org/x/oauth2"
)

const (
	webexAuthURL  = "https://webexapis.com/v1/authorize"
	webexTokenURL = "https://webexapis.com/v1/access_token"
	webexAPIBase  = "https://webexapis.com/v1"
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
	webexScope      = "meeting:schedules_read meeting:transcripts_read meeting:summaries_read spark:rooms_read"
	webexAdminScope = "meeting:admin_transcript_read meeting:admin_summaries_read"
	redirectURI = "http://localhost:47823/callback"
)

// WebexClient wraps an authenticated HTTP client for the Webex REST API.
type WebexClient struct {
	httpClient *http.Client
}

// Transcript holds the metadata, raw VTT content, and optional AI summary of
// a single Webex meeting transcript.
type Transcript struct {
	ID           string
	MeetingID    string
	MeetingTitle string
	StartTime    time.Time
	Content      string
	// AISummary is the AI-generated meeting summary. It is empty when the
	// meeting had no AI summary or when the summary API returned an error.
	AISummary string
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
// from and to must be in "YYYY-MM-DD" format; empty strings default to 30 days
// ago and today (UTC) respectively. When spaceID is non-empty only transcripts
// from meetings that took place in that Webex Space are returned.
func listTranscriptItems(ctx context.Context, client *WebexClient, from, to, spaceID string) ([]transcriptItem, error) {
	now := time.Now().UTC()
	fromTime := now.AddDate(0, 0, -30)
	toTime := now

	if from != "" {
		t, err := time.Parse("2006-01-02", from)
		if err != nil {
			return nil, fmt.Errorf("invalid --from date %q: %w", from, err)
		}
		fromTime = t
	}
	if to != "" {
		t, err := time.Parse("2006-01-02", to)
		if err != nil {
			return nil, fmt.Errorf("invalid --to date %q: %w", to, err)
		}
		// Include the full end day.
		toTime = t.Add(24*time.Hour - time.Second)
	}

	firstPage := fmt.Sprintf(
		"%s/meetingTranscripts?from=%s&to=%s&max=100",
		webexAPIBase,
		url.QueryEscape(fromTime.Format(time.RFC3339)),
		url.QueryEscape(toTime.Format(time.RFC3339)),
	)
	if spaceID != "" {
		firstPage += "&roomId=" + url.QueryEscape(spaceID)
	}

	var allItems []transcriptItem
	for pageURL := firstPage; pageURL != ""; {
		items, nextURL, err := listTranscriptPage(client, pageURL)
		if err != nil {
			return nil, err
		}
		allItems = append(allItems, items...)
		pageURL = nextURL
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
		transcripts = append(transcripts, Transcript{
			ID:           item.ID,
			MeetingID:    item.MeetingID,
			MeetingTitle: item.MeetingTopic,
			StartTime:    startTime,
			Content:      content,
			AISummary:    summary,
		})
	}
	return transcripts
}

// transcriptItem is the JSON shape returned by the Webex
// GET /meetingTranscripts list endpoint.
type transcriptItem struct {
	ID           string `json:"id"`
	MeetingID    string `json:"meetingId"`
	MeetingTopic string `json:"meetingTopic"`
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
