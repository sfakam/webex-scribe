// Package main implements webex-scribe, a command-line tool that
// downloads Webex meeting transcripts and creates one Google Doc per meeting,
// titled with the meeting name and date.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// version is set at build time via:
//
//	go build -ldflags "-X main.version=$(git describe --tags --always --dirty)"
//
// Falls back to "dev" when built without the flag (e.g. `go run`).
var version = "dev"

// loadDotEnv reads a .env file from path and sets any key=value pairs as
// environment variables, skipping keys that are already set in the process
// environment. Lines beginning with '#' and blank lines are ignored.
//
// Values may optionally be wrapped in single or double quotes, which are
// stripped before the value is applied. No error is returned when the file
// does not exist.
func loadDotEnv(path string) error {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("opening %s: %w", path, err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		// Strip surrounding quotes.
		if len(val) >= 2 {
			if (val[0] == '"' && val[len(val)-1] == '"') ||
				(val[0] == '\'' && val[len(val)-1] == '\'') {
				val = val[1 : len(val)-1]
			}
		}
		// Don't override variables already set in the environment.
		if os.Getenv(key) == "" {
			if err := os.Setenv(key, val); err != nil {
				return fmt.Errorf("setting %s from .env: %w", key, err)
			}
		}
	}
	return scanner.Err()
}

