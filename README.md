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

Accounts, messages, folders, outbound jobs and leases live in S3. TLS certificates
come from S3 by default, or a read-only mounted TLS Secret in Kubernetes.
The mail binary has no registration API, user management commands, CSV import,
local database, disk cache, or temporary file management. HTTP provides only a
liveness endpoint. DEBUG/INFO/WARN logs go to stdout; ERROR/FATAL go to stderr.

## Install a release

Install the latest release directly:

```sh
curl -fsSL https://raw.githubusercontent.com/Jabberwocky238/fma/main/install.sh | bash
```

The installer downloads the latest stable GitHub Release for Linux, macOS or Windows
(amd64 or arm64), checks its SHA-256 digest and reported version, and installs
`~/.local/bin/fma` for ordinary users or `/usr/local/bin/fma` for root. Check the installed version with `fma --version`.
An existing current or newer version is left untouched. An older or unrecognized
version prompts `Update? [y/N]`; only `y` proceeds. Download or verification failures
preserve the existing binary. Add `~/.local/bin` to your PATH if needed.
`FMA_INSTALL_DIR` overrides the installation directory; `FMA_REPO` selects a fork.
Windows installation uses Git Bash/MSYS/Cygwin with Bash, curl and unzip; the installer
detects Windows and installs `fma.exe` from the matching ZIP. Linux/macOS use tar.gz.
`--systemd` is Linux-only. Architecture is detected using `uname -m`.

To install the binary and a **systemd service** on Linux, use:

```sh
bash <(curl -fsSL https://raw.githubusercontent.com/Jabberwocky238/fma/main/install.sh) --systemd
```

This uses the release's deployment templates and interactive generator, then installs,
enables and starts `fma.service`. It needs a working systemd manager (a user session for non-root installs) and existing
S3 bucket/certificate objects. The generator and its `generated/` configuration are
kept in `~/.config/fma/deploy` for users or `/etc/fma/deploy` for root (`FMA_DEPLOY_DIR` overrides this location). To reuse
configuration generated in a checkout:

```sh
bash install.sh --systemd --config-dir deploy/generated
```

Generated paths and service identity take precedence over `FMA_INSTALL_DIR` in this
mode. An up-to-date binary can still have its service installed. Declining a binary
update also skips service changes. Inspect it with `systemctl --user status fma` and
`journalctl --user -u fma`; persistent operation after logout requires user lingering
configured on the host. Without `--systemd`, the installer only installs the binary.

The installer prints a warning identifying **root** or **user** mode and its paths:

| Mode | Binary | Configuration | Service | Manager |
| --- | --- | --- | --- | --- |
| root | `/usr/local/bin/fma` | `/etc/fma` | `/etc/systemd/system/fma.service` | `systemctl` |
| user | `~/.local/bin/fma` | `~/.config/fma` | `~/.config/systemd/user/fma.service` | `systemctl --user` |

Root services start with `multi-user.target`; user services use `default.target`.
Use the matching manager for status and logs (root: `journalctl -u fma`).
Uninstall directly as the same user who installed:

```sh
bash <(curl -fsSL https://raw.githubusercontent.com/Jabberwocky238/fma/main/install.sh) --uninstall
```

Uninstall stops and disables the matching service, removes the binary, unit, installed
environment files and the installer's generated deployment artifacts. It makes no
release download requests. Custom installation paths are recorded in `install.paths`
under the mode's default configuration directory. Unrelated files and all S3 data
are preserved; root uninstallation does not search other users' home directories.

Root installation also creates `/bin/fma` as a symlink to the installed binary. Root
uninstallation removes this link only when it points to that binary. An unrelated
existing `/bin/fma` is never overwritten.

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

Each ID is a bucket-root prefix. **`<id>/.kind` is required** and selects exactly
one type; configuration files for other types are ignored, so stale files cannot
silently change routing or authentication.

