// local.go implements local filesystem output for webex-scribe.
// When --local is set, transcripts are written as markdown files instead of
// being uploaded to Google Docs. The --upcoming flag uses the same output
// layer to write a scheduled-meeting agenda file.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// MeetingCategory classifies a meeting using the same rules as the
// /sf:meeting-notes skill.
type MeetingCategory int

const (
	CategoryOneOff    MeetingCategory = iota // standalone (one-time) meeting
	CategoryRecurring                        // appears 2+ times in calendar
	Category1on1                             // title contains 1:1 / 1-1 / one on one
)

func (c MeetingCategory) String() string {
	switch c {
	case Category1on1:
		return "1:1"
	case CategoryRecurring:
		return "recurring"
	default:
		return "standalone"
	}
}

// DriveFolder returns the Drive subfolder name for this category.
func (c MeetingCategory) DriveFolder() string {
	switch c {
	case Category1on1:
		return "1-1s"
	case CategoryRecurring:
		return "recurring"
	default:
		return "standalone"
	}
}

// titleIs1on1 reports whether the meeting title contains the standard 1:1
// keywords or the display name of any of the given participants.
func titleIs1on1(title string, participants []MeetingParticipant) bool {
	lower := strings.ToLower(title)
	if strings.Contains(lower, "1:1") || strings.Contains(lower, "1-1") ||
		strings.Contains(lower, "one on one") || strings.Contains(lower, "one-on-one") {
		return true
	}
	// Match any name token (>2 chars) from any participant's display name.
	for _, p := range participants {
		for _, token := range strings.Fields(p.DisplayName) {
			if len(token) > 2 && strings.Contains(lower, strings.ToLower(token)) {
				return true
			}
		}
	}
	return false
}

func containsDigit(s string) bool {
	for _, r := range s {
		if r >= '0' && r <= '9' {
			return true
		}
	}
	return false
}

// deduplicateParticipants returns a slice with one entry per unique email.
// Webex logs each join/rejoin as a separate participant entry, so the same
// person can appear multiple times. The first occurrence is kept.
func deduplicateParticipants(participants []MeetingParticipant) []MeetingParticipant {
	seen := make(map[string]struct{})
	out := make([]MeetingParticipant, 0, len(participants))
	for _, p := range participants {
		key := strings.ToLower(p.Email)
		if key == "" {
			key = strings.ToLower(p.DisplayName)
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, p)
	}
	return out
}

// selfTokens returns lower-cased name tokens (≥3 chars) for the current user's
// display name. Used to identify self in participant lists and title checks.
func selfTokens(currentUserDisplayName string) []string {
	var out []string
	for _, tok := range strings.Fields(strings.ToLower(currentUserDisplayName)) {
		if len(tok) >= 3 {
			out = append(out, tok)
		}
	}
	return out
}

// findSelf returns the index in unique of the current user, or -1.
// Email match is tried first; falls back to exact display-name token match.
func findSelf(unique []MeetingParticipant, currentUserEmail string, selfToks []string) int {
	for i, p := range unique {
		if currentUserEmail != "" && strings.EqualFold(p.Email, currentUserEmail) {
			return i
		}
	}
	for i, p := range unique {
		for _, pTok := range strings.Fields(strings.ToLower(p.DisplayName)) {
			for _, sTok := range selfToks {
				if pTok == sTok {
					return i
				}
			}
		}
	}
	return -1
}

// titleHasParticipantName reports whether lower contains any name token (≥3 chars)
// from any of the given participants.
func titleHasParticipantName(lower string, participants []MeetingParticipant) bool {
	for _, p := range participants {
		for _, tok := range strings.Fields(strings.ToLower(p.DisplayName)) {
			if len(tok) >= 3 && strings.Contains(lower, tok) {
				return true
			}
		}
	}
	return false
}

