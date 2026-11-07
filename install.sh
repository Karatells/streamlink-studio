#!/usr/bin/env bash
# Streamlink Studio - installer / updater
# https://github.com/Karatells/streamlink-studio
#
#   curl -fsSL https://raw.githubusercontent.com/Karatells/streamlink-studio/main/install.sh | sudo bash
#   ... | sudo bash -s -- --update --channel=stable
#   ... | sudo bash -s -- --install --channel=beta
#
# Options:
#   --install | --update | --uninstall   action (omit for an interactive menu)
#   --channel=stable|beta|nightly        release channel (default: stable)
#   --port=8080                          listen port for a fresh install
#   --user=NAME --password=SECRET        protect the panel with HTTP basic auth (fresh install)
#   --purge                              with --uninstall: also delete data and recordings
#   -h, --help
set -euo pipefail

REPO="Karatells/streamlink-studio"
APP="streamlink-studio"
INSTALL_DIR="/opt/${APP}"
DATA_DIR="/var/lib/${APP}"
ENV_FILE="/etc/${APP}.env"
SERVICE="${APP}.service"
SERVICE_FILE="/etc/systemd/system/${SERVICE}"
VENV="${INSTALL_DIR}/venv"
SVC_USER="${APP}"

ACTION=""; CHANNEL=""; PORT="8080"; AUTH_USER=""; AUTH_PASS=""; PURGE=0

c_red=$'\e[31m'; c_grn=$'\e[32m'; c_ylw=$'\e[33m'; c_off=$'\e[0m'
info() { echo "${c_grn}==>${c_off} $*"; }
warn() { echo "${c_ylw}!!${c_off} $*" >&2; }
die()  { echo "${c_red}ERROR:${c_off} $*" >&2; exit 1; }

usage() { sed -n '2,15p' "$0" 2>/dev/null | sed 's/^# \{0,1\}//' || true; exit 0; }

for arg in "$@"; do
  case "$arg" in
    --install) ACTION=install ;;
    --update) ACTION=update ;;
    --uninstall) ACTION=uninstall ;;
    --channel=*) CHANNEL="${arg#*=}" ;;
    --port=*) PORT="${arg#*=}" ;;
    --user=*) AUTH_USER="${arg#*=}" ;;
    --password=*) AUTH_PASS="${arg#*=}" ;;
    --purge) PURGE=1 ;;
    -h|--help) usage ;;
    *) die "unknown option: $arg (see --help)" ;;
  esac
done

[ "$(id -u)" -eq 0 ] || die "run as root (use sudo)"
command -v systemctl >/dev/null 2>&1 || die "systemd is required"

# Prompts must work when the script is piped from curl: read from the terminal.
ask() { # ask "question" "default" -> echoes the answer
  local ans=""
  if [ -r /dev/tty ] && [ -w /dev/tty ]; then
    printf '%s ' "$1" >/dev/tty
    read -r ans </dev/tty || true
  fi
  echo "${ans:-$2}"
}

# ------------------------------------------------------------------ action
if [ -z "$ACTION" ]; then
  if [ ! -r /dev/tty ]; then
    [ -x "${INSTALL_DIR}/${APP}" ] && ACTION=update || ACTION=install
    warn "no terminal available - assuming '${ACTION}'"
  else
    echo "Streamlink Studio"
    echo "  1) Fresh Install"
    echo "  2) Update (keeps database, settings and recordings)"
    echo "  3) Uninstall"
    case "$(ask 'Choose [1-3]:' '')" in
      1) ACTION=install ;; 2) ACTION=update ;; 3) ACTION=uninstall ;;
      *) die "nothing selected" ;;
    esac
  fi
fi

# --------------------------------------------------------------- uninstall
if [ "$ACTION" = uninstall ]; then
  info "Stopping and removing the service"
  systemctl disable --now "$SERVICE" 2>/dev/null || true
  rm -f "$SERVICE_FILE"; systemctl daemon-reload
  rm -rf "$INSTALL_DIR"
  if [ "$PURGE" -eq 1 ]; then
    rm -rf "$DATA_DIR" "$ENV_FILE"; userdel "$SVC_USER" 2>/dev/null || true
    info "Removed data and recordings"
  else
    info "Kept ${DATA_DIR} and ${ENV_FILE} (use --purge to delete them)"
  fi
  exit 0
fi

if [ "$ACTION" = update ] && [ ! -x "${INSTALL_DIR}/${APP}" ]; then
  warn "Streamlink Studio is not installed yet - doing a fresh install"
  ACTION=install
fi

# ----------------------------------------------------------------- channel
# on update, keep the channel the program was installed from (recorded in the env file)
if [ -z "$CHANNEL" ] && [ "$ACTION" = update ] && [ -r "$ENV_FILE" ]; then
  CHANNEL="$(grep -E '^STUDIO_CHANNEL=' "$ENV_FILE" | tail -1 | cut -d= -f2- | tr -d '"' || true)"
  case "$CHANNEL" in stable|beta|nightly) info "Using the installed channel: ${CHANNEL}" ;; *) CHANNEL="" ;; esac
