#!/usr/bin/env bash
# setup.sh - Install dependencies and build webex-scribe.
#
# Installs:
#   - Google Cloud SDK (gcloud) via the official package manager
#   - Go 1.22+ if not already present
# Then builds the binary and prompts for Google authentication.
#
# Tested on Ubuntu 20.04, 22.04, 24.04 and macOS (Homebrew).

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

# Detect OS
OS="$(uname -s)"

# --------------------------------------------------------------------------- #
# 1. Check OS
# --------------------------------------------------------------------------- #

case "${OS}" in
    Linux)
        if [[ ! -f /etc/debian_version ]]; then
            die "Linux support is Debian/Ubuntu only. Install gcloud manually: https://cloud.google.com/sdk/docs/install"
        fi
        ;;
    Darwin)
        if ! command -v brew &>/dev/null; then
            die "Homebrew is required on macOS. Install it from https://brew.sh then re-run this script."
        fi
        ;;
    *)
        die "Unsupported OS: ${OS}. Install dependencies manually."
        ;;
esac

# --------------------------------------------------------------------------- #
# 2. Install Go (if missing or too old; minimum 1.22)
# --------------------------------------------------------------------------- #

MIN_GO_MINOR=22

install_go_linux() {
    local GO_VERSION="1.23.6"
    local ARCH
    ARCH="$(uname -m)"
    local GO_ARCH="amd64"
    [[ "${ARCH}" == "aarch64" || "${ARCH}" == "arm64" ]] && GO_ARCH="arm64"
    local GO_TARBALL="go${GO_VERSION}.linux-${GO_ARCH}.tar.gz"
    local GO_URL="https://go.dev/dl/${GO_TARBALL}"

    info "Downloading Go ${GO_VERSION} (linux/${GO_ARCH})..."
    curl -fsSL "${GO_URL}" -o "/tmp/${GO_TARBALL}"

    info "Installing Go ${GO_VERSION} to /usr/local/go ..."
    sudo rm -rf /usr/local/go
    sudo tar -C /usr/local -xzf "/tmp/${GO_TARBALL}"
    rm "/tmp/${GO_TARBALL}"

    export PATH="/usr/local/go/bin:${PATH}"
    info "Go $(go version) installed."
}

install_go_mac() {
    info "Installing Go via Homebrew..."
    brew install go
    export PATH="$(brew --prefix go)/bin:${PATH}"
    info "Go $(go version) installed."
}

if command -v go &>/dev/null; then
    # grep -oP is GNU grep; macOS needs a different approach
    CURRENT_MINOR=$(go version | sed -E 's/.*go1\.([0-9]+).*/\1/')
    if (( CURRENT_MINOR < MIN_GO_MINOR )); then
        warn "Go 1.${CURRENT_MINOR} is too old (need >= 1.${MIN_GO_MINOR}). Upgrading..."
        [[ "${OS}" == "Darwin" ]] && install_go_mac || install_go_linux
    else
        info "Go $(go version) found — OK."
    fi
else
    info "Go not found. Installing..."
    [[ "${OS}" == "Darwin" ]] && install_go_mac || install_go_linux
fi

# Make sure go is on PATH for subsequent steps.
export PATH="/usr/local/go/bin:${HOME}/go/bin:${PATH}"

# --------------------------------------------------------------------------- #
# 3. Install Google Cloud SDK
# --------------------------------------------------------------------------- #

if command -v gcloud &>/dev/null; then
    info "gcloud $(gcloud version --format='value(Google Cloud SDK)' 2>/dev/null || echo '') found — OK."
else
    info "Installing Google Cloud SDK..."

    if [[ "${OS}" == "Darwin" ]]; then
        info "Installing via Homebrew..."
        brew install --cask google-cloud-sdk
        # Homebrew cask doesn't modify PATH; source the profile if present.
        GCLOUD_BREW_PATH="$(brew --prefix)/share/google-cloud-sdk/bin"
        export PATH="${GCLOUD_BREW_PATH}:${PATH}"
    else
        require_sudo
        sudo apt-get update -qq
        sudo apt-get install -y -qq apt-transport-https ca-certificates gnupg curl

        curl -fsSL https://packages.cloud.google.com/apt/doc/apt-key.gpg \
            | sudo gpg --dearmor -o /usr/share/keyrings/cloud.google.gpg

        echo "deb [signed-by=/usr/share/keyrings/cloud.google.gpg] \
https://packages.cloud.google.com/apt cloud-sdk main" \
            | sudo tee /etc/apt/sources.list.d/google-cloud-sdk.list > /dev/null

        sudo apt-get update -qq
        sudo apt-get install -y -qq google-cloud-cli
    fi

    info "gcloud installed: $(gcloud version --format='value(Google Cloud SDK)' 2>/dev/null)"
fi

# --------------------------------------------------------------------------- #
# 4. Build the binary
# --------------------------------------------------------------------------- #

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "${SCRIPT_DIR}"

info "Building webex-scribe..."
go build -o webex-scribe .
info "Binary built: ${SCRIPT_DIR}/webex-scribe"

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
echo " Run the app — it will prompt for your Webex Personal Access Token"
echo " on first use (valid for 12 hours, saved to ~/.webex-meeting-sync/.env):"
echo ""
echo "   ${SCRIPT_DIR}/webex-scribe"
echo ""
echo " Optional: copy .env.example to .env to persist tokens or pin a"
echo " long-lived integration token:"
echo ""
echo "   cp ${SCRIPT_DIR}/.env.example ${SCRIPT_DIR}/.env"
echo ""