// classifyMeeting applies the /sf:meeting-notes classification rules:
//
//  1. spaceType == "direct" → 1:1 (Webex direct space is always a 1:1).
//  2. Title contains 1:1/1-1/one-on-one keyword → 1:1 (unconditional).
//  3. PRIMARY: exactly 2 unique attendees, one is the current user, AND the
//     title contains a name token from at least one attendee → 1:1.
//  4. FALLBACK for inflated participant lists (API + VTT noise): title matches a
//     1:1 name pattern (colon / slash / auto-generated "X and Y's meeting") AND
//     a participant name or the current user's name validates it → 1:1.
//  5. Title appears 2+ times in the calendar DB → recurring.
//  6. Everything else → standalone.
//
// Participants are deduplicated by email before counting so that rejoins do not
// inflate the count. All arguments are optional — pass nil/"" when unavailable.
func classifyMeeting(title string, cf *calendarFile, participants []MeetingParticipant, currentUserEmail, currentUserDisplayName, spaceType string) MeetingCategory {
	unique := deduplicateParticipants(participants)
	lower := strings.ToLower(title)

	// 1. Direct Webex space — strong signal, but cap at ≤2 speakers to guard
	// against title-lookup false positives that bring in unrelated meetings.
	if strings.EqualFold(spaceType, "direct") && (len(unique) == 0 || len(unique) <= 2) {
		return Category1on1
	}

	// 2. Explicit 1:1 keywords in the title.
	if strings.Contains(lower, "1:1") || strings.Contains(lower, "1-1") ||
		strings.Contains(lower, "one on one") || strings.Contains(lower, "one-on-one") {
		return Category1on1
	}

	sToks := selfTokens(currentUserDisplayName)
	titleHasSelf := func() bool {
		for _, tok := range sToks {
			if strings.Contains(lower, tok) {
				return true
			}
		}
		return false
	}

	// 3. PRIMARY: exactly 2 unique attendees, one is the current user, title
	// confirms with any attendee name. This is the ground-truth signal.
	if len(unique) == 2 {
		if findSelf(unique, currentUserEmail, sToks) >= 0 && titleHasParticipantName(lower, unique) {
			return Category1on1
		}
	}

	// 4. FALLBACK: title patterns handle inflated participant lists caused by the
	// participants API returning the host AND the VTT adding extra speakers.
	// Each pattern fires when a participant name validates it, OR when the title
	// contains the current user's own name (e.g. "Gabe:Sherif Sync").

	// 4a. Colon pattern: "Name:Sherif Sync", "Tong:Sherif Sync"
	if idx := strings.Index(lower, ":"); idx >= 3 && idx <= 15 {
		before := lower[:idx]
		if !strings.Contains(before, " ") && !containsDigit(before) {
			for _, p := range unique {
				for _, tok := range strings.Fields(strings.ToLower(p.DisplayName)) {
					if len(tok) >= 3 && tok == before {
						return Category1on1
					}
				}
			}
			if titleHasSelf() {
				return Category1on1
			}
		}
	}

	// 4b. Slash pattern: "Sheriff/Mindy", "Sherif/Damien"
	if idx := strings.Index(lower, "/"); idx >= 3 && idx <= 15 {
		before := lower[:idx]
		after := strings.Fields(lower[idx+1:])
		if !strings.Contains(before, " ") && !containsDigit(before) &&
			len(after) > 0 && len(after[0]) >= 3 && !containsDigit(after[0]) {
			for _, p := range unique {
				for _, tok := range strings.Fields(strings.ToLower(p.DisplayName)) {
					if len(tok) >= 3 && (tok == before || tok == after[0]) {
						return Category1on1
					}
				}
			}
			if titleHasSelf() {
				return Category1on1
			}
		}
	}

	// 4c. Webex auto-generated "'s meeting": "PersonA and PersonB's meeting"
	if strings.HasSuffix(lower, "'s meeting") {
		if andIdx := strings.Index(lower, " and "); andIdx >= 0 {
			afterAnd := lower[andIdx+5:]
			if titleHasSelf() {
				return Category1on1
			}
			for _, p := range unique {
				for _, tok := range strings.Fields(strings.ToLower(p.DisplayName)) {
					if len(tok) >= 4 && strings.HasPrefix(afterAnd, tok) {
						return Category1on1
					}
				}
			}
		}
	}

	// 5. Calendar recurring check.
	if cf != nil {
		count := 0
		for _, entries := range cf.Meetings {
			for _, e := range entries {
				if strings.EqualFold(e.Title, title) {
					count++
					if count >= 2 {
						return CategoryRecurring
					}
				}
			}
		}
	}
	return CategoryOneOff
}