fi
if [ -z "$CHANNEL" ]; then
  if [ -r /dev/tty ]; then
    echo "Release channel:  1) stable   2) beta   3) nightly (main branch)"
    case "$(ask 'Choose [1-3, default 1]:' 1)" in 2) CHANNEL=beta ;; 3) CHANNEL=nightly ;; *) CHANNEL=stable ;; esac
  else
    CHANNEL=stable
  fi
fi
case "$CHANNEL" in stable|beta|nightly) ;; *) die "invalid channel '$CHANNEL' (stable, beta, nightly)" ;; esac

# ------------------------------------------------------------ architecture
case "$(uname -m)" in
  x86_64|amd64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) die "unsupported architecture: $(uname -m) (x86_64 and aarch64 are supported)" ;;
esac
[ "$(uname -s)" = Linux ] || die "Linux only"
info "Action: ${ACTION}, channel: ${CHANNEL}, architecture: ${ARCH}"

# ------------------------------------------------------------ dependencies
pm=""
for p in apt-get dnf yum pacman zypper; do command -v "$p" >/dev/null 2>&1 && { pm="$p"; break; }; done

install_pkgs() {
  case "$pm" in
    apt-get) DEBIAN_FRONTEND=noninteractive apt-get update -qq
             DEBIAN_FRONTEND=noninteractive apt-get install -y -qq curl ca-certificates python3 python3-venv python3-pip ffmpeg ;;
    dnf)     dnf install -y -q curl ca-certificates python3 python3-pip ffmpeg-free || dnf install -y -q curl ca-certificates python3 python3-pip ffmpeg ;;
    yum)     yum install -y -q curl ca-certificates python3 python3-pip ffmpeg ;;
    pacman)  pacman -Sy --noconfirm --needed curl ca-certificates python python-pip ffmpeg ;;
    zypper)  zypper -n install curl ca-certificates python3 python3-pip ffmpeg ;;
    *)       warn "unknown package manager - make sure curl, python3 (with venv/pip) and ffmpeg are installed" ;;
  esac
}
need_deps=0
for b in curl python3 ffmpeg ffprobe; do command -v "$b" >/dev/null 2>&1 || need_deps=1; done
if [ "$need_deps" -eq 1 ]; then info "Installing dependencies (curl, python3, ffmpeg)"; install_pkgs; fi
for b in curl python3 ffmpeg; do command -v "$b" >/dev/null 2>&1 || die "'$b' is missing and could not be installed"; done

info "Installing / updating streamlink in ${VENV}"
mkdir -p "$INSTALL_DIR"
[ -x "${VENV}/bin/pip" ] || python3 -m venv "$VENV" || die "python3 venv failed (install python3-venv)"
"${VENV}/bin/pip" install -q --upgrade pip
"${VENV}/bin/pip" install -q --upgrade streamlink

# --------------------------------------------------------- resolve release
API="https://api.github.com/repos/${REPO}"
ASSET="${APP}_linux_${ARCH}.tar.gz"
case "$CHANNEL" in
  stable)  TAG="$(curl -fsSL "${API}/releases/latest" | python3 -c 'import json,sys; print(json.load(sys.stdin)["tag_name"])')" \
             || die "no stable release found" ;;
  beta)    TAG="$(curl -fsSL "${API}/releases?per_page=30" | python3 -c '
import json,re,sys
best=None
for r in json.load(sys.stdin):
    m=re.match(r"^v?(\d+)\.(\d+)\.(\d+)(?:-(.+))?$", r["tag_name"])
    if r.get("draft") or not m: continue
    # a release sorts above its own pre-releases
    k=(int(m[1]),int(m[2]),int(m[3]),m[4] is None,m[4] or "")
    if best is None or k>best[0]: best=(k,r["tag_name"])
if not best: sys.exit(1)
print(best[1])')" || die "no beta (pre-release) found" ;;
  nightly) TAG="nightly" ;;
esac
URL="https://github.com/${REPO}/releases/download/${TAG}/${ASSET}"
info "Downloading ${TAG}"

TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT
curl -fSL --retry 3 -o "${TMP}/${ASSET}" "$URL" || die "download failed: $URL"
if curl -fsSL -o "${TMP}/checksums.txt" "https://github.com/${REPO}/releases/download/${TAG}/checksums.txt" 2>/dev/null; then
  ( cd "$TMP" && grep " ${ASSET}\$" checksums.txt | sha256sum -c - >/dev/null ) || die "checksum mismatch for ${ASSET}"
  info "Checksum OK"
else
  warn "no checksums.txt published for ${TAG} - skipping verification"
