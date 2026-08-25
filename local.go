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
	"sort"
	"strings"
	"time"
)

const localManifestFile = ".wts-manifest.json"

// localEntry records a single locally-saved transcript.
type localEntry struct {
	Title       string    `json:"title"`
	MeetingDate string    `json:"meetingDate"`
	Path        string    `json:"path"`
	WrittenAt   time.Time `json:"writtenAt"`
}

// localManifest tracks which transcript IDs have been saved to disk so
// repeated runs skip already-written files.
type localManifest struct {
	path    string
	entries map[string]localEntry
}

// loadLocalManifest reads the manifest from outputDir. Missing file → empty
// manifest. Corrupt file → logs a warning and returns an empty manifest so
// the run can proceed, overwriting the corrupt file on first record().
func loadLocalManifest(outputDir string) *localManifest {
	path := filepath.Join(outputDir, localManifestFile)
	lm := &localManifest{path: path, entries: make(map[string]localEntry)}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return lm
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not read local manifest (starting fresh): %v\n", err)
		return lm
	}
	if err := json.Unmarshal(data, &lm.entries); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not parse local manifest (starting fresh): %v\n", err)
	}
	return lm
}

func (lm *localManifest) known(id string) bool {
	_, ok := lm.entries[id]
	return ok
}

func (lm *localManifest) record(id string, e localEntry) error {
	lm.entries[id] = e
	data, err := json.MarshalIndent(lm.entries, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(lm.path, data, 0644)
}

// writeTranscriptLocally writes a Transcript to outputDir as markdown.
// Layout: outputDir/<space-or-meeting-title>/<YYYY-MM-DD>-transcript.md
//
//	outputDir/<space-or-meeting-title>/<YYYY-MM-DD>-summary.md (when present)
//
// Returns the path of the transcript file.
func writeTranscriptLocally(outputDir string, t Transcript) (string, error) {
	label := t.SpaceName
	if label == "" {
		label = t.MeetingTitle
	}
	dir := filepath.Join(outputDir, sanitizeFilename(label))
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", fmt.Errorf("creating output directory: %w", err)
	}

	dateStr := t.StartTime.Format("2006-01-02")
	transcriptPath := filepath.Join(dir, dateStr+"-transcript.md")

	var sb strings.Builder
	fmt.Fprintf(&sb, "# %s\n\n", t.MeetingTitle)
	fmt.Fprintf(&sb, "**Date:** %s\n", t.StartTime.Format("January 2, 2006 15:04 MST"))
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
		summaryPath := filepath.Join(dir, dateStr+"-summary.md")
		var ss strings.Builder
		fmt.Fprintf(&ss, "# %s — Summary\n\n", t.MeetingTitle)
		fmt.Fprintf(&ss, "**Date:** %s\n\n", t.StartTime.Format("January 2, 2006"))
		ss.WriteString("---\n\n")
		ss.WriteString(t.AISummary)
		if err := os.WriteFile(summaryPath, []byte(ss.String()), 0644); err != nil {
			fmt.Fprintf(os.Stderr, "  warning: could not write summary: %v\n", err)
		}
	}

	return transcriptPath, nil
}

// CalendarEntry is a single meeting stored in calendar.json.
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

// upsertCalendarJSON merges meetings into calendarDir/calendar.json.
// Existing entries with the same ID are updated; new ones are added.
// Entries are grouped by local start date and sorted by start time within
// each date, so repeated runs accumulate a rolling calendar rather than
// overwriting previous data.
func upsertCalendarJSON(calendarDir string, meetings []ScheduledMeeting) (string, error) {
	if err := os.MkdirAll(calendarDir, 0755); err != nil {
		return "", fmt.Errorf("creating calendar dir: %w", err)
	}
	path := filepath.Join(calendarDir, "calendar.json")

	// Load existing file — missing is fine, corrupt logs a warning.
	cf := calendarFile{Meetings: make(map[string][]CalendarEntry)}
	if data, err := os.ReadFile(path); err == nil {
		if parseErr := json.Unmarshal(data, &cf); parseErr != nil {
			fmt.Fprintf(os.Stderr, "warning: could not parse existing calendar.json (starting fresh): %v\n", parseErr)
			cf.Meetings = make(map[string][]CalendarEntry)
		}
	}

	// Flatten all existing entries into a map keyed by meeting ID.
	all := make(map[string]CalendarEntry)
	for _, entries := range cf.Meetings {
		for _, e := range entries {
			all[e.ID] = e
		}
	}

	// Merge: new entries overwrite existing ones with the same ID.
	for _, m := range meetings {
		all[m.ID] = CalendarEntry{
			ID:              m.ID,
			Title:           m.Title,
			Start:           m.Start.UTC().Format(time.RFC3339),
			End:             m.End.UTC().Format(time.RFC3339),
			DurationMinutes: int(m.End.Sub(m.Start).Minutes()),
			Agenda:          m.Agenda,
			Host:            m.HostDisplayName,
			WebLink:         m.WebLink,
			Status:          m.Status,
		}
	}

	// Re-bucket by local date.
	cf.Meetings = make(map[string][]CalendarEntry)
	for _, entry := range all {
		t, _ := time.Parse(time.RFC3339, entry.Start)
		dateKey := t.Local().Format("2006-01-02")
		cf.Meetings[dateKey] = append(cf.Meetings[dateKey], entry)
	}

	// Sort entries within each date by start time (RFC3339 strings sort correctly).
	for dateKey := range cf.Meetings {
		entries := cf.Meetings[dateKey]
		sort.Slice(entries, func(i, j int) bool {
			return entries[i].Start < entries[j].Start
		})
		cf.Meetings[dateKey] = entries
	}

	cf.LastUpdated = time.Now().UTC().Format(time.RFC3339)

	data, err := json.MarshalIndent(cf, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshalling calendar: %w", err)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		return "", fmt.Errorf("writing calendar.json: %w", err)
	}
	return path, nil
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