// classifyMeetingFull runs classifyMeeting and then falls back to icsCountTitle
// when the result is standalone and the calendar DB has fewer than 2 entries.
// This handles personal room meetings whose titles were resolved at fetch time
// and therefore may not yet appear in the calendar DB.
func classifyMeetingFull(title string, cf *calendarFile, participants []MeetingParticipant, currentUserEmail, currentUserDisplayName, spaceType string) (MeetingCategory, int) {
	cat := classifyMeeting(title, cf, participants, currentUserEmail, currentUserDisplayName, spaceType)
	n := countOccurrences(title, cf)
	if cat == CategoryOneOff && n < 2 {
		icsN := icsCountTitle(title)
		fmt.Fprintf(os.Stderr, "  [classify-debug] %q: calDB=%d icsCount=%d\n", title, n, icsN)
		if icsN >= 2 {
			return CategoryRecurring, icsN
		}
		// An unresolved personal room title means ICS had no match for that
		// specific time slot, but the personal room itself is used repeatedly.
		// Defaulting to recurring is more accurate than standalone.
		if isPersonalRoomTitle(title) {
			return CategoryRecurring, n
		}
	}
	return cat, n
}

// countOccurrences returns how many times title appears across all dates in cf.
func countOccurrences(title string, cf *calendarFile) int {
	if cf == nil {
		return 0
	}
	n := 0
	for _, entries := range cf.Meetings {
		for _, e := range entries {
			if strings.EqualFold(e.Title, title) {
				n++
			}
		}
	}
	return n
}

// eastern is America/New_York, used for all display-facing time formatting.
// Falls back to UTC-5 (EST) if the timezone database is unavailable.
var eastern = func() *time.Location {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		return time.FixedZone("EST", -5*60*60)
	}
	return loc
}()

// localEntry is the data written to the manifest for each saved transcript.
// The SQLite manifest (manifest.go) stores scalar fields in the transcripts
// table and Participants in a separate participants table.
type localEntry struct {
	Title           string               `json:"title"`
	Category        string               `json:"category"`
	MeetingID       string               `json:"meetingId"`
	MeetingDate     string               `json:"meetingDate"`
	StartTime       string               `json:"startTime"`
	EndTime         string               `json:"endTime"`
	DurationMinutes int                  `json:"durationMinutes"`
	HostDisplayName string               `json:"hostDisplayName,omitempty"`
	HostEmail       string               `json:"hostEmail,omitempty"`
	SpaceName       string               `json:"spaceName,omitempty"`
	SpaceType       string               `json:"spaceType,omitempty"`
	Participants    []MeetingParticipant `json:"participants,omitempty"`
	TranscriptPath  string               `json:"transcriptPath"`
	InfoPath        string               `json:"infoPath,omitempty"`
	WrittenAt       time.Time            `json:"writtenAt"`
}

