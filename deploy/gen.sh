#!/bin/bash
set -euo pipefail
umask 077
root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
staging=
trap '[[ -z "$staging" ]] || rm -rf -- "$staging"' EXIT

ask() {
    local name=$1 label=$2 fallback=${3-} secret=${4-false} input
    while true; do
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
        if [[ "$input" == *$'\r'* || "$input" == *$'\n'* ]]; then
            printf 'Newlines are not allowed.\n' >&2
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
ask BINDIR 'Binary installation directory' "$DEPLOY_HOME/.local/bin"
ask CONFIG_DIR 'Installed configuration directory' "$DEPLOY_HOME/.config/fma"
ask SYSTEMD_USER_DIR 'Systemd user unit directory' "$DEPLOY_HOME/.config/systemd/user"
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