| `.kind` | Configuration | Behavior |
| --- | --- | --- |
| `account` | `.password`: nonempty password | A real mailbox with protocol login |
| `alias` | `.alias`: another local ID | Resolves to that ID; login only if the chain ends at an account |
| `proxy` | `.proxy`: one full email address | Forward-only, no login and no local copy of the message |

Prepare the type-specific object first, then write `.kind` to activate it:


```sh
printf '%s' 'your-password' | curl -f -X PUT --data-binary @- \
  http://127.0.0.1:9000/fma/alice/.password
printf '%s' account | curl -f -X PUT --data-binary @- \
  http://127.0.0.1:9000/fma/alice/.kind
```

Once `.kind=account` exists, overwriting `.password` changes the password;
deleting it rejects subsequent logins and local SMTP recipient checks. No server
restart is needed. Existing authenticated sessions are not automatically revoked.

Usernames contain 1–64 lowercase letters, digits, dots, hyphens, or underscores,
starting with a letter or digit. Passwords must be nonempty and contain no embedded
newlines; trailing CR/LF characters are stripped. Password objects contain plaintext
credentials, protected by the bucket's access controls. Authentication and local
recipient checks read S3 on every request, without an account list or password cache.

Alias accounts are provisioned externally: set `<alias>/.kind` to `alias` and put
the target local ID in `<alias>/.alias`.
The alias keeps its own prefix and uses the root account password and mailbox.
Alias chains are resolved on each login and recipient lookup; cycles and missing
accounts are rejected. Hidden metadata objects such as `.profile.json` are excluded
from mail listings. Protocol logins retain the login ID separately from the root ID;
SMTP, POP3 and IMAP do not expose an avatar/profile management API.

For forwarding, write the destination address to `<id>/.proxy` and set `.kind` to
`proxy`. The destination may be local or external. Local targets resolve immediately;
external forwarding uses the durable S3 outbound queue and requires `direct` or `relay`.
Unauthenticated inbound SMTP may deliver to a configured local proxy, but cannot
choose arbitrary external destinations. MIME bodies and attachments remain intact;
forwarding adds only an `X-FMA-Proxy-Hops` header for external loop protection.
Alias/proxy chains are limited to 16 local hops, and external proxy hops to 16.

Routing is read from S3 on each RCPT and stored with accepted tasks. Changing the
proxy affects later SMTP transactions, not already queued mail. Multiple recipients
forwarding to the same destination produce one delivery. Forwarding preserves the
original envelope sender; no SRS rewriting is implemented, so the destination's
sender policy can still reject forwarded mail. Permanent failures are recorded as
metadata under `<proxy>/.proxy-errors/<task-id>.json`, without retaining the message
body. The completed task and its preclaim are then removed together.

Missing or invalid `.kind` values reject login and delivery. **Existing accounts
must be provisioned externally with `.kind=account`; existing aliases need
`.kind=alias`.** There is no automatic migration or registration API. To change a
type, prepare its new configuration first and replace `.kind` last.

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
Certificates and relay configuration are loaded once at startup. `-tls-dir /run/fma/tls`
reads `tls.crt` and `tls.key` from a read-only mounted Secret instead of S3.

Logging uses the global structured logger. Set `LOG_LEVEL=debug|info|warn|error`
(default `info`). DEBUG/INFO/WARN use stdout and ERROR/FATAL use stderr; this variable has no `FMA_` prefix and is read before configuration.
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
| `<id>/.kind` | Required type: `account`, `alias` or `proxy` |
| `<user>/.password` | Account password |
| `<alias>/.alias` | Target local ID |
| `<proxy>/.proxy` | Forwarding destination email address |
| `<proxy>/.proxy-errors/<id>.json` | Failure diagnostics without message bodies |
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
delivery, or `relay` to use an SMTP relay. A relay is another SMTP server that
accepts outbound mail from fma and delivers it to the recipient's mail provider;
configure its address and credentials. These modes affect external delivery only,
not receiving mail or reading local mailboxes.

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

