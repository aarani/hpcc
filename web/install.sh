#!/usr/bin/env bash
#
# hpcc one-shot installer. Run:
#
#   curl -fsSL https://hpcc.dev/install.sh | bash
#
# Detects your platform, downloads the matching hpcc binary from the
# latest GitHub release, then walks you through `hpcc init
# client|worker|scheduler` interactively. Pure bash, no dependencies
# beyond curl + unzip (or tar). Reads input from /dev/tty so the curl
# pipe doesn't eat the answers.
#
# Env overrides (set before piping into bash):
#   HPCC_REPO       — override the GitHub repo (default: aarani/hpcc)
#   HPCC_VERSION    — pin a specific release tag (default: latest)
#   HPCC_PREFIX     — install dir (default: /usr/local/bin, falls back
#                     to ~/.local/bin if not writable and no sudo)
#   HPCC_KERNEL     — worker only: kernel version (5.10|6.1, default 6.1)
#   HPCC_FC_VERSION — worker only: firecracker tag (default v1.15.1)

set -euo pipefail

REPO="${HPCC_REPO:-aarani/hpcc}"
VERSION="${HPCC_VERSION:-latest}"
PREFIX="${HPCC_PREFIX:-}"
KERNEL="${HPCC_KERNEL:-6.1}"
FC_VERSION="${HPCC_FC_VERSION:-v1.15.1}"

# ---- TTY plumbing ----------------------------------------------------
# When run as `curl … | bash`, stdin is the pipe — read blocks against
# closed input. /dev/tty always works when a controlling terminal
# exists.
if [ ! -r /dev/tty ]; then
  echo "error: no /dev/tty (this installer is interactive — run it directly, not under nohup/CI)" >&2
  exit 1
fi

# ---- pretty output --------------------------------------------------
if [ -t 1 ]; then
  bold=$(printf '\033[1m'); dim=$(printf '\033[2m')
  cyan=$(printf '\033[36m'); red=$(printf '\033[31m'); reset=$(printf '\033[0m')
else
  bold=""; dim=""; cyan=""; red=""; reset=""
fi

say()  { printf '%s\n' "$*"; }
hd()   { printf '%s%s%s\n' "$bold" "$*" "$reset"; }
note() { printf '%s%s%s\n' "$dim" "$*" "$reset"; }
die()  { printf '%serror:%s %s\n' "$red" "$reset" "$*" >&2; exit 1; }

ask() {
  local prompt="$1" default="${2:-}" p="$1"
  [ -n "$default" ] && p="$prompt [$default]"
  printf '%s%s%s: ' "$cyan" "$p" "$reset" >/dev/tty
  local ans
  IFS= read -r ans </dev/tty
  printf '%s' "${ans:-$default}"
}

ask_required() {
  local v
  while :; do
    v=$(ask "$1")
    [ -n "$v" ] && { printf '%s' "$v"; return; }
    note "  (required — please answer)"
  done
}

# ---- platform detection ----------------------------------------------
uname_s=$(uname -s); uname_m=$(uname -m)
case "$uname_s" in
  Linux)  os=linux  ;;
  Darwin) os=darwin ;;
  *) die "unsupported OS: $uname_s (this installer supports Linux + macOS; on Windows use tools/userdata/install-worker.ps1)" ;;
esac
case "$uname_m" in
  x86_64|amd64)  arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
  *) die "unsupported arch: $uname_m" ;;
esac

# ---- install dir ----------------------------------------------------
pick_install_dir() {
  if [ -n "$PREFIX" ]; then echo "$PREFIX"; return; fi
  if [ -w /usr/local/bin ]; then echo /usr/local/bin; return; fi
  if command -v sudo >/dev/null 2>&1 && sudo -n true 2>/dev/null; then
    echo /usr/local/bin; return
  fi
  # Fall back to per-user; warn if it isn't on PATH.
  mkdir -p "$HOME/.local/bin"
  echo "$HOME/.local/bin"
}

