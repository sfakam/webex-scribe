// gdocs.go provides Google Drive folder management and Google Docs creation
// for webex-scribe.
//
// Google Drive is used to organise documents into a folder hierarchy:
//
//	webex-meetings/
//	  YYYY-MM/          (one per calendar month)
//	    <transcript>    (one Google Doc per meeting)
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	docsapi "google.golang.org/api/docs/v1"
	driveapi "google.golang.org/api/drive/v3"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
)

const (
	// rootFolderName is the top-level Google Drive folder that contains all
	// year-month subfolders for personal transcripts.
	rootFolderName = "webex-meetings"

	// botRootFolderName is the top-level Drive folder used when running in
	// bot mode (--bot), keeping bot-sourced transcripts separate.
	botRootFolderName = "plx-webex-meetings"

	// driveFolderMIME is the MIME type that identifies a Google Drive folder.
	driveFolderMIME = "application/vnd.google-apps.folder"

	// googleCallbackPort is the local port used for the Google OAuth2 redirect.
	// Chosen to avoid conflicts with common development servers.
	googleCallbackPort = "47824"
)

// googleScopes are the OAuth2 scopes required by this tool.
var googleScopes = []string{
	"https://www.googleapis.com/auth/drive",
	"https://www.googleapis.com/auth/documents",
}

// googleClients groups authenticated API clients for Google Docs and Drive.
// Both services share a single OAuth2 HTTP client.
type googleClients struct {
	docs  *docsapi.Service
	drive *driveapi.Service

	// folderMu guards folderCache.
	folderMu sync.Mutex
	// folderCache maps "parentID/name" to Drive folder ID, avoiding redundant
	// API calls and preventing duplicate-folder races during parallel uploads.
	folderCache map[string]string
}

// googleTokenPath returns the path to the persisted Google OAuth2 token file.
func googleTokenPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "webex-scribe", "google_token.json")
}

// loadGoogleToken reads and unmarshals a previously saved Google oauth2.Token.
func loadGoogleToken() (*oauth2.Token, error) {
	data, err := os.ReadFile(googleTokenPath())
	if err != nil {
		return nil, err
	}
	var tok oauth2.Token
	if err := json.Unmarshal(data, &tok); err != nil {
		return nil, err
	}
	return &tok, nil
}

