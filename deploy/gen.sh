#!/bin/bash
set -euo pipefail
umask 077
non_interactive=false
case "${1-}" in
    '') ;;
    --non-interactive) non_interactive=true; shift ;;
    *) printf 'Usage: gen.sh [--non-interactive]\n' >&2; exit 1 ;;
esac
[[ $# == 0 ]] || { printf 'Unexpected arguments.\n' >&2; exit 1; }
root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
staging=
if [[ $(id -u) == 0 ]]; then
    printf 'WARNING: root mode; system service with defaults /etc/fma and /etc/systemd/system.\n'
else
    printf 'WARNING: user mode; configuration and systemd user service default to your home directory.\n'
fi
trap '[[ -z "$staging" ]] || rm -rf -- "$staging"' EXIT

ask() {
    local name=$1 label=$2 fallback=${3-} secret=${4-false} input env_name automated=false
    case "$name" in
        LOG_LEVEL) env_name=LOG_LEVEL ;;
        ACCESS_KEY) env_name=FMA_S3_ACCESS_KEY_ID ;;
        SECRET_KEY) env_name=FMA_S3_SECRET_ACCESS_KEY ;;
        SESSION_TOKEN) env_name=FMA_S3_SESSION_TOKEN ;;
        OUTBOUND) env_name=FMA_OUTBOUND_MODE ;;
        RELAY_PASSWORD_KEY) env_name=FMA_RELAY_PASSWORD_FILE ;;
        RELAY_CA_KEY) env_name=FMA_RELAY_CA_FILE ;;
        CERT_KEY) env_name=FMA_CERT_KEY ;;
        KEY_KEY) env_name=FMA_KEY_KEY ;;
        overwrite) env_name=FMA_OVERWRITE ;;
        *) env_name=FMA_$name ;;
    esac
    if [[ ${!env_name+x} ]]; then
        input=${!env_name}
        automated=true
        printf 'Using %s for %s.\n' "$env_name" "$name"
    elif [[ "$non_interactive" == true ]]; then
        input=$fallback
        automated=true
    fi
    while true; do
        if [[ "$automated" == false ]]; then
            printf '%s' "$label" >&2
            [[ -z "$fallback" ]] || printf ' [%s]' "$fallback" >&2
            printf ': ' >&2
            if [[ "$secret" == true && -t 0 ]]; then
                IFS= read -r -s input || { printf '\nInput cancelled.\n' >&2; exit 1; }
                printf '\n' >&2
            else
                IFS= read -r input || { printf '\nInput cancelled.\n' >&2; exit 1; }
            fi
            input=${input:-$fallback}
        fi
        if [[ "$secret" != true ]]; then
            input="${input#"${input%%[![:space:]]*}"}"
            input="${input%"${input##*[![:space:]]}"}"
        fi
        if [[ "$automated" == false ]]; then input=${input:-$fallback}; fi
        case "$name" in DOMAIN|OUTBOUND|RELAY_TLS|LOG_LEVEL) input=$(printf '%s' "$input" | tr '[:upper:]' '[:lower:]') ;; esac
        if [[ "$input" == *$'\r'* || "$input" == *$'\n'* ]]; then
            printf 'Newlines are not allowed in %s.\n' "$env_name" >&2
            [[ "$automated" == false ]] || exit 1
            continue
        fi
        if ! valid_input "$name" "$input"; then
            if [[ "$automated" == true ]]; then
                printf 'Invalid or missing %s. %s\n' "$env_name" "$input_hint" >&2
                exit 1
            fi
            printf 'Invalid %s. %s Please try again.\n' "$name" "$input_hint" >&2
            continue
        fi
        printf -v "$name" '%s' "$input"
        return
    done
}
required() { [[ -n "${!1}" ]] || { printf '%s is required.\n' "$1" >&2; exit 1; }; }
safe_path() {
    [[ "${!1}" =~ ^/[a-zA-Z0-9_./-]+$ && "${!1}" != *'/../'* ]] || { printf '%s must be an absolute path using letters, digits, /, ., _, or -.\n' "$1" >&2; exit 1; }
}
safe_key() {
    [[ "${!1}" =~ ^[a-zA-Z0-9_./-]+$ && "${!1}" != /* && "${!1}" != *'..'* ]] || { printf '%s must be a relative object key using letters, digits, /, ., _, or -.\n' "$1" >&2; exit 1; }
}
valid_port() { [[ "$1" =~ ^[0-9]{1,5}$ ]] && ((10#$1 >= 1 && 10#$1 <= 65535)); }
valid_ipv4() {
    local part parts
    [[ "$1" =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$ ]] || return 1
    IFS=. read -r -a parts <<< "$1"
    for part in "${parts[@]}"; do
        [[ "$part" =~ ^[0-9]{1,3}$ ]] && ((10#$part <= 255)) || return 1
    done
}
valid_domain() {
    local part parts
    [[ ${#1} -le 253 && "$1" == *.* && "$1" != *..* && "$1" != *. && ! "$1" =~ ^[0-9.]+$ ]] || return 1
    IFS=. read -r -a parts <<< "$1"
    for part in "${parts[@]}"; do
        [[ "$part" =~ ^[a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?$ ]] || return 1
    done
}
valid_ipv6() {
    local value=$1 tail part parts count=0 compressed=false
    [[ "$value" == *:* && "$value" != *:::* ]] || return 1
    [[ "$value" != :* || "$value" == ::* ]] || return 1
    [[ "$value" != *: || "$value" == *:: ]] || return 1
    if [[ "$value" == *.* ]]; then
        tail=${value##*:}; valid_ipv4 "$tail" || return 1
        value=${value%:*}:0:0
    fi
    [[ "$value" =~ ^[a-fA-F0-9:]+$ ]] || return 1
    if [[ "$value" == *::* ]]; then
        compressed=true
        tail=${value#*::}; [[ "$tail" != *::* ]] || return 1
    else
        [[ "$value" != :* && "$value" != *: ]] || return 1
    fi
    IFS=: read -r -a parts <<< "$value"
    for part in "${parts[@]}"; do
        [[ -n "$part" ]] || continue
        [[ "$part" =~ ^[a-fA-F0-9]{1,4}$ ]] || return 1
        count=$((count + 1))
    done
    if [[ "$compressed" == true ]]; then ((count < 8)); else ((count == 8)); fi
}
valid_host() {
    [[ "$1" == localhost ]] || valid_ipv4 "$1" || valid_domain "$1"
}
valid_authority() {
    local value=$1 required_port=$2 host port=
    if [[ "$value" == \[* ]]; then
        [[ "$value" == *\]* ]] || return 1
        host=${value#\[}; host=${host%%\]*}
        valid_ipv6 "$host" || return 1
        value=${value#*\]}
        [[ -z "$value" || "$value" == :* ]] || return 1
        port=${value#:}
        [[ "$value" != : ]] || return 1
    else
        host=${value%%:*}; valid_host "$host" || return 1
        if [[ "$value" == *:* ]]; then port=${value#*:}; [[ -n "$port" ]] || return 1; fi
    fi
    if [[ -n "$port" ]]; then valid_port "$port"; else [[ "$required_port" == false ]]; fi
}
valid_input() {
    local name=$1 value=$2 authority
    input_hint='A nonempty value is required.'
    case "$name" in
        DOMAIN) input_hint='Enter a DNS domain such as example.com, without a scheme or path.'; valid_domain "$value" ;;
        JMAP_URL)
            input_hint='Use an HTTPS origin, without a path, credentials, query or fragment.'
            [[ "$value" == https://* ]] || return 1
            authority=${value#https://}
            [[ "$authority" != */* && "$authority" != *[[:space:]]* && "$authority" != *\?* && "$authority" != *\#* && "$authority" != *@* ]] || return 1
            valid_authority "$authority" false ;;
        S3_ENDPOINT)
            input_hint='Use http(s)://domain-or-IP[:port], with brackets around IPv6; no credentials, query or fragment.'
            [[ -n "$value" ]] || return 0
            [[ "$value" == http://* || "$value" == https://* ]] || return 1
            [[ "$value" != *[[:space:]]* && "$value" != *\?* && "$value" != *\#* && "$value" != *@* && "$value" != *\\* ]] || return 1
            authority=${value#*://}; authority=${authority%%/*}
            valid_authority "$authority" false ;;
        RELAY_ADDR) input_hint='Use domain:port, IPv4:port, or [IPv6]:port.'; valid_authority "$value" true ;;
        *_PORT)
            input_hint='Use a distinct integer port from 1 to 65535, without leading zeroes.'
            [[ "$value" =~ ^[1-9][0-9]{0,4}$ ]] && valid_port "$value" && [[ "$used_ports" != *" $value "* ]] ;;
        DEPLOY_USER) input_hint='Use a Unix username (letters, digits, underscores and hyphens).'; [[ "$value" =~ ^[a-zA-Z_][a-zA-Z0-9_-]*$ ]] ;;
        DEPLOY_UID) input_hint='Use a numeric UID.'; [[ "$value" =~ ^[0-9]{1,10}$ ]] && ((10#$value <= 4294967294)) ;;
        DEPLOY_HOME|BINDIR|CONFIG_DIR|SYSTEMD_USER_DIR|LINEAGE|WEBROOT)
            input_hint='Use an absolute path containing letters, digits, /, ., _ or -; no .. segments.'
            [[ "$value" =~ ^/[a-zA-Z0-9_./-]+$ && "$value/" != *'/../'* ]] ;;
        CERT_KEY|KEY_KEY|RELAY_PASSWORD_KEY|RELAY_CA_KEY)
            input_hint='Use a relative S3 object key containing letters, digits, /, ., _ or -; no .. segments.'
            if [[ -z "$value" && ( "$name" == RELAY_PASSWORD_KEY || "$name" == RELAY_CA_KEY ) ]]; then return 0; fi
            [[ "$value" =~ ^[a-zA-Z0-9_./-]+$ && "$value" != /* && "$value" != *'..'* ]] ;;
        S3_BUCKET) input_hint='Use a bucket name containing lowercase letters, digits, dots or hyphens.'; [[ "$value" =~ ^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$ && "$value" != *..* ]] ;;
        S3_REGION) input_hint='Use a region identifier such as us-east-1.'; [[ "$value" =~ ^[a-zA-Z0-9][a-zA-Z0-9-]*$ ]] ;;
        LOG_LEVEL) input_hint='Choose debug, info, warn or error.'; [[ "$value" == debug || "$value" == info || "$value" == warn || "$value" == error ]] ;;
        OUTBOUND) input_hint='Choose disabled, direct or relay.'; [[ "$value" == disabled || "$value" == direct || "$value" == relay ]] ;;
        RELAY_TLS) input_hint='Choose starttls or implicit.'; [[ "$value" == starttls || "$value" == implicit ]] ;;
        QUEUE_RETRY) input_hint='Use a positive integer followed by ms, s, m or h.'; [[ "$value" =~ ^[1-9][0-9]{0,8}(ms|s|m|h)$ ]] ;;
        overwrite) input_hint='Choose yes or no.'; [[ "$value" == yes || "$value" == no ]] ;;
        SESSION_TOKEN) return 0 ;;
        *) [[ -n "$value" ]] ;;
    esac
}

# Quote once for both systemd EnvironmentFile and Bash sourcing in the hook.
quote_env() {
    local value=${!1}
    value=${value//\\/\\\\}
    value=${value//\"/\\\"}
    value=${value//\$/\\\$}
    value=${value//\`/\\\`}
    printf -v "${1}_Q" '"%s"' "$value"
}
# Substitute tokens from the template only. Never interpret or rescan input.
render() {
    local text token rest
    text=$(cat -- "$1")
    while [[ "$text" == *'@@'* ]]; do
        printf '%s' "${text%%@@*}"
        rest=${text#*@@}
        [[ "$rest" == *'@@'* ]] || { printf 'Unclosed template token\n' >&2; return 1; }
        token=${rest%%@@*}
        [[ "$token" =~ ^[A-Z][A-Z0-9_]*$ ]] || return 1
        [[ ${!token+x} ]] || { printf 'Missing template value: %s\n' "$token" >&2; return 1; }
        printf '%s' "${!token}"
        text=${rest#*@@}
    done
    printf '%s\n' "$text"
}

printf '\nfma deployment configuration\nValues are saved only in deploy/generated/. Press Ctrl+C to cancel.\n\n'
ask DOMAIN 'Mail domain (example.com)'
required DOMAIN
[[ "$DOMAIN" =~ ^[a-z0-9][a-z0-9.-]*\.[a-z0-9-]+$ && "$DOMAIN" != *..* ]] || { printf 'Invalid domain.\n' >&2; exit 1; }
ask DEPLOY_USER 'Linux service user' "$(id -un)"
[[ "$DEPLOY_USER" =~ ^[a-zA-Z_][a-zA-Z0-9_-]*$ ]] || { printf 'Invalid service user.\n' >&2; exit 1; }
default_uid=$(id -u "$DEPLOY_USER" 2>/dev/null || printf 1000)
ask DEPLOY_UID 'Linux service UID' "$default_uid"
[[ "$DEPLOY_UID" =~ ^[0-9]+$ ]] || { printf 'Invalid UID.\n' >&2; exit 1; }
default_home=/home/$DEPLOY_USER
[[ "$DEPLOY_USER" != "$(id -un)" ]] || default_home=$HOME
ask DEPLOY_HOME 'Service user home' "$default_home"
safe_path DEPLOY_HOME
SYSTEMD_FLAGS=--user
WANTED_BY=default.target
default_bin=$DEPLOY_HOME/.local/bin
default_config=$DEPLOY_HOME/.config/fma
default_units=$DEPLOY_HOME/.config/systemd/user
if [[ "$DEPLOY_UID" == 0 ]]; then
    SYSTEMD_FLAGS=
    WANTED_BY=multi-user.target
    default_bin=/usr/local/bin
    default_config=/etc/fma
    default_units=/etc/systemd/system
fi
ask BINDIR 'Binary installation directory' "$default_bin"
ask CONFIG_DIR 'Installed configuration directory' "$default_config"
ask SYSTEMD_USER_DIR 'Systemd unit directory' "$default_units"
for name in BINDIR CONFIG_DIR SYSTEMD_USER_DIR; do safe_path "$name"; done

printf '\nS3 connection\n'
ask S3_ENDPOINT 'S3 endpoint URL (empty for AWS)'
[[ -z "$S3_ENDPOINT" || "$S3_ENDPOINT" == http://* || "$S3_ENDPOINT" == https://* ]] || { printf 'Invalid S3 endpoint.\n' >&2; exit 1; }
ask S3_BUCKET 'Existing bucket name' fma
[[ "$S3_BUCKET" =~ ^[a-z0-9][a-z0-9.-]*$ ]] || { printf 'Invalid bucket name.\n' >&2; exit 1; }
ask S3_REGION 'S3 region' us-east-1
required S3_REGION
ask ACCESS_KEY 'S3 access key' '' true
required ACCESS_KEY
ask SECRET_KEY 'S3 secret key' '' true
required SECRET_KEY
ask SESSION_TOKEN 'S3 session token (optional)' '' true
ask CERT_KEY 'Certificate object key' cert.pem
ask KEY_KEY 'Private key object key' key.pem
safe_key CERT_KEY; safe_key KEY_KEY

printf '\nOutbound delivery\n'
ask OUTBOUND 'Outbound mode: disabled, direct, relay' disabled
RELAY_ADDR= RELAY_TLS= RELAY_USER= RELAY_PASSWORD= RELAY_PASSWORD_KEY= RELAY_CA_KEY=
case "$OUTBOUND" in
    disabled|direct) ;;
    relay)
        ask RELAY_ADDR 'Relay host:port'
        [[ "$RELAY_ADDR" =~ ^[^[:space:]]+:[0-9]+$ ]] || { printf 'Invalid relay address.\n' >&2; exit 1; }
        ask RELAY_TLS 'Relay TLS mode: starttls, implicit' starttls
        [[ "$RELAY_TLS" == starttls || "$RELAY_TLS" == implicit ]] || exit 1
        ask RELAY_USER 'Relay username'; required RELAY_USER
        ask RELAY_PASSWORD_KEY 'Relay password S3 key (empty to enter a password)'
        if [[ -n "$RELAY_PASSWORD_KEY" ]]; then safe_key RELAY_PASSWORD_KEY
        else ask RELAY_PASSWORD 'Relay password' '' true; required RELAY_PASSWORD; fi
        ask RELAY_CA_KEY 'Relay CA S3 key (optional)'
        [[ -z "$RELAY_CA_KEY" ]] || safe_key RELAY_CA_KEY
        ;;
    *) printf 'Invalid outbound mode.\n' >&2; exit 1 ;;
esac
ask QUEUE_RETRY 'Initial retry delay (e.g. 1m)' 1m
[[ "$QUEUE_RETRY" =~ ^[1-9][0-9]*(ms|s|m|h)$ ]] || { printf 'Invalid retry delay.\n' >&2; exit 1; }

printf '\nLocal protocol ports (Nginx uses standard public ports)\n'
used_ports=' '
for spec in SMTP:2525 SUBMISSION:1587 SMTPS:1465 POP3:1110 POP3S:1995 IMAP:1143 IMAPS:1993 HTTP:8080; do
    name=${spec%:*}_PORT
    ask "$name" "${spec%:*} loopback port" "${spec#*:}"
    value=${!name}
    [[ "$value" =~ ^[1-9][0-9]{0,4}$ ]] && ((value <= 65535)) || { printf 'Invalid port.\n' >&2; exit 1; }
    [[ "$used_ports" != *" $value "* ]] || { printf 'Ports must be distinct.\n' >&2; exit 1; }
    used_ports+="$value "
done
ask LINEAGE 'Certbot certificate directory' "/etc/letsencrypt/live/mail.$DOMAIN"
ask WEBROOT 'ACME webroot directory' /var/www/certbot
safe_path LINEAGE; safe_path WEBROOT
ask LOG_LEVEL 'Log level: debug, info, warn, error' info
quote_env LOG_LEVEL
ask JMAP_URL 'Public JMAP HTTPS origin' "https://mail.$DOMAIN"
quote_env JMAP_URL

for name in S3_ENDPOINT S3_BUCKET S3_REGION ACCESS_KEY SECRET_KEY SESSION_TOKEN OUTBOUND RELAY_ADDR RELAY_TLS RELAY_USER RELAY_PASSWORD RELAY_PASSWORD_KEY RELAY_CA_KEY; do quote_env "$name"; done
if [[ -e "$root/generated" ]]; then
    [[ -d "$root/generated" && ! -L "$root/generated" ]] || { printf 'generated must be a regular directory.\n' >&2; exit 1; }
    ask overwrite 'Replace existing generated configuration? yes/no' no
    [[ "$overwrite" == yes ]] || { printf 'Existing configuration preserved.\n'; exit 0; }
fi
staging=$(mktemp -d "$root/.generated.XXXXXX")
for template in "$root"/template/*.tmpl; do
    render "$template" > "$staging/$(basename -- "${template%.tmpl}")"
done
chmod 700 "$staging/renew-hook.sh"
# All input and rendering succeeds before replacing the previous configuration.
if [[ -d "$root/generated" ]]; then
    backup=$(mktemp -d "$root/.generated-backup.XXXXXX")
    rmdir "$backup"
    mv "$root/generated" "$backup"
    if ! mv "$staging" "$root/generated"; then mv "$backup" "$root/generated"; exit 1; fi
    rm -rf -- "$backup"
else
    mv "$staging" "$root/generated"
fi
staging=
printf '\nGenerated deployment files in %s/generated/\nRun make install as %s on the target Linux host.\n' "$root" "$DEPLOY_USER"
printf 'Install the generated Nginx configs and Certbot hook separately as root.\n'
