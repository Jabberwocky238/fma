#!/bin/bash
set -euo pipefail

systemd=false
uninstall=false
generated=
while (($#)); do
    case "$1" in
        --uninstall) uninstall=true; shift ;;
        --systemd) systemd=true; shift ;;
        --config-dir) [[ $# -ge 2 ]] || { printf 'Missing --config-dir value\n' >&2; exit 1; }; generated=$2; shift 2 ;;
        -h|--help) printf 'Usage: install.sh [--systemd [--config-dir deploy/generated]] | --uninstall\n'; exit 0 ;;
        *) printf 'Unknown argument: %s\n' "$1" >&2; exit 1 ;;
    esac
done
[[ -z "$generated" || "$systemd" == true ]] || { printf '--config-dir requires --systemd\n' >&2; exit 1; }

# Override FMA_REPO only when installing from a fork.
repo=${FMA_REPO:-Jabberwocky238/fma}
[[ "$repo" =~ ^[a-zA-Z0-9_.-]+/[a-zA-Z0-9_.-]+$ ]] || { printf 'Invalid GitHub repository.\n' >&2; exit 1; }
ctl=(systemctl)
if [[ $(id -u) == 0 ]]; then
    mode=root
    default_bin=/usr/local/bin
    default_config=/etc/fma
    default_units=/etc/systemd/system
else
    mode=user
    default_bin=$HOME/.local/bin
    default_config=$HOME/.config/fma
    default_units=$HOME/.config/systemd/user
    ctl+=(--user)
fi
printf 'WARNING: %s mode (UID %s); binary: %s; config: %s; systemd: %s\n' "$mode" "$(id -u)" "$default_bin" "$default_config" "$default_units"
case "$(uname -s)" in
    Linux) platform=linux; binary_name=fma; archive_ext=tar.gz; extractor=tar ;;
    Darwin) platform=darwin; binary_name=fma; archive_ext=tar.gz; extractor=tar ;;
    MINGW*|MSYS*|CYGWIN*|Windows_NT) platform=windows; binary_name=fma.exe; archive_ext=zip; extractor=unzip ;;
    *) printf 'Unsupported operating system.\n' >&2; exit 1 ;;
