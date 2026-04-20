#!/usr/bin/env bash
# install.sh - One-shot installer for webex-scribe.
#
# Designed to be run directly without cloning the repo first:
#
#   bash <(curl -fsSL "https://raw.githubusercontent.com/sfakam/webex-scribe/main/install.sh")
#
# What it does:
#   1. Tries to download a pre-built binary from the latest GitHub Release
#   2. Falls back to cloning the repo and building from source (requires Go 1.22+)
#   3. Installs gcloud if missing
#   4. Authenticates with Google Drive/Docs (skipped if already authed)
#
# Environment variables (all optional):
#   WEBEX_SCRIBE_SRC_DIR   Where to clone the source for fallback build (default: /tmp/webex-scribe-src)
#   WEBEX_SCRIBE_INSTALL   Binary install location                       (default: /usr/local/bin)
#
# Tested on Ubuntu 20.04, 22.04, 24.04 and macOS (Homebrew).

set -euo pipefail

REPO_SSH="git@github.com:sfakam/webex-scribe.git"
REPO_HTTPS="https://github.com/sfakam/webex-scribe.git"
GITHUB_RELEASES="https://github.com/sfakam/webex-scribe/releases/latest/download"
SRC_DIR="${WEBEX_SCRIBE_SRC_DIR:-/tmp/webex-scribe-src}"
INSTALL_DIR="${WEBEX_SCRIBE_INSTALL:-/usr/local/bin}"

# --------------------------------------------------------------------------- #
# Helpers
# --------------------------------------------------------------------------- #

info()  { echo "==> $*"; }
warn()  { echo "WARN: $*" >&2; }
die()   { echo "ERROR: $*" >&2; exit 1; }

OS="$(uname -s)"
ARCH="$(uname -m)"

# --------------------------------------------------------------------------- #
# 1. Check OS / prerequisites
# --------------------------------------------------------------------------- #

echo ""
echo "============================================================"
echo " webex-scribe installer"
echo "============================================================"
echo ""
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

# git is required for the source-build fallback.
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
# 2. Resolve platform binary suffix
# --------------------------------------------------------------------------- #

BINARY_SUFFIX=""
case "${OS}" in
    Darwin)
        [[ "${ARCH}" == "arm64" ]] && BINARY_SUFFIX="macos-arm64" || BINARY_SUFFIX="macos-amd64"
        ;;
    Linux)
        [[ "${ARCH}" == "aarch64" || "${ARCH}" == "arm64" ]] && BINARY_SUFFIX="linux-arm64" || BINARY_SUFFIX="linux-amd64"
        ;;
esac

# --------------------------------------------------------------------------- #
# 3. Download pre-built binary (primary) or build from source (fallback)
# --------------------------------------------------------------------------- #

BUILT_FROM_SOURCE=false

if [[ -n "${BINARY_SUFFIX}" ]]; then
    RELEASE_URL="${GITHUB_RELEASES}/webex-scribe-${BINARY_SUFFIX}"
    DOWNLOAD_TMP="$(mktemp /tmp/webex-scribe-download.XXXXXX)"
    info "Downloading pre-built binary: ${RELEASE_URL}"
    CURL_EXIT=0
    HTTP_CODE=$(curl -fsSL \
        --write-out "%{http_code} url=%{url_effective} size=%{size_download} time=%{time_total}s" \
        --output "${DOWNLOAD_TMP}" \
        "${RELEASE_URL}" 2>&1) || CURL_EXIT=$?
    info "Download result: exit=${CURL_EXIT} ${HTTP_CODE}"
    if [[ "${CURL_EXIT}" -eq 0 && -s "${DOWNLOAD_TMP}" ]]; then
        mv "${DOWNLOAD_TMP}" /tmp/webex-scribe
        chmod +x /tmp/webex-scribe
        info "Downloaded: $(/tmp/webex-scribe --version)"
    else
        rm -f "${DOWNLOAD_TMP}"
        warn "GitHub release download failed (exit=${CURL_EXIT}) — falling back to building from source."
        BUILT_FROM_SOURCE=true
    fi
else
    warn "No pre-built binary available for ${OS}/${ARCH} — building from source."
    BUILT_FROM_SOURCE=true
fi

if [[ "${BUILT_FROM_SOURCE}" == "true" ]]; then

    # Install Go if missing or too old (minimum 1.22)
    MIN_GO_MINOR=22

    install_go_linux() {
        local GO_VERSION="1.23.6"
        local GO_ARCH="amd64"
        [[ "${ARCH}" == "aarch64" || "${ARCH}" == "arm64" ]] && GO_ARCH="arm64"
        local GO_TARBALL="go${GO_VERSION}.linux-${GO_ARCH}.tar.gz"
        info "Downloading Go ${GO_VERSION} (linux/${GO_ARCH})..."
        curl -fsSL "https://go.dev/dl/${GO_TARBALL}" -o "/tmp/${GO_TARBALL}"
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

    # Clone / update source
    if [[ -d "${SRC_DIR}/.git" ]]; then
        info "Source exists at ${SRC_DIR} — pulling latest..."
        git -C "${SRC_DIR}" pull --ff-only
    else
        info "Cloning webex-scribe into ${SRC_DIR}..."
        if git clone "${REPO_SSH}" "${SRC_DIR}" 2>/dev/null; then
            info "Cloned via SSH."
        else
            warn "SSH clone failed — falling back to HTTPS..."
            git clone "${REPO_HTTPS}" "${SRC_DIR}"
            info "Cloned via HTTPS."
        fi
    fi

    # Build
    info "Building webex-scribe..."
    cd "${SRC_DIR}"
    VERSION=$(git describe --tags --always --dirty 2>/dev/null || echo "dev")
    go build -ldflags "-X main.version=${VERSION}" -o /tmp/webex-scribe .
    info "Binary built (version: ${VERSION})."
fi

# --------------------------------------------------------------------------- #
# 4. Install binary to INSTALL_DIR
# --------------------------------------------------------------------------- #

info "Installing webex-scribe to ${INSTALL_DIR}/webex-scribe ..."
sudo install -m 755 /tmp/webex-scribe "${INSTALL_DIR}/webex-scribe"
info "Installed: $(which webex-scribe) — $(webex-scribe --version)"

# --------------------------------------------------------------------------- #
# 5. Install gcloud (if missing)
# --------------------------------------------------------------------------- #

if command -v gcloud &>/dev/null; then
    info "gcloud $(gcloud version --format='value(Google Cloud SDK)' 2>/dev/null || echo '') found — OK."
else
    info "Installing Google Cloud SDK..."
    if [[ "${OS}" == "Darwin" ]]; then
        brew install --cask google-cloud-sdk
        export PATH="$(brew --prefix)/share/google-cloud-sdk/bin:${PATH}"
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
# 6. Google authentication
# --------------------------------------------------------------------------- #

GCLOUD_AUTHED=false
if gcloud auth list --filter="status=ACTIVE" --format="value(account)" 2>/dev/null | grep -q '@'; then
    # Check that a Drive-scoped token can actually be minted — this confirms
    # the account was authenticated with --enable-gdrive-access.
    if gcloud auth print-access-token \
        --scopes=https://www.googleapis.com/auth/drive 2>/dev/null | grep -q '.'; then
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
