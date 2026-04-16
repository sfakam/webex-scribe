#!/usr/bin/env bash
# setup.sh - Install dependencies and build webex-transcript-sync.
#
# Installs:
#   - Google Cloud SDK (gcloud) via the official apt repository
#   - Go 1.22+ if not already present
# Then builds the binary and prompts for Google authentication.
#
# Tested on Ubuntu 20.04, 22.04, and 24.04 (Debian-based distros).

set -euo pipefail

# --------------------------------------------------------------------------- #
# Helpers
# --------------------------------------------------------------------------- #

info()  { echo "==> $*"; }
warn()  { echo "WARN: $*" >&2; }
die()   { echo "ERROR: $*" >&2; exit 1; }

require_sudo() {
    if [[ $EUID -ne 0 ]] && ! sudo -n true 2>/dev/null; then
        info "This step requires sudo access."
    fi
}

# --------------------------------------------------------------------------- #
# 1. Check OS
# --------------------------------------------------------------------------- #

if [[ ! -f /etc/debian_version ]]; then
    die "This script supports Debian/Ubuntu only. Install gcloud manually from https://cloud.google.com/sdk/docs/install"
fi

# --------------------------------------------------------------------------- #
# 2. Install Go (if missing or too old; minimum 1.22)
# --------------------------------------------------------------------------- #

MIN_GO_MINOR=22
REQUIRED_GO="go1.${MIN_GO_MINOR}"

install_go() {
    GO_VERSION="1.23.6"
    GO_TARBALL="go${GO_VERSION}.linux-amd64.tar.gz"
    GO_URL="https://go.dev/dl/${GO_TARBALL}"

    info "Downloading Go ${GO_VERSION}..."
    curl -fsSL "${GO_URL}" -o "/tmp/${GO_TARBALL}"

    info "Installing Go ${GO_VERSION} to /usr/local/go ..."
    sudo rm -rf /usr/local/go
    sudo tar -C /usr/local -xzf "/tmp/${GO_TARBALL}"
    rm "/tmp/${GO_TARBALL}"

    # Ensure /usr/local/go/bin is on PATH for the rest of this script.
    export PATH="/usr/local/go/bin:${PATH}"
    info "Go $(go version) installed."
}

if command -v go &>/dev/null; then
    # Extract the minor version number, e.g. "1.23.6" -> 23
    CURRENT_MINOR=$(go version | grep -oP 'go1\.\K[0-9]+')
    if (( CURRENT_MINOR < MIN_GO_MINOR )); then
        warn "Go 1.${CURRENT_MINOR} is too old (need >= 1.${MIN_GO_MINOR}). Upgrading..."
        install_go
    else
        info "Go $(go version) found — OK."
    fi
else
    info "Go not found. Installing..."
    install_go
fi

# Make sure go is on PATH for subsequent steps (already set if we just installed).
export PATH="/usr/local/go/bin:${HOME}/go/bin:${PATH}"

# --------------------------------------------------------------------------- #
# 3. Install Google Cloud SDK
# --------------------------------------------------------------------------- #

if command -v gcloud &>/dev/null; then
    info "gcloud $(gcloud version --format='value(Google Cloud SDK)' 2>/dev/null || echo '') found — OK."
else
    info "Installing Google Cloud SDK..."
    require_sudo

    # Add the Google Cloud apt repository.
    sudo apt-get update -qq
    sudo apt-get install -y -qq apt-transport-https ca-certificates gnupg curl

    curl -fsSL https://packages.cloud.google.com/apt/doc/apt-key.gpg \
        | sudo gpg --dearmor -o /usr/share/keyrings/cloud.google.gpg

    echo "deb [signed-by=/usr/share/keyrings/cloud.google.gpg] \
https://packages.cloud.google.com/apt cloud-sdk main" \
        | sudo tee /etc/apt/sources.list.d/google-cloud-sdk.list > /dev/null

    sudo apt-get update -qq
    sudo apt-get install -y -qq google-cloud-cli

    info "gcloud installed: $(gcloud version --format='value(Google Cloud SDK)' 2>/dev/null)"
fi

# --------------------------------------------------------------------------- #
# 4. Build the binary
# --------------------------------------------------------------------------- #

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "${SCRIPT_DIR}"

info "Building webex-transcript-sync..."
go build -o webex-transcript-sync .
info "Binary built: ${SCRIPT_DIR}/webex-transcript-sync"

# --------------------------------------------------------------------------- #
# 5. Authenticate with Google
# --------------------------------------------------------------------------- #

echo ""
echo "============================================================"
echo " Google Authentication"
echo "============================================================"
echo " The next command will open a browser so you can log in"
echo " with your Google account and grant Drive/Docs access."
echo ""
read -rp "Press Enter to authenticate with Google (Ctrl-C to skip)..."
gcloud auth login --enable-gdrive-access

# --------------------------------------------------------------------------- #
# Done
# --------------------------------------------------------------------------- #

echo ""
echo "============================================================"
echo " Setup complete!"
echo "============================================================"
echo ""
echo " Next step: create a Webex OAuth2 integration at"
echo "   https://developer.webex.com → My Apps → Create a New App"
echo ""
echo " Redirect URI : http://localhost:47823/callback"
echo " Scopes       : meeting:schedules_read"
echo "                meeting:transcripts_read"
echo "                meeting:admin_transcript_read"
echo "                spark:rooms_read"
echo ""
echo " Then run:"
echo "   export WEBEX_CLIENT_ID=<your-client-id>"
echo "   export WEBEX_CLIENT_SECRET=<your-client-secret>"
echo "   ${SCRIPT_DIR}/webex-transcript-sync"
echo ""