install_bin() {
  local src="$1" dst="$2"
  if [ -w "$(dirname "$dst")" ]; then
    install -m 0755 "$src" "$dst"
  else
    sudo install -m 0755 "$src" "$dst" || die "couldn't install to $dst (no write access and sudo failed)"
  fi
}

# ---- banner ---------------------------------------------------------
clear 2>/dev/null || true
cat <<'BANNER'
 _
| |__  _ __   ___ ___
| '_ \| '_ \ / __/ __|
| | | | |_) | (_| (__
|_| |_| .__/ \___\___|
      |_|             interactive installer

BANNER

note "Platform: ${os}/${arch}"
say

# ---- resolve release tag --------------------------------------------
if [ "$VERSION" = "latest" ]; then
  hd "Resolving latest hpcc release…"
  # /releases/latest skips prereleases; the project only ships
  # prereleases today, so take the first entry from /releases (most
  # recent overall, prerelease included).
  #
  # Capture curl output first instead of piping straight into grep:
  # `grep -m1` closes stdin as soon as it matches, which races with
  # curl's writes and gives curl a SIGPIPE → non-zero exit → the
  # pipefail-enabled pipeline fails even though the data was fine.
  api="https://api.github.com/repos/${REPO}/releases"
  releases_json=$(curl -fsSL "$api") \
    || die "couldn't query GitHub releases for $REPO (network or rate-limit?)"
  VERSION=$(printf '%s' "$releases_json" \
              | grep -m1 '"tag_name":' \
              | sed 's/.*"tag_name":[[:space:]]*"\([^"]*\)".*/\1/') || true
  [ -n "$VERSION" ] || die "no releases found for $REPO"
fi
note "Using release ${VERSION}"
say

# ---- download + install hpcc ----------------------------------------
install_dir=$(pick_install_dir)
zip_url="https://github.com/${REPO}/releases/download/${VERSION}/hpcc-${VERSION}-${os}-${arch}.zip"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

hd "Downloading $zip_url"
curl -fsSL --progress-bar -o "$tmp/hpcc.zip" "$zip_url" \
  || die "download failed (does the release have a hpcc-${VERSION}-${os}-${arch}.zip asset?)"

# Prefer unzip; fall back to bsdtar if missing (Alpine, some macOS).
if command -v unzip >/dev/null 2>&1; then
  unzip -j -o "$tmp/hpcc.zip" hpcc -d "$tmp" >/dev/null
elif command -v bsdtar >/dev/null 2>&1; then
  bsdtar -xf "$tmp/hpcc.zip" -C "$tmp" hpcc
else
  die "need 'unzip' or 'bsdtar' to extract the release zip"
fi
chmod +x "$tmp/hpcc"

# macOS quarantines downloaded executables; strip the attribute so
# Gatekeeper doesn't pop a dialog on first run.
if [ "$os" = darwin ] && command -v xattr >/dev/null 2>&1; then
  xattr -d com.apple.quarantine "$tmp/hpcc" 2>/dev/null || true
fi

install_bin "$tmp/hpcc" "$install_dir/hpcc"
hd "Installed $install_dir/hpcc"

# PATH check for the per-user fallback.
case ":$PATH:" in
  *":$install_dir:"*) ;;
  *) note "warning: $install_dir is not on your PATH. Add this to your shell rc:"
     note "    export PATH=\"$install_dir:\$PATH\"" ;;
esac
say

# ---- pick role ------------------------------------------------------
# Worker is Linux-only (Firecracker needs /dev/kvm); scheduler is a
# real production service nobody sensibly stands up from a curl-piped
# laptop installer. On macOS the only sensible interactive flow is
# the client, so the menu collapses to client / skip.
hd "What would you like to set up?"
if [ "$os" = "linux" ]; then
  say "  1) Client     — local cache (optionally pointed at a scheduler)"
  say "  2) Worker     — Firecracker worker on this Linux host"
  say "  3) Scheduler  — route-only scheduler service"
  say "  s) Skip       — just leave the binary installed; configure later"
  valid_picks="1 2 3 s S"