fi
tar -xzf "${TMP}/${ASSET}" -C "$TMP"
[ -f "${TMP}/${APP}" ] || die "archive does not contain '${APP}'"
chmod 0755 "${TMP}/${APP}"

# ------------------------------------------------------------ service user
if ! id "$SVC_USER" >/dev/null 2>&1; then
  useradd --system --home-dir "$DATA_DIR" --shell /usr/sbin/nologin "$SVC_USER" 2>/dev/null \
    || useradd -r -d "$DATA_DIR" -s /bin/false "$SVC_USER"
fi
mkdir -p "$DATA_DIR/recordings"
chown -R "${SVC_USER}:${SVC_USER}" "$DATA_DIR"
chmod 0750 "$DATA_DIR"

# ------------------------------------------------------ stop, swap, restart
if systemctl is-active --quiet "$SERVICE" 2>/dev/null; then
  info "Stopping ${SERVICE} (active recordings are finished gracefully)"
  systemctl stop "$SERVICE"
fi
# atomic replacement of the binary; keep the previous one for rollback
[ -f "${INSTALL_DIR}/${APP}" ] && cp -f "${INSTALL_DIR}/${APP}" "${INSTALL_DIR}/${APP}.prev"
install -m 0755 "${TMP}/${APP}" "${INSTALL_DIR}/${APP}.new"
mv -f "${INSTALL_DIR}/${APP}.new" "${INSTALL_DIR}/${APP}"
# the service user owns the install dir (not the venv) so the in-app "Update" button can swap the binary
chown "${SVC_USER}:${SVC_USER}" "$INSTALL_DIR" "${INSTALL_DIR}/${APP}"
[ -f "${INSTALL_DIR}/${APP}.prev" ] && chown "${SVC_USER}:${SVC_USER}" "${INSTALL_DIR}/${APP}.prev"

if [ ! -f "$ENV_FILE" ]; then
  umask 077
  {
    echo "STUDIO_ADDR=:${PORT}"
    echo "STUDIO_DATA=${DATA_DIR}"
    echo "STUDIO_RECORDINGS=${DATA_DIR}/recordings"
    [ -n "$AUTH_USER" ] && echo "STUDIO_USER=${AUTH_USER}"
    [ -n "$AUTH_PASS" ] && echo "STUDIO_PASS=${AUTH_PASS}"
  } > "$ENV_FILE"
  chmod 0640 "$ENV_FILE"; chown "root:${SVC_USER}" "$ENV_FILE"
  umask 022
  info "Wrote ${ENV_FILE}"
  [ -z "$AUTH_USER" ] && warn "The panel has NO password. Set STUDIO_USER / STUDIO_PASS in ${ENV_FILE} or bind to localhost behind a reverse proxy."
fi

# remember the channel the program was installed from (used by the in-app update check)
if grep -q '^STUDIO_CHANNEL=' "$ENV_FILE" 2>/dev/null; then
  sed -i "s/^STUDIO_CHANNEL=.*/STUDIO_CHANNEL=${CHANNEL}/" "$ENV_FILE"
else
  echo "STUDIO_CHANNEL=${CHANNEL}" >> "$ENV_FILE"
fi

cat > "$SERVICE_FILE" <<EOF
[Unit]
Description=Streamlink Studio
After=network-online.target
Wants=network-online.target

[Service]
User=${SVC_USER}
Group=${SVC_USER}
EnvironmentFile=${ENV_FILE}
Environment=PATH=${VENV}/bin:/usr/local/bin:/usr/bin:/bin
WorkingDirectory=${DATA_DIR}
ExecStart=${INSTALL_DIR}/${APP}
Restart=always
RestartSec=5
# give running recordings time to finish and be remuxed on stop
TimeoutStopSec=60
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
ReadWritePaths=${DATA_DIR} ${INSTALL_DIR}

[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload
systemctl enable "$SERVICE" >/dev/null 2>&1
systemctl restart "$SERVICE"

sleep 2
if systemctl is-active --quiet "$SERVICE"; then
  PORT_NOW="$(grep -E '^STUDIO_ADDR=' "$ENV_FILE" | head -1 | sed 's/.*://')"
  info "Done - Streamlink Studio ${TAG} is running: http://$(hostname -I 2>/dev/null | awk '{print $1}' || echo localhost):${PORT_NOW:-8080}"
  info "Logs: journalctl -u ${SERVICE} -f"
else
  warn "The service failed to start. Check: journalctl -u ${SERVICE} -e"
  if [ -f "${INSTALL_DIR}/${APP}.prev" ] && [ "$ACTION" = update ]; then
    warn "Rolling back to the previous binary"
    mv -f "${INSTALL_DIR}/${APP}.prev" "${INSTALL_DIR}/${APP}"; systemctl restart "$SERVICE" || true
  fi
  exit 1
fi