// writeTranscriptLocally writes a Transcript to outputDir as markdown.
// Layout: outputDir/webex-meetings/<category>/<space-or-meeting-title>/<YYYY-MM-DD>-<HHmm>-transcript.md
//
//	outputDir/webex-meetings/<category>/<space-or-meeting-title>/<YYYY-MM-DD>-<HHmm>-summary.md (when present)
//
// peerDisplayName returns the display name of the other participant in a 1:1.
// It excludes the current user by email first, then by display name tokens.
// Returns "" when the peer cannot be determined.
func peerDisplayName(participants []MeetingParticipant, currentUserEmail, currentUserDisplayName string) string {
	unique := deduplicateParticipants(participants)
	if len(unique) < 2 {
		return ""
	}
	// Build a set of current-user name tokens for fallback matching.
	selfTokens := make(map[string]bool)
	for _, tok := range strings.Fields(strings.ToLower(currentUserDisplayName)) {
		if len(tok) >= 3 {
			selfTokens[tok] = true
		}
	}
	for _, p := range unique {
		// Skip by email.
		if currentUserEmail != "" && strings.EqualFold(p.Email, currentUserEmail) {
			continue
		}
		// Skip by display name when emails are empty.
		if p.Email == "" && currentUserDisplayName != "" {
			isSelf := false
			for _, tok := range strings.Fields(strings.ToLower(p.DisplayName)) {
				if selfTokens[tok] {
					isSelf = true
					break
				}
			}
			if isSelf {
				continue
			}
		}
		if strings.TrimSpace(p.DisplayName) != "" {
			return p.DisplayName
		}
	}
	return ""
}

// <category> is one of "1-1s", "recurring", or "standalone" derived from t.Category.
// The start time (HHmm in ET, 24h) is always included in the filename so that
// multiple meetings in the same space on the same day never collide.
// t.Category must be set before calling; zero value routes to "standalone".
// currentUserEmail and currentUserDisplayName are used to normalise 1:1 folder names.
//
// Returns the path of the transcript file.
func writeTranscriptLocally(outputDir string, t Transcript, currentUserEmail, currentUserDisplayName string) (string, error) {
	label := t.SpaceName
	if label == "" {
		label = t.MeetingTitle
	}
	if t.Category == Category1on1 {
		if peer := peerDisplayName(t.Participants, currentUserEmail, currentUserDisplayName); peer != "" {
			if currentUserDisplayName != "" {
				label = peer + " : " + currentUserDisplayName
			} else {
				label = peer
			}
		}
	} else if t.Category == CategoryOneOff {
		label = t.StartTime.In(eastern).Format("2006-01-02") + "-" + label
	}
	dir := filepath.Join(outputDir, "webex-meetings", t.Category.DriveFolder(), sanitizeFilename(label))
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", fmt.Errorf("creating output directory: %w", err)
	}

	startET := t.StartTime.In(eastern)
	dateStr := startET.Format("2006-01-02")
	timeStr := startET.Format("1504")

	transcriptPath := filepath.Join(dir, dateStr+"-"+timeStr+"-transcript.md")

	var sb strings.Builder
	fmt.Fprintf(&sb, "# %s\n\n", t.MeetingTitle)
	fmt.Fprintf(&sb, "**Date:** %s\n", startET.Format("January 2, 2006 15:04 MST"))
	if t.SpaceName != "" {
		fmt.Fprintf(&sb, "**Space:** %s\n", t.SpaceName)
	}
	if len(t.RoomMembers) > 0 {
		parts := make([]string, 0, len(t.RoomMembers))
		for _, m := range t.RoomMembers {
			n := m.DisplayName
			if m.IsModerator {
				n += " *(host)*"
			}
			parts = append(parts, n)
		}
		fmt.Fprintf(&sb, "**Participants:** %s\n", strings.Join(parts, ", "))
	}
	sb.WriteString("\n---\n\n")
	sb.WriteString(t.Content)

	if err := os.WriteFile(transcriptPath, []byte(sb.String()), 0644); err != nil {
		return "", fmt.Errorf("writing transcript: %w", err)
	}

	if t.AISummary != "" {
		summaryPath := filepath.Join(dir, dateStr+"-"+timeStr+"-summary.md")
		var ss strings.Builder
		fmt.Fprintf(&ss, "# %s — Summary\n\n", t.MeetingTitle)
		fmt.Fprintf(&ss, "**Date:** %s\n\n", t.StartTime.In(eastern).Format("January 2, 2006"))
		ss.WriteString("---\n\n")
		ss.WriteString(t.AISummary)
		if err := os.WriteFile(summaryPath, []byte(ss.String()), 0644); err != nil {
			fmt.Fprintf(os.Stderr, "  warning: could not write summary: %v\n", err)
		}
	}

	// Write meeting-info.json alongside the transcript.
	infoPath := filepath.Join(dir, dateStr+"-"+timeStr+"-meeting-info.json")
	if err := writeMeetingInfo(infoPath, t); err != nil {
		fmt.Fprintf(os.Stderr, "  warning: could not write meeting-info.json: %v\n", err)
	}

	return transcriptPath, nil
}

