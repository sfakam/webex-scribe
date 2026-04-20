#!/usr/bin/env bash
# install.sh - One-shot installer for webex-scribe.
#
# Designed to be run directly without cloning the repo first:
#
#   bash <(git archive --remote=ssh://git@git.source.akamai.com:7999/~sfathall/webex-scribe.git HEAD install.sh | tar -xOf -)
#
# Requires SSH key access to git.source.akamai.com.
# HTTPS is not supported (server requires mTLS client certificates).
#
# What it does:
#   1. Installs Go 1.22+ (Homebrew on macOS, tarball on Linux)
#   2. Installs gcloud (Homebrew cask on macOS, apt on Debian/Ubuntu)
#   3. Clones/updates the webex-scribe repo to WEBEX_SCRIBE_SRC_DIR
#   4. Builds the binary and installs it to /usr/local/bin/webex-scribe
#   5. Authenticates with Google Drive/Docs (skipped if already authed)
#
# Environment variables (all optional):
#   WEBEX_SCRIBE_SRC_DIR   Where to clone the source (default: ~/webex-scribe-src)
#   WEBEX_SCRIBE_INSTALL   Binary install location   (default: /usr/local/bin)
#
# Tested on Ubuntu 20.04, 22.04, 24.04 and macOS (Homebrew).

set -euo pipefail

REPO_SSH="ssh://git@git.source.akamai.com:7999/~sfathall/webex-scribe.git"
REPO_HTTPS="https://git.source.akamai.com/scm/~sfathall/webex-scribe.git"
SRC_DIR="${WEBEX_SCRIBE_SRC_DIR:-/tmp/webex-scribe-src}"
INSTALL_DIR="${WEBEX_SCRIBE_INSTALL:-/usr/local/bin}"

# --------------------------------------------------------------------------- #
# Helpers
# --------------------------------------------------------------------------- #

info()  { echo "==> $*"; }
warn()  { echo "WARN: $*" >&2; }
die()   { echo "ERROR: $*" >&2; exit 1; }

OS="$(uname -s)"

# --------------------------------------------------------------------------- #
# 1. Check OS / prerequisites
# --------------------------------------------------------------------------- #

echo ""
echo "============================================================"
echo " webex-scribe installer"
echo "============================================================"
echo ""
echo " Source will be cloned to : ${SRC_DIR} (in /tmp; deleted on reboot)"
echo " Binary will be installed  : ${INSTALL_DIR}/webex-scribe"
echo ""
read -rp "Continue? [yes/no]: " REPLY
if [[ ! "${REPLY}" =~ ^([yY][eE][sS]|[yY])$ ]]; then
    echo "Aborted."
    exit 0
fi

case "${OS}" in
    Linux)
        if [[ ! -f /etc/debian_version ]]; then
            die "Linux support is Debian/Ubuntu only. For other distros, install manually."
        fi
        ;;
    Darwin)
        if ! command -v brew &>/dev/null; then
            die "Homebrew is required on macOS. Install from https://brew.sh then re-run."
        fi
        ;;
    *)
        die "Unsupported OS: ${OS}."
        ;;
esac

# git is required for cloning and for the one-liner invocation itself.
if ! command -v git &>/dev/null; then
    info "git not found. Installing..."
    if [[ "${OS}" == "Darwin" ]]; then
        brew install git
    else
        sudo apt-get update -qq
        sudo apt-get install -y -qq git
    fi
    info "git $(git --version) installed."
else
    info "git $(git --version) found — OK."
fi

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

export PATH="/usr/local/go/bin:${HOME}/go/bin:${PATH}"

# --------------------------------------------------------------------------- #
# 3. Install gcloud (if missing)
# --------------------------------------------------------------------------- #

if command -v gcloud &>/dev/null; then
    info "gcloud $(gcloud version --format='value(Google Cloud SDK)' 2>/dev/null || echo '') found — OK."
else
    info "Installing Google Cloud SDK..."
    if [[ "${OS}" == "Darwin" ]]; then
        brew install --cask google-cloud-sdk
        GCLOUD_BREW_PATH="$(brew --prefix)/share/google-cloud-sdk/bin"
        export PATH="${GCLOUD_BREW_PATH}:${PATH}"
    else
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
# 4. Clone / update the source
# --------------------------------------------------------------------------- #

if [[ -d "${SRC_DIR}/.git" ]]; then
    info "Source already exists at ${SRC_DIR} — pulling latest..."
    git -C "${SRC_DIR}" pull --ff-only
else
    info "Cloning webex-scribe into ${SRC_DIR}..."
    if git clone "${REPO_SSH}" "${SRC_DIR}" 2>/dev/null; then
        info "Cloned via SSH."
    else
        warn "SSH clone failed (no SSH key?). Falling back to HTTPS..."
        git clone "${REPO_HTTPS}" "${SRC_DIR}"
        info "Cloned via HTTPS."
    fi
fi

# --------------------------------------------------------------------------- #
# 5. Build the binary
# --------------------------------------------------------------------------- #

info "Building webex-scribe..."
cd "${SRC_DIR}"
go build -o webex-scribe .
info "Binary built."

# --------------------------------------------------------------------------- #
# 6. Install binary to INSTALL_DIR
# --------------------------------------------------------------------------- #

info "Installing webex-scribe to ${INSTALL_DIR}/webex-scribe ..."
sudo install -m 755 "${SRC_DIR}/webex-scribe" "${INSTALL_DIR}/webex-scribe"
info "Installed: $(which webex-scribe)"

# --------------------------------------------------------------------------- #
# 7. Google authentication
# --------------------------------------------------------------------------- #

GCLOUD_AUTHED=false
if gcloud auth list --filter="status=ACTIVE" --format="value(account)" 2>/dev/null | grep -q '@'; then
    if gcloud auth print-access-token 2>/dev/null | xargs -I{} curl -sf \
        -H "Authorization: Bearer {}" \
        "https://www.googleapis.com/drive/v3/about?fields=user" > /dev/null 2>&1; then
        GCLOUD_AUTHED=true
    fi
fi

if [[ "${GCLOUD_AUTHED}" == "true" ]]; then
    ACTIVE_ACCOUNT="$(gcloud auth list --filter="status=ACTIVE" --format="value(account)" 2>/dev/null | head -1)"
    info "Already authenticated with Google as ${ACTIVE_ACCOUNT} — skipping login."
else
    echo ""
    echo "============================================================"
    echo " Google Authentication"
    echo "============================================================"
    echo " The next command will open a browser to authenticate with"
    echo " your Google account and grant Drive/Docs access."
    echo ""
    read -rp "Press Enter to authenticate with Google (Ctrl-C to skip)..."
    gcloud auth login --enable-gdrive-access
fi

# --------------------------------------------------------------------------- #
# Done
# --------------------------------------------------------------------------- #

echo ""
echo "============================================================"
echo " Installation complete!"
echo "============================================================"
echo ""
echo " Run webex-scribe from anywhere:"
echo ""
echo "   webex-scribe"
echo ""
echo " On first run it will prompt for your Webex Personal Access Token:"
echo "   https://developer.webex.com/docs/getting-started"
echo ""
echo " The token is saved to ~/.webex-meeting-sync/.env and reused"
echo " until it expires (12 hours)."
echo ""
echo " To sync a custom date range:"
echo "   webex-scribe --from 2026-03-01 --to 2026-03-31"
echo ""
echo " To update webex-scribe later, re-run this installer."
echo ""