The generator checks environment variables **before each prompt**. Set values are
validated and used without asking; invalid environment values fail immediately and
secrets are not echoed. For unattended deployments (including Kubernetes setup):

```sh
export FMA_DOMAIN=example.com
export FMA_S3_ENDPOINT=https://s3.example.com
export FMA_S3_BUCKET=fma
export FMA_S3_ACCESS_KEY_ID=your-access-key
export FMA_S3_SECRET_ACCESS_KEY=your-secret-key
bash deploy/gen.sh --non-interactive
```

In this mode, unset fields use defaults and missing required fields fail instead of
waiting for input. Region defaults to `us-east-1`. Set `FMA_OVERWRITE=yes` to replace
existing generated files; the default preserves them. Environment lookup also works
in interactive mode. Generator-only variables are rendered into service arguments;
they do not add registration or configuration-management APIs to the mail process.

| Generator fields | Environment variables |
| --- | --- |
| Domain, service identity | `FMA_DOMAIN`, `FMA_DEPLOY_USER`, `FMA_DEPLOY_UID`, `FMA_DEPLOY_HOME` |
| Installation paths | `FMA_BINDIR`, `FMA_CONFIG_DIR`, `FMA_SYSTEMD_USER_DIR` (also for system units) |
| S3 connection | `FMA_S3_ENDPOINT`, `FMA_S3_BUCKET`, `FMA_S3_REGION`, `FMA_S3_ACCESS_KEY_ID`, `FMA_S3_SECRET_ACCESS_KEY`, `FMA_S3_SESSION_TOKEN` |
| Certificate object keys | `FMA_CERT_KEY`, `FMA_KEY_KEY` |
| Outbound | `FMA_OUTBOUND_MODE`, `FMA_RELAY_ADDR`, `FMA_RELAY_TLS`, `FMA_RELAY_USER`, `FMA_RELAY_PASSWORD`, `FMA_RELAY_PASSWORD_FILE`, `FMA_RELAY_CA_FILE`, `FMA_QUEUE_RETRY` |
| Ports | `FMA_SMTP_PORT`, `FMA_SUBMISSION_PORT`, `FMA_SMTPS_PORT`, `FMA_POP3_PORT`, `FMA_POP3S_PORT`, `FMA_IMAP_PORT`, `FMA_IMAPS_PORT`, `FMA_HTTP_PORT` |
| Certbot paths | `FMA_LINEAGE`, `FMA_WEBROOT` |
| Logging, overwrite | `LOG_LEVEL` (no prefix), `FMA_OVERWRITE` |

Run `make install` as the selected user on the target Linux host. It requires the
generated configuration, builds the binary, installs the generated systemd unit
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

## Docker and Docker Compose