// meetingInfoJSON is the on-disk structure for <date>-<HHmm>-meeting-info.json.
type meetingInfoJSON struct {
	ID              string               `json:"id"`
	MeetingID       string               `json:"meetingId"`
	Title           string               `json:"title"`
	Category        string               `json:"category"`
	OccurrenceCount int                  `json:"occurrenceCount,omitempty"` // how many times this title appears in calendar
	StartTime       string               `json:"startTime"`                 // RFC3339 UTC
	EndTime         string               `json:"endTime"`                   // RFC3339 UTC, omitted if zero
	DurationMinutes int                  `json:"durationMinutes"`           // 0 if end time unknown
	HostDisplayName string               `json:"hostDisplayName,omitempty"`
	HostEmail       string               `json:"hostEmail,omitempty"`
	Agenda          string               `json:"agenda,omitempty"`
	SpaceName       string               `json:"spaceName,omitempty"`
	SpaceType       string               `json:"spaceType,omitempty"`
	Participants    []MeetingParticipant `json:"participants,omitempty"`
	SpaceMembers    []spaceMember        `json:"spaceMembers,omitempty"`
}

type spaceMember struct {
	DisplayName string `json:"displayName"`
	Email       string `json:"email"`
	IsHost      bool   `json:"isHost"`
}