// saveGoogleToken serializes tok as JSON and writes it to googleTokenPath with
// permissions 0600.
func saveGoogleToken(tok *oauth2.Token) error {
	path := googleTokenPath()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	data, err := json.Marshal(tok)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

// newGoogleClients returns authenticated Google Docs and Drive API services.
//
// Authentication is attempted in the following order:
//
//  1. Application Default Credentials (ADC) via GOOGLE_APPLICATION_CREDENTIALS
//     or the well-known ADC path — covers service accounts and gcloud ADC.
//  2. Saved Google OAuth2 token from a previous in-app authorization.
//  3. Interactive in-app OAuth2 flow using GOOGLE_CLIENT_ID and
//     GOOGLE_CLIENT_SECRET (set in .env). Opens a browser and listens on
//     localhost:47824 for the redirect callback.
//  4. gcloud auth print-access-token subprocess (legacy fallback).
func newGoogleClients(ctx context.Context) (*googleClients, error) {
	// Path 1: Application Default Credentials (service account or gcloud ADC).
	if creds, err := google.FindDefaultCredentials(ctx, googleScopes...); err == nil {
		httpClient := oauth2.NewClient(ctx, creds.TokenSource)
		if c, err := buildGoogleClients(ctx, option.WithHTTPClient(httpClient)); err == nil {
			return c, nil
		}
	}

	// Path 2 & 3: in-app OAuth2 using GOOGLE_CLIENT_ID / GOOGLE_CLIENT_SECRET.
	gClientID := os.Getenv("GOOGLE_CLIENT_ID")
	gClientSecret := os.Getenv("GOOGLE_CLIENT_SECRET")
	if gClientID != "" && gClientSecret != "" {
		cfg := &oauth2.Config{
			ClientID:     gClientID,
			ClientSecret: gClientSecret,
			RedirectURL:  "http://localhost:" + googleCallbackPort + "/callback",
			Scopes:       googleScopes,
			Endpoint:     google.Endpoint,
		}

		// Try a persisted token first.
		var ts oauth2.TokenSource
		if tok, err := loadGoogleToken(); err == nil {
			ts = cfg.TokenSource(ctx, tok)
		} else {
			// No saved token — run the interactive flow.
			tok, err := doGoogleOAuthFlow(ctx, cfg)
			if err != nil {
				return nil, fmt.Errorf("Google OAuth2 flow: %w", err)
			}
			if saveErr := saveGoogleToken(tok); saveErr != nil {
				fmt.Fprintf(os.Stderr, "warning: could not save Google token: %v\n", saveErr)
			}
			ts = cfg.TokenSource(ctx, tok)
		}

		// Wrap in a persisting source so refreshes are saved to disk.
		pts := &persistingGoogleTokenSource{base: ts}
		httpClient := oauth2.NewClient(ctx, pts)
		return buildGoogleClients(ctx, option.WithHTTPClient(httpClient))
	}

	// Path 4: gcloud subprocess (last resort).
	if tok, err := gcloudAccessToken(); err == nil {
		httpClient := oauth2.NewClient(ctx, oauth2.StaticTokenSource(&oauth2.Token{AccessToken: tok}))
		return buildGoogleClients(ctx, option.WithHTTPClient(httpClient))
	}

	return nil, fmt.Errorf(
		"Google authentication failed. Add one of the following to your .env file:\n\n" +
			"  GOOGLE_CLIENT_ID=<id>\n" +
			"  GOOGLE_CLIENT_SECRET=<secret>\n\n" +
			"Create a Desktop OAuth2 credential at:\n" +
			"  https://console.cloud.google.com/apis/credentials\n" +
			"and enable the Google Docs and Drive APIs for your project.\n",
	)
}

// persistingGoogleTokenSource wraps an oauth2.TokenSource and saves the token
// to disk whenever it is refreshed.
type persistingGoogleTokenSource struct {
	base oauth2.TokenSource
	last string
}

// Token returns the current Google OAuth2 token, persisting it on refresh.
func (p *persistingGoogleTokenSource) Token() (*oauth2.Token, error) {
	tok, err := p.base.Token()
	if err != nil {
		return nil, err
	}
	if tok.AccessToken != p.last {
		p.last = tok.AccessToken
		if saveErr := saveGoogleToken(tok); saveErr != nil {
			fmt.Fprintf(os.Stderr, "warning: could not persist refreshed Google token: %v\n", saveErr)
		}
	}
	return tok, nil
}

// doGoogleOAuthFlow runs an interactive OAuth2 authorization code flow for
// Google. It prints the authorization URL, starts a local callback server on
// port 47824, and blocks until the browser completes the consent screen.
func doGoogleOAuthFlow(ctx context.Context, cfg *oauth2.Config) (*oauth2.Token, error) {
	state := fmt.Sprintf("wts-google-%d", time.Now().UnixNano())
	authURL := cfg.AuthCodeURL(state, oauth2.AccessTypeOffline)

	fmt.Printf("Open the following URL in your browser to authorize Google access:\n\n  %s\n\n", authURL)
	fmt.Printf("Waiting for callback on http://localhost:%s/callback ...\n", googleCallbackPort)

	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("state") != state {
			http.Error(w, "state mismatch", http.StatusBadRequest)
			errCh <- fmt.Errorf("Google OAuth2 state mismatch — possible CSRF attack")
			return
		}
		code := r.URL.Query().Get("code")
		if code == "" {
			desc := r.URL.Query().Get("error_description")
			if desc == "" {
				desc = r.URL.Query().Get("error")
			}
			http.Error(w, "missing authorization code", http.StatusBadRequest)
			errCh <- fmt.Errorf("no authorization code in Google callback: %s", desc)
			return
		}
		fmt.Fprintln(w, "Google authorization successful! You can close this window.")
		codeCh <- code
	})

	ln, err := net.Listen("tcp", "localhost:"+googleCallbackPort)
	if err != nil {
		return nil, fmt.Errorf(
			"could not listen on localhost:%s: %w\n\nEnsure that port %s is free and that your Google OAuth2 credential's redirect URI includes http://localhost:%s/callback",
			googleCallbackPort, err, googleCallbackPort, googleCallbackPort,
		)
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
		return nil, fmt.Errorf("exchanging Google authorization code: %w", err)
	}
	return tok, nil
}

