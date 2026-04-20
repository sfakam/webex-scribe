package main

import (
	"testing"
	"time"
)

// --------------------------------------------------------------------------- #
// contentHash
// --------------------------------------------------------------------------- #

func TestContentHash_Deterministic(t *testing.T) {
	h1 := contentHash("hello world")
	h2 := contentHash("hello world")
	if h1 != h2 {
		t.Errorf("contentHash not deterministic: %q != %q", h1, h2)
	}
}

func TestContentHash_DifferentInputs(t *testing.T) {
	h1 := contentHash("transcript A")
	h2 := contentHash("transcript B")
	if h1 == h2 {
		t.Error("different inputs produced the same hash")
	}
}

func TestContentHash_EmptyString(t *testing.T) {
	h := contentHash("")
	// SHA-256 of empty string is well-known.
	want := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if h != want {
		t.Errorf("contentHash(\"\") = %q; want %q", h, want)
	}
}

// --------------------------------------------------------------------------- #
// driveManifest.knownID
// --------------------------------------------------------------------------- #

func TestKnownID(t *testing.T) {
	dm := &driveManifest{entries: map[string]manifestEntry{
		"id-1": {Hash: "abc", Title: "Meeting A"},
	}}

	if !dm.knownID("id-1") {
		t.Error("expected id-1 to be known")
	}
	if dm.knownID("id-2") {
		t.Error("expected id-2 to be unknown")
	}
	if dm.knownID("") {
		t.Error("expected empty ID to be unknown")
	}
}

// --------------------------------------------------------------------------- #
// driveManifest.alreadyUploaded
// --------------------------------------------------------------------------- #

func TestAlreadyUploaded_MatchingHash(t *testing.T) {
	content := "WEBVTT\n\n00:00:01.000 --> 00:00:05.000\nHello world"
	dm := &driveManifest{entries: map[string]manifestEntry{
		"t-1": {Hash: contentHash(content), Title: "Test Meeting"},
	}}
	tr := Transcript{ID: "t-1", Content: content}
	if !dm.alreadyUploaded(tr) {
		t.Error("expected transcript to be already uploaded")
	}
}

func TestAlreadyUploaded_ChangedContent(t *testing.T) {
	original := "WEBVTT\n\noriginal content"
	updated := "WEBVTT\n\nupdated content"
	dm := &driveManifest{entries: map[string]manifestEntry{
		"t-2": {Hash: contentHash(original), Title: "Test Meeting"},
	}}
	tr := Transcript{ID: "t-2", Content: updated}
	if dm.alreadyUploaded(tr) {
		t.Error("expected changed content to trigger re-upload")
	}
}

func TestAlreadyUploaded_UnknownID(t *testing.T) {
	dm := &driveManifest{entries: make(map[string]manifestEntry)}
	tr := Transcript{ID: "new-id", Content: "some content"}
	if dm.alreadyUploaded(tr) {
		t.Error("expected unknown ID to not be already uploaded")
	}
}

// --------------------------------------------------------------------------- #
// manifestEntry fields
// --------------------------------------------------------------------------- #

func TestManifestEntry_Fields(t *testing.T) {
	now := time.Now().UTC()
	entry := manifestEntry{
		Hash:          "abc123",
		TranscriptURL: "https://docs.google.com/d/1",
		SummaryURL:    "https://docs.google.com/d/2",
		Title:         "Weekly Sync",
		MeetingDate:   "2026-04-20",
		UploadedAt:    now,
	}
	if entry.Hash != "abc123" {
		t.Errorf("Hash = %q", entry.Hash)
	}
	if entry.SummaryURL == "" {
		t.Error("SummaryURL should not be empty")
	}
	if !entry.UploadedAt.Equal(now) {
		t.Error("UploadedAt mismatch")
	}
}

// --------------------------------------------------------------------------- #
// knownID after record (in-memory only, no Drive)
// --------------------------------------------------------------------------- #

func TestKnownID_AfterInMemoryRecord(t *testing.T) {
	dm := &driveManifest{entries: make(map[string]manifestEntry)}
	id := "new-transcript"

	if dm.knownID(id) {
		t.Fatal("should not be known before record")
	}

	// Simulate what record() writes (without the Drive save).
	content := "vtt content"
	dm.entries[id] = manifestEntry{
		Hash:          contentHash(content),
		TranscriptURL: "https://docs.google.com/d/xyz",
		Title:         "New Meeting",
		MeetingDate:   "2026-04-20",
		UploadedAt:    time.Now().UTC(),
	}

	if !dm.knownID(id) {
		t.Error("should be known after in-memory record")
	}
	tr := Transcript{ID: id, Content: content}
	if !dm.alreadyUploaded(tr) {
		t.Error("alreadyUploaded should be true after record with same content")
	}
}

// --------------------------------------------------------------------------- #
// contentHash is hex-encoded (no uppercase, no line breaks)
// --------------------------------------------------------------------------- #

func TestContentHash_Format(t *testing.T) {
	h := contentHash("test")
	for _, ch := range h {
		if !((ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f')) {
			t.Errorf("contentHash contains non-hex char %q in %q", ch, h)
		}
	}
	if len(h) != 64 {
		t.Errorf("expected 64-char hex, got len %d: %q", len(h), h)
	}
}
