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

// classifyMeeting applies the /sf:meeting-notes classification rules:
//  1. spaceType == "direct" → 1:1 (Webex direct space is always a 1:1).
//  2. Title contains 1:1/1-1/one-on-one keyword → 1:1 (unconditional).
//  3. Exactly 2 unique participants (one is currentUserEmail) AND the other
//     person's name appears in the title → 1:1.
//  4. Title appears 2+ times in the calendar DB → recurring.
//  5. Everything else → standalone.
//
// Participants are deduplicated by email before counting so that rejoins
// (Webex logs each join as a separate entry) don't inflate the count.
// participants, currentUserEmail, and spaceType are optional — pass nil/"" when unavailable.
func classifyMeeting(title string, cf *calendarFile, participants []MeetingParticipant, currentUserEmail, spaceType string) MeetingCategory {
	// Direct Webex space = definitively a 1:1.
	if strings.EqualFold(spaceType, "direct") {
		return Category1on1
	}

	// Title keywords are an unconditional signal — check before participant count.
	lower := strings.ToLower(title)
	if strings.Contains(lower, "1:1") || strings.Contains(lower, "1-1") ||
		strings.Contains(lower, "one on one") || strings.Contains(lower, "one-on-one") {
		return Category1on1
	}

	// Deduplicate participants by email (rejoins create duplicate entries).
	unique := deduplicateParticipants(participants)

	if len(unique) == 2 && currentUserEmail != "" {
		// 2-person meeting: require current user is one, then check if the
		// OTHER person's name appears in the title.
		for _, p := range unique {
			if strings.EqualFold(p.Email, currentUserEmail) {
				var others []MeetingParticipant
				for _, o := range unique {
					if !strings.EqualFold(o.Email, currentUserEmail) {
						others = append(others, o)
					}
				}
				if titleIs1on1(title, others) {
					return Category1on1
				}
				break
			}
		}
	}
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
// <category> is one of "1-1s", "recurring", or "standalone" derived from t.Category.
// The start time (HHmm in ET, 24h) is always included in the filename so that
// multiple meetings in the same space on the same day never collide.
// t.Category must be set before calling; zero value routes to "standalone".
//
// Returns the path of the transcript file.
func writeTranscriptLocally(outputDir string, t Transcript) (string, error) {
	label := t.SpaceName
	if label == "" {
		label = t.MeetingTitle
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
			t = parsed.UTC()
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