else
  say "  1) Client     — local cache (optionally pointed at a scheduler)"
  say "  s) Skip       — just leave the binary installed; configure later"
  note "  (worker/scheduler are Linux-only; on macOS this installer only configures the client)"
  valid_picks="1 s S"
fi
say

role=""
while [ -z "$role" ]; do
  pick=$(ask "Choice" "1")
  if ! printf ' %s ' $valid_picks | grep -q " $pick "; then
    note "Pick one of: $valid_picks"
    continue
  fi
  case "$pick" in
    1) role=client ;;
    2) role=worker ;;
    3) role=scheduler ;;
    s|S) role=skip ;;
  esac
done

if [ "$role" = "skip" ]; then
  say
  hd "Done. Run \`hpcc init client|worker|scheduler --help\` when you're ready."
  exit 0
fi

say
hd "Configuring as: $role"
say

HPCC="$install_dir/hpcc"

# ---- per-role init flows --------------------------------------------
case "$role" in
  client)
    if confirm "Point this client at an hpcc scheduler for remote dispatch?" "n"; then
      sched=$(ask_required "Scheduler URL (host:port)")
      tenant=$(ask_required "Tenant ID")
      digest=$(ask_required "Image digest (sha256:...)")
      ref=$(ask "Image reference (optional, e.g. ghcr.io/...)")

      args=( init client
             --scheduler   "$sched"
             --tenant      "$tenant"
             --image-digest "$digest" )
      [ -n "$ref" ] && args+=( --image-ref "$ref" )

      "$HPCC" "${args[@]}"
      say
      hd "Next steps"
      say "  hpcc auth login    # cache an OAuth token (one-shot)"
      say "  hpcc start         # run the daemon"
    else
      # Local-only client. `hpcc init client` requires --scheduler/
      # --tenant/--image-digest; write a minimal config directly
      # instead. Mirrors init client's local-cache defaults, just
      # without the [remote] block.
      cache_dir=$(ask "Local cache directory" "$HOME/.cache/hpcc")
      cache_size=$(ask "Local cache size limit (e.g. 10G)" "10G")
      cfg_dir="$HOME/.config/hpcc"
      mkdir -p "$cfg_dir"
      cfg="$cfg_dir/config.toml"
      if [ -e "$cfg" ] && ! confirm "$cfg already exists; overwrite?" "n"; then
        die "aborted (existing config preserved)"
      fi
      cat > "$cfg" <<EOF
# hpcc client config — generated by install.sh (local-only).
# See docs/client.toml for the full annotated reference. To enable
# remote dispatch later, run \`hpcc init client --force --scheduler
# ... --tenant ... --image-digest ...\` against the same path.

source_mode = "cas"

