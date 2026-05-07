#!/bin/sh
# Read-only probe executed on each target node. It must:
#   * never modify state
#   * be portable across busybox/ash, bash, and dash
#   * print exclusively key=value lines to stdout, one per line
#
# The keys are parsed by internal/bootstrap/probe.go::Parse.
set -u

emit() { printf '%s=%s\n' "$1" "$2"; }

# --- distro / version ---
distro=""; version=""
if [ -r /etc/os-release ]; then
  # shellcheck disable=SC1091
  . /etc/os-release
  distro="${ID:-unknown}"
  version="${VERSION_ID:-}"
fi
emit distro "$distro"
emit version "$version"

# --- arch / kernel ---
arch="$(uname -m 2>/dev/null || echo unknown)"
case "$arch" in
  x86_64) arch=amd64 ;;
  aarch64) arch=arm64 ;;
  armv7l) arch=armv7 ;;
esac
emit arch "$arch"
emit kernel "$(uname -r 2>/dev/null || echo unknown)"

# --- package manager ---
pkg=""
for c in apt-get dnf yum zypper pacman apk; do
  if command -v "$c" >/dev/null 2>&1; then
    case "$c" in
      apt-get) pkg=apt ;;
      *) pkg="$c" ;;
    esac
    break
  fi
done
emit pkg "$pkg"

# --- selinux ---
selinux=""
if command -v getenforce >/dev/null 2>&1; then
  selinux="$(getenforce 2>/dev/null || true)"
fi
emit selinux "$selinux"

# --- apparmor ---
apparmor=false
if [ -d /sys/kernel/security/apparmor ] && [ -e /sys/kernel/security/apparmor/profiles ]; then
  apparmor=true
fi
emit apparmor "$apparmor"

# --- firewall ---
fw=""
if command -v firewall-cmd >/dev/null 2>&1 && firewall-cmd --state >/dev/null 2>&1; then
  fw=firewalld
elif command -v ufw >/dev/null 2>&1 && ufw status 2>/dev/null | grep -qi "Status: active"; then
  fw=ufw
elif command -v nft >/dev/null 2>&1 && nft list ruleset 2>/dev/null | grep -q '^table'; then
  fw=nftables
elif command -v iptables >/dev/null 2>&1 && iptables -L -n 2>/dev/null | grep -q '^Chain'; then
  fw=iptables
fi
emit firewall "$fw"

# --- existing software ---
has_k3s=false; k3s_version=""
if command -v k3s >/dev/null 2>&1; then
  has_k3s=true
  k3s_version="$(k3s --version 2>/dev/null | head -n1 | awk '{print $3}')"
fi
emit has_k3s "$has_k3s"
emit k3s_version "$k3s_version"

has_containerd=false
command -v containerd >/dev/null 2>&1 && has_containerd=true
emit has_containerd "$has_containerd"

has_docker=false
command -v docker >/dev/null 2>&1 && has_docker=true
emit has_docker "$has_docker"

# --- swap ---
swap=false
if [ -r /proc/swaps ]; then
  if [ "$(wc -l < /proc/swaps)" -gt 1 ]; then swap=true; fi
fi
emit swap "$swap"

# --- cgroup v2 ---
cgv2=false
if [ -r /proc/mounts ] && grep -q 'cgroup2 ' /proc/mounts; then
  cgv2=true
fi
emit cgroupv2 "$cgv2"

# --- kernel modules / built-ins ---
mod_br=false
if lsmod 2>/dev/null | awk '{print $1}' | grep -qx br_netfilter \
   || [ -d /sys/module/br_netfilter ]; then
  mod_br=true
fi
emit mod_br_netfilter "$mod_br"

mod_overlay=false
if lsmod 2>/dev/null | awk '{print $1}' | grep -qx overlay \
   || [ -d /sys/module/overlay ]; then
  mod_overlay=true
fi
emit mod_overlay "$mod_overlay"

# --- resources ---
free_mb=0
if command -v df >/dev/null 2>&1; then
  free_mb="$(df -Pm /var/lib 2>/dev/null | awk 'NR==2 {print $4+0}')"
  [ -z "$free_mb" ] && free_mb=0
fi
emit free_disk_mb "$free_mb"

mem_mb=0
if [ -r /proc/meminfo ]; then
  kb="$(awk '/^MemAvailable:/ {print $2; exit}' /proc/meminfo)"
  [ -z "$kb" ] && kb="$(awk '/^MemFree:/ {print $2; exit}' /proc/meminfo)"
  if [ -n "$kb" ]; then mem_mb=$((kb / 1024)); fi
fi
emit free_memory_mb "$mem_mb"

cpus=1
if [ -r /proc/cpuinfo ]; then
  cpus="$(grep -c '^processor' /proc/cpuinfo)"
fi
emit cpu_count "$cpus"

# --- privilege ---
is_root=false
[ "$(id -u)" = "0" ] && is_root=true
emit is_root "$is_root"

sudo_nopass=false
if [ "$is_root" = false ] && command -v sudo >/dev/null 2>&1; then
  if sudo -n true 2>/dev/null; then sudo_nopass=true; fi
fi
emit sudo_nopass "$sudo_nopass"