// buildGoogleClients creates Docs and Drive services from a shared client option.
func buildGoogleClients(ctx context.Context, opt option.ClientOption) (*googleClients, error) {
	docsSvc, err := docsapi.NewService(ctx, opt)
	if err != nil {
		return nil, fmt.Errorf("creating Docs service: %w", err)
	}
	driveSvc, err := driveapi.NewService(ctx, opt)
	if err != nil {
		return nil, fmt.Errorf("creating Drive service: %w", err)
	}
	return &googleClients{docs: docsSvc, drive: driveSvc, folderCache: make(map[string]string)}, nil
}

// ensureFolderCached is a concurrency-safe wrapper around ensureFolder that
// caches results by "parentID/name" key. The mutex is held for the entire
// find-or-create operation so that concurrent goroutines requesting the same
// folder name cannot both race past the cache check and create duplicate folders.
func (c *googleClients) ensureFolderCached(ctx context.Context, name, parentID string) (string, error) {
	key := parentID + "/" + name
	c.folderMu.Lock()
	defer c.folderMu.Unlock()

	if id, ok := c.folderCache[key]; ok {
		return id, nil
	}

	id, err := ensureFolder(ctx, c.drive, name, parentID)
	if err != nil {
		return "", err
	}
	c.folderCache[key] = id
	return id, nil
}

// ensureFolder returns the Drive folder ID for a folder named name whose
// parent is parentID (use "root" for the top level of My Drive).
//
// If a matching non-trashed folder already exists it is returned; otherwise a
// new folder is created. When the same name exists more than once the first
// result returned by the API is used.
func ensureFolder(ctx context.Context, svc *driveapi.Service, name, parentID string) (string, error) {
	// Drive query syntax uses single-quoted string literals; escape any
	// literal single quotes in the name by replacing ' with \'.
	safeName := strings.ReplaceAll(name, "'", `\'`)
	q := fmt.Sprintf(
		"name='%s' and mimeType='%s' and '%s' in parents and trashed=false",
		safeName, driveFolderMIME, parentID,
	)
	list, err := svc.Files.List().Q(q).Fields("files(id)").Context(ctx).Do()
	if err != nil {
		return "", fmt.Errorf("searching for Drive folder %q: %w", name, err)
	}
	if len(list.Files) > 0 {
		return list.Files[0].Id, nil
	}

	// Folder does not exist — create it.
	f, err := svc.Files.Create(&driveapi.File{
		Name:     name,
		MimeType: driveFolderMIME,
		Parents:  []string{parentID},
	}).Fields("id").Context(ctx).Do()
	if err != nil {
		return "", fmt.Errorf("creating Drive folder %q: %w", name, err)
	}
	return f.Id, nil
}