// main is the entry point for webex-scribe.
//
// It authenticates with Webex using OAuth2, fetches all meeting transcripts in
// the requested date range, then creates one Google Doc per transcript inside
// a Drive folder hierarchy:
//
//	webex-meetings/
//	  YYYY-MM/
//	    <meeting title> — <date>
//
// Each document URL is printed to stdout. Errors for individual transcripts
// are reported as warnings; the tool continues processing the remaining ones.
func main() {
	// Load .env from the working directory before parsing flags so that
	// WEBEX_CLIENT_ID and WEBEX_CLIENT_SECRET are available as fallbacks.
	if err := loadDotEnv(".env"); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not load .env: %v\n", err)
	}
	// Load the user-level config (~/.webex-meeting-sync/.env) which stores the
	// persisted WEBEX_TOKEN. Project .env takes precedence (already loaded above
	// sets the env var, so loadDotEnv won't override it).
	if err := loadDotEnv(userConfigPath()); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not load user config: %v\n", err)
	}

	clientID := flag.String("client-id", "", "Webex OAuth2 client ID (or env WEBEX_CLIENT_ID)")
	clientSecret := flag.String("client-secret", "", "Webex OAuth2 client secret (or env WEBEX_CLIENT_SECRET)")
	from := flag.String("from", "", "Start date YYYY-MM-DD (default: --days ago)")
	to := flag.String("to", "", "End date YYYY-MM-DD (default: today)")
	days := flag.Int("days", 30, "How many days back to fetch transcripts (default 30; up to 180+). Ignored when --from/--to are set.")
	spaceID := flag.String("space-id", "", "Webex Space (room) ID to fetch transcripts from; omit for all meetings")
	admin := flag.Bool("admin", false, "Include meeting:admin_transcript_read scope to fetch all org transcripts (requires scope enabled in Webex app integration)")
	botMode := flag.Bool("bot", false, "Use WEBEX_BOT_TOKEN to list all spaces the bot is in and download transcripts into plx-webex-meetings/")
	reauth := flag.Bool("reauth", false, "Force re-authentication with Webex (deletes saved token)")
	googleReauth := flag.Bool("google-reauth", false, "Force re-authentication with Google (deletes saved token)")
	showVersion := flag.Bool("version", false, "Print version and exit")

	// Advanced flags hidden from default --help output.
	advanced := map[string]bool{"client-id": true, "client-secret": true, "help-advanced": true}
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage of webex-scribe:\n")
		flag.VisitAll(func(f *flag.Flag) {
			if advanced[f.Name] {
				return
			}
			if f.DefValue != "" && f.DefValue != "false" {
				fmt.Fprintf(os.Stderr, "  -%s string\n\t%s\n", f.Name, f.Usage)
			} else {
				fmt.Fprintf(os.Stderr, "  -%s\n\t%s\n", f.Name, f.Usage)
			}
		})
		fmt.Fprintf(os.Stderr, "\nFor advanced OAuth2 options run: webex-scribe -help-advanced\n")
	}
	helpAdvanced := flag.Bool("help-advanced", false, "Show all flags including advanced OAuth2 options")
	flag.Parse()

	if *showVersion {
		fmt.Println("webex-scribe", version)
		os.Exit(0)
	}

	if *helpAdvanced {
		fmt.Fprintf(os.Stderr, "Usage of webex-scribe (all flags):\n")
		flag.VisitAll(func(f *flag.Flag) {
			if f.Name == "help-advanced" {
				return
			}
			if f.DefValue != "" && f.DefValue != "false" {
				fmt.Fprintf(os.Stderr, "  -%s string\n\t%s\n", f.Name, f.Usage)
			} else {
				fmt.Fprintf(os.Stderr, "  -%s\n\t%s\n", f.Name, f.Usage)
			}
		})
		os.Exit(0)
	}

	// Prefer flags, fall back to environment variables.
	if *clientID == "" {
		*clientID = os.Getenv("WEBEX_CLIENT_ID")
	}
	if *clientSecret == "" {
		*clientSecret = os.Getenv("WEBEX_CLIENT_SECRET")
	}

	// If no WEBEX_TOKEN and no OAuth2 client credentials, prompt the user
	// for a personal access token now — before the usingOAuth check so the
	// prompt runs instead of exiting with an error.
	if !*botMode && *clientID == "" && os.Getenv("WEBEX_TOKEN") == "" {
		if err := ensureWebexToken(); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
	}

	// Client ID and secret are only required for the OAuth2 flow.
	// When WEBEX_TOKEN is set, newWebexClient uses it directly.
	// --bot always skips the personal sync regardless of WEBEX_TOKEN.
	usingOAuth := os.Getenv("WEBEX_TOKEN") == ""
	if !*botMode {
		if usingOAuth && *clientID == "" {
			fmt.Fprintln(os.Stderr, "error: provide -client-id / WEBEX_CLIENT_ID, or set WEBEX_TOKEN for token-based auth")
			flag.Usage()
			os.Exit(1)
		}
		if usingOAuth && *clientSecret == "" {
			fmt.Fprintln(os.Stderr, "error: provide -client-secret / WEBEX_CLIENT_SECRET, or set WEBEX_TOKEN for token-based auth")
			flag.Usage()
			os.Exit(1)
		}
	}

	if *reauth {
		if err := os.Remove(webexTokenPath()); err != nil && !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "warning: could not remove saved Webex token: %v\n", err)
		}
	}
	if *googleReauth {
		if err := os.Remove(googleTokenPath()); err != nil && !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "warning: could not remove saved Google token: %v\n", err)
		}
	}

	ctx := context.Background()

	// Authenticate with Google first so auth problems surface before the
	// (potentially slow) Webex transcript download begins.
	fmt.Println("Authenticating with Google...")
	clients, err := newGoogleClients(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	// Bot mode: skip personal sync entirely.
	if *botMode {
		runBotMode(ctx, clients, *from, *to)
		return
	}

	fmt.Printf("Ensuring Drive folder structure (%s/YYYY-MM)...\n", rootFolderName)
	rootFolderID, err := ensureFolder(ctx, clients.drive, rootFolderName, "root")
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	// Load the deduplication manifest from the webex-meetings Drive folder so
	// state is shared across machines.
	mf, err := loadDriveManifest(ctx, clients.drive, rootFolderID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not load manifest (starting fresh): %v\n", err)
		mf = &driveManifest{entries: make(map[string]manifestEntry), drive: clients.drive, folderID: rootFolderID}
	}
	fmt.Printf("Manifest loaded: %d previously uploaded transcript(s).\n\n", len(mf.entries))

	fmt.Println("Authenticating with Webex...")
	// Re-validate the token if we haven't already prompted (i.e. WEBEX_TOKEN
	// was already set from the environment or .env before we started).
	if !*botMode && *clientID == "" && os.Getenv("WEBEX_TOKEN") != "" {
		if err := ensureWebexToken(); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
	}
	webexClient, err := newWebexClient(ctx, *clientID, *clientSecret, *admin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	fmt.Print("Listing transcripts")
	if *from != "" || *to != "" {
		fmt.Printf(" (from=%s to=%s)", *from, *to)
	} else {
		fmt.Printf(" (last %d days)", *days)
	}
	if *spaceID != "" {
		fmt.Printf(" (space-id=%s)", *spaceID)
	}
	if *admin {
		fmt.Print(" (admin mode)")
	}
	fmt.Println("...")

	allItems, err := listTranscriptItems(ctx, webexClient, *from, *to, *days, *spaceID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	if len(allItems) == 0 {
		fmt.Println("No transcripts found for the specified date range.")
		return
	}

	// Filter out transcripts already in the manifest before downloading any
	// content — this avoids transferring data for transcripts we will skip.
	var toDownload []transcriptItem
	var skipped int
	for _, item := range allItems {
		if mf.knownID(item.ID) {
			t, _ := time.Parse(time.RFC3339, item.StartTime)
			fmt.Printf("  [skip] %s (%s) — already uploaded\n",
				item.MeetingTopic, t.Format("Jan 2, 2006"))
			skipped++
		} else {
			toDownload = append(toDownload, item)
		}
	}

	if len(toDownload) == 0 {
		fmt.Printf("\nAll %d transcript(s) already uploaded. Nothing to do.\n", skipped)
		fmt.Printf("\n  webex-meetings folder: %s\n", makeFolderURL(rootFolderID))
		return
	}

	fmt.Printf("\nDownloading %d new transcript(s)...\n", len(toDownload))
	transcripts := downloadTranscriptItems(webexClient, toDownload)

	if len(transcripts) == 0 {
		fmt.Println("No transcripts could be downloaded.")
		return
	}

	total := len(transcripts)
	const workers = 3
	fmt.Printf("\nUploading %d transcript(s) to Google Docs (%d parallel workers)...\n", total, workers)
	uploadStart := time.Now()

	type uploadResult struct {
		t             Transcript
		transcriptURL string
		summaryURL    string
		duration      time.Duration
		err           error
	}

	work := make(chan Transcript, total)
	results := make(chan uploadResult, total)

	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for t := range work {
				t0 := time.Now()
				tURL, sURL, err := createMeetingDocs(ctx, clients, rootFolderID, t)
				results <- uploadResult{t: t, transcriptURL: tURL, summaryURL: sURL, duration: time.Since(t0), err: err}
			}
		}()
	}
	for _, t := range transcripts {
		work <- t
	}
	close(work)

	// Close results once all workers are done so the range below terminates.
	go func() { wg.Wait(); close(results) }()

	// printBar renders an ASCII progress bar on a single line, overwriting
	// itself with \r so the terminal shows only the latest state.
	const barWidth = 40
	printBar := func(done int, label string) {
		filled := barWidth * done / total
		bar := strings.Repeat("=", filled)
		if done < total {
			bar += ">"
			bar += strings.Repeat(" ", barWidth-filled-1)
		} else {
			bar += strings.Repeat("=", barWidth-filled)
		}
		// Pad the suffix to 40 chars to erase any longer previous label.
		suffix := ""
		if label != "" {
			suffix = "  " + label
		}
		fmt.Printf("\r  [%s] %d/%d%-40s", bar, done, total, suffix)
	}
	printBar(0, "")

	// Collect results as they arrive, updating the progress bar live.
	var allResults []uploadResult
	for r := range results {
		allResults = append(allResults, r)
		label := r.t.MeetingTitle
		if r.err != nil {
			label += " [ERROR]"
		} else {
			label += fmt.Sprintf(" (%s)", r.duration.Round(time.Millisecond))
		}
		printBar(len(allResults), label)
	}
	fmt.Printf("\n\n") // move past the progress bar

	var uploaded int
	for _, r := range allResults {
		if r.err != nil {
			fmt.Fprintf(os.Stderr, "  [error]  %s: %v\n", r.t.MeetingTitle, r.err)
			continue
		}
		if err := mf.record(ctx, r.t, r.transcriptURL, r.summaryURL); err != nil {
			fmt.Fprintf(os.Stderr, "  warning: could not update manifest for %q: %v\n", r.t.MeetingTitle, err)
		}
		fmt.Printf("  [done] %s (%s) — %s\n    transcript: %s\n",
			r.t.MeetingTitle, r.t.StartTime.Format("Jan 2, 2006"),
			r.duration.Round(time.Millisecond), r.transcriptURL)
		if r.summaryURL != "" {
			fmt.Printf("    summary:    %s\n", r.summaryURL)
		}
		uploaded++
	}

	fmt.Printf("\nDone! Uploaded: %d  Skipped (already uploaded): %d  Total time: %s\n",
		uploaded, skipped, time.Since(uploadStart).Round(time.Second))
	fmt.Printf("\n  webex-meetings folder: %s\n", makeFolderURL(rootFolderID))

	// Bot mode: after the personal sync, also run the bot space sweep if
	// --bot was requested.
	if *botMode {
		runBotMode(ctx, clients, *from, *to)
	}
}

