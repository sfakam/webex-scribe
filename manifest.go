// manifest.go implements the local transcript manifest and calendar backed by
// SQLite. Both are stored in a single webex-scribe.db file in the output dir.
//
// Tables:
//   - transcripts  — one row per saved transcript (dedup + metadata)
//   - participants — normalised attendee rows per transcript
//   - calendar     — scheduled meetings from the Webex API, with status tracking
//
// Legacy .wts-manifest.json files are auto-migrated on first open.
// webex-calendar.json is exported after every calendar update for backward
// compatibility with skills that read the JSON directly.
package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	_ "modernc.org/sqlite"
)

const (
	webexScribeDB     = "webex-scribe.db"
	localManifestJSON = ".wts-manifest.json" // legacy — migrated on first open
)

const dbSchema = `
CREATE TABLE IF NOT EXISTS transcripts (
	transcript_id    TEXT PRIMARY KEY,
	meeting_id       TEXT NOT NULL DEFAULT '',
	title            TEXT NOT NULL DEFAULT '',
	category         TEXT NOT NULL DEFAULT '',
	meeting_date     TEXT NOT NULL DEFAULT '',
	start_time       TEXT NOT NULL DEFAULT '',
	end_time         TEXT NOT NULL DEFAULT '',
	duration_minutes INTEGER NOT NULL DEFAULT 0,
	host_name        TEXT NOT NULL DEFAULT '',
	host_email       TEXT NOT NULL DEFAULT '',
	space_name       TEXT NOT NULL DEFAULT '',
	participant_count INTEGER NOT NULL DEFAULT 0,
	transcript_path  TEXT NOT NULL DEFAULT '',
	info_path        TEXT NOT NULL DEFAULT '',
	written_at       TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS participants (
	transcript_id TEXT NOT NULL,
	display_name  TEXT NOT NULL DEFAULT '',
	email         TEXT NOT NULL DEFAULT '',
	is_host       INTEGER NOT NULL DEFAULT 0,
	FOREIGN KEY (transcript_id) REFERENCES transcripts(transcript_id)
);

CREATE TABLE IF NOT EXISTS calendar (
	meeting_id       TEXT PRIMARY KEY,
	title            TEXT NOT NULL DEFAULT '',
	start_time       TEXT NOT NULL DEFAULT '',
	end_time         TEXT NOT NULL DEFAULT '',
	duration_minutes INTEGER NOT NULL DEFAULT 0,
	agenda           TEXT NOT NULL DEFAULT '',
	host_name        TEXT NOT NULL DEFAULT '',
	web_link         TEXT NOT NULL DEFAULT '',
	status           TEXT NOT NULL DEFAULT 'active',
	last_updated     TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_participants_tid  ON participants(transcript_id);
CREATE INDEX IF NOT EXISTS idx_transcripts_date  ON transcripts(meeting_date);
CREATE INDEX IF NOT EXISTS idx_transcripts_cat   ON transcripts(category);
CREATE INDEX IF NOT EXISTS idx_calendar_start    ON calendar(start_time);
CREATE INDEX IF NOT EXISTS idx_calendar_title    ON calendar(title);
CREATE INDEX IF NOT EXISTS idx_calendar_status   ON calendar(status);
`

// localManifest is the central data store for webex-scribe, backed by SQLite.
type localManifest struct {
	db *sql.DB
}

// loadLocalManifest opens (or creates) webex-scribe.db in outputDir.
// Legacy .wts-manifest.json is auto-migrated on first open.
func loadLocalManifest(outputDir string) *localManifest {
	dbPath := filepath.Join(outputDir, webexScribeDB)
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not open db — falling back to in-memory: %v\n", err)
		db, _ = sql.Open("sqlite", ":memory:")
	}
	db.Exec("PRAGMA journal_mode=WAL")
	db.Exec("PRAGMA synchronous=NORMAL")

	if _, err := db.Exec(dbSchema); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not initialise schema: %v\n", err)
	}

	lm := &localManifest{db: db}

	// Auto-migrate legacy JSON manifest.
	jsonPath := filepath.Join(outputDir, localManifestJSON)
	if _, statErr := os.Stat(jsonPath); statErr == nil {
		if err := lm.migrateFromJSON(jsonPath); err != nil {
			fmt.Fprintf(os.Stderr, "warning: manifest migration failed: %v\n", err)
		}
	}

	return lm
}