esac
install_dir=${FMA_INSTALL_DIR:-$default_bin}
service_config=$default_config
service_units=$default_units
deploy_dir=${FMA_DEPLOY_DIR:-$default_config/deploy}
manifest=$default_config/install.paths
valid_path() { [[ "$1" == /* && "$1/" != *'/../'* && "$1" != / && "$1" != *$'\n'* && "$1" != *$'\r'* ]]; }
if [[ "$uninstall" == true ]]; then
    [[ "$systemd" == false && -z "$generated" ]] || { printf '--uninstall cannot be combined with install options.\n' >&2; exit 1; }
    if [[ -f "$manifest" && ! -L "$manifest" ]]; then
        { IFS= read -r install_dir; IFS= read -r service_config; IFS= read -r service_units; IFS= read -r deploy_dir; } < "$manifest"
    fi
    for value in "$install_dir" "$service_config" "$service_units" "$deploy_dir"; do
        valid_path "$value" || { printf 'Invalid uninstall path.\n' >&2; exit 1; }
    done
    printf 'Removing %s/%s, %s/fma.service and fma configuration from %s\n' "$install_dir" "$binary_name" "$service_units" "$service_config"
    if [[ -e "$service_units/fma.service" || -L "$service_units/fma.service" ]]; then
        "${ctl[@]}" disable --now fma.service
        rm -f -- "$service_units/fma.service"
        "${ctl[@]}" daemon-reload
    fi
    if [[ "$mode" == root && -L /bin/fma && "$(readlink /bin/fma)" == "$install_dir/$binary_name" ]]; then
        rm -f -- /bin/fma
    fi
    rm -f -- "$install_dir/$binary_name" "$service_config/s3.env" "$service_config/outbound.env" "$manifest"
    # Remove only fma's generated artifacts, preserving unrelated files.
    for name in install.mk fma.service s3.env outbound.env nginx-http.conf nginx-https.conf nginx-stream.conf renew-hook.sh; do
        rm -f -- "$deploy_dir/generated/$name" "$deploy_dir/template/$name.tmpl"
    done
    rm -f -- "$deploy_dir/gen.sh"
    rmdir -- "$deploy_dir/generated" "$deploy_dir/template" "$deploy_dir" "$service_config" "$default_config" 2>/dev/null || true
    printf 'Uninstalled fma in %s mode. S3 data is preserved.\n' "$mode"
    exit 0
fi
binary=$install_dir/$binary_name
for tool in curl "$extractor" mktemp; do
    command -v "$tool" >/dev/null || { printf 'Required command not found: %s\n' "$tool" >&2; exit 1; }
done
if [[ "$systemd" == true ]]; then
    [[ "$platform" == linux ]] || { printf '--systemd requires Linux.\n' >&2; exit 1; }
    command -v systemctl >/dev/null || { printf 'systemctl is required.\n' >&2; exit 1; }
    "${ctl[@]}" show-environment >/dev/null
fi
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

work=$(mktemp -d)
staged=
trap 'rm -rf -- "$work"; [[ -z "$staged" ]] || rm -f -- "$staged"' EXIT
downloaded=false
fetch_release() {
    [[ "$downloaded" == false ]] || return 0
archive=fma_${latest}_${platform}_${arch}.${archive_ext}
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
downloaded=true
}
install_service() {
    [[ "$systemd" == true ]] || return 0
    install -d -m 700 "$default_config" "$service_config"
    (umask 077; printf '%s\n' "$install_dir" "$service_config" "$service_units" "$deploy_dir" > "$manifest")
    install -d "$service_units"
    install -m 600 "$generated/s3.env" "$service_config/s3.env"
    install -m 600 "$generated/outbound.env" "$service_config/outbound.env"
    install -m 644 "$generated/fma.service" "$service_units/fma.service"
    "${ctl[@]}" daemon-reload
    "${ctl[@]}" enable fma.service
    "${ctl[@]}" restart fma.service
    printf 'Installed and started %s systemd service fma.service.\n' "$mode"
}
if [[ "$systemd" == true ]]; then
    if [[ -z "$generated" ]]; then
        fetch_release
        tar -xzf "$work/$archive" -C "$work" deploy/gen.sh deploy/template
        install -d -m 700 "$deploy_dir" "$deploy_dir/template"
        install -m 700 "$work/deploy/gen.sh" "$deploy_dir/gen.sh"
        cp "$work"/deploy/template/*.tmpl "$deploy_dir/template/"
        # A piped installer must not consume its own source as interactive input.
        if [[ -t 0 ]]; then
            bash "$deploy_dir/gen.sh"
        elif [[ -r /dev/tty ]] && (exec </dev/tty) 2>/dev/null; then
            bash "$deploy_dir/gen.sh" </dev/tty
        else
            printf 'Interactive configuration needs a terminal; supply --config-dir instead.\n' >&2
            exit 1
        fi
        generated=$deploy_dir/generated
    fi
    for file in install.mk fma.service s3.env outbound.env; do
        [[ -s "$generated/$file" ]] || { printf '配置文件没有找到: %s/%s\n' "$generated" "$file" >&2; exit 1; }
    done
    setting() { awk -v key="$1" '$1 == key && $2 == ":=" {print $3}' "$generated/install.mk"; }
    [[ "$(setting DEPLOY_USER)" == "$(id -un)" && "$(setting DEPLOY_UID)" == "$(id -u)" ]] || { printf 'Run as the user selected in deploy/gen.sh.\n' >&2; exit 1; }
    install_dir=$(setting BINDIR)
    service_config=$(setting CONFIG_DIR)
    service_units=$(setting SYSTEMD_USER_DIR)
    for value in "$install_dir" "$service_config" "$service_units"; do
        [[ "$value" =~ ^/[a-zA-Z0-9_./-]+$ && "$value" != *'/../'* ]] || { printf 'Invalid generated installation path.\n' >&2; exit 1; }
    done
    binary=$install_dir/$binary_name
fi

install_link() {
    [[ "$mode" == root ]] || return 0
    if [[ -e /bin/fma || -L /bin/fma ]]; then
        [[ -L /bin/fma && "$(readlink /bin/fma)" == "$binary" ]] || { printf '/bin/fma belongs to another installation; refusing to replace it.\n' >&2; exit 1; }
    else
        ln -s "$binary" /bin/fma
    fi
}
# Check collisions before replacing the binary.
if [[ "$mode" == root && ( -e /bin/fma || -L /bin/fma ) ]]; then
    [[ -L /bin/fma && "$(readlink /bin/fma)" == "$binary" ]] || { printf '/bin/fma already exists and is not our symlink.\n' >&2; exit 1; }
fi

if [[ -e "$binary" || -L "$binary" ]]; then
    current=$({ "$binary" --version || true; } 2>/dev/null)
    current=${current%%$'\n'*}
    current=${current#fma }
    if up_to_date "$current" "$latest"; then
        printf 'fma %s is already installed at %s; no update needed.\n' "$current" "$binary"
        install_link
        install_service
        exit 0
    fi
    printf 'fma is already installed (%s). Update to %s? [y/N]: ' "${current:-unknown version}" "$tag" >&2
    answer=
    if [[ -t 0 ]]; then
        IFS= read -r answer || true
    elif [[ -r /dev/tty ]] && (exec </dev/tty) 2>/dev/null; then
        IFS= read -r answer </dev/tty || true
    elif [[ -n "${BASH_SOURCE[0]:-}" ]]; then
        IFS= read -r answer || true
    fi
    case "$answer" in
        y|Y) ;;
        *) printf 'Existing installation preserved.\n'; exit 0 ;;
    esac
fi

fetch_release
# Extract only the binary, not paths supplied by other archive entries.
if [[ "$platform" == windows ]]; then
    unzip -q "$work/$archive" "$binary_name" -d "$work"
else
    tar -xzf "$work/$archive" -C "$work" "$binary_name"
fi
[[ -f "$work/$binary_name" && ! -L "$work/$binary_name" ]] || { printf 'Archive has no regular fma binary.\n' >&2; exit 1; }
chmod 755 "$work/$binary_name"
reported=$("$work/$binary_name" --version)
[[ "${reported%%$'\n'*}" == "fma $latest" ]] || { printf 'Binary version does not match the release.\n' >&2; exit 1; }
mkdir -p "$install_dir"
staged=$(mktemp "$install_dir/.fma-install.XXXXXX")
cp "$work/$binary_name" "$staged"
chmod 755 "$staged"
mv -f "$staged" "$binary"
staged=
if [[ "$systemd" == false && ! -e "$manifest" ]]; then
    install -d -m 700 "$default_config"
    (umask 077; printf '%s\n' "$install_dir" "$service_config" "$service_units" "$deploy_dir" > "$manifest")
fi
printf 'Installed fma %s at %s\n'  "$latest" "$binary"
case ":$PATH:" in
    *":$install_dir:"*) ;;
    *) printf 'Add %s to PATH to run fma by name.\n' "$install_dir" ;;
esac

install_link
install_service