// runBotMode uses WEBEX_BOT_TOKEN to list every space the bot is a member of,
// then sweeps each space for transcripts and uploads them to plx-webex-meetings/
// in Google Drive using the same folder hierarchy and deduplication logic.
func runBotMode(ctx context.Context, clients *googleClients, from, to string) {
	fmt.Println("\n--- Bot mode: syncing transcripts from bot spaces ---")

	botClient, err := newBotClient(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bot mode error: %v\n", err)
		return
	}

	fmt.Println("Retrieving spaces the bot is a member of...")
	spaces, err := listBotSpaces(botClient)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bot mode error listing spaces: %v\n", err)
		return
	}
	fmt.Printf("Bot is a member of %d space(s).\n", len(spaces))

	// Ensure the bot root Drive folder and its manifest.
	fmt.Printf("Ensuring Drive folder structure (%s/YYYY-MM)...\n", botRootFolderName)
	botRootID, err := ensureFolder(ctx, clients.drive, botRootFolderName, "root")
	if err != nil {
		fmt.Fprintf(os.Stderr, "bot mode error ensuring Drive folder: %v\n", err)
		return
	}

	mf, err := loadDriveManifest(ctx, clients.drive, botRootID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not load bot manifest (starting fresh): %v\n", err)
		mf = &driveManifest{entries: make(map[string]manifestEntry), drive: clients.drive, folderID: botRootID}
	}
	fmt.Printf("Bot manifest loaded: %d previously uploaded transcript(s).\n\n", len(mf.entries))

	var totalUploaded, totalSkipped int

	for i, space := range spaces {
		fmt.Printf("[%d/%d] Checking space: %s\n", i+1, len(spaces), space.Title)
		items, err := listTranscriptItems(ctx, botClient, from, to, 30, space.ID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  [warn] listing transcripts failed: %v\n", err)
			continue
		}
		if len(items) == 0 {
			fmt.Printf("  no transcripts found\n")
			continue
		}

		var toDownload []transcriptItem
		for _, item := range items {
			if mf.knownID(item.ID) {
				totalSkipped++
			} else {
				toDownload = append(toDownload, item)
			}
		}

		if len(toDownload) == 0 {
			fmt.Printf("  all %d transcript(s) already uploaded\n", len(items))
			continue
		}

		fmt.Printf("  %d new transcript(s) to download\n", len(toDownload))
		transcripts := downloadTranscriptItems(botClient, toDownload)

		for _, t := range transcripts {
			tURL, sURL, err := createMeetingDocs(ctx, clients, botRootID, t)
			if err != nil {
				fmt.Fprintf(os.Stderr, "    [error] %s: %v\n", t.MeetingTitle, err)
				continue
			}
			if err := mf.record(ctx, t, tURL, sURL); err != nil {
				fmt.Fprintf(os.Stderr, "    warning: manifest update failed for %q: %v\n", t.MeetingTitle, err)
			}
			fmt.Printf("    [upload] %s (%s)\n      transcript: %s\n",
				t.MeetingTitle, t.StartTime.Format("Jan 2, 2006"), tURL)
			if sURL != "" {
				fmt.Printf("      summary:    %s\n", sURL)
			}
			totalUploaded++
		}
	}

	fmt.Printf("\nBot mode done! Uploaded: %d  Skipped: %d\n", totalUploaded, totalSkipped)
}
