# webex-scribe

Downloads meeting transcripts and AI summaries from Webex and creates one Google Doc per meeting, organised in Google Drive under `webex-meetings/{personal-room,direct-rooms,shared-rooms}/<space or meeting name>/`. Each meeting folder contains a **Transcript** doc and, when available, a **Summary** doc and a **Members** doc listing space participants.

Beyond the Google Docs sync, webex-scribe can also run fully offline (`--local`, `--calendar`), send Webex messages as yourself (`--send-message`), and schedule new Webex meetings (`--schedule-meeting`).

## Getting Started

### Fast install (recommended)

Run this single command — no need to clone the repo first:

```sh
bash <(curl -fsSL "https://raw.githubusercontent.com/sfakam/webex-scribe/main/install.sh")
```

> No SSH key required — the script is fetched directly from GitHub.

Or if you already have the repo cloned, run `setup.sh` instead:

```sh
./setup.sh
```

Both scripts will:
1. Install Go 1.22+ if not present (via Homebrew on macOS, tarball on Linux)
2. Install `gcloud` if not present (Homebrew cask on macOS, apt on Debian/Ubuntu)
3. Build the `webex-scribe` binary
4. Open a browser to authenticate with Google Drive/Docs

> **macOS:** requires [Homebrew](https://brew.sh). **Linux:** requires Debian/Ubuntu (apt).

### Manual install

<details>
<summary>Click to expand manual steps</summary>

**1. Build the binary** (requires Go 1.22+):

```sh
git clone <repo>
cd webex-scribe
go build -o webex-scribe .
```

Or with `make` (embeds a version string from `git describe` and installs to `/usr/local/bin`):

```sh
make build
make install
```

**2. Authenticate with Google:**

```sh
gcloud auth login --enable-gdrive-access
```

If `gcloud` isn't installed:

```sh
sudo apt-get install google-cloud-cli   # Debian/Ubuntu
brew install --cask google-cloud-sdk    # macOS
# or see https://cloud.google.com/sdk/docs/install
```

</details>

### Run the app

```sh
./webex-scribe
```

On the **first run** (after setup):
- The app checks for a Webex personal access token. If none is found or it has expired (tokens last 12 hours), you are prompted to paste one:
  ```
  No Webex token found.

    1. Open https://developer.webex.com/docs/getting-started
    2. Sign in and copy the Personal Access Token (valid for 12 hours).

  Paste your Webex Personal Access Token: <paste here>
  Token saved to /home/you/.webex-meeting-sync/.env
  Signed in as Your Name.
  ```
  You can skip the prompt by passing the token directly: `./webex-scribe --token <paste here>`. It's validated, saved, and used for the current run.
- The token is saved to `~/.webex-meeting-sync/.env` and reused on subsequent runs until it expires.
- Google OAuth2 opens a browser tab for authorization (one-time). Skipped entirely in `--local` and `--calendar` modes.

On **subsequent runs** the app authenticates silently and goes straight to syncing.

### (Optional) Configure via `.env`

Copy `.env.example` to `.env` in the project directory and fill in any values you want to persist:

```sh
cp .env.example .env
```

Project `.env` values take precedence over `~/.webex-meeting-sync/.env`. Useful for pinning a long-lived integration token or OAuth2 credentials.

## Drive Folder Structure

Meetings are routed into one of three top-level categories based on where the meeting happened, then grouped into a per-space (or per-meeting-series) subfolder:

```
webex-meetings/
  personal-room/
    1:1 with Manager/
      1:1 with Manager — Apr 9, 2026 — Transcript
      1:1 with Manager — Apr 9, 2026 — Summary
  direct-rooms/
    Jane Doe/
      Jane Doe — Apr 15, 2026 — Transcript
  shared-rooms/
    Team Weekly Sync/
      Team Weekly Sync — Apr 15, 2026 — Transcript
      Team Weekly Sync — Apr 15, 2026 — Summary
      Team Weekly Sync — Apr 1, 2026 — Transcript
      Team Weekly Sync — Apr 1, 2026 — Summary
      Members
  ...
```

Routing logic:

| Meeting has... | Lands in |
|---|---|
| No Webex Space (Personal Meeting Room, or a scheduled meeting not linked to a space) | `personal-room/` |
| A Space of type `direct` (1:1) | `direct-rooms/` |
| A Space of type `group` (team space) | `shared-rooms/` |

A **Members** doc is created once per space folder (not recreated on later runs) listing all space participants, with moderators flagged.

`--bot` mode uploads into a separate `webex-meetings-bot/` root using the same per-space folder convention, without the personal/direct/shared split.

## Deduplication

A manifest file (`.wts-manifest.json`) is stored in the Drive root folder (`webex-meetings/` or `webex-meetings-bot/`). On every run the app checks the manifest before downloading — transcripts already uploaded are skipped automatically. The manifest is shared across machines via Drive. Pass `--reroute` to bypass the manifest and re-upload everything (useful after a folder-routing fix).

`--local` mode uses its own manifest (`.wts-manifest.json` inside `--output-dir`) instead of the Drive one.

## Usage

```sh
# Sync all transcripts from the last 30 days (default) to Google Docs
./webex-scribe

# Sync the last 180 days (auto-split into 30-day chunks)
./webex-scribe --days 180

# Specify an explicit date range
./webex-scribe --from 2026-03-01 --to 2026-03-31

# Fetch from a specific Webex Space only
./webex-scribe --space-id Y2lzY29zcGFyazovL...

# Fetch all org transcripts (requires meeting:admin_transcript_read scope approved by org admin)
./webex-scribe --admin

# Sync transcripts from all spaces a bot is a member of (uses WEBEX_BOT_TOKEN)
./webex-scribe --bot

# Preview what would be uploaded and where, without writing anything
./webex-scribe --dry-run

# Save transcripts as local markdown files instead of Google Docs (no Google auth)
./webex-scribe --local

# Write a local markdown/JSON agenda for the next 7 days (negative N = past N days)
./webex-scribe --calendar 7

# Send a message to a Webex Space as yourself
./webex-scribe --send-message "Standup moved to 10am" --space-name "Team Weekly Sync"

# Schedule a new meeting, inviting all members of a space, and post a notice
./webex-scribe --schedule-meeting "Sprint Planning" --start 2026-09-08T14:00:00-04:00 \
  --duration 45 --agenda "Review backlog" --space-name "Team Weekly Sync" --notify-space

# Non-interactive auth with a supplied token
./webex-scribe --token <personal-access-token>

# Force re-authentication with Webex
./webex-scribe --reauth

# Force re-authentication with Google
./webex-scribe --google-reauth
```

## Flags

### Sync (default mode — Google Docs)

| Flag | Default | Description |
|------|---------|-------------|
| `-days` | 30 | How many days back to fetch (e.g. `180`). Ignored when `--from`/`--to` are set. Requests are split into 30-day chunks automatically. |
| `-from` | *days ago* | Start date `YYYY-MM-DD` |
| `-to` | today | End date `YYYY-MM-DD` |
| `-space-id` | *(empty)* | Webex Space (room) ID to filter transcripts |
| `-admin` | false | Include `meeting:admin_transcript_read` scope (org-wide transcripts) |
| `-bot` | false | Use `WEBEX_BOT_TOKEN` to list bot spaces and sync transcripts into `webex-meetings-bot/` |
| `-dry-run` | false | List transcripts that would be uploaded and the resolved Drive folder for each, without writing anything |
| `-reroute` | false | Re-upload all transcripts regardless of manifest, to correct folder routing (advanced) |

### Offline modes (no Google auth)

| Flag | Default | Description |
|------|---------|-------------|
| `-local` | false | Save transcripts to local markdown files instead of uploading to Google Docs |
| `-calendar` | 0 | Write a local calendar of scheduled meetings: positive N = next N days, negative N = past N days |
| `-output-dir` | `~/webex-scribe-home` | Output directory for `--local` and `--calendar` modes |

### Messaging & scheduling

| Flag | Default | Description |
|------|---------|-------------|
| `-send-message` | *(empty)* | Send a message to a Webex Space as yourself. Supports markdown. Use `-` to read the message from stdin. |
| `-space-name` | *(empty)* | Webex Space name — resolved to an ID via the API (for `--send-message` and `--schedule-meeting`). Use `--space-id` for an exact ID. |
| `-schedule-meeting` | *(empty)* | Schedule a new Webex meeting. Value is the meeting title. |
| `-start` | *(empty)* | Meeting start time, e.g. `2026-09-08T14:00:00-04:00` (RFC3339, or `YYYY-MM-DD HH:MM` in local time) — required with `-schedule-meeting` |
| `-duration` | 60 | Meeting duration in minutes |
| `-agenda` | *(empty)* | Meeting agenda text |
| `-invitees` | *(empty)* | Comma-separated email addresses to invite (combined with space members when `-space-name`/`-space-id` is set) |
| `-notify-space` | false | After scheduling, post a meeting-info message to the space as yourself (requires `-space-name` or `-space-id`) |

### Authentication

| Flag | Default | Description |
|------|---------|-------------|
| `-token` | *(empty)* | Webex Personal Access Token — validated, saved to disk, and used for this run (skips the interactive prompt) |
| `-reauth` | false | Delete saved Webex token and re-authenticate |
| `-google-reauth` | false | Delete saved Google token and re-authenticate |
| `-version` | false | Print version and exit |

For advanced OAuth2 and debug flags (`-client-id`, `-client-secret`, `-debug`, `-reroute`) run `webex-scribe -help-advanced`.

## Authentication Details

**Webex:** The app checks for a token in this order:
1. `-token` flag (validated and saved for future runs)
2. `WEBEX_TOKEN` env var / `.env` file (personal access token — recommended)
3. `~/.webex-meeting-sync/.env` (saved interactively on first run)
4. Saved OAuth2 token at `~/.config/webex-scribe/token.json`
5. Interactive OAuth2 browser flow (if `WEBEX_CLIENT_ID` / `WEBEX_CLIENT_SECRET` are set)

Non-interactive environments (no TTY) exit with status **2** and print `WEBEX_TOKEN_EXPIRED` when no valid token is available, instead of prompting.

**Google:** Checks in this order (skipped entirely in `--local` / `--calendar` modes):
1. Application Default Credentials (`gcloud auth application-default login`)
2. Saved in-app OAuth2 token at `~/.config/webex-scribe/google_token.json`
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

Pass `--space-name "<exact title>"` instead of `--space-id` for `--send-message` and `--schedule-meeting` — it is resolved to an ID via the rooms API.

## Offline / local modes

These modes skip Google auth entirely and write to `--output-dir` (default `~/webex-scribe-home`).

**`--local`** downloads transcripts for the requested window (same `--days`/`--from`/`--to`/`--space-id`/`--admin` flags as the default sync) and writes one markdown file per meeting:

```
<output-dir>/<space-or-meeting-title>/<YYYY-MM-DD>-transcript.md
<output-dir>/<space-or-meeting-title>/<YYYY-MM-DD>-summary.md   (when an AI summary is available)
```

Each transcript file includes the meeting date (Eastern time), space name, and participant list before the transcript body. Already-saved meetings are tracked in `<output-dir>/.wts-manifest.json` and skipped on later runs.

**`--calendar N`** fetches scheduled meetings (positive N = next N days, negative N = past N days) and merges them into a rolling `<output-dir>/webex-calendar/webex-calendar.json`, keyed by local date and deduplicated by meeting ID — repeated runs accumulate rather than overwrite.

## Messaging and scheduling meetings

**`--send-message "<text>"`** posts to a Webex Space as the authenticated user (not a bot). Requires `--space-id` or `--space-name`. Pass `-` as the message to read from stdin, e.g. `git log -1 --format=%B | ./webex-scribe --send-message -`.

**`--schedule-meeting "<title>"`** creates a new Webex meeting. Requires `--start`. When `--space-name`/`--space-id` is given, the meeting is linked to that space (appears in its Meetings tab) and all space members are automatically invited; `--invitees` adds people outside the space. Without a space, the meeting is standalone. `--notify-space` posts a follow-up message to the space showing the meeting time across ET, Ireland, and India, plus the agenda.

## Google Apps Script: Action Items → Google Tasks

After transcripts are uploaded, you can automatically create Google Tasks from the Webex AI summary action items using the included Apps Script. See the script comments for setup instructions — it reads from the Drive manifest to avoid duplicates and creates one Task list per meeting named `<Meeting Name> — YYYY-MM-DD followup`.

## Transcript Format

Webex returns transcripts in **VTT** (WebVTT) format:

```
00:00:05.000 --> 00:00:10.000
Speaker Name: This is what they said.
```
