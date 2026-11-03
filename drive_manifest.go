// manifest.go implements a Drive-backed deduplication manifest for
// webex-scribe.
//
// The manifest is stored as a JSON file named ".wts-manifest.json" inside the
// "webex-meetings" root folder in Google Drive, so the same state is shared
// across all machines that run the tool against the same Google account.
//
// Each entry in the manifest records:
//
//   - Transcript ID  → stable unique key from the Webex API.
//   - SHA-256 hash   → computed from the raw VTT content. If the hash changes
//     between runs the transcript is re-uploaded (e.g. Webex
//     edited the transcript after the meeting).
//   - Doc URL        → URL of the Google Doc created for this transcript.
//   - Title          → human-readable label for the manifest file.
//   - UploadedAt     → timestamp of the last successful upload.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"time"

	driveapi "google.golang.org/api/drive/v3"
)

const manifestFileName = ".wts-manifest.json"

// manifestEntry records the details of a single uploaded transcript.
type manifestEntry struct {
	// Hash is the hex-encoded SHA-256 digest of the transcript VTT content at
	// the time it was last uploaded.
	Hash string `json:"hash"`

	// TranscriptURL is the Google Docs edit URL of the Transcript document.
	TranscriptURL string `json:"transcriptURL"`

	// SummaryURL is the Google Docs edit URL of the Summary document.
	// It is empty when no AI summary was available for the meeting.
	SummaryURL string `json:"summaryURL,omitempty"`

	// Title is the meeting title as returned by the Webex API.
	Title string `json:"title"`

	// MeetingDate is the UTC start date of the meeting (YYYY-MM-DD).
	MeetingDate string `json:"meetingDate,omitempty"`

	// UploadedAt records when this entry was last written.
	UploadedAt time.Time `json:"uploadedAt"`
}

// driveManifest wraps a manifest map together with the Drive file ID of the
// backing file, so saves can update the existing file rather than creating a
// new one each time.
type driveManifest struct {
	entries map[string]manifestEntry
	drive   *driveapi.Service
	folderID string // ID of the webex-meetings root Drive folder
	fileID   string // Drive file ID of the manifest; empty if not yet created
}

// loadDriveManifest fetches the manifest JSON file from the given Drive folder.
// It returns an empty manifest when the file does not exist yet.
func loadDriveManifest(ctx context.Context, svc *driveapi.Service, folderID string) (*driveManifest, error) {
	dm := &driveManifest{
		entries:  make(map[string]manifestEntry),
		drive:    svc,
		folderID: folderID,
	}

	// Search for an existing manifest file in the root folder.
	q := fmt.Sprintf("name='%s' and '%s' in parents and trashed=false", manifestFileName, folderID)
	list, err := svc.Files.List().Q(q).Fields("files(id)").Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("searching for manifest in Drive: %w", err)
	}

	if len(list.Files) == 0 {
		// No manifest yet — start fresh.
		return dm, nil
	}

	dm.fileID = list.Files[0].Id

	// Download the file content.
	resp, err := svc.Files.Get(dm.fileID).Context(ctx).Download()
	if err != nil {
		return nil, fmt.Errorf("downloading manifest from Drive: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading manifest body: %w", err)
	}

	if err := json.Unmarshal(data, &dm.entries); err != nil {
		return nil, fmt.Errorf("parsing manifest JSON: %w", err)
	}
	return dm, nil
}

// save writes the current manifest back to Google Drive, updating the existing
// file or creating a new one if this is the first save.
func (dm *driveManifest) save(ctx context.Context) error {
	data, err := json.MarshalIndent(dm.entries, "", "  ")
	if err != nil {
		return fmt.Errorf("marshalling manifest: %w", err)
	}

	content := bytes.NewReader(data)

	if dm.fileID == "" {
		// First save — create the file inside the root folder.
		f, err := dm.drive.Files.Create(&driveapi.File{
			Name:    manifestFileName,
			Parents: []string{dm.folderID},
		}).Media(content).Fields("id").Context(ctx).Do()
		if err != nil {
			return fmt.Errorf("creating manifest file in Drive: %w", err)
		}
		dm.fileID = f.Id
		return nil
	}

	// Update the existing file in place.
	if _, err := dm.drive.Files.Update(dm.fileID, &driveapi.File{}).
		Media(content).Context(ctx).Do(); err != nil {
		return fmt.Errorf("updating manifest file in Drive: %w", err)
	}
	return nil
}

// contentHash returns the hex-encoded SHA-256 digest of content.
func contentHash(content string) string {
	sum := sha256.Sum256([]byte(content))
	return fmt.Sprintf("%x", sum)
}

// knownID reports whether the transcript ID is already recorded in the manifest,
// regardless of content. Used to skip the content download entirely for
// transcripts that have been previously uploaded.
func (dm *driveManifest) knownID(id string) bool {
	_, ok := dm.entries[id]
	return ok
}

// alreadyUploaded reports whether t has been uploaded before with identical
// content. It returns true only when the transcript ID is present in the
// manifest and its stored hash matches the hash of t.Content.
func (dm *driveManifest) alreadyUploaded(t Transcript) bool {
	entry, ok := dm.entries[t.ID]
	if !ok {
		return false
	}
	return entry.Hash == contentHash(t.Content)
}

// record stores a successful upload in the in-memory manifest and persists it
// to Drive immediately so a partial run is never lost.
//
// summaryURL may be empty when no AI summary was available for the meeting.
func (dm *driveManifest) record(ctx context.Context, t Transcript, transcriptURL, summaryURL string) error {
	dm.entries[t.ID] = manifestEntry{
		Hash:          contentHash(t.Content),
		TranscriptURL: transcriptURL,
		SummaryURL:    summaryURL,
		Title:         t.MeetingTitle,
		MeetingDate:   t.StartTime.UTC().Format("2006-01-02"),
		UploadedAt:    time.Now().UTC(),
	}
	return dm.save(ctx)
}
