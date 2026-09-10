# fma

[English](README.md) | [Chinese](README.ZH-CN.md)

**fma is a lightweight mail service whose only external service dependency is S3.** It serves SMTP, POP3, IMAP and JMAP, with a design for high availability, concurrent connections and a small footprint.

Quick install and start (prepare an existing bucket, account and TLS certificate first; replace the credentials below):

```sh
bash <(curl -fsSL https://raw.githubusercontent.com/Jabberwocky238/fma/main/install.sh) --systemd
```

**Contents**

1. [Introduction](#fma)
2. [Philosophy and design](#design)
3. [How to run](#run)
   - [3.1. Direct startup](#run-direct)
   - [3.2. systemd startup and gen.sh](#run-systemd)
   - [3.3. Docker startup](#run-docker)
   - [3.4. Kubernetes startup](#run-kubernetes)
4. [Using JMAP](#jmap)
5. [CI, testing and Fals3y](#testing)
6. [Bucket layout](#bucket)
7. [License and acknowledgements](#credits)

<a id="design"></a>

## 2. Philosophy and design

All runtime code stays in `main.go`, with Go tests in `main_test.go`. Reuse protocol libraries and connect them to S3 through explicit types and JSON fields; scripts, deployment templates and documentation may live separately. PRs for any part of the project, additions and AI-assisted programming are welcome. The one non-negotiable architectural rule is the single-file philosophy; see [CONTRIBUTING.md](CONTRIBUTING.md) and [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md).

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
local database, disk cache, or temporary file management. HTTP serves JMAP and a liveness endpoint. DEBUG/INFO/WARN logs go to stdout; ERROR/FATAL go to stderr.

JMAP and the other protocols share mail records and MIME objects under each account’s `mail/` prefix. The account file retains folders, identities, UID/state counters, the lease and the current transaction decision. Property, membership, blob-reference and ordered lookup indexes live only in memory and are rebuilt from authoritative records on cold start. Updates write changed records and one conditional account commit, instead of rewriting the mailbox. Same-account writers still coordinate through S3 conditional writes.

### Supported RFCs and scope

| RFC | Protocol | Current scope |
| --- | --- | --- |
| [RFC 5321](https://www.rfc-editor.org/rfc/rfc5321.html) | SMTP | Inbound delivery and external SMTP transport |
| [RFC 6409](https://www.rfc-editor.org/rfc/rfc6409.html) | SMTP Submission | Authenticated application/client submission; public port 587 |
| [RFC 4954](https://www.rfc-editor.org/rfc/rfc4954.html) | SMTP AUTH | Authentication on TLS-protected submission connections |
| [RFC 3207](https://www.rfc-editor.org/rfc/rfc3207.html) | SMTP STARTTLS | Upgrade SMTP connections to TLS |
| [RFC 1939](https://www.rfc-editor.org/rfc/rfc1939.html) | POP3 | Download, UIDL, deferred DELE and QUIT commit |
| [RFC 3501](https://www.rfc-editor.org/rfc/rfc3501.html) | IMAP4rev1 | Mailbox reads/writes, search, flags, folders and subscriptions |
| [RFC 2595](https://www.rfc-editor.org/rfc/rfc2595.html) | IMAP/POP3 TLS | IMAP STARTTLS and POP3 STLS |
| [RFC 4616](https://www.rfc-editor.org/rfc/rfc4616.html) | SASL PLAIN | Password authentication inside TLS |
| [RFC 2045](https://www.rfc-editor.org/rfc/rfc2045.html) | MIME | Preserve raw MIME, encoded bodies and attachments |
| [RFC 2046](https://www.rfc-editor.org/rfc/rfc2046.html) | MIME media types | Multipart messages and attachments |
| [RFC 8620](https://www.rfc-editor.org/rfc/rfc8620.html) | JMAP Core | Session, HTTP/JSON methods, result references, blobs and state-based synchronization; push subscriptions are not implemented |
| [RFC 8621](https://www.rfc-editor.org/rfc/rfc8621.html) | JMAP Mail | Mailbox, Email, Thread, SearchSnippet, Identity and EmailSubmission; reading, writing, search, attachments and changes |

### Queue ownership and recovery

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

<a id="run"></a>

## 3. How to run

### Parameters

Command-line values override the corresponding environment variables, then defaults apply. `—` means there is no corresponding binary environment input or flag. Generator-only variables are listed separately. Logging uses unprefixed `LOG_LEVEL`.

| Flag | Environment read by the binary | Default | Meaning |
| --- | --- | --- | --- |
| `-domain` | `—` | `t12e.cc` | Local mail domain |
| `-s3-endpoint` | `FMA_S3_ENDPOINT` | `empty` | S3 endpoint; empty selects AWS |
| `-s3-bucket` | `FMA_S3_BUCKET` | `required` | Existing bucket |
| `-s3-region` | `FMA_S3_REGION` | `us-east-1` | S3 region |
| `—` | `FMA_S3_ACCESS_KEY_ID` | `required` | S3 access key |
| `—` | `FMA_S3_SECRET_ACCESS_KEY` | `required` | S3 secret key |
| `—` | `FMA_S3_SESSION_TOKEN` | `empty` | Optional temporary credential token |
| `-cert` | `—` | `cert.pem` | Certificate chain object key in S3 |
| `-key` | `—` | `key.pem` | Private key object key in S3 |
| `-tls-dir` | `—` | `empty` | Read tls.crt and tls.key from a mounted directory instead of S3 |
| `-smtp` | `—` | `127.0.0.1:2525` | Inbound SMTP |
| `-submission` | `—` | `127.0.0.1:1587` | Authenticated SMTP with STARTTLS |
| `-smtps` | `—` | `127.0.0.1:1465` | Authenticated SMTP over TLS |
| `-pop3` | `—` | `127.0.0.1:1110` | POP3 with STLS |
| `-pop3s` | `—` | `127.0.0.1:1995` | POP3 over TLS |
| `-imap` | `—` | `127.0.0.1:1143` | IMAP with STARTTLS |
| `-imaps` | `—` | `127.0.0.1:1993` | IMAP over TLS |
| `-http` | `—` | `127.0.0.1:8080` | JMAP HTTP backend and / health check; publish through HTTPS |
| `-jmap-url` | `FMA_JMAP_URL` | `https://mail.<domain>` | Public HTTPS origin advertised to JMAP clients |
| `-outbound` | `FMA_OUTBOUND_MODE` | `disabled` | disabled, direct (MX), or relay (another SMTP server) |
| `—` | `FMA_RELAY_ADDR` | `required in relay` | Relay host:port |
| `—` | `FMA_RELAY_USER` | `required in relay` | Relay authentication username |
| `—` | `FMA_RELAY_PASSWORD` | `empty` | Relay password; required unless PASSWORD_FILE is set |
| `—` | `FMA_RELAY_PASSWORD_FILE` | `empty` | S3 object key containing the relay password |
| `—` | `FMA_RELAY_TLS` | `starttls` | starttls or implicit |
| `—` | `FMA_RELAY_CA_FILE` | `empty` | Optional CA certificate object key in S3 |
| `-queue-retry` | `—` | `1m` | Initial retry delay for SMTP delivery jobs |
| `-stream-workers` | `FMA_STREAM_WORKERS` | `4` | MIME ingestion workers (1–128); flag overrides environment |
| `-queue` | `—` | `false` | Inspect SMTP delivery jobs without message bodies |
| `-version` | `—` | `false` | Print version, commit and release time; no S3 needed |
| `-h` | `—` | `—` | Print command-line help |
| `—` | `LOG_LEVEL` | `info` | debug, info, warn, error; read before mail configuration |

The generator also reads the table below. S3, relay, outbound and logging environment variables from the binary table work in the generator too; its bucket default is `fma`, while other shared defaults match above.

| Generator environment variable | Default | Purpose |
| --- | --- | --- |
| `FMA_DOMAIN` | `required` | Mail domain |
| `FMA_JMAP_URL` | `https://mail.<domain>` | Public JMAP origin |
| `FMA_DEPLOY_USER` | `current user` | Service user |
| `FMA_DEPLOY_UID` | `selected user UID` | Service UID |
| `FMA_DEPLOY_HOME` | `selected user home` | Service home |
| `FMA_BINDIR` | `root: /usr/local/bin; user: ~/.local/bin` | Binary directory |
| `FMA_CONFIG_DIR` | `root: /etc/fma; user: ~/.config/fma` | Configuration directory |
| `FMA_SYSTEMD_USER_DIR` | `root: /etc/systemd/system; user: ~/.config/systemd/user` | Unit directory for the selected mode |
| `FMA_CERT_KEY` | `cert.pem` | Maps to -cert |
| `FMA_KEY_KEY` | `key.pem` | Maps to -key |
| `FMA_QUEUE_RETRY` | `1m` | Maps to -queue-retry |
| `FMA_STREAM_WORKERS` | `4` | MIME ingestion workers created at startup |
| `FMA_SMTP_PORT` | `2525` | Loopback listener port |
| `FMA_SUBMISSION_PORT` | `1587` | Loopback listener port |
| `FMA_SMTPS_PORT` | `1465` | Loopback listener port |
| `FMA_POP3_PORT` | `1110` | Loopback listener port |
| `FMA_POP3S_PORT` | `1995` | Loopback listener port |
| `FMA_IMAP_PORT` | `1143` | Loopback listener port |
| `FMA_IMAPS_PORT` | `1993` | Loopback listener port |
| `FMA_HTTP_PORT` | `8080` | Loopback listener port |
| `FMA_LINEAGE` | `/etc/letsencrypt/live/mail.<domain>` | Certbot certificate directory |
| `FMA_WEBROOT` | `/var/www/certbot` | ACME webroot |
| `FMA_OVERWRITE` | `no` | Allow replacing generated configuration |

Installer options are fixed as follows:

| Option / environment variable | Default | Purpose |
| --- | --- | --- |
| `--systemd` | off | Install binary and service |
| `--config-dir PATH` | generated by installer | Reuse generated configuration with --systemd |
| `--uninstall` | off | Remove this execution mode's installation |
| `FMA_INSTALL_DIR` | root: /usr/local/bin; user: ~/.local/bin | Binary installation directory |
| `FMA_DEPLOY_DIR` | root: /etc/fma/deploy; user: ~/.config/fma/deploy | Generator workspace |
| `FMA_REPO` | Jabberwocky238/fma | Release repository |

<a id="run-direct"></a>

### 3.1. Direct startup

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

<a id="run-systemd"></a>

### 3.2. systemd startup and gen.sh

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

<a id="run-docker"></a>

### 3.3. Docker startup

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

JMAP uses the HTTP backend on host loopback port 8080. Connect an HTTPS reverse proxy and set the matching `FMA_JMAP_URL`. Allow uploads of at least 4 GiB and disable proxy request buffering to local disk.

<a id="run-kubernetes"></a>

### 3.4. Kubernetes startup

[deploy/kubernetes/](deploy/kubernetes/) provides a Kustomize deployment: two replicas,
a configuration ConfigMap, a TCP LoadBalancer Service and a PodDisruptionBudget.
The pods run without root privileges, local data volumes or Kubernetes API credentials.
They share the same S3 bucket. The TCP Service exposes the seven mail ports; a separate HTTP Service
connects the JMAP backend to the Ingress. Your cluster must support LoadBalancer services, or you can
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

JMAP uses the `fma-http` ClusterIP Service and `ingress.yaml`, with HTTPS certificates from the same `fma-tls` Secret in namespace `fma`. Change the Ingress host and ConfigMap `FMA_JMAP_URL` to your domain. An existing Ingress controller is required; configure its upload limits and disable request buffering.

<a id="jmap"></a>

## 4. Using JMAP

JMAP clients discover the session at `https://mail.example.com/.well-known/jmap`
and authenticate using HTTP Basic with their existing account or alias and password.
Use the returned `apiUrl`, `uploadUrl`, `downloadUrl` and `primaryAccounts`; account
IDs are opaque, not the login name. Set `FMA_JMAP_URL` when the public origin differs.
The HTTP listener is behind your HTTPS proxy; `/` remains the unauthenticated health check.

```sh
export JMAP_USER=alice
export JMAP_PASSWORD='your-password'
curl --fail --user "$JMAP_USER:$JMAP_PASSWORD" \
  https://mail.example.com/.well-known/jmap
```

A JMAP client must support the Mail capability, not only JMAP contacts/calendars.
Configure its server/session URL and credentials above. Server-side API calls use
these capabilities:

```json
{
  "using": ["urn:ietf:params:jmap:core", "urn:ietf:params:jmap:mail"],
  "methodCalls": [
    ["Mailbox/get", {"accountId": "ACCOUNT_ID"}, "folders"],
    ["Email/query", {"accountId": "ACCOUNT_ID", "limit": 20,
      "sort": [{"property": "receivedAt", "isAscending": false}]}, "messages"],
    ["Email/get", {"accountId": "ACCOUNT_ID",
      "#ids": {"resultOf": "messages", "name": "Email/query", "path": "/ids"},
      "fetchAllBodyValues": true}, "bodies"]
  ]
}
```

Save this as `request.json`, replace `ACCOUNT_ID` with the discovered account ID,
and POST it to `apiUrl`:

```sh
curl --fail --user "$JMAP_USER:$JMAP_PASSWORD" \
  --header 'Content-Type: application/json' --data-binary @request.json \
  https://mail.example.com/api
```

| Operation | Methods / flow |
| --- | --- |
| Read and search | `Mailbox/get`, `Email/query`, `Email/get`, `Thread/get`, `SearchSnippet/get` |
| Folder edits | `Mailbox/set`: create, rename, parent, subscription and destroy |
| Message edits | `Email/set`: create drafts, update `keywords`/`mailboxIds`, destroy |
| Import MIME | POST RFC 5322 bytes to `uploadUrl`, then `Email/import` with its `blobId` and destination `mailboxIds` |
| Attachments | POST bytes to `uploadUrl`; reference the returned `blobId` in `Email/set` attachments. Download `Email/get` attachment `blobId` through `downloadUrl` |
| Send | `Identity/get` → `Email/set` draft → `EmailSubmission/set` using `emailId` and `identityId` |
| Delivery status | `EmailSubmission/get`; inspect `undoStatus` and per-recipient `deliveryStatus` |
| Incremental sync | Persist each type's `state`, call `/changes` with `sinceState`; refresh created/updated IDs and remove destroyed IDs |
| Search-result sync | `Email/queryChanges` with the prior `queryState`; restart the query if the server returns `cannotCalculateChanges` |
| Optimistic writes | Supply `ifInState`; on `stateMismatch`, refresh and reconcile before retrying |

For sending, add `urn:ietf:params:jmap:submission` to `using`. Example request body
(replace all capitalized IDs with values from the server):

```json
{
  "using": ["urn:ietf:params:jmap:core", "urn:ietf:params:jmap:mail", "urn:ietf:params:jmap:submission"],
  "methodCalls": [["EmailSubmission/set", {
    "accountId": "ACCOUNT_ID",
    "create": {"send": {"emailId": "DRAFT_EMAIL_ID", "identityId": "IDENTITY_ID"}},
    "onSuccessUpdateEmail": {"#send": {
      "mailboxIds/DRAFTS_MAILBOX_ID": null,
      "mailboxIds/SENT_MAILBOX_ID": true,
      "keywords/$draft": null
    }}
  }, "submit"]]
}
```

Upload a file using the session's upload URL after substituting `{accountId}`:

```sh
curl --fail --user "$JMAP_USER:$JMAP_PASSWORD" \
  --header 'Content-Type: application/octet-stream' \
  --header 'Content-Disposition: attachment; filename="document.pdf"' --data-binary @document.pdf \
  'https://mail.example.com/upload/ACCOUNT_ID/'
```

Include `{"blobId":"UPLOADED_BLOB_ID","type":"application/pdf","name":"document.pdf"}`
in the draft's `attachments` array. Multipart MIME, binary/empty attachments and
Unicode filenames are supported. Incoming attachments are resolved from messages
in the authenticated account; knowing another account's blob hash grants no access.
A single JMAP HTTP upload is limited to 4 GiB, and composed attachments total at most
4 GiB. Complete MIME messages may reach 6 GiB to allow for Base64 encoding and headers.
JMAP uploads stream raw bytes; Base64 encoding happens when composing MIME.

To/Cc/Bcc produce envelope recipients; Bcc is removed from the transmitted MIME.
External recipients need `direct` or `relay`; local delivery works with outbound
disabled. Queued submissions survive restarts and are swept by the existing
15-second scheduler. JMAP submission records retain delivery receipts, while their
active claim and retry index are cleared on completion. The claim window is 20
seconds; each JMAP transmission attempt is limited to 6 seconds to leave time for
fenced finalization and the library's clock-skew margin. Temporary failures retry.

SMTP, IMAP, POP3 and JMAP share mailboxes: SMTP delivery appears in JMAP; JMAP folder,
flag and message edits appear in IMAP/POP3. Synchronization uses durable `/changes`
and `/queryChanges` cursors and works across restarts and nodes. EventSource and
Web Push subscriptions are not enabled in this version; clients must poll changes.
Do not interpret the RFC table as a claim to implement every optional extension.

<a id="testing"></a>

## 5. CI, testing and Fals3y

### Local Fals3y

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

POP3/STLS and POP3S use [Jabberwocky238/go-pop3](https://github.com/Jabberwocky238/go-pop3), a performance fork of migadu/go-pop3.
The library handles protocol framing, TLS and SASL PLAIN; fma supplies S3 authentication
and mailbox sessions. `DELE` marks messages, `RSET` clears those marks, and `QUIT`
commits deletion. Disconnecting without `QUIT` keeps the messages.

### Tests and builds

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

JMAP integration test entry point:

```sh
python3 scripts/verify_jmap.py
```

This script reuses the isolated native Fals3y fixture and is included in `make test` and CI. It covers authentication/account isolation, alias/proxy restrictions, folder hierarchy, state conflicts, import/create/destroy, search/threads, attachment upload/download/reuse, cross-protocol reads and writes, To/Cc/Bcc, queueing and process restart. The storage adapter also runs naust-jmap's contract tests for atomic batches, assertions, ordered scans, concurrency and reopen.

### CI and releases

GitHub Actions runs formatting, vet, race, native Fals3y integration tests, and a
build on branch pushes and pull requests, using Go 1.25 and the current stable Go.
Release configuration is checked with GoReleaser as part of CI.

Push a semantic version tag to publish a GitHub Release after the same checks pass:

```sh
git tag v0.2.0
git push origin v0.2.0
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

<a id="bucket"></a>

## 6. Bucket layout

### Account kinds

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

| Object key | Contents |
| --- | --- |
| `<id>/.kind` | account / alias / proxy |
| `<account>/.password` | Account password |
| `<alias>/.alias` | Target local ID |
| `<proxy>/.proxy` | Forwarding address |
| `<proxy>/.proxy-errors/<id>.json` | Failure diagnostic without body |
| `<account>/.jmap/state.json` | Minimal account state: folders, identities, UID/state counters, lease and current transaction decision; no mail records or query indexes |
| `<account>/mail/<escaped-subject>_<timestamp>/<escaped-filename>` | Immutable streamed MIME or uploaded attachment bytes; gzip above 10 MiB |
| `<physical-object>.blob-<blobId>.json` | Immutable blob descriptor beside its bytes, recording the physical object and encoding/size; the ID lookup table exists only in memory |
| `<account>/mail/<mail-id>/attachments/<part-id>/<escaped-filename>` | Decoded MIME parts; gzip above 1 MiB |
| `<blob-descriptor>.mime.json` | MIME structure, part identities, sizes and preview under the same mail prefix |
| `<account>/mail/<mail-id>/.fma/*.fma.json` | Email records and single-message change history |
| `<account>/mail/.records/`, `mail/.history/`, `mail/.uploads/` | Threads/submissions, batch change history and upload records registered before content publication |
| `<owner-record>.prepare.<transaction>` | Immutable transaction record written before conditional publication; not a query index |
| `<account>/.jmap/blobs/*` | Compatibility reads of legacy content, descriptors, MIME metadata and part locators |
| `.outbox/<id>.json` | SMTP outbound task and embedded preclaim |
| `.lock` | Shared 15-second scan/renewal lease |
| `cert.pem, key.pem` | TLS certificate and key unless mounted from a Secret |

Legacy `<account>/<uid>.json`, `next`, `folders` and `.folders/` data is converted on the account's first access, retaining the original objects. A conditional create publishes the complete metadata image; failures cannot publish a partial mailbox. Existing UIDs and folder storage identities are preserved. Stop every old-version node before upgrading; do not mix legacy writers with the new version. Old objects no longer receive updates and may be cleaned externally after verifying backups and the new mailbox. The binary exposes no registration or management commands. Unreferenced uploads and MIME objects remain in S3; no local temporary files or new background garbage collector are used.

Physical mail directory IDs are `subject + UTC timestamp` with nanosecond precision.
RFC 2047 subjects are decoded, and each subject/filename path segment is percent-escaped
(including `/`, `%`, `?`, `#`, Unicode and dot-only segments), with a bounded length.
Object creation is conditional: a timestamp/name collision fails instead of overwriting data.
Raw MIME uses `message.eml`. HTTP uploads accept a filename in `Content-Disposition`;
without one they use `attachment.bin`, and without a subject they use `untitled`.
Incoming MIME is streamed concurrently to its original object and a parser that writes
individual decoded parts. JSON metadata is published after successful part and MIME
commits. First JMAP metadata reads use this record and attachment downloads read the
part object directly. The original MIME remains for IMAP/POP3, increasing storage use.
Legacy messages without these records retain their existing read path.
A JMAP blob ID remains a content identifier. Multipart upload writes directly to the
named object, followed by an immutable descriptor beside it, without copying the
whole object at commit. Descriptors retain the encoding information needed to read
bytes; ID lookup tables are rebuilt from object names and never written to S3.
Attachment parent relationships are derived from directories and checked against
MIME metadata; new `.origin.json` indexes are not written. Legacy part locators
are rebuilt in memory without new `.part` writes. Submission scheduling
discovers accounts through top-level prefixes, without `.jmap-queue` markers.

Existing objects remain readable. On first access, the old whole-account
`state.json` is split into owner records before a conditional account-file switch;
legacy indexes are omitted from the new format. Stop all older nodes before this
upgrade: they cannot read the new format. Each batch writes immutable prepare
objects, then publishes the transaction decision with an ETag condition. The next
write, a one-second flush loop or clean shutdown materializes committed records at
stable paths. Recovery follows the published decision; unpublished prepare objects
are invisible. Prepare objects, deletion tombstones and unreferenced blobs are
currently retained without automatic garbage collection.

Warm Get/MultiGet calls read memory, Scan uses ordered key ranges, and index-only
batches never write S3. Reads check the account version at most once per second;
writes always check it. Cold starts and changes made by other nodes require record
reads and index reconstruction, so recovery still scales with mailbox size. Indexes
for accessed accounts remain in process memory; there is currently no cache eviction.

SMTP uses a pinned performance fork through `go.mod replace` ([PR #312](https://github.com/emersion/go-smtp/pull/312)). Its single-file DATA reader patch adapts the cross-line scan from [uponusolutions/go-smtp](https://github.com/uponusolutions/go-smtp/blob/86ff2622fb52f86371265b74a976333ff53c10a0/internal/textsmtp/dotreader.go), retaining the existing server API, default 4 KiB protocol buffer, and line-length checks. fma adds a reusable 1 MiB TCP input buffer below TLS to coalesce small reads. POP3 directly imports the independent `github.com/Jabberwocky238/go-pop3` module; its `main` includes the performance fix and fork documentation, while `pr` submits only the patch to upstream ([PR #3](https://github.com/migadu/go-pop3/pull/3)).

POP3 v0.1.6 adds bounded ARM64 vector scanning with a portable fallback. The isolated 2 GiB writer benchmark gains another 2.04x throughput; complete-download gains remain unproven. CPU, RSS, input-shape comparisons and reproduction commands are recorded in [PERFORMANCE.md](PERFORMANCE.md).

The task pool creates four workers at startup; configure `FMA_STREAM_WORKERS=16` or `-stream-workers 16`. Idle workers take MIME ingestion tasks from one shared queue whose capacity equals the worker count. A full queue applies backpressure. The three 1 MiB rotating blocks are allocated only when an admitted task starts. Each admitted task runs two auxiliary goroutines for original-MIME SHA-256 and gzip/object writing alongside reception and parsing; each block is reused only after all consumers finish. These helpers do not use additional pool slots, so a single configured worker remains safe. Protocol connections and S3 multipart requests retain their own concurrency. More workers allow more messages to be processed concurrently; they do not split one Base64 stream into eight parallel decoders. Cancellation and shutdown notify tasks and drain the queue; raw and decoded compression pools remain separate.

S3 upload buffers use 1 MiB blocks, grouped into 8 MiB multipart requests without concatenation. Up to four parts are active per upload, with a global 1 GiB active-buffer limit allocated on demand. Cross-account object copies use S3 CopyObject or UploadPartCopy. JMAP MIME parsing uses the pinned [streaming fork](https://github.com/Jabberwocky238/naust-jmap/commit/ea60168) to reuse buffers and decode blocks. It now persists MIME metadata and decoded parts at ingestion; these are durable storage records, available on the first request after a restart. The receive/parse pipeline transfers ownership of three 1 MiB blocks. Raw and decoded streams have separate bounded compression pools to avoid mutual resource starvation.

See [PERFORMANCE.md](PERFORMANCE.md) for measured throughput and the remaining limits.

The throughput benchmark defaults to a 2 GiB attachment and reports both wire-byte and
attachment-byte MiB/s. `--smtp-transfer bdat` separately measures the existing SMTP
CHUNKING path; the default remains DATA so its dot-processing cost stays visible:

```sh
python3 scripts/test_streaming.py --mail --size-mib 2048 --report /tmp/fma-data.json
python3 scripts/test_streaming.py --mail --size-mib 2048 --smtp-transfer bdat --report /tmp/fma-bdat.json
```

<a id="credits"></a>

## 7. License and acknowledgements

[MIT](LICENSE). Copyright © 2026 Jabberwocky238.

Thanks to the following projects. Each retains its own license; fma's MIT license does not replace dependency licenses.

| Project | Purpose | License |
| --- | --- | --- |
| [emersion/go-smtp](https://github.com/emersion/go-smtp) | SMTP | [MIT](https://github.com/emersion/go-smtp/blob/v0.25.0/LICENSE) |
| [emersion/go-imap](https://github.com/emersion/go-imap) | IMAP | [MIT](https://github.com/emersion/go-imap/blob/v1.2.1/LICENSE) |
| [Jabberwocky238/go-pop3](https://github.com/Jabberwocky238/go-pop3) (fork of migadu/go-pop3) | POP3 | [MIT](https://github.com/migadu/go-pop3/blob/v0.1.4/LICENSE) |
| [naust-mail/naust-jmap](https://github.com/naust-mail/naust-jmap) | JMAP Core and Mail | [Apache-2.0](https://github.com/naust-mail/naust-jmap/blob/main/LICENSE) |
| [emersion/go-message](https://github.com/emersion/go-message) | MIME | [MIT](https://github.com/emersion/go-message/blob/v0.18.2/LICENSE) |
| [emersion/go-sasl](https://github.com/emersion/go-sasl) | SASL | [MIT](https://github.com/emersion/go-sasl/blob/master/LICENSE) |
| [aws/aws-sdk-go-v2](https://github.com/aws/aws-sdk-go-v2) | S3 SDK | [Apache-2.0](https://github.com/aws/aws-sdk-go-v2/blob/v1.36.3/LICENSE) |
| [aws/smithy-go](https://github.com/aws/smithy-go) | AWS SDK support | [Apache-2.0](https://github.com/aws/smithy-go/blob/v1.22.2/LICENSE) |
| [klauspost/compress](https://github.com/klauspost/compress) | Streaming gzip compression and decompression | [BSD-3-Clause](https://github.com/klauspost/compress/blob/v1.20.0/LICENSE) |
| [WireGuard](https://github.com/WireGuard/wireguard-go/blob/ecfc5a8d54462e18e13c72173e2623d16d8e25a0/device/pools.go) | WaitPool design, adapted with cancellation | [MIT](https://github.com/WireGuard/wireguard-go/blob/ecfc5a8d54462e18e13c72173e2623d16d8e25a0/LICENSE) |
| [LukeOfEarth/fals3y](https://github.com/LukeOfEarth/fals3y) | Native S3 for development and integration tests | [MIT](https://github.com/LukeOfEarth/fals3y/blob/v0.3.0/LICENSE) |
| [golang.org/x/text](https://pkg.go.dev/golang.org/x/text) | Character encodings | [BSD-3-Clause](https://cs.opensource.google/go/x/text/+/refs/tags/v0.34.0:LICENSE) |
