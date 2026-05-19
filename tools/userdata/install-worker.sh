#!/usr/bin/env bash
# AWS EC2 userdata — installs and supervises an hpcc worker on RHEL 8/9
# or Amazon Linux 2023 with the Firecracker runtime. Paste verbatim
# into the userdata field; edit the parameter block first.
#
# Everything below is glue around `hpcc init worker` plus the standard
# install paths the init command's template defaults to — the binary,
# kernel, and in-VM agent all come from the matching hpcc release.
#
# Requires /dev/kvm on the host (bare-metal *.metal instance types).
set -euo pipefail

# ---- parameters -------------------------------------------------------
HPCC_VERSION="v0.1.0-alpha"     # hpcc release tag to install
HPCC_REPO="aarani/hpcc"
ARCH="amd64"                    # amd64 | arm64
KERNEL="6.1"                    # 5.10 | 6.1  (both ship per release)
FC_VERSION="v1.15.1"            # firecracker release tag

# Scheduler pairing — paste the worker_token printed by
# `hpcc init scheduler` on the scheduler host.
SCHEDULER_URL="scheduler.internal:9091"
WORKER_TOKEN="replace-with-a-long-random-string"

# Listening. PUBLIC_ADDR is auto-discovered via IMDSv2 when empty.
PUBLIC_ADDR=""
LISTEN=":9092"
METRICS_LISTEN=":9192"

# Per-tenant VM sizing. Tune per instance type; `hpcc init worker`'s
# defaults (2GB / 4 vCPUs / 32 pool) are conservative.
VM_MEMORY="2GB"
VM_VCPUS=4
POOL_MAX_ACTIVE=32

# ---- prereqs ----------------------------------------------------------
dnf install -y curl tar unzip

install -d -m 0755 /var/lib/hpcc /var/lib/hpcc/rootfs /srv/jailer /etc/hpcc

# ---- hpcc binary ------------------------------------------------------
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
curl -fsSL -o "$tmp/hpcc.zip" \
  "https://github.com/${HPCC_REPO}/releases/download/${HPCC_VERSION}/hpcc-${HPCC_VERSION}-linux-${ARCH}.zip"
# -j drops the zip's directory prefix (the release packs hpcc at the
# root of the zip already, so -j is belt-and-suspenders).
unzip -j -o "$tmp/hpcc.zip" hpcc -d /usr/local/bin
chmod +x /usr/local/bin/hpcc

# ---- microvm kernel (ships standalone with every hpcc release) -------
curl -fsSL -o /var/lib/hpcc/vmlinux \
  "https://github.com/${HPCC_REPO}/releases/download/${HPCC_VERSION}/vmlinux-${KERNEL}-${ARCH}"
chmod 0644 /var/lib/hpcc/vmlinux

# ---- in-VM agent (also a standalone release artifact) ----------------
curl -fsSL -o "/var/lib/hpcc/hpcc-agent-linux-${ARCH}" \
  "https://github.com/${HPCC_REPO}/releases/download/${HPCC_VERSION}/hpcc-agent-linux-${ARCH}"
chmod 0755 "/var/lib/hpcc/hpcc-agent-linux-${ARCH}"

# ---- firecracker + jailer --------------------------------------------
# firecracker uses x86_64/aarch64 in its tar names; hpcc uses
# amd64/arm64. Map across once at the top.
case "$ARCH" in
  amd64) FC_ARCH="x86_64"  ;;
  arm64) FC_ARCH="aarch64" ;;
  *) echo "unknown ARCH=$ARCH" >&2; exit 1 ;;
esac
curl -fsSL "https://github.com/firecracker-microvm/firecracker/releases/download/${FC_VERSION}/firecracker-${FC_VERSION}-${FC_ARCH}.tgz" \
  | tar -xz -C "$tmp"
install -m 0755 "$tmp/release-${FC_VERSION}-${FC_ARCH}/firecracker-${FC_VERSION}-${FC_ARCH}" /usr/bin/firecracker
install -m 0755 "$tmp/release-${FC_VERSION}-${FC_ARCH}/jailer-${FC_VERSION}-${FC_ARCH}"      /usr/bin/jailer

# ---- jailer uid/gid (needs r/w on /dev/kvm) --------------------------
# RHEL/AL2023 ship the kvm group out of the box; this is idempotent so
# re-running the userdata on existing state is safe.
getent group  kvm  >/dev/null || groupadd -r kvm
getent passwd hpcc >/dev/null || useradd  -r -g kvm -d /var/lib/hpcc -s /sbin/nologin hpcc
HPCC_UID="$(id -u hpcc)"
HPCC_GID="$(getent group kvm | cut -d: -f3)"
chown -R hpcc:kvm /var/lib/hpcc /srv/jailer

# ---- public_addr via IMDSv2 if not set -------------------------------
if [[ -z "$PUBLIC_ADDR" ]]; then
  TOK="$(curl -fsS -X PUT 'http://169.254.169.254/latest/api/token' \
            -H 'X-aws-ec2-metadata-token-ttl-seconds: 60')"
  IP="$(curl -fsS -H "X-aws-ec2-metadata-token: $TOK" \
            http://169.254.169.254/latest/meta-data/local-ipv4)"
  PUBLIC_ADDR="${IP}${LISTEN}"
fi

# ---- generate worker.toml + TLS material via `hpcc init worker` -----
# Writes /etc/hpcc/worker.toml with the standard firecracker defaults
# (kernel at /var/lib/hpcc/vmlinux, agent at
# /var/lib/hpcc/hpcc-agent-linux-<arch>, firecracker + jailer at
# /usr/bin), mints a self-signed leaf at /etc/hpcc/worker.{crt,key},
# and runs Validate() before returning.
hpcc init worker \
  --config "/etc/hpcc/worker.toml" \
  --force \
  --scheduler "$SCHEDULER_URL" \
  --token "$WORKER_TOKEN" \
  --public-addr "$PUBLIC_ADDR" \
  --listen "$LISTEN" \
  --metrics-listen "$METRICS_LISTEN" \
  --runtime firecracker

# ---- patch fields init worker can't parameterize today --------------
# init worker hardcodes uid=1000/gid=1000 + a fixed VM/pool default
# block. We patch the file with our actual hpcc:kvm uid/gid (1000 is
# typically already taken by ec2-user on AL2023) and the operator-
# tunable sizing knobs at the top of this script.
sed -i -E \
  -e "s|^uid[[:space:]]+= [0-9]+|uid             = ${HPCC_UID}|" \
  -e "s|^gid[[:space:]]+= [0-9]+|gid             = ${HPCC_GID}|" \
  -e "s|^memory[[:space:]]+= \".*\"|memory          = \"${VM_MEMORY}\"|" \
  -e "s|^vcpus[[:space:]]+= [0-9]+|vcpus           = ${VM_VCPUS}|" \
  -e "s|^max_active = [0-9]+|max_active = ${POOL_MAX_ACTIVE}|" \
  /etc/hpcc/worker.toml

# ---- systemd unit ----------------------------------------------------
cat >/etc/systemd/system/hpcc-worker.service <<'UNIT'
[Unit]
Description=hpcc worker
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=/usr/local/bin/hpcc worker --config /etc/hpcc/worker.toml
Restart=always
RestartSec=5
LimitNOFILE=65536
# jailer needs CAP_SYS_ADMIN + access to /dev/kvm; simplest is root,
# and jailer itself drops to the uid/gid in worker.toml before
# exec'ing firecracker.
User=root

[Install]
WantedBy=multi-user.target
UNIT

systemctl daemon-reload
systemctl enable --now hpcc-worker.service