[[cache]]
type     = "disk"
location = "$cache_dir"
max_size = "$cache_size"
EOF
      chmod 0600 "$cfg"
      hd "Wrote $cfg"
      say
      hd "Next steps"
      say "  hpcc wrap cc -c hello.c -o hello.o    # one-shot wrap"
      say "  hpcc start                            # supervise as a daemon"
    fi
    ;;

  worker)
    sched=$(ask_required "Scheduler URL (host:port)")
    token=$(ask_required "Worker token (from \`hpcc init scheduler\` output)")
    pub=$(ask_required   "Public address this worker advertises (host:port)")
    rt=$(ask "Runtime [firecracker | really_really_dangerous]" "firecracker")

    if [ "$rt" = "firecracker" ]; then
      KERNEL=$(ask "Kernel version [5.10 | 6.1]" "$KERNEL")
      case "$KERNEL" in 5.10|6.1) ;; *) die "unknown kernel version: $KERNEL" ;; esac

      # firecracker uses x86_64/aarch64 in its asset names; hpcc uses
      # amd64/arm64. Map across once.
      case "$arch" in
        amd64) fc_arch=x86_64  ;;
        arm64) fc_arch=aarch64 ;;
      esac

      say
      hd "Staging firecracker runtime prereqs"
      note "These match \`hpcc init worker\`'s default paths so the generated"
      note "config validates as-is. Sudo may be requested."
      say

      # Working dir under the existing tmp (auto-cleaned by EXIT trap).
      stage="$tmp/stage"; mkdir -p "$stage"

      # /var/lib/hpcc holds the kernel + the agent. /var/lib/hpcc/rootfs
      # is the prepared-rootfs cache. /srv/jailer is jailer's chroot
      # base. All four paths are init worker's defaults.
      sudo install -d -m 0755 /var/lib/hpcc /var/lib/hpcc/rootfs /srv/jailer

      # firecracker + jailer ----------------------------------------
      hd "  firecracker ${FC_VERSION} (${fc_arch})"
      fc_url="https://github.com/firecracker-microvm/firecracker/releases/download/${FC_VERSION}/firecracker-${FC_VERSION}-${fc_arch}.tgz"
      curl -fsSL --progress-bar -o "$stage/fc.tgz" "$fc_url" \
        || die "firecracker download failed (asset name = firecracker-${FC_VERSION}-${fc_arch}.tgz)"
      tar -xzf "$stage/fc.tgz" -C "$stage"
      sudo install -m 0755 "$stage/release-${FC_VERSION}-${fc_arch}/firecracker-${FC_VERSION}-${fc_arch}" /usr/bin/firecracker
      sudo install -m 0755 "$stage/release-${FC_VERSION}-${fc_arch}/jailer-${FC_VERSION}-${fc_arch}"      /usr/bin/jailer

      # microvm kernel ----------------------------------------------
      hd "  vmlinux ${KERNEL} (${arch})"
      sudo curl -fsSL --progress-bar -o /var/lib/hpcc/vmlinux \
        "https://github.com/${REPO}/releases/download/${VERSION}/vmlinux-${KERNEL}-${arch}" \
        || die "kernel download failed (asset = vmlinux-${KERNEL}-${arch})"
      sudo chmod 0644 /var/lib/hpcc/vmlinux

      # in-VM agent --------------------------------------------------
      hd "  hpcc-agent-linux-${arch}"
      sudo curl -fsSL --progress-bar -o "/var/lib/hpcc/hpcc-agent-linux-${arch}" \
        "https://github.com/${REPO}/releases/download/${VERSION}/hpcc-agent-linux-${arch}" \
        || die "agent download failed"
      sudo chmod 0755 "/var/lib/hpcc/hpcc-agent-linux-${arch}"
      say
    fi

    "$HPCC" init worker \
      --scheduler  "$sched" \
      --token      "$token" \
      --public-addr "$pub" \
      --runtime    "$rt"

    say
    hd "Next steps"
    if [ "$rt" = "firecracker" ]; then
      note "init worker's defaults assume uid=1000/gid=1000 for jailer — if a different"
      note "user holds 1000 on this host (e.g. ec2-user on AL2023), edit [runtime.firecracker]"
      note "uid/gid in the generated config to a user that can read /dev/kvm."
    fi
    say "  hpcc worker"
    ;;

  scheduler)
    cert=$(ask "Path to TLS cert (PEM)" "/etc/hpcc/scheduler.crt")
    key=$(ask  "Path to TLS key (PEM)"  "/etc/hpcc/scheduler.key")
    [ -f "$cert" ] || note "warning: $cert does not exist yet — create or replace it before \`hpcc scheduler\`"
    [ -f "$key"  ] || note "warning: $key does not exist yet"

    tenant=$(ask_required  "Tenant ID")
    issuer=$(ask_required  "Issuer URL")
    jwks=$(ask_required    "JWKS URL")
    tok_url=$(ask_required "Token URL")
    aud=$(ask              "Audience" "hpcc")

    "$HPCC" init scheduler \
      --cert-file "$cert" --key-file "$key" \
      --tenant-id "$tenant" --issuer "$issuer" --jwks-url "$jwks" \
      --token-url "$tok_url" --audience "$aud"

    say
    hd "Next steps"
    say "  hpcc scheduler"
    note "Copy the worker_token printed above onto each worker host."
    ;;
esac
