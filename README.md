# fma

[English](README.md) | [简体中文](README.zh-CN.md)

**fma is a lightweight mail service whose only external service dependency is S3.**
It supports SMTP, POP3 and IMAP, with a single S3 bucket holding all durable state.

- **High availability:** multiple nodes share one bucket; persisted delivery tasks
  can be reclaimed after a node crashes.
- **Concurrent operation:** protocol connections run concurrently, while conditional
  S3 writes coordinate task ownership and mailbox updates across nodes.
- **Small footprint:** one Go binary, with no local database, Redis, spool, or Docker
  requirement. Memory usage and throughput depend on message size, active connections
  and S3 latency; this project does not yet publish production benchmarks.

Availability depends on S3 and routing clients to healthy nodes. Existing connections
must reconnect after a node fails.

Accounts, certificates, messages, folders, outbound jobs, and leases live in S3.
The mail binary has no registration API, user management commands, CSV import,
local database, disk cache, or temporary file management. HTTP provides only a
liveness endpoint. Logs go to stderr.

## Install a release

Install the latest release directly:

```sh
curl -fsSL https://raw.githubusercontent.com/Jabberwocky238/fma/main/install.sh | bash
```

The installer downloads the latest stable GitHub Release for Linux or macOS
(amd64 or arm64), checks its SHA-256 digest and reported version, and installs
`~/.local/bin/fma`. Check the installed version with `fma --version`.
An existing current or newer version is left untouched. An older or unrecognized
version prompts `Update? [y/N]`; only `y` proceeds. Download or verification failures
preserve the existing binary. Add `~/.local/bin` to your PATH if needed.
`FMA_INSTALL_DIR` overrides the installation directory; `FMA_REPO` selects a fork.
Windows binaries are available as ZIP archives on the Releases page.

To install the binary and a **systemd user service** on Linux, use:

```sh
bash <(curl -fsSL https://raw.githubusercontent.com/Jabberwocky238/fma/main/install.sh) --systemd
```

This uses the release's deployment templates and interactive generator, then installs,
enables and starts `fma.service`. It needs a working systemd user session and existing
S3 bucket/certificate objects. The generator and its `generated/` configuration are
kept in `~/.config/fma/deploy` (`FMA_DEPLOY_DIR` overrides this location). To reuse
configuration generated in a checkout:

```sh
bash install.sh --systemd --config-dir deploy/generated
```

Generated paths and service identity take precedence over `FMA_INSTALL_DIR` in this
mode. An up-to-date binary can still have its service installed. Declining a binary
update also skips service changes. Inspect it with `systemctl --user status fma` and
`journalctl --user -u fma`; persistent operation after logout requires user lingering
configured on the host. Without `--systemd`, the installer only installs the binary.

## Run locally

