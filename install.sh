#!/bin/bash
set -euo pipefail

# Override FMA_REPO only when installing from a fork.
repo=${FMA_REPO:-Jabberwocky238/fma}
[[ "$repo" =~ ^[a-zA-Z0-9_.-]+/[a-zA-Z0-9_.-]+$ ]] || { printf 'Invalid GitHub repository.\n' >&2; exit 1; }
install_dir=${FMA_INSTALL_DIR:-$HOME/.local/bin}
binary=$install_dir/fma
for tool in curl tar mktemp; do
    command -v "$tool" >/dev/null || { printf 'Required command not found: %s\n' "$tool" >&2; exit 1; }
done
case "$(uname -s)" in
    Linux) platform=linux ;;
    Darwin) platform=darwin ;;
    *) printf 'This installer supports Linux and macOS. Download Windows ZIPs from Releases.\n' >&2; exit 1 ;;
esac
case "$(uname -m)" in
    x86_64|amd64) arch=amd64 ;;
    aarch64|arm64) arch=arm64 ;;
    *) printf 'Unsupported CPU architecture.\n' >&2; exit 1 ;;
esac

latest_url=$(curl --proto '=https' --proto-redir '=https' --fail --silent --show-error --location --retry 3 \
    --connect-timeout 10 --max-time 60 --output /dev/null --write-out '%{url_effective}' \
    "https://github.com/$repo/releases/latest")
tag=${latest_url##*/}
[[ "$tag" =~ ^v([0-9]{1,9})\.([0-9]{1,9})\.([0-9]{1,9})$ ]] || { printf 'No stable vMAJOR.MINOR.PATCH release found.\n' >&2; exit 1; }
latest=${tag#v}

# Return success if the installed semantic version is at least the latest stable.
up_to_date() {
    local current=${1#v} target=$2 i
    local current_parts target_parts
    [[ "$current" =~ ^[0-9]{1,9}\.[0-9]{1,9}\.[0-9]{1,9}([-+][a-zA-Z0-9.+-]+)?$ ]] || return 1
    IFS=. read -r -a current_parts <<< "${current%%[-+]*}"
    IFS=. read -r -a target_parts <<< "$target"
    for i in 0 1 2; do
        ((10#${current_parts[i]} > 10#${target_parts[i]})) && return 0
        ((10#${current_parts[i]} < 10#${target_parts[i]})) && return 1
    done
    [[ "$current" != *-* ]]
}

if [[ -e "$binary" || -L "$binary" ]]; then
    current=$({ "$binary" --version || true; } 2>/dev/null)
    current=${current%%$'\n'*}
    current=${current#fma }
    if up_to_date "$current" "$latest"; then
        printf 'fma %s is already installed at %s; no update needed.\n' "$current" "$binary"
        exit 0
    fi
    printf 'fma is already installed (%s). Update to %s? [y/N]: ' "${current:-unknown version}" "$tag" >&2
    answer=
    IFS= read -r answer || true
    case "$answer" in
        y|Y) ;;
        *) printf 'Existing installation preserved.\n'; exit 0 ;;
    esac
fi

work=$(mktemp -d)
staged=
trap 'rm -rf -- "$work"; [[ -z "$staged" ]] || rm -f -- "$staged"' EXIT
archive=fma_${latest}_${platform}_${arch}.tar.gz
base=https://github.com/$repo/releases/download/$tag
for asset in "$archive" checksums.txt; do
    curl --proto '=https' --proto-redir '=https' --fail --silent --show-error --location --retry 3 \
        --connect-timeout 10 --max-time 300 "$base/$asset" --output "$work/$asset"
done
expected=$(awk -v name="$archive" '$2 == name {print $1}' "$work/checksums.txt")
[[ "$expected" =~ ^[a-fA-F0-9]{64}$ ]] || { printf 'Missing or invalid checksum.\n' >&2; exit 1; }
if command -v sha256sum >/dev/null; then
    actual=$(sha256sum "$work/$archive")
elif command -v shasum >/dev/null; then
    actual=$(shasum -a 256 "$work/$archive")
else
    printf 'sha256sum or shasum is required.\n' >&2; exit 1
fi
[[ "${actual%% *}" == "$expected" ]] || { printf 'SHA-256 mismatch; installation unchanged.\n' >&2; exit 1; }
# Extract only the binary, not paths supplied by other archive entries.
tar -xzf "$work/$archive" -C "$work" fma
[[ -f "$work/fma" && ! -L "$work/fma" ]] || { printf 'Archive has no regular fma binary.\n' >&2; exit 1; }
chmod 755 "$work/fma"
reported=$("$work/fma" --version)
[[ "${reported%%$'\n'*}" == "fma $latest" ]] || { printf 'Binary version does not match the release.\n' >&2; exit 1; }
mkdir -p "$install_dir"
staged=$(mktemp "$install_dir/.fma-install.XXXXXX")
cp "$work/fma" "$staged"
chmod 755 "$staged"
mv -f "$staged" "$binary"
staged=
printf 'Installed fma %s at %s\n' "$latest" "$binary"
case ":$PATH:" in
    *":$install_dir:"*) ;;
    *) printf 'Add %s to PATH to run fma by name.\n' "$install_dir" ;;
esac
