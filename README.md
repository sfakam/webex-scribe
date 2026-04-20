# webex-scribe

Downloads meeting transcripts and AI summaries from Webex and creates one Google Doc per meeting, organised in Google Drive under `webex-meetings/YYYY-MM/<meeting name>/`. Each meeting folder contains a **Transcript** doc and, when available, a **Summary** doc.

## Getting Started

### Fast install (recommended)

Run this single command — no need to clone the repo first:

```sh
bash <(curl -fsSLk "https://git.source.akamai.com/rest/api/1.0/users/sfathall/repos/webex-scribe/raw/install.sh?at=refs/heads/main")
```

> **macOS only:** if `curl` fails with a LibreSSL SSL error, use this Python 3 alternative (avoids LibreSSL):
>
> ```sh
> python3 -c "
> import urllib.request, ssl, subprocess
> ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)
> ctx.check_hostname = False; ctx.verify_mode = ssl.CERT_NONE
> subprocess.run(['bash'], input=urllib.request.urlopen(
>   'https://git.source.akamai.com/rest/api/1.0/users/sfathall/repos/webex-scribe/raw/install.sh?at=refs/heads/main',
>   context=ctx).read())
> "
> ```

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
- The token is saved to `~/.webex-meeting-sync/.env` and reused on subsequent runs until it expires.
- Google OAuth2 opens a browser tab for authorization (one-time).

On **subsequent runs** the app authenticates silently and goes straight to syncing.

### (Optional) Configure via `.env`

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
./webex-scribe

# Specify a date range
./webex-scribe --from 2026-03-01 --to 2026-03-31

# Fetch from a specific Webex Space only
./webex-scribe --space-id Y2lzY29zcGFyazovL...

# Fetch all org transcripts (requires meeting:admin_transcript_read scope approved by org admin)
./webex-scribe --admin

# Sync transcripts from all spaces a bot is a member of (uses WEBEX_BOT_TOKEN)
./webex-scribe --bot

# Force re-authentication with Webex
./webex-scribe --reauth

# Force re-authentication with Google
./webex-scribe --google-reauth
```

## Flags

| Flag | Default | Description |
|------|---------|-------------|
| `-from` | 30 days ago | Start date `YYYY-MM-DD` |
| `-to` | today | End date `YYYY-MM-DD` |
| `-space-id` | *(empty)* | Webex Space (room) ID to filter transcripts |
| `-admin` | false | Include `meeting:admin_transcript_read` scope (org-wide transcripts) |
| `-bot` | false | Use `WEBEX_BOT_TOKEN` to list bot spaces and sync transcripts into `plx-webex-meetings/` |
| `-reauth` | false | Delete saved Webex token and re-authenticate |
| `-google-reauth` | false | Delete saved Google token and re-authenticate |

For advanced OAuth2 flags (`-client-id`, `-client-secret`) run `webex-scribe -help-advanced`.

## Authentication Details

**Webex:** The app checks for a token in this order:
1. `WEBEX_TOKEN` env var / `.env` file (personal access token — recommended)
2. `~/.webex-meeting-sync/.env` (saved interactively on first run)
3. Saved OAuth2 token at `~/.config/webex-scribe/token.json`
4. Interactive OAuth2 browser flow (if `WEBEX_CLIENT_ID` / `WEBEX_CLIENT_SECRET` are set)

**Google:** Checks in this order:
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

## Google Apps Script: Action Items → Google Tasks

After transcripts are uploaded, you can automatically create Google Tasks from the Webex AI summary action items using the included Apps Script. See the script comments for setup instructions — it reads from the Drive manifest to avoid duplicates and creates one Task list per meeting named `<Meeting Name> — YYYY-MM-DD followup`.

## Transcript Format

Webex returns transcripts in **VTT** (WebVTT) format:

```
00:00:05.000 --> 00:00:10.000
Speaker Name: This is what they said.
```