// gcloudAccessToken returns a short-lived bearer token by invoking
// `gcloud auth print-access-token` with the Drive scope, which also covers
// the Docs API. This matches the approach used by claude-usage-tracker and
// requires the user to have previously run:
//
//	gcloud auth login --enable-gdrive-access
func gcloudAccessToken() (string, error) {
	var out, errBuf bytes.Buffer
	cmd := exec.Command("gcloud", "auth", "print-access-token",
		"--scopes=https://www.googleapis.com/auth/drive")
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errBuf.String())
		if strings.Contains(msg, "invalid_scope") || strings.Contains(msg, "Bad Request") {
			return "", fmt.Errorf(
				"gcloud token lacks required scopes — re-authenticate with:\n\n" +
					"  gcloud auth login --enable-gdrive-access\n",
			)
		}
		return "", fmt.Errorf("gcloud auth print-access-token: %w", err)
	}
	tok := strings.TrimSpace(out.String())
	if tok == "" {
		return "", fmt.Errorf("gcloud returned an empty token")
	}
	return tok, nil
}

// createMeetingDocs creates a meeting-level subfolder inside the appropriate
// month folder and populates it with a Transcript doc and, when an AI summary
// is available, a Summary doc.
//
// The Drive folder hierarchy after this call is:
//
//	<rootFolderID>/
//	  YYYY-MM/
//	    <meeting title> — Jan 2, 2006/
//	      Transcript
//	      Summary          ← only when t.AISummary is non-empty
//
// It returns the URLs of the Transcript doc and the Summary doc. summaryURL is
// empty when no AI summary was available.
func createMeetingDocs(ctx context.Context, clients *googleClients, rootFolderID string, t Transcript) (transcriptURL, summaryURL string, err error) {
	// Ensure the year-month subfolder, e.g. "2026-04".
	monthFolder := t.StartTime.Format("2006-01")
	monthFolderID, err := clients.ensureFolderCached(ctx, monthFolder, rootFolderID)
	if err != nil {
		return "", "", fmt.Errorf("ensuring month folder %q: %w", monthFolder, err)
	}

	// Ensure a per-meeting subfolder titled "<meeting title> — Jan 2, 2006".
	meetingFolderName := fmt.Sprintf("%s — %s", t.MeetingTitle, t.StartTime.Format("Jan 2, 2006"))
	meetingFolderID, err := clients.ensureFolderCached(ctx, meetingFolderName, monthFolderID)
	if err != nil {
		return "", "", fmt.Errorf("ensuring meeting folder %q: %w", meetingFolderName, err)
	}

	// Create the Transcript doc.
	transcriptURL, err = createDocInFolder(ctx, clients, meetingFolderID, "Transcript", "Transcript", t.Content)
	if err != nil {
		return "", "", fmt.Errorf("creating transcript doc: %w", err)
	}

	// Create the Summary doc only when an AI summary was fetched.
	if t.AISummary != "" {
		summaryURL, err = createDocInFolder(ctx, clients, meetingFolderID, "Summary", "AI Summary", t.AISummary)
		if err != nil {
			return "", "", fmt.Errorf("creating summary doc: %w", err)
		}
	}

	return transcriptURL, summaryURL, nil
}