func writeMeetingInfo(path string, t Transcript) error {
	info := meetingInfoJSON{
		ID:              t.ID,
		MeetingID:       t.MeetingID,
		Title:           t.MeetingTitle,
		Category:        t.Category.String(),
		OccurrenceCount: t.OccurrenceCount,
		StartTime:       t.StartTime.UTC().Format(time.RFC3339),
		HostDisplayName: t.HostDisplayName,
		HostEmail:       t.HostEmail,
		Agenda:          t.Agenda,
		SpaceName:       t.SpaceName,
		SpaceType:       t.SpaceType,
		Participants:    t.Participants,
	}
	if !t.EndTime.IsZero() {
		info.EndTime = t.EndTime.UTC().Format(time.RFC3339)
		info.DurationMinutes = int(t.EndTime.Sub(t.StartTime).Minutes())
	}
	for _, m := range t.RoomMembers {
		info.SpaceMembers = append(info.SpaceMembers, spaceMember{
			DisplayName: m.DisplayName,
			Email:       m.Email,
			IsHost:      m.IsModerator,
		})
	}
	data, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

// CalendarEntry is a single meeting row from the calendar DB.
type CalendarEntry struct {
	ID              string `json:"id"`
	Title           string `json:"title"`
	Start           string `json:"start"`             // RFC3339 UTC
	End             string `json:"end"`               // RFC3339 UTC
	DurationMinutes int    `json:"duration_minutes"`
	Agenda          string `json:"agenda,omitempty"`
	Host            string `json:"host,omitempty"`
	WebLink         string `json:"web_link,omitempty"`
	Status          string `json:"status,omitempty"`
}

// calendarFile is the on-disk JSON structure.
// Meetings is keyed by local date (YYYY-MM-DD) for easy date-based lookup.
type calendarFile struct {
	LastUpdated string                     `json:"last_updated"`
	Meetings    map[string][]CalendarEntry `json:"meetings"`
}

// transcriptsToScheduledMeetings converts a slice of Transcripts to
// ScheduledMeetings so they can be upserted into the calendar DB.
// MeetingID is used as the stable dedup key.
func transcriptsToScheduledMeetings(transcripts []Transcript) []ScheduledMeeting {
	meetings := make([]ScheduledMeeting, 0, len(transcripts))
	for _, t := range transcripts {
		meetings = append(meetings, ScheduledMeeting{
			ID:              t.MeetingID,
			Title:           t.MeetingTitle,
			Start:           t.StartTime,
			End:             t.EndTime,
			Agenda:          t.Agenda,
			HostDisplayName: t.HostDisplayName,
		})
	}
	return meetings
}

// itemsToScheduledMeetings converts transcript list metadata (no VTT content)
// to ScheduledMeetings so the calendar DB can be pre-populated for recurring
// detection before any content is downloaded.
func itemsToScheduledMeetings(items []transcriptItem) []ScheduledMeeting {
	meetings := make([]ScheduledMeeting, 0, len(items))
	for _, item := range items {
		start, _ := time.Parse(time.RFC3339, item.StartTime)
		meetings = append(meetings, ScheduledMeeting{
			ID:    item.MeetingID,
			Title: item.MeetingTopic,
			Start: start,
		})
	}
	return meetings
}

// readTranscriptFiles reads the content and AI summary from disk files written
// by writeTranscriptLocally. The summary file path is derived from transcriptPath
// by replacing the transcript.md suffix. Returns empty strings on any error.
func readTranscriptFiles(transcriptPath string) (content, summary string) {
	if data, err := os.ReadFile(transcriptPath); err == nil {
		if parts := strings.SplitN(string(data), "\n---\n\n", 2); len(parts) == 2 {
			content = parts[1]
		}
	}
	summaryPath := strings.TrimSuffix(transcriptPath, "transcript.md") + "summary.md"
	if data, err := os.ReadFile(summaryPath); err == nil {
		if parts := strings.SplitN(string(data), "\n---\n\n", 2); len(parts) == 2 {
			summary = parts[1]
		}
	}
	return
}

// sanitizeFilename replaces characters that are unsafe in file/directory names.
func sanitizeFilename(s string) string {
	r := strings.NewReplacer(
		"/", "-", "\\", "-", ":", "-",
		"*", "", "?", "", "\"", "",
		"<", "", ">", "", "|", "-",
	)
	s = strings.TrimSpace(r.Replace(s))
	if len(s) > 80 {
		s = s[:80]
	}
	return s
}

var (
	icsDTStartRe = regexp.MustCompile(`(?m)^DTSTART(?:;[^:]*)?:(.+)`)
	icsSummaryRe = regexp.MustCompile(`(?m)^SUMMARY:(.+)`)
)

// isPersonalRoomTitle returns true when the meeting title indicates a Webex
// personal room session that has no meaningful scheduled title — e.g.
// "Sherif Fathalla's Personal Room", "Tracey's Webex", "Mindy's Webex Room".
func isPersonalRoomTitle(title string) bool {
	lower := strings.ToLower(title)
	return strings.Contains(lower, "personal room") ||
		strings.Contains(lower, "'s webex")
}

// icsTitleForMeeting scans ~/mail-cal-sync/calendar for an Outlook ICS event
// whose start time is within ±30 minutes of start. Returns the ICS SUMMARY
// (meeting title) if found, or "" if mail-cal-sync is absent or no match.
// Intended as a fallback for personal-room meetings that lack a Webex title.
func icsTitleForMeeting(start time.Time) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	calRoot := filepath.Join(home, "mail-cal-sync", "calendar",
		start.UTC().Format("2006"), start.UTC().Format("01"))
	dirs, err := os.ReadDir(calRoot)
	if err != nil {
		return "" // mail-cal-sync not installed — silent fallback
	}
	prefix := start.UTC().Format("20060102")
	skipCals := map[string]bool{
		"united_states_holidays": true,
		"us_holidays":            true,
	}
	for _, d := range dirs {
		if !d.IsDir() || skipCals[strings.ToLower(d.Name())] {
			continue
		}
		matches, _ := filepath.Glob(filepath.Join(calRoot, d.Name(), prefix+"_*.ics"))
		for _, path := range matches {
			if title, ok := icsParseTitle(path, start); ok {
				return title
			}
		}
	}
	return ""
}