// ── Transcript methods ───────────────────────────────────────────────────────

func (lm *localManifest) count() int {
	var n int
	lm.db.QueryRow("SELECT COUNT(*) FROM transcripts").Scan(&n)
	return n
}

func (lm *localManifest) known(id string) bool {
	var n int
	lm.db.QueryRow("SELECT COUNT(*) FROM transcripts WHERE transcript_id = ?", id).Scan(&n)
	return n > 0
}

func (lm *localManifest) record(id string, e localEntry) error {
	tx, err := lm.db.Begin()
	if err != nil {
		return fmt.Errorf("beginning transaction: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.Exec(`INSERT OR REPLACE INTO transcripts
		(transcript_id, meeting_id, title, category, meeting_date,
		 start_time, end_time, duration_minutes, host_name, host_email,
		 space_name, participant_count, transcript_path, info_path, written_at)
		VALUES (?,?,?,?,?, ?,?,?,?,?, ?,?,?,?,?)`,
		id, e.MeetingID, e.Title, e.Category, e.MeetingDate,
		e.StartTime, e.EndTime, e.DurationMinutes, e.HostDisplayName, e.HostEmail,
		e.SpaceName, len(e.Participants), e.TranscriptPath, e.InfoPath,
		e.WrittenAt.UTC().Format(time.RFC3339),
	)
	if err != nil {
		return fmt.Errorf("upserting transcript: %w", err)
	}

	if _, err := tx.Exec("DELETE FROM participants WHERE transcript_id = ?", id); err != nil {
		return fmt.Errorf("clearing participants: %w", err)
	}
	for _, p := range e.Participants {
		isHost := 0
		if p.IsHost {
			isHost = 1
		}
		if _, err := tx.Exec(
			"INSERT INTO participants (transcript_id, display_name, email, is_host) VALUES (?,?,?,?)",
			id, p.DisplayName, p.Email, isHost,
		); err != nil {
			return fmt.Errorf("inserting participant: %w", err)
		}
	}
	return tx.Commit()
}

// ── Calendar methods ─────────────────────────────────────────────────────────

// UpsertCalendar inserts or updates scheduled meetings, marking them active.
func (lm *localManifest) UpsertCalendar(meetings []ScheduledMeeting) error {
	if len(meetings) == 0 {
		return nil
	}
	tx, err := lm.db.Begin()
	if err != nil {
		return fmt.Errorf("beginning calendar transaction: %w", err)
	}
	defer tx.Rollback()

	now := time.Now().UTC().Format(time.RFC3339)
	for _, m := range meetings {
		endStr := ""
		dur := 0
		if !m.End.IsZero() {
			endStr = m.End.UTC().Format(time.RFC3339)
			dur = int(m.End.Sub(m.Start).Minutes())
		}
		status := m.Status
		if status == "" {
			status = "active"
		}
		_, err := tx.Exec(`INSERT OR REPLACE INTO calendar
			(meeting_id, title, start_time, end_time, duration_minutes,
			 agenda, host_name, web_link, status, last_updated)
			VALUES (?,?,?,?,?,?,?,?,?,?)`,
			m.ID, m.Title, m.Start.UTC().Format(time.RFC3339), endStr, dur,
			m.Agenda, m.HostDisplayName, m.WebLink, status, now,
		)
		if err != nil {
			return fmt.Errorf("upserting calendar entry %q: %w", m.Title, err)
		}
	}
	return tx.Commit()
}

// MarkCancelledInWindow marks any previously-active meetings in [from, to]
// whose IDs are not in activeIDs as "cancelled". Call after fetching the API
// response for that window so disappearing meetings are detected.
func (lm *localManifest) MarkCancelledInWindow(from, to time.Time, activeIDs map[string]bool) error {
	fromStr := from.UTC().Format(time.RFC3339)
	toStr := to.UTC().Format(time.RFC3339)

	rows, err := lm.db.Query(
		`SELECT meeting_id FROM calendar WHERE start_time >= ? AND start_time <= ? AND status = 'active'`,
		fromStr, toStr,
	)
	if err != nil {
		return err
	}
	var toCancel []string
	for rows.Next() {
		var id string
		rows.Scan(&id)
		if !activeIDs[id] {
			toCancel = append(toCancel, id)
		}
	}
	rows.Close()

	for _, id := range toCancel {
		lm.db.Exec(`UPDATE calendar SET status = 'cancelled', last_updated = ? WHERE meeting_id = ?`,
			time.Now().UTC().Format(time.RFC3339), id)
	}
	if len(toCancel) > 0 {
		fmt.Printf("  [calendar] marked %d meeting(s) as cancelled\n", len(toCancel))
	}
	return nil
}

// BuildCalendarFile constructs the in-memory calendarFile struct from the DB,
// used by classifyMeeting. Only active meetings are included in the count.
func (lm *localManifest) BuildCalendarFile() *calendarFile {
	rows, err := lm.db.Query(`SELECT meeting_id, title, start_time, end_time,
		duration_minutes, agenda, host_name, web_link, status FROM calendar`)
	if err != nil {
		return nil
	}
	defer rows.Close()

	cf := &calendarFile{
		LastUpdated: time.Now().UTC().Format(time.RFC3339),
		Meetings:    make(map[string][]CalendarEntry),
	}
	for rows.Next() {
		var e CalendarEntry
		rows.Scan(&e.ID, &e.Title, &e.Start, &e.End, &e.DurationMinutes,
			&e.Agenda, &e.Host, &e.WebLink, &e.Status)
		t, _ := time.Parse(time.RFC3339, e.Start)
		dateKey := t.In(eastern).Format("2006-01-02")
		cf.Meetings[dateKey] = append(cf.Meetings[dateKey], e)
	}
	// Sort within each date.
	for k, entries := range cf.Meetings {
		sort.Slice(entries, func(i, j int) bool { return entries[i].Start < entries[j].Start })
		cf.Meetings[k] = entries
	}
	return cf
}

// ExportCalendarJSON writes webex-calendar.json to dir from the DB, for
// backward compatibility with skills that read the JSON file directly.
func (lm *localManifest) ExportCalendarJSON(dir string) error {
	cf := lm.BuildCalendarFile()
	if cf == nil {
		return nil
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cf, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "webex-calendar.json"), data, 0644)
}

func (lm *localManifest) close() {
	if lm.db != nil {
		lm.db.Close()
	}
}

// migrateFromJSON imports all entries from the legacy JSON manifest into SQLite,
// then renames the JSON file so it is not re-processed on subsequent runs.
func (lm *localManifest) migrateFromJSON(jsonPath string) error {
	var existing int
	lm.db.QueryRow("SELECT COUNT(*) FROM transcripts").Scan(&existing)
	if existing > 0 {
		return nil
	}
	data, err := os.ReadFile(jsonPath)
	if err != nil {
		return err
	}
	var entries map[string]localEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return fmt.Errorf("parsing legacy manifest: %w", err)
	}
	if len(entries) == 0 {
		return nil
	}
	ok := 0
	for id, e := range entries {
		if err := lm.record(id, e); err != nil {
			fmt.Fprintf(os.Stderr, "  warning: could not migrate %q: %v\n", e.Title, err)
			continue
		}
		ok++
	}
	fmt.Printf("  Migrated %d/%d entries from .wts-manifest.json → webex-scribe.db\n", ok, len(entries))
	return os.Rename(jsonPath, jsonPath+".migrated")
}
