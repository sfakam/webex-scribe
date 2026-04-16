# webex-transcript-sync

Downloads meeting transcripts and AI summaries from Webex and creates one Google Doc per meeting, organised in Google Drive under `webex-meetings/YYYY-MM/<meeting name>/`. Each meeting folder contains a **Transcript** doc and, when available, a **Summary** doc.

## Getting Started

### 1. Build the binary

```sh
git clone <repo>
cd webex-transcript-sync
go build -o webex-transcript-sync .
```

### 2. Set up Google authentication

The app needs access to Google Drive and Docs. On the first run it will open a browser to authenticate. To set up credentials:

1. Go to [console.cloud.google.com](https://console.cloud.google.com) → **APIs & Services** → **Credentials** → **Create Credentials** → **OAuth client ID** → **Desktop app**.
2. Enable **Google Drive API** and **Google Docs API** for your project.
3. Copy the **Client ID** and **Client Secret** into your `.env` file:
   ```
   GOOGLE_CLIENT_ID="..."
   GOOGLE_CLIENT_SECRET="..."
   ```

Alternatively, if you have `gcloud` installed and authenticated with Drive access, no Google credentials file is needed — the app will fall back to `gcloud auth print-access-token` automatically.

### 3. Run the app

```sh
./webex-transcript-sync
```

On the **first run**:
- The app checks for a Webex personal access token. If none is found or it has expired (tokens last 12 hours), you are prompted to paste one:
  ```
  No Webex token found.

    1. Open https://developer.webex.com/docs/getting-started
    2. Sign in and copy the Personal Access Token (valid for 12 hours).

  Paste your Webex Personal Access Token: <paste here>
  Token saved to /home/you/.webex-meeting-sync/.env
  Signed in as Your Name.
  ```
- The token is saved to `~/.webex-meeting-sync/.env` and reused on subsequent runs until it expires.
- Google OAuth2 opens a browser tab for authorization (one-time).

On **subsequent runs** the app authenticates silently and goes straight to syncing.

### 4. (Optional) Configure via `.env`

Copy `.env.example` to `.env` in the project directory and fill in any values you want to persist:

```sh
cp .env.example .env
```

Project `.env` values take precedence over `~/.webex-meeting-sync/.env`. Useful for pinning a long-lived integration token or OAuth2 credentials.

## Drive Folder Structure

```
webex-meetings/
  2026-04/
    Weekly Sync — Apr 15, 2026/
      Transcript        ← raw VTT content
      Summary           ← Webex AI summary + action items (if available)
    Demo Review — Apr 9, 2026/
      Transcript
      Summary
  2026-03/
    ...
```

## Deduplication

A manifest file (`.wts-manifest.json`) is stored in the `webex-meetings/` Drive folder. On every run the app checks the manifest before downloading — transcripts already uploaded are skipped automatically. The manifest is shared across machines via Drive.

## Usage

```sh
# Sync all transcripts from the last 30 days (default)
./webex-transcript-sync

# Specify a date range
./webex-transcript-sync --from 2026-03-01 --to 2026-03-31

# Fetch from a specific Webex Space only
./webex-transcript-sync --space-id Y2lzY29zcGFyazovL...

# Fetch all org transcripts (requires meeting:admin_transcript_read scope approved by org admin)
./webex-transcript-sync --admin

# Force re-authentication with Webex
./webex-transcript-sync --reauth

# Force re-authentication with Google
./webex-transcript-sync --google-reauth
```

## Flags

| Flag | Default | Description |
|------|---------|-------------|
| `-from` | 30 days ago | Start date `YYYY-MM-DD` |
| `-to` | today | End date `YYYY-MM-DD` |
| `-space-id` | *(empty)* | Webex Space (room) ID to filter transcripts |
| `-admin` | false | Include `meeting:admin_transcript_read` scope (org-wide transcripts) |
| `-reauth` | false | Delete saved Webex token and re-authenticate |
| `-google-reauth` | false | Delete saved Google token and re-authenticate |
| `-client-id` | `$WEBEX_CLIENT_ID` | Webex OAuth2 client ID (not needed when using personal access token) |
| `-client-secret` | `$WEBEX_CLIENT_SECRET` | Webex OAuth2 client secret |

## Authentication Details

**Webex:** The app checks for a token in this order:
1. `WEBEX_TOKEN` env var / `.env` file (personal access token — recommended)
2. `~/.webex-meeting-sync/.env` (saved interactively on first run)
3. Saved OAuth2 token at `~/.config/webex-transcript-sync/token.json`
4. Interactive OAuth2 browser flow (if `WEBEX_CLIENT_ID` / `WEBEX_CLIENT_SECRET` are set)

**Google:** Checks in this order:
1. Application Default Credentials (`gcloud auth application-default login`)
2. Saved in-app OAuth2 token at `~/.config/webex-transcript-sync/google_token.json`
3. Interactive OAuth2 browser flow (if `GOOGLE_CLIENT_ID` / `GOOGLE_CLIENT_SECRET` are set)
4. `gcloud auth print-access-token`

## Space Meetings vs Scheduled Meetings

| Type | How to fetch |
|------|-------------|
| **Scheduled meetings** | Returned by default — no extra flag needed |
| **Space meetings** | Pass `--space-id <room-id>` to scope to that space |

> **Note:** `meeting:transcripts_read` only returns meetings you **hosted**. To get all org meetings, your Webex org admin must approve `meeting:admin_transcript_read` in your integration and you pass `--admin`.

**Finding a Space ID:** Open the Space in the Webex web app — the room ID appears in the URL. Or:

```sh
curl -H "Authorization: Bearer $WEBEX_TOKEN" \
  'https://webexapis.com/v1/rooms?type=group' | jq '.items[] | {id, title}'
```

## Google Apps Script: Action Items → Google Tasks

After transcripts are uploaded, you can automatically create Google Tasks from the Webex AI summary action items using the included Apps Script. See the script comments for setup instructions — it reads from the Drive manifest to avoid duplicates and creates one Task list per meeting named `<Meeting Name> — YYYY-MM-DD followup`.

## Transcript Format

Webex returns transcripts in **VTT** (WebVTT) format:

```
00:00:05.000 --> 00:00:10.000
Speaker Name: This is what they said.
```