// icsCountTitle counts how many ICS events across all available months in
// ~/mail-cal-sync/calendar/ have a SUMMARY exactly matching title
// (case-insensitive). Returns early once count reaches 2 since callers only
// need to distinguish "once" from "recurring". Returns 0 if mail-cal-sync is
// absent or no matches are found.
func icsCountTitle(title string) int {
	home, err := os.UserHomeDir()
	if err != nil {
		return 0
	}
	calBase := filepath.Join(home, "mail-cal-sync", "calendar")
	if _, err := os.Stat(calBase); err != nil {
		return 0
	}
	lowerTitle := strings.ToLower(title)
	skipCals := map[string]bool{
		"united_states_holidays": true,
		"us_holidays":            true,
	}
	unescaper := strings.NewReplacer(`\,`, ",", `\;`, ";", `\n`, " ", `\\`, `\`)
	count := 0
	_ = filepath.WalkDir(calBase, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".ics") {
			return nil
		}
		// Skip holiday calendars by directory name.
		parts := strings.Split(filepath.ToSlash(path), "/")
		for _, p := range parts {
			if skipCals[strings.ToLower(p)] {
				return nil
			}
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		m := icsSummaryRe.FindSubmatch(data)
		if m == nil {
			return nil
		}
		summary := unescaper.Replace(strings.TrimSpace(string(m[1])))
		if strings.ToLower(summary) == lowerTitle {
			count++
			if count >= 2 {
				return filepath.SkipAll // found enough — stop early
			}
		}
		return nil
	})
	return count
}

// icsParseTitle returns the SUMMARY of an ICS file if its DTSTART is within
// ±30 minutes of target, otherwise ("", false).
func icsParseTitle(path string, target time.Time) (string, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	text := string(data)

	dtm := icsDTStartRe.FindStringSubmatch(text)
	summ := icsSummaryRe.FindStringSubmatch(text)
	if dtm == nil || summ == nil {
		return "", false
	}

	raw := strings.TrimSpace(dtm[1])
	var t time.Time
	for _, layout := range []string{"20060102T150405Z", "20060102T150405", "20060102"} {
		if parsed, err2 := time.Parse(layout, raw); err2 == nil {
			if strings.HasSuffix(raw, "Z") {
				// Explicit UTC timestamp.
				t = parsed.UTC()
			} else {
				// No timezone indicator — Outlook exports local time without Z.
				// Interpret as Eastern (the user's timezone) and convert to UTC.
				t = time.Date(parsed.Year(), parsed.Month(), parsed.Day(),
					parsed.Hour(), parsed.Minute(), parsed.Second(), 0, eastern).UTC()
			}
			break
		}
	}
	if t.IsZero() {
		return "", false
	}
	diff := t.Sub(target)
	if diff < 0 {
		diff = -diff
	}
	if diff > 30*time.Minute {
		return "", false
	}

	title := strings.TrimSpace(summ[1])
	// Unescape ICS backslash sequences.
	title = strings.NewReplacer(`\,`, ",", `\;`, ";", `\n`, " ", `\\`, `\`).Replace(title)
	if title == "" {
		return "", false
	}
	return title, true
}