// createDocInFolder creates a new Google Doc with the given title inside
// folderID. The document body consists of an H1 heading followed by body text.
// The URL of the new document is returned.
//
// Index arithmetic in the Docs BatchUpdate API addresses UTF-16 code units.
// Headings and body text are assumed to be BMP-only Unicode, so rune count is
// used as an equivalent approximation of the code-unit count.
func createDocInFolder(ctx context.Context, clients *googleClients, folderID, title, heading, body string) (string, error) {
	var doc *docsapi.Document
	if err := retryOnRateLimit(func() error {
		var err error
		doc, err = clients.docs.Documents.Create(&docsapi.Document{Title: title}).Context(ctx).Do()
		return err
	}); err != nil {
		return "", fmt.Errorf("creating Google Doc %q: %w", title, err)
	}

	// Move the newly created doc from My Drive root into the target folder.
	if _, err := clients.drive.Files.Update(doc.DocumentId, &driveapi.File{}).
		AddParents(folderID).
		RemoveParents("root").
		Fields("id, parents").
		Context(ctx).Do(); err != nil {
		return "", fmt.Errorf("moving doc %s to folder: %w", doc.DocumentId, err)
	}

	headingText := heading + "\n"
	bodyText := body
	if !strings.HasSuffix(bodyText, "\n") {
		bodyText += "\n"
	}

	// Insert body first (at index 1), then heading (at index 1), so the final
	// order is: heading | body.
	insertReqs := []*docsapi.Request{
		insertTextReq(1, bodyText),
		insertTextReq(1, headingText),
	}
	if err := retryOnRateLimit(func() error {
		_, err := clients.docs.Documents.BatchUpdate(doc.DocumentId, &docsapi.BatchUpdateDocumentRequest{
			Requests: insertReqs,
		}).Context(ctx).Do()
		return err
	}); err != nil {
		return "", fmt.Errorf("inserting content into doc %s: %w", doc.DocumentId, err)
	}

	// Apply H1 style to the heading paragraph.
	headingLen := int64(len([]rune(headingText)))
	styleReqs := []*docsapi.Request{
		headingStyleReq(1, 1+headingLen, "HEADING_1"),
	}
	if err := retryOnRateLimit(func() error {
		_, err := clients.docs.Documents.BatchUpdate(doc.DocumentId, &docsapi.BatchUpdateDocumentRequest{
			Requests: styleReqs,
		}).Context(ctx).Do()
		return err
	}); err != nil {
		return "", fmt.Errorf("applying styles to doc %s: %w", doc.DocumentId, err)
	}

	return makeDocURL(doc.DocumentId), nil
}

// headingStyleReq returns a Docs API request that applies a named heading style
// to the paragraph spanning [startIndex, endIndex).
func headingStyleReq(startIndex, endIndex int64, style string) *docsapi.Request {
	return &docsapi.Request{
		UpdateParagraphStyle: &docsapi.UpdateParagraphStyleRequest{
			Range: &docsapi.Range{
				StartIndex: startIndex,
				EndIndex:   endIndex,
			},
			ParagraphStyle: &docsapi.ParagraphStyle{NamedStyleType: style},
			Fields:         "namedStyleType",
		},
	}
}

// insertTextReq returns a Docs API request that inserts text at the given
// UTF-16 code unit index.
func insertTextReq(index int64, text string) *docsapi.Request {
	return &docsapi.Request{
		InsertText: &docsapi.InsertTextRequest{
			Location: &docsapi.Location{Index: index},
			Text:     text,
		},
	}
}

// retryOnRateLimit calls fn and retries up to maxRetries times when the Docs
// or Drive API responds with a rate-limit error (HTTP 429 or 403
// rateLimitExceeded). Each retry waits 2^attempt seconds (capped at 64 s) to
// let the per-minute quota window reset.
func retryOnRateLimit(fn func() error) error {
	const maxRetries = 6
	for attempt := range maxRetries {
		err := fn()
		if err == nil {
			return nil
		}
		var gErr *googleapi.Error
		if !errors.As(err, &gErr) {
			return err
		}
		fmt.Printf("        warning: rate limit hit, wait...\n")
		if gErr.Code != http.StatusTooManyRequests &&
			!(gErr.Code == http.StatusForbidden && strings.Contains(gErr.Message, "rateLimitExceeded")) {
			return err
		}
		wait := time.Duration(math.Pow(2, float64(attempt+1))) * time.Second
		if wait > 64*time.Second {
			wait = 64 * time.Second
		}
		fmt.Printf("        rate limit hit, retrying in %s (attempt %d/%d)...\n", wait, attempt+1, maxRetries)
		time.Sleep(wait)
	}
	return fmt.Errorf("rate limit retry exhausted after %d attempts", maxRetries)
}

// makeDocURL returns the browser-accessible edit URL for a Google Doc.
func makeDocURL(docID string) string {
	return "https://docs.google.com/document/d/" + docID + "/edit"
}

// makeFolderURL returns the browser-accessible URL for a Google Drive folder.
func makeFolderURL(folderID string) string {
	return "https://drive.google.com/drive/folders/" + folderID
}