Install the native [Fals3y](https://github.com/LukeOfEarth/fals3y) binary, then run:

```sh
make build
python3 scripts/local.py
```

The script starts Fals3y, creates the `fma` bucket and test certificates if needed,
and starts the mail server. It does not create accounts or use Docker. Fals3y owns
its data directory, which defaults to `~/.local/share/fals3y/data`; the mail process
accesses it exclusively through S3. Press Ctrl+C to stop both services.

Use `--bucket <name>` to select an existing bucket. `--data` configures Fals3y's
storage directory, not the mail process. This local setup uses an unauthenticated
S3 endpoint and a self-signed TLS certificate.

POP3/STLS and POP3S use [migadu/go-pop3](https://github.com/migadu/go-pop3).
The library handles protocol framing, TLS and SASL PLAIN; fma supplies S3 authentication
and mailbox sessions. `DELE` marks messages, `RSET` clears those marks, and `QUIT`
commits deletion. Disconnecting without `QUIT` keeps the messages.

## Accounts

Each username is a bucket-root prefix. The contents of `<user>/.password` define
the password. Provision accounts directly through S3:

```sh
printf '%s' 'your-password' | curl -f -X PUT --data-binary @- \
  http://127.0.0.1:9000/fma/alice/.password
```

Creating this object enables the account; overwriting it changes the password;
deleting it rejects subsequent logins and local SMTP recipient checks. No server
restart is needed. Existing authenticated sessions are not automatically revoked.

Usernames contain 1–64 lowercase letters, digits, dots, hyphens, or underscores,
starting with a letter or digit. Passwords must be nonempty and contain no embedded
newlines; trailing CR/LF characters are stripped. Password objects contain plaintext
credentials, protected by the bucket's access controls. Authentication and local
recipient checks read S3 on every request, without an account list or password cache.

Alias accounts are provisioned externally: put the root username in `<alias>/.alias`.
The alias keeps its own prefix and uses the root account password and mailbox.
Alias chains are resolved on each login and recipient lookup; cycles and missing
accounts are rejected. Hidden metadata objects such as `.profile.json` are excluded
from mail listings. Protocol logins retain the login ID separately from the root ID;
SMTP, POP3 and IMAP do not expose an avatar/profile management API.

## Connect to S3

Build with Go 1.25 or newer. The bucket must already exist and support consistent
reads and listings, ETags, and atomic conditional PUTs (`If-None-Match` and `If-Match`).

```sh
export FMA_S3_ENDPOINT=http://127.0.0.1:9000
export FMA_S3_BUCKET=fma
export FMA_S3_REGION=us-east-1
export FMA_S3_ACCESS_KEY_ID=local
export FMA_S3_SECRET_ACCESS_KEY=local
./fma
```

Fals3y accepts arbitrary credentials, but the SDK requires a key pair. Use real
credentials for other S3 services; `FMA_S3_SESSION_TOKEN` supports temporary credentials.
The binary does not load local AWS configuration files. Mail configuration environment settings use the `FMA_` prefix, which is added centrally by the
configuration reader. Flags `-s3-endpoint`,
`-s3-bucket`, and `-s3-region` override connection settings. Startup fails if the bucket
is unavailable. Listeners bind to loopback by default; see `./fma -h` for ports.

Upload a TLS certificate chain and private key as `cert.pem` and `key.pem` before
starting. The `-cert` and `-key` flags specify object keys, not filesystem paths.
`FMA_RELAY_PASSWORD_FILE` and `FMA_RELAY_CA_FILE` also name objects in the same bucket.
Certificates and relay configuration are loaded once at startup.

Logging uses the global structured logger. Set `LOG_LEVEL=debug|info|warn|error`
(default `info`); this variable has no `FMA_` prefix and is read before configuration.
Startup validates the complete configuration before connecting to S3 or opening
listeners, failing on missing required values and reporting warnings such as disabled
outbound delivery. `--version` needs no S3 configuration; `--queue` only needs S3.

## Queue ownership and recovery

Multiple nodes may share a bucket. `.lock` controls outbound scanning and claiming;
it does not block protocol traffic or previously claimed jobs. The lease records
its owner, start time, renewal time, and expiry. It renews every 15 seconds and expires
after 30 seconds, allowing one interval of renewal slack. Release conditionally marks
the lease expired, avoiding deletion of a successor's lock. Keep node clocks synchronized.

The holder scans `.outbox/` immediately and every 15 seconds. Before execution, it
claims each eligible task with a conditional PUT. A node holds at most 1024 active
tasks. Claiming a batch of 1024 or reaching capacity immediately releases the scanner
lease; the node waits until the next scan interval before competing again. Already
claimed tasks continue running after release.

Each preclaim records its owner, start time, and a fixed 20-second expiry. It is not
renewed. Expiry cancels execution and leaves the task in S3 for another claim. Recovery
occurs on a subsequent scan; a crashed scanner also requires lease expiry. An old
worker cannot overwrite a successor using a stale ETag.

Temporary SMTP errors persist per-recipient retry state with exponential backoff,
starting at `-queue-retry`. Confirmed recipients are skipped on retries. Permanent
failures complete after a local delivery-failure notice is stored.

The preclaim is embedded in the task object, so deleting the task removes both.
Completion first conditionally writes a terminal state that cannot be claimed again,
then deletes the object. If deletion fails or the process crashes between these steps,
a later scan retries deletion without sending again. `-queue` lists outstanding tasks;
completed tasks are not retained as queue history.

S3 and remote SMTP do not share a transaction. If a remote accepts a message but the
worker times out or cannot persist confirmation, redelivery may duplicate it. Delivery
is at least once, not exactly once. Mailbox UID allocation and catalog updates use
conditional S3 writes to avoid concurrent nodes overwriting each other.

## Bucket layout

| Object key | Contents |
| --- | --- |
| `<user>/.password` | Account password |
| `<alias>/.alias` | Root username for a shared account |
| `<user>/<uid>.json`, `<user>/next` | Inbox messages and UID counter |
| `<user>/folders` | Folder catalog, UIDVALIDITY, subscriptions, storage IDs |
| `<user>/.folders/<id>/` | Other folders' messages and counters |
| `.outbox/<id>.json` | Task body, recipient state, archive state, and preclaim |
| `.lock` | Scanner lease |
| `cert.pem`, `key.pem` | TLS certificate and private key |

Renaming a folder updates its catalog without copying message bodies. Recreating a
deleted folder assigns a new storage ID. Failed folder cleanup may leave unreachable
objects. Migrate legacy layouts externally while the old service is stopped; the
binary has no import path.

## Outbound mail and deployment

Outbound delivery is disabled by default. Set `FMA_OUTBOUND_MODE=direct` to use MX
delivery, or `relay` to use an SMTP relay.

Generate deployment configuration interactively:

```sh
bash deploy/gen.sh
make install
```

The generator asks for the domain, Linux service user and UID, installation paths,
S3 connection and credentials, certificate object keys, outbound settings, retry
delay, local protocol ports, and Certbot paths. Secret input is hidden on a terminal.
Each field is checked before continuing. Malformed domains, IPv4/IPv6 addresses,
endpoint URLs, relay addresses, paths and ports show an error and repeat that prompt.
Non-secret surrounding whitespace is trimmed and mail domains are lowercased.
Press Enter to accept defaults such as region `us-east-1`; secrets are preserved
verbatim. Validation checks syntax, while the server checks S3 access at startup.
Templates live in `deploy/template/` and use `@@NAME@@` placeholders. Generated
files live in `deploy/generated/`, which is ignored by Git and excluded from release
archives. Files are private by default; the generator asks before replacing existing
configuration and preserves it if input is cancelled.

Run `make install` as the selected user on the target Linux host. It requires the
generated configuration, builds the binary, installs the generated systemd user unit
and environment files, then enables and restarts `fma.service`. Missing configuration
fails before building or installing anything. Installation paths come from the
generated `install.mk`; regenerate configuration to change them.

Install `nginx-http.conf` and `nginx-https.conf` from `deploy/generated/` in the Nginx
HTTP context. The generated `nginx-stream.conf` belongs at the top level, outside
`http {}`. Public mail ports are standard; upstream loopback ports match the generated
service. The HTTPS certificate must cover the configured domain, `www.<domain>`,
and `mail.<domain>`. Upload the initial TLS certificate and account password objects
to S3 before starting the mail service.

Install `deploy/generated/renew-hook.sh` as a root-run Certbot deploy hook. It uploads
renewed certificates using the selected user's S3 configuration and restarts that
user's mail service. The target needs Bash, AWS CLI, Nginx, `runuser`, and systemd;
the generator itself only needs Bash and standard Unix utilities. Nginx configuration
and root-owned Certbot hooks are installed separately from `make install`.

## Test

```sh
make test
```

Runs formatting checks, `go vet`, Go race tests, and S3/SMTP/POP3/IMAP integration tests
against native Fals3y. Coverage includes concurrent claims, expiry takeover, rejection
of stale completion, yielding at 1024 tasks, interrupted cleanup, concurrent UID
allocation, listing over 1000 objects, outbound retries, folders, and process restart.
The mail test process runs in an empty, read-only working directory. Tests do not use
Docker. Set `FALS3Y_BIN` to override the default `~/.local/bin/fals3y` executable.

Build metadata is injected at link time into three separate variables: `version`,
`commit`, and `releaseTime`. `make build` and `make test` default to a UTC version
such as `dev-20260910T120000Z`, the full Git commit, and an RFC3339 UTC build time.
Override these with `VERSION`, `COMMIT`, and `RELEASE_TIME` when needed. Use Make
instead of bare `go build` to populate this metadata. `fma --version` prints the
version on its first line, followed by commit and release time.

## CI and releases

GitHub Actions runs formatting, vet, race, native Fals3y integration tests, and a
build on branch pushes and pull requests, using Go 1.25 and the current stable Go.
Release configuration is checked with GoReleaser as part of CI.

Push a semantic version tag to publish a GitHub Release after the same checks pass:

```sh
git tag v0.1.0
git push origin v0.1.0
```

GoReleaser builds Linux, macOS, and Windows binaries for amd64 and arm64 without
CGO. GoReleaser injects the tag version, full commit and UTC release-build time;
snapshot builds use `dev-{datetime}`. This time identifies the build, which precedes
the GitHub Release publication. Releases include tar.gz archives (ZIP on Windows), both READMEs, MIT license,
deployment examples, and SHA-256 checksums. Version tags with prerelease suffixes
produce prereleases. Publishing uses the workflow's built-in `GITHUB_TOKEN` with
`contents: write`; no personal token or Docker daemon is required.

For a local packaging check with GoReleaser v2:

```sh
goreleaser check
goreleaser release --snapshot --clean
```

These workflows expect this project directory to be the GitHub repository root.

## License

[MIT](LICENSE). Copyright © 2026 Jabberwocky238.

## Acknowledgements

Thanks to the projects that make fma possible:

- [emersion/go-smtp](https://github.com/emersion/go-smtp) — SMTP server and client.
- [emersion/go-imap](https://github.com/emersion/go-imap) — IMAP protocol and server.
- [migadu/go-pop3](https://github.com/migadu/go-pop3) — POP3 protocol and server.
- [Fals3y](https://github.com/LukeOfEarth/fals3y) — the native S3-compatible service
  used for local development and integration tests.
