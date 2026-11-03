// manifest.go implements the local transcript manifest backed by SQLite.
// The manifest tracks which transcripts have been saved so repeated runs skip
// already-written files. It replaces the legacy .wts-manifest.json format;
// existing JSON manifests are auto-migrated on first open.
package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

const (
	localManifestDB   = ".wts-manifest.db"
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

CREATE INDEX IF NOT EXISTS idx_participants_tid  ON participants(transcript_id);
CREATE INDEX IF NOT EXISTS idx_transcripts_date  ON transcripts(meeting_date);
CREATE INDEX IF NOT EXISTS idx_transcripts_cat   ON transcripts(category);
`

// localManifest tracks transcript downloads in a SQLite database.
// The map key is the Webex transcript record ID (from /meetingTranscripts).
type localManifest struct {
	db *sql.DB
}

// loadLocalManifest opens (or creates) the SQLite manifest in outputDir.
// If a legacy .wts-manifest.json exists and the DB is empty, it is migrated
// automatically and renamed to .wts-manifest.json.migrated.
func loadLocalManifest(outputDir string) *localManifest {
	dbPath := filepath.Join(outputDir, localManifestDB)
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not open manifest db — falling back to in-memory: %v\n", err)
		db, _ = sql.Open("sqlite", ":memory:")
	}
	db.Exec("PRAGMA journal_mode=WAL")
	db.Exec("PRAGMA synchronous=NORMAL")

	if _, err := db.Exec(dbSchema); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not initialise manifest schema: %v\n", err)
	}

	lm := &localManifest{db: db}

	// Auto-migrate legacy JSON manifest if the DB is still empty.
	jsonPath := filepath.Join(outputDir, localManifestJSON)
	if _, statErr := os.Stat(jsonPath); statErr == nil {
		if err := lm.migrateFromJSON(jsonPath); err != nil {
			fmt.Fprintf(os.Stderr, "warning: manifest migration failed: %v\n", err)
		}
	}

	return lm
}

// count returns the number of transcripts recorded in the manifest.
func (lm *localManifest) count() int {
	var n int
	lm.db.QueryRow("SELECT COUNT(*) FROM transcripts").Scan(&n)
	return n
}

// known reports whether a transcript ID has already been saved.
func (lm *localManifest) known(id string) bool {
	var n int
	lm.db.QueryRow("SELECT COUNT(*) FROM transcripts WHERE transcript_id = ?", id).Scan(&n)
	return n > 0
}

// record upserts a transcript entry and replaces its participant rows.
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
		return nil // DB already has data — skip migration
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
	fmt.Printf("  Migrated %d/%d entries from .wts-manifest.json → .wts-manifest.db\n", ok, len(entries))
	return os.Rename(jsonPath, jsonPath+".migrated")
}