The [Dockerfile](Dockerfile) uses a Go build stage and an Alpine 3.23 runtime with
CA certificates. The runtime runs as UID/GID 65532 and needs no data volumes;
accounts, TLS keys, messages and queues remain in your existing S3 bucket.
See [Docker's multi-stage build documentation](https://docs.docker.com/build/building/multi-stage/).

```sh
cp .env.example .env
# Edit .env: set your domain, S3 endpoint/bucket and credentials.
# Upload cert.pem, key.pem and account objects to that bucket first.
docker compose up -d --build
docker compose logs -f fma
docker compose down
```

The Compose configuration publishes standard SMTP/submission/POP3/IMAP TCP ports.
HTTP health is published only on host loopback port 8080. Listeners inside the
container bind `0.0.0.0`; `localhost` in an S3 endpoint refers to that container,
so use a reachable external S3 address. `.env` is ignored by Git. No S3 container,
local database or Docker volume is created. The container root filesystem is read-only.
Containers run fma directly rather than invoking systemd or `install.sh`.

Build an image yourself, optionally injecting version metadata:

```sh
docker build --build-arg COMMIT="$(git rev-parse HEAD)" -t fma:local .
```

`VERSION` and `RELEASE_TIME` are optional build arguments; without them the image uses
`dev-{datetime}` and the UTC build time. The build context only includes the Go source,
module files and Dockerfile; environment files and generated credentials are excluded.
To use a published image with the same Compose settings:

```sh
FMA_IMAGE=ghcr.io/jabberwocky238/fma:latest docker compose up -d --no-build --pull always
```

## Kubernetes

[deploy/kubernetes/](deploy/kubernetes/) provides a Kustomize deployment: two replicas,
a configuration ConfigMap, a TCP LoadBalancer Service and a PodDisruptionBudget.
The pods run without root privileges, local data volumes or Kubernetes API credentials.
They share the same S3 bucket. The Service exposes the seven mail ports and keeps
HTTP health internal. Your cluster must support LoadBalancer services, or you can
adapt its type to your existing TCP entry point.

Edit `deploy/kubernetes/configmap.yaml` for your domain and S3 endpoint/bucket.
TLS comes directly from the Kubernetes `fma-tls` Secret (`kubernetes.io/tls`), mounted
read-only at `/run/fma/tls`. Use an existing cert-manager Secret by changing
`secretName` in `deployment.yaml`, or create one before applying the deployment:

```sh
kubectl create namespace fma --dry-run=client -o yaml | kubectl apply -f -
kubectl -n fma create secret tls fma-tls --cert=fullchain.pem --key=privkey.pem
```

No certificate objects are required in S3 for this deployment. Certificates are loaded
at startup; after Secret renewal, restart the pods (or use your cluster's Secret
reload controller). The process does not write or manage mounted certificate files.

Set the image and tag in `kustomization.yaml` to a published
GHCR version or your own pushed image. Create the credentials Secret in the same
namespace as the deployment (`fma`):

```sh
kubectl -n fma create secret generic fma-s3 \
  --from-literal=FMA_S3_ACCESS_KEY_ID="$FMA_S3_ACCESS_KEY_ID" \
  --from-literal=FMA_S3_SECRET_ACCESS_KEY="$FMA_S3_SECRET_ACCESS_KEY" \
  --from-literal=FMA_S3_SESSION_TOKEN="${FMA_S3_SESSION_TOKEN:-}" \
  --dry-run=client -o yaml | kubectl -n fma apply -f -
kubectl apply -k deploy/kubernetes
kubectl -n fma rollout status deployment/fma
kubectl -n fma get service fma
```

For a private registry package, configure an image pull Secret on the deployment.
Pin an image version for reproducible rollouts; `latest` is pulled when a pod starts,
but publishing it does not restart existing pods. Use `kubectl -n fma rollout restart deployment/fma`
after changing environment settings or to refresh `latest`. Set resource requests/limits
based on your workloads. Graceful shutdown allows 120 seconds for in-flight work.
Startup, readiness and liveness probes use HTTP `/`, which checks the running process;
it does not continuously verify S3 availability. See the
[Kubernetes probe documentation](https://kubernetes.io/docs/concepts/workloads/pods/probes/).

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

Container publication is separate and **manual only**: open **Actions → Publish GHCR
image → Run workflow**, or run:

```sh
gh workflow run ghcr.yml
```

The workflow resolves the latest stable GitHub Release, checks out that tag's commit,
and publishes **Linux amd64/arm64 only** to `ghcr.io/jabberwocky238/fma:<release-tag>`
and `:latest`. It uses the release version, source commit and release publication time
for build metadata. `latest` moves to that release's image. The release must include
the Dockerfile; this workflow does not build arbitrary main-branch changes or publish
macOS/Windows images. It authenticates with `GITHUB_TOKEN` and `packages: write`.
Binary releases remain available for all three operating systems and both architectures.

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
