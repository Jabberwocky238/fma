# Streaming performance measurements

## Old and new library results

All tests below ran on Apple M4, 32 GiB RAM, macOS arm64. POP3 uses Go 1.25.3;
the isolated SMTP contributor/reader-only comparisons use Go 1.25.14. An isolated
2 GiB writer test is distinct from a 2 GiB decoded attachment, which becomes
2,938,662,361 MIME bytes. Small sequential samples are not production guarantees.

| Comparison | Old library / revision | New library / revision | Old result | New result | Interpretation |
| --- | --- | --- | ---: | ---: | --- |
| POP3 isolated writer, three-run median | migadu/go-pop3, 8794dc8d9e68 | Jabberwocky238/go-pop3, cbaefcdb4447 | 16.839746083 s | 0.248280291 s | 67.8x throughput; portable batching |
| POP3 matched isolated writer, three samples × five operations | v0.1.6, portable scanner selected by pop3scalar | v0.1.6, ARM64 vector scanner | 0.267945275 s | 0.131334250 s | 2.04x additional throughput; about 128x versus historical upstream |
| POP3 historical complete RETR | Upstream writer, named-storage run | First portable batching run | 30.077 s / 32.96 CPU s | 1.790 s / 1.58 CPU s | Historical runs; application state differs |
| SMTP isolated DATA reader, historical median | Original upstream reader | Fork bulk DATA reader | 5.039 s | 0.486 s | 10.4x; excludes complete SMTP work |
| SMTP matched reader-only comparison | Fork f3ad4805e305 | Fork b0673510e580 | 634.885 ms | 431.226 ms | 1.47x; historical SMTP record retained |
| SMTP same fixture, different readers | Fork f3ad4805e305 | uponusolutions 86ff2622fb52 | 466.296 ms | 308.776 ms | Contributor is 1.51x; correctness differences remain |

## Repositories, versions, optimization and PRs

| Component | Repository and revision / version | Optimization or role | Corresponding PR |
| --- | --- | --- | --- |
| POP3 baseline | [migadu/go-pop3](https://github.com/migadu/go-pop3/tree/8794dc8d9e6897cf312f9ef10ebc1e07c28a87c2), 8794dc8d9e68 | Per-byte body writer, upstream baseline | [PR #3](https://github.com/migadu/go-pop3/pull/3) |
| POP3 portable fix | [Jabberwocky238/go-pop3](https://github.com/Jabberwocky238/go-pop3/commit/cbaefcdb4447f8b60524b06472740ff228a10361), cbaefcdb4447; independent v0.1.5 at c27acc581bb3 | Batch unchanged spans and reuse inserted-byte storage; about 2 GiB/op allocation becomes 4,248 B/op | [PR #3](https://github.com/migadu/go-pop3/pull/3); earlier #1/#2 closed |
| POP3 vector fix / application dependency | [Jabberwocky238/go-pop3 v0.1.6](https://github.com/Jabberwocky238/go-pop3/tree/v0.1.6), b70bb165ca14; patch 5a8a1ebd43ca | Bounded 16-byte ARM64 NEON validation, scalar fallback for exceptional input; no new buffer | [pr → migadu/main, PR #3](https://github.com/migadu/go-pop3/pull/3) |
| SMTP historical forks | [Jabberwocky238/go-smtp](https://github.com/Jabberwocky238/go-smtp), f3ad4805e305 then b0673510e580; v0.25.1-0.20260910174640-b0673510e580 | Bulk DATA scan; latest recorded patch narrows production changes to data.go | [emersion/go-smtp PR #312](https://github.com/emersion/go-smtp/pull/312); reference only, outside current POP3 work |
| SMTP contributor | [uponusolutions/go-smtp](https://github.com/uponusolutions/go-smtp/tree/86ff2622fb52f86371265b74a976333ff53c10a0), 86ff2622fb52 | Cross-line CRLF/dot search; separate correctness limitations | [Source linked in PR #312](https://github.com/emersion/go-smtp/pull/312) |
| Compression | [klauspost/compress](https://github.com/klauspost/compress/tree/v1.20.0), v1.20.0 | gzip BestSpeed with bounded buffers | No PR from this investigation |
| New storage dependency | [Fals3y source revision](https://github.com/Jabberwocky238/fals3y/commit/b8e48bfb7fc05fcaec6e21326b4ea34ec5dfa3a6), CLI 0.3.1-dev.b8e48bf | Shared-file storage optimization supplied by the user; not a POP3 library patch | No storage PR filed in this POP3 work |

Benchmark source: [POP3 writer and input-shape benchmarks](https://github.com/Jabberwocky238/go-pop3/blob/v0.1.6/pop3server/dotstuff_bulk_test.go). The implementation and reproducible test parameters are described here; embedded source and console logs are omitted.

## UTC measurement provenance

Dates and times use UTC (Z). The historical programs did not record per-operation
UTC start/end instants. The intervals below come from the local measurement
artifact's creation and last-write metadata: they bracket log recording, include
startup/build/other phases, and must not be confused with the timed operation.
They are evidence timestamps, not invented exact RETR start/end times. Where an
artifact was unavailable, only the previously recorded date is retained.
All rows below were recorded on **2026-09-10 UTC**.

| Measurement | Recording start UTC | Recording end UTC | Evidence |
| --- | --- | --- | --- |
| Old POP3 writer | 13:38:40.872Z | 13:39:31.776Z | fma-pop-fork-before.log |
| New portable POP3 writer | 13:37:49.647Z | 13:37:50.727Z | fma-pop-fork-bench.log |
| v0.1.6 portable isolated control | 17:46:28.647Z | 17:46:34.207Z | fma-pop-clean-scalar.log |
| v0.1.6 vector isolated | 17:46:34.217Z | 17:46:37.260Z | fma-pop-clean-vector.log |
| Old SMTP reader | 13:38:25.008Z | 13:38:40.612Z | fma-smtp-fork-before.log |
| First fixed SMTP reader | 13:37:34.203Z | 13:37:36.077Z | fma-smtp-fork-bench.log |
| v0.1.5 resource run | 17:26:38.394Z | 17:27:30.349Z | fma-pop-v015-resources-2g.log |
| Latest vector RETR / full run | 18:28:43.159Z | 18:29:21.519Z | fma-shared-b8e48bf-vector.log |
| Latest portable RETR / full run | 18:29:21.680Z | 18:30:01.383Z | fma-shared-b8e48bf-scalar.log |
| Latest compressible RETR / full run | 18:30:01.502Z | 18:30:44.570Z | fma-shared-b8e48bf-compressible.log |
| SMTP reader-only matched samples f3ad4805e305 / b0673510e580 | Exact interval unrecorded | Exact interval unrecorded | Date retained: 2026-09-10 |

## Latest POP3 results: Fals3y shared-file build

Storage CLI: **0.3.1-dev.b8e48bf**, source **b8e48bfb7fc05fcaec6e21326b4ea34ec5dfa3a6**,
built with Go 1.26.0, unchanged source according to embedded build metadata.
Executable SHA-256: **1298e405c1631fbded92892eadc7252d8e2e319dfdf42f48c188ae3fd82ffb4b**.
Application dependency: **github.com/Jabberwocky238/go-pop3 v0.1.6**; no local
replacement. Vector, portable control and compressible runs ran sequentially.
The portable control changes only the POP3-specific pop3scalar build tag.

| Mode / payload | RETR wall (s) | Server CPU (s) | Average CPU, one core = 100% | Initial RSS (bytes) | Peak RSS (bytes) | Sampled increase (bytes) | MIME MiB/s | Attachment MiB/s |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| vector | 1.893 | 1.550 | 81.90% | 182108160 | 182108160 | 0 | 1480.47 | 1081.88 |
| scalar | 1.848 | 1.630 | 88.22% | 173441024 | 173457408 | 16384 | 1516.52 | 1108.23 |
| compressible | 2.274 | 2.350 | 103.36% | 106987520 | 107003904 | 16384 | 1232.42 | 900.62 |

Each RETR downloads 2,938,662,361 MIME bytes for a 2,147,483,648-byte attachment.
TLS setup/login and QUIT are outside timing. CPU covers all email-server threads,
excluding Fals3y/client CPU; RSS is sampled every 0.05 s and includes prior phases'
retained memory. Zero sampled growth is not zero allocation. All runs passed
content hashes, protocol checks and the 256 MiB server RSS ceiling. Main source,
module files and harness hashes matched across runs and were unchanged within each
run; the storage fingerprint also remained unchanged.

The vector run used 4.9% less CPU time than the portable run (1.55 vs 1.63 s),
but took 2.4% longer wall time (1.893 vs 1.848 s). One sample per mode does not
establish significance. The earlier storage build's vector runs were 1.849 and
2.269 s, using 1.56 and 1.77 CPU s. These results do not demonstrate a stable
POP3 download speedup from either the vector patch or shared-file storage.
The controlled isolated writer result remains 2.04x; it excludes storage and TLS.

Optimization priorities for POP3: retain portable batching and bounded NEON
validation; preserve exceptional-input fallback and newline-free scanning; target
network/TLS costs only after profiling; measure concurrent CPU/RSS before making
capacity claims. No additional runtime change or SMTP PR edit was made for this
storage retest. Detailed historical measurements and alternative-library findings
remain below, with embedded source and logs replaced by result tables.

### Exact original POP3 writer samples

Old upstream; original samples follow.

| Series / benchmark | Iterations per sample | ns/op | MB/s (decimal) | B/op | allocs/op |
| --- | --- | --- | --- | --- | --- |
| 1: BenchmarkDotStuffWriter-10 | 1, 1, 1 | 16839746083, 16708265083, 16887654625 | 127.52, 128.53, 127.16 | 2147538968, 2147531064, 2147526712 | 2147483931, 2147483890, 2147483894 |

All samples passed. Package test durations: github.com/migadu/go-pop3/pop3server 50.600s.

Portable fix; original samples follow.

| Series / benchmark | Iterations per sample | ns/op | MB/s (decimal) | B/op | allocs/op |
| --- | --- | --- | --- | --- | --- |
| 1: BenchmarkDotStuffWriter-10 | 1, 1, 1 | 260682083, 247973417, 248280291 | 8237.94, 8660.14, 8649.43 | 4248, 4248, 4248 | 6, 6, 6 |

All samples passed. Package test durations: github.com/migadu/go-pop3/pop3server 0.915s.

## Historical SMTP reader-only patch (2026-09-10)

The application now pins `Jabberwocky238/go-smtp` at
`b0673510e58009b47a2c9b6e6ca5fc189c3c5ba4`
(`v0.25.1-0.20260910174640-b0673510e580`) through its existing module replacement.
[PR #312](https://github.com/emersion/go-smtp/pull/312) remains open and now changes
only `data.go` in production, plus two test files. The cross-line `CRLF + dot`
scan is adapted from [ml1nk / UPONU Solutions' reader at `86ff2622fb52`](https://github.com/uponusolutions/go-smtp/blob/86ff2622fb52f86371265b74a976333ff53c10a0/internal/textsmtp/dotreader.go),
with source attribution and MIT copyright credit in the file. We retained the
existing state machine for first-line dots, empty live-connection terminators,
fragment boundaries, limits and consecutive CR behavior. The application's SMTP
API is unchanged; it does not depend on the contributor's reorganized server API.

The previous `ReadBufferSize` option and line-limit optimization have been removed
from the PR to keep the production patch confined to one file. The application
therefore uses the default 4 KiB SMTP input buffer and original line-length checks.
Its separate compressed-S3 buffering remains as configured. Older numbers below
were recorded with different revisions/settings and are historical.

### Controlled reader comparison

Apple M4, macOS arm64, Go 1.25.14, default GOMAXPROCS, no race/profiling;
sequential runs of the same bounded 2 GiB MIME-like fixture, 4 KiB input buffer,
128 KiB copy buffer, `-benchtime=1x -count=3`:

| Revision | Runs (ms) | Median (ms) | B/op | allocs/op |
| --- | --- | ---: | ---: | ---: |
| Previous PR `f3ad4805e305` | 634.885, 627.195, 648.223 | 634.885 | 4,360 | 7 |
| Reader-only `b0673510e580` | 427.729, 435.903, 431.226 | 431.226 | 4,360 | 7 |

The new reader takes 32.1% less time (1.47x throughput). This excludes line-length
validation, sockets, TLS, MIME parsing, compression and storage. It is not an
end-to-end speedup claim, nor a rerun of the unmodified contributor's reader.

### Application validation on the new pinned module

Library race tests, a 20-second differential fuzz run, and 5,000 deterministic
random differential cases passed. The application's `make test` passed with the
same source before publishing, including native S3/protocol tests, deployment and
installation tests, and the 32 MiB streamed attachment regression. After pinning
the published module, `go test ./...` and the full 2 GiB round trip also passed:

The new end-to-end run used Go 1.25.3, native Fals3y 0.3.1-rc.1, and the
incompressible repeated-block fixture: 2,147,483,648 attachment bytes and
2,938,662,361 MIME bytes. SMTP upload took **10.099 s** (202.79 attachment MiB/s),
with 5.461 s receiving/compressing/writing, 3.426 s committing, and 1.202 s importing.
Maximum sampled RSS across the run was **167.4 MiB**, below the 256 MiB test ceiling.
All download hashes matched. The report is `/tmp/fma-smtp-cross-line.json`.

This single run is slower than the historical 8.583 s application result. It uses
the restored default SMTP buffer and original line checks; the runs are not paired,
so the timing difference cannot be attributed to the reader alone. The reader-only
scope trades the previous broader optimization for a smaller upstream patch.

## Earlier measurements

Measured on 2026-09-10, Apple M4, 32 GiB RAM, Go 1.25.3, macOS arm64,
using native local Fals3y. Baseline: `427b06e`; final comparison: named physical
objects plus the pinned SMTP/POP3 fixes in the working tree.

Each end-to-end variant used one 2 GiB synthetic binary attachment (a repeated
random 1 MiB block), bounded client buffers, and SHA-256 verification after download.
Base64 and CRLF expand it to 2,938,662,361 MIME bytes. These are single runs per
end-to-end variant, not production performance guarantees.

| Operation | Original (s) | Named objects (s) | First library fixes (s) | Final (s) | Final attachment MiB/s |
| --- | ---: | ---: | ---: | ---: | ---: |
| JMAP upload | 8.166 | 4.903 | 5.059 | 4.964 | 412.6 |
| JMAP download | 0.871 | 0.875 | 0.878 | 0.869 | 2356.7 |
| SMTP DATA upload | 21.654 | 16.892 | 12.479 | 8.583 | 238.6 |
| IMAP APPEND | 13.788 | 8.914 | 9.088 | 9.043 | 226.5 |
| IMAP FETCH | 1.961 | 1.868 | 1.905 | 1.920 | 1066.7 |
| POP3 RETR | 29.998 | 30.077 | 1.790 | 1.794 | 1141.6 |

Maximum sampled fma RSS in the final run: 166.2 MiB.
The original final S3 object copy cost 2.878 s and has been eliminated. In the final
run, CompleteMultipartUpload took 3.432 s and
index publication 0.0065 s.

The requested **upload + download <= 3 s** target is not met. JMAP totaled
5.833 s,
and SMTP DATA + IMAP FETCH totaled
10.503 s.
Three direct S3 PUT/GET baseline runs, bypassing mail and gzip, totaled
3.863, 3.907 and 3.938 s. These results describe this local Fals3y setup only.

The [SMTP reader comparison](#smtp-data-benchmark-comparison) below includes both
benchmark methods and exact results. The [POP3 server review](#pop3-server-replacement-review)
records the alternative-library investigation and reproducible protocol findings.

## Protocol fixes

| Isolated transformation, 2 GiB | Before median | After median | Speedup |
| --- | ---: | ---: | ---: |
| SMTP DATA reader | 5.039 s | 0.486 s | 10.4× |
| POP3 dot-stuffing writer | 16.840 s | 0.248 s | 67.8× |

The same benchmark was run three times on upstream HEAD and on the fix, without
CPU profiling. POP3 allocations fell from approximately 2.15 billion/op to 6/op,
and allocated memory from about 2 GiB/op to 4,248 B/op. SMTP remained at 7 allocs/op.
These benchmarks exclude network, TLS, MIME parsing, Base64 conversion and storage.
They must not be presented as end-to-end throughput.

- SMTP fork: `Jabberwocky238/go-smtp`, commit `f3ad4805e305`;
  [upstream PR #312](https://github.com/emersion/go-smtp/pull/312).
- POP3 fork: `Jabberwocky238/go-pop3`, performance commit `cbaefcdb4447`;
  [upstream PR #3](https://github.com/migadu/go-pop3/pull/3) uses the dedicated `pr` branch.
  The fork's `main` additionally uses its independent module path and documents the
  performance changes and benchmark in its README. Earlier PRs #1 and #2 are closed.

SMTP remains pinned through `go.mod replace`. POP3 is now a direct, pinned dependency
on `github.com/Jabberwocky238/go-pop3`, without a replacement directive. The timing
results above were measured before this module-path-only migration.
The fixes batch ordinary byte spans, preserving special boundary handling. Protocol
unit/race tests, fragmented input/output cases, POP3 fuzzing, the embedding server's
full test suite and 2 GiB attachment round trips passed.

SMTP CHUNKING/BDAT was measured separately before these library fixes: 8.242 s for
the 2 GiB attachment upload. This is a different, existing protocol path and is not
substituted for the DATA measurement.

The follow-up fixes `lineLimitReader`'s byte loop and configures 128 KiB SMTP and
compressed-S3 input buffers. The combined DATA + line-limit microbenchmark improved
from 1.739 s to 0.705 s (three-run medians versus the first fork commit; both 8 allocs).
End-to-end SMTP fell from 12.479 s to **8.583 s**, a 31.2% reduction. Its measured
stages were 2.676 s receiving/writing multipart data, 4.778 s committing S3, and
1.120 s importing the message. In this setup S3 finalization is now the largest
stage; it is included before the SMTP success reply. The report retains these stage measurements. Temporary CPU profiling was used only for diagnosis and is not
part of the production server or final timed run.

## Compressible 2 GiB round trip

A separate final run with the deterministic compressible fixture measured JMAP
upload at 1.466 s and download at 0.822 s (2.288 s combined). SMTP DATA upload took
5.524 s and IMAP FETCH 2.127 s. SMTP spent 3.752 s receiving/compressing/writing,
0.227 s committing, and 1.535 s importing. All downloaded hashes matched.
This fixture meets the 3 s goal for the direct blob round trip, but not SMTP mail;
its numbers must not be substituted for the incompressible table above.

## Gzip baseline

A separate cached, in-memory gzip.BestSpeed microbenchmark (128 KiB writes/reads,
three repetitions, median) took about 0.285 s to compress and 0.237 s to decompress
2 GiB of synthetic binary data. Corresponding Base64 MIME took 0.397 s and 0.336 s.
These timings exclude storage, network, TLS, SHA-256 and Base64 processing.
The compression implementation and level were not changed.

## Reproduce

Run variants sequentially so they do not compete for disk or CPU:

JSON reports include process CPU seconds, sampled RSS, elapsed time, wire-byte and
attachment-byte throughput, physical object key, storage encoding, S3 commit timings,
and content hashes. The metadata-only phase reports time rather than wire throughput.
CI keeps a 32 MiB regression run; `make benchmark` uses the 2 GiB default.

## Library investigation method

The investigation used GitHub metadata to discover candidates, followed by source
inspection at fixed commits. Star counts and repository update timestamps were not
treated as evidence of protocol correctness or large-message throughput.

For SMTP, both ordinary PR comments and inline review comments were read.

emersion suggested `ReadSlice` plus `UnreadByte` as an alternative implementation.
This was a code-review suggestion, not an existing application acceleration setting.
ml1nk supplied the uponusolutions fork for comparison. The review covered its
`internal/textsmtp/{dotreader,bdatreader,dotwriter,bdatwriter,textproto}.go`,
corresponding tests, `server/conn.go`, and `client/client.go`.
Its DATA reader searches across lines for CRLF followed by a dot; its BDAT reader
consumes declared chunk lengths; its client `Content(size)` prefers BDAT when
CHUNKING is available. Existing tests were run before adding boundary cases and
the shared benchmark below. Production code in the candidate checkout was unchanged.

For POP3, the initially proposed `knadh/go-pop3` was excluded after reading its
README and `pop3.go`: it is a client library, and RETR/RETRRaw collect the full
response in `bytes.Buffer`. It cannot replace our server's Session interface.
Other server candidates were discovered through GitHub search, repository metadata, README inspection and fixed-revision checkouts.

The four POP3 reviews covered storage interfaces, RETR/TOP, authentication, TLS,
deferred deletion, connection lifecycle and existing tests. Small reproduction
tests were added only in temporary checkouts. No candidate was integrated into
this application, and no candidate received a measured 2 GiB end-to-end ranking.
The results below distinguish source observations, existing test results and
newly reproduced failures. No review comments or PRs were posted to these four
candidate projects during this investigation.

## SMTP DATA benchmark comparison

This comparison supplements [PR #312](https://github.com/emersion/go-smtp/pull/312).
It compares the reader implementations, reproduces the contributor's existing
benchmark separately, and distinguishes both from the application's SMTP upload.
No end-to-end result for the contributor's fork has been measured here.

### Environment and revisions

Measured on 2026-09-10, Apple M4, 32 GiB RAM, macOS arm64. Both reader implementations
were tested with **Go 1.25.14**, default GOMAXPROCS (benchmark suffix `-10`), without
CPU profiling or race instrumentation. Commands ran sequentially, without another
benchmark running alongside them. This is a local comparison, not a production SLA.

- Our PR branch: [`f3ad4805e305`](https://github.com/Jabberwocky238/go-smtp/tree/f3ad4805e30536827a70fffd0dbb79cdc76d9461).
- Contributor's fork: [`86ff2622fb52`](https://github.com/uponusolutions/go-smtp/tree/86ff2622fb52f86371265b74a976333ff53c10a0).
- Contributor's production code was unchanged; only local comparison tests were added.

### A. Same benchmark, different DATA readers

**Input:** exactly 2 GiB (2,147,483,648 bytes) from a repeating 1,048,554-byte block
of 76 ASCII `x` bytes followed by CRLF, then the suffix `\r\n.\r\n`.
This models Base64 line lengths; it is **2 GiB of MIME-like wire content, not a
2 GiB decoded attachment**. It is not random data and does not exercise ordinary
dot-stuffed lines. The final suffix supplies a complete DATA terminator.

**Pipeline:** bounded repeated reader → 4 KiB `bufio.Reader` → DATA reader →
`io.CopyBuffer` with a 128 KiB buffer → a discard writer without `ReaderFrom`.
The input fixture and copy buffer are allocated before the timer starts.
Reader construction, input generation by copying the repeated block, and DATA
processing are timed. No whole 2 GiB buffer is allocated.

**Checks:** the copy must return no error and exactly 2 GiB + 2 decoded bytes.
The extra two bytes are the suffix's initial CRLF; the terminator is removed.
This benchmark checks length and completion, not a content hash. Protocol correctness
is tested separately. The configured message-size limit is disabled in both readers.

**Excluded:** line-length validation, sockets, TLS, gzip, Base64 conversion, MIME
parsing, S3, mailbox updates and the SMTP success reply. In particular, neither our
128 KiB connection setting nor our line-limit optimization is measured here.

Three single-iteration samples in one process per implementation (`-benchtime=1x -count=3`), summarized by median:

| Reader | Run 1 (ms) | Run 2 (ms) | Run 3 (ms) | Median (ms) | B/op | allocs/op |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| Our PR | 473.348 | 466.296 | 464.717 | **466.296** | 4,360 | 7 |
| Contributor | 309.619 | 308.776 | 306.564 | **308.776** | 4,360 | 7 |

On this fixture, the contributor's median is **33.8% less time**, or **1.51× throughput**.
These three runs do not establish a statistical confidence interval or an equivalent
reduction in total SMTP upload time. Go's printed MB/s uses decimal megabytes.

#### Exact individual measurements

Our checkout already contains `BenchmarkDataReader/MIME` in
[`data_test.go`](https://github.com/Jabberwocky238/go-smtp/blob/f3ad4805e30536827a70fffd0dbb79cdc76d9461/data_test.go).

Our exact samples:

| Series / benchmark | Iterations per sample | ns/op | MB/s (decimal) | B/op | allocs/op |
| --- | --- | --- | --- | --- | --- |
| 1: BenchmarkDataReader/MIME-10 | 1, 1, 1 | 473347708, 466295875, 464717083 | 4536.80, 4605.41, 4621.06 | 4360, 4360, 4360 | 7, 7, 7 |

All samples passed. Package test durations: github.com/emersion/go-smtp 2.255s.

Contributor, under the same benchmark:

| Series / benchmark | Iterations per sample | ns/op | MB/s (decimal) | B/op | allocs/op |
| --- | --- | --- | --- | --- | --- |
| 1: BenchmarkDataReader/MIME-10 | 1, 1, 1 | 309618917, 308775708, 306564167 | 6935.89, 6954.83, 7005.01 | 4360, 4360, 4360 | 7, 7, 7 |

All samples passed. Package test durations: github.com/uponusolutions/go-smtp/internal/textsmtp 1.076s.

### B. Contributor's original benchmark, unchanged

Source: [`BenchmarkDotReader`](https://github.com/uponusolutions/go-smtp/blob/86ff2622fb52f86371265b74a976333ff53c10a0/internal/textsmtp/dotreader_test.go#L226).

This uses **4 MiB of crypto-random input**, transformed using the standard library's
DotWriter, then repeatedly read from memory. `Legacy` is **`net/textproto.DotReader`**,
not emersion's DATA reader and not our PR. The standard-library reader normalizes
CRLF to LF, whereas SMTP DATA retains CRLF. The `SimpleReader` variants use the
contributor's `tester.Buffer` instead of `bytes.Reader`.

The original fixture setup does not close the DotWriter before taking `buf.Bytes()`;
therefore it does not explicitly finalize/flush the DATA stream. It also ignores
copy errors and output length. `SetBytes(4 MiB)` refers to source bytes, not a
verified encoded or decoded byte count. These are limitations of the original
benchmark as written; it is measured unchanged, not used as a correctness
check or as a direct speedup comparison with our PR.

Three benchmark samples per subbenchmark, each using adaptive iteration counts targeting approximately one second:

| Original subbenchmark | Median ms/op |
| --- | ---: |
| Legacy | 10.920 |
| Optimized | 0.584 |
| LegacySimpleReader | 11.124 |
| OptimizedSimpleReader | 0.587 |

| Series / benchmark | Iterations per sample | ns/op | MB/s (decimal) | B/op | allocs/op |
| --- | --- | --- | --- | --- | --- |
| 1: BenchmarkDotReader/Legacy-10 | 109, 100, 100 | 10919608, 10891542, 10968429 | 384.11, 385.10, 382.40 | 4392, 4318, 4400 | 5, 5, 5 |
| 1: BenchmarkDotReader/Optimized-10 | 1880, 2090, 1951 | 607847, 570910, 584152 | 6900.27, 7346.70, 7180.16 | 4277, 4273, 4276 | 4, 4, 4 |
| 1: BenchmarkDotReader/LegacySimpleReader-10 | 100, 100, 100 | 11123928, 10987695, 11178118 | 377.05, 381.73, 375.22 | 4384, 4384, 4384 | 5, 5, 5 |
| 1: BenchmarkDotReader/OptimizedSimpleReader-10 | 1872, 2114, 2145 | 613129, 573687, 586674 | 6840.82, 7311.13, 7149.30 | 4261, 4261, 4261 | 4, 4, 4 |

All samples passed. Package test durations: github.com/uponusolutions/go-smtp/internal/textsmtp 13.960s.

### C. Application measurements have a different scope

Our previously measured end-to-end run used Go **1.25.3**, native local Fals3y,
and a **2 GiB decoded attachment** made from a repeating random 1 MiB block.
Base64, MIME headers and boundaries expand that to **2,938,662,361 MIME bytes**.
The timer covers SMTP connection/EHLO/envelope, DATA transfer and the successful
server response after storage and import. Downloads were SHA-256 checked.
The process stayed within the benchmark's 256 MiB RSS ceiling.

| Application stage | Seconds |
| --- | ---: |
| Receive/compress/write multipart data | 2.676 |
| Commit S3 object and index | 4.778 |
| Import into mailbox | 1.120 |
| Complete SMTP operation, including protocol overhead | **8.583** |

Earlier application measurements were 16.892 s before the protocol fixes and
12.479 s after the first reader fixes. The 8.583 s final result includes additional
line-limit and buffering changes. Each is a single end-to-end run; they are not
paired statistical samples. The 0.466 s / 0.309 s reader comparison above cannot
be substituted into this table or used to claim a measured end-to-end result for
the contributor's fork. The requested 3 s SMTP round-trip target remains unmet.

The application method and other protocol results are recorded at the beginning of this document.

### Correctness observations separate from performance

The contributor's existing `go test ./internal/textsmtp` suite passed. Two additional
local cases failed at the pinned revision: initial `..first` remains `..first`
instead of `.first`, and an empty DATA terminator (`.\r\n`) blocks on a live pipe
kept open after the terminator (a 200 ms deadline was used). With a finite reader,
the empty case instead returns the terminator bytes with `io.ErrUnexpectedEOF`.
These observations are why the faster scan is a candidate for adaptation and
further testing, rather than evidence that the whole fork can replace ours unchanged.

## POP3 server replacement review

Reviewed on 2026-09-10, Apple M4, macOS arm64, Go 1.25.3. The reviewed versions of
all four candidates require protocol fixes or substantial integration changes
before they could replace our patched `migadu/go-pop3` server.

The application needs bounded `io.ReadCloser` RETR/TOP streams, S3 cancellation
and closure, account authentication, SASL, TLS/STLS, maildrop locking, and deletion
committed only on QUIT. These requirements must be preserved before comparing
throughput. The existing patched implementation remains in use; this is not a
claim that it is defect-free.

### Reviewed revisions

| Project | Commit |
| --- | --- |
| [rest-mail/go-pop3](https://github.com/rest-mail/go-pop3) | `ccda54aa3da698a66907401ddc284dcd784dbccb` |
| [pkierski/pop3srv](https://github.com/pkierski/pop3srv) | `0a0ba05f2dec2afd1795744472e5934f0b017bc9` |
| [inbucket/inbucket](https://github.com/inbucket/inbucket) | `9663b0622521a2a6b5b31603f9a3c99039af7eae` |
| [dzeromsk/pop3](https://github.com/dzeromsk/pop3) | `bee104b87d0caf5b1489581171fb162f83bf79b8` |

### Candidate implementation and adoption findings

| Candidate | Streaming / output | Protocol and integration findings | Required work |
| --- | --- | --- | --- |
| rest-mail/go-pop3 | Retrieve and TOP require whole-message byte slices; RETR also builds strings, normalizes byte by byte, splits lines and flushes every line | TLS/STLS, deferred deletion, UIDL checks and connection limits exist; write/flush errors ignored; no context, Session.Close-equivalent lifecycle or SASL AUTH/PLAIN | Streaming interface/output, cancellation, lock release, error handling and SASL; current memory growth cannot fit GiB messages under 256 MiB |
| pkierski/pop3srv | Message returns io.ReadCloser; RETR copies into standard-library DotWriter; TOP uses Scanner with default token limit | Reproduced bare-dot TOP terminator and deletion error overwritten by successful Close; oversized lines fail; no built-in STLS or SASL; external TLS listener supports implicit TLS only | Closest storage adapter, but fix protocol errors, line handling, TLS/STLS and authentication; no measured throughput advantage |
| inbucket/inbucket | Source returns a stream; RETR/TOP scan lines; every line sets a deadline and calls formatted network output | PASS and APOP do not validate credentials; RETR/TOP ignore retain deletion marks; QUIT acknowledges before deletion and only logs failures; default Scanner limit; TLS/STLS exist | Production authentication, batched output, deletion semantics and configuration/storage integration; only POP3 package tested |
| dzeromsk/pop3 | S3-oriented Get accepts a writer, but RETR first fills a whole-message buffer | Reproduced argument-free RSET rejection and retrieval after DELE; TOP/SASL absent; source shows TLS upgrade before plaintext acknowledgement; TLS ordering was not separately reproduced | Rewrite RETR, fix protocol/TLS behavior and update tests; substantial replacement work |

These fixed revisions are the review scope, not claims about future versions.
Existing tests do exist in pkierski despite its README TODO. Passing an existing
suite does not establish GiB suitability. Inbucket intentionally serves test mail;
its authentication behavior was a source finding, not a separate reproduction.
Dzeromsk's temporary checkout alone received a module and goexpect
v0.0.0-20210430020637-ab937bf7fd6f; its production code stayed unchanged.
Its configuration-mutating test writes Server.Auth at server_test.go:335 while
server.go:390 reads it; that race does not prove a failure under fixed configuration.

Source evidence: [Mailbox interface](https://github.com/rest-mail/go-pop3/blob/ccda54aa3da698a66907401ddc284dcd784dbccb/pop3.go#L30); [RETR/TOP](https://github.com/rest-mail/go-pop3/blob/ccda54aa3da698a66907401ddc284dcd784dbccb/session.go#L460); [per-line flush](https://github.com/rest-mail/go-pop3/blob/ccda54aa3da698a66907401ddc284dcd784dbccb/session.go#L679); [whole-message canonicalization](https://github.com/rest-mail/go-pop3/blob/ccda54aa3da698a66907401ddc284dcd784dbccb/util.go#L56); [RETR/TOP](https://github.com/pkierski/pop3srv/blob/0a0ba05f2dec2afd1795744472e5934f0b017bc9/session.go#L264); [TOP copy](https://github.com/pkierski/pop3srv/blob/0a0ba05f2dec2afd1795744472e5934f0b017bc9/uidl_wrapper.go#L15); [deletion error handling](https://github.com/pkierski/pop3srv/blob/0a0ba05f2dec2afd1795744472e5934f0b017bc9/session.go#L114); [authentication](https://github.com/inbucket/inbucket/blob/9663b0622521a2a6b5b31603f9a3c99039af7eae/pkg/server/pop3/handler.go#L249); [RETR/TOP/QUIT and Scanner](https://github.com/inbucket/inbucket/blob/9663b0622521a2a6b5b31603f9a3c99039af7eae/pkg/server/pop3/handler.go#L403); [deletion and output](https://github.com/inbucket/inbucket/blob/9663b0622521a2a6b5b31603f9a3c99039af7eae/pkg/server/pop3/handler.go#L583); [server construction](https://github.com/inbucket/inbucket/blob/9663b0622521a2a6b5b31603f9a3c99039af7eae/pkg/server/pop3/listener.go#L26); [backend interface](https://github.com/dzeromsk/pop3/blob/bee104b87d0caf5b1489581171fb162f83bf79b8/server.go#L50); [STLS](https://github.com/dzeromsk/pop3/blob/bee104b87d0caf5b1489581171fb162f83bf79b8/server.go#L485); [RETR buffering and RSET](https://github.com/dzeromsk/pop3/blob/bee104b87d0caf5b1489581171fb162f83bf79b8/server.go#L605).

### Candidate validation outcomes

Existing suites ran before the additional reproductions. These are compatibility
and correctness results, not 2 GiB throughput benchmarks. No candidate PR was filed.

| Candidate / test | Result | Package duration (s) | Detail |
| --- | --- | ---: | --- |
| rest-mail, existing race suite | Pass | 1.646 | Streaming/interface limitations remain |
| pkierski, existing race suite | Pass | 1.206 | Mock package has no tests |
| pkierski, additional reproductions | Fail | 0.194 | TOP emits a bare dot; failed deletion returns success; both cases rounded to 0.00 s; exit 1 |
| Inbucket, existing POP3 race suite | Pass | 2.503 | Only the POP3 package was tested |
| dzeromsk, default race invocation | Build rejected | Unrecorded | Vet rejects nonconstant format at server.go:504 |
| dzeromsk, original suite with vet disabled | Race failure | 0.433 | TestPOP3Server: 0.26 s; test writes Server.Auth concurrently with connection read |
| dzeromsk, additional reproductions | Fail | 0.158 | RSET rejected; deleted six-octet message retrievable; both cases rounded to 0.00 s; exit 1 |

Recorded test dependencies: testify 1.10.0; zerolog 1.35.1; envconfig 1.4.0;
loguago 0.0.0; gopher-lua 1.1.2; enmime/v2 2.5.0; go-isatty 0.0.24;
gluamapper 0.0.0-20150323120927-d836955830e7; mapstructure 1.5.0;
x/text 0.41.0; x/net 0.58.0; go-runewidth 0.0.29.
The reproduction log's timezone-free timestamp, September 10 at 13:10:26,
cannot independently establish a UTC instant and is not relabeled as UTC.

## Historical POP3 resources: direct fork v0.1.5

Module github.com/Jabberwocky238/go-pop3 v0.1.5, c27acc581bb3, contains the original
portable batching patch, before the vector follow-up. One authenticated POP3S RETR
on Apple M4/Go 1.25.3 streams a 2 GiB attachment / 2,938,662,361 MIME bytes through
S3 gzip decoding, dot-stuffing and TLS. Setup/login/QUIT are outside timing;
SHA-256 and terminator checks pass. RSS/CPU scope and 0.05 s sampling are defined
in the latest-results section. The 256 MiB server RSS ceiling passed.

| Resource | New run |
| --- | ---: |
| RETR wall time | 1.862 s |
| Server process CPU time, all threads | 1.650 s |
| Average CPU, 100% = one logical core | 88.62% |
| Server process RSS at RETR start | 139.828 MiB |
| Sampled peak server process RSS during RETR | 139.875 MiB |
| Sampled peak RSS increase over RETR start | 48 KiB |
| Decoded-attachment throughput | 1099.89 MiB/s |
| Wire-MIME throughput | 1505.12 MiB/s |

| Historical run | RETR wall time | Process CPU time | Average CPU, one-core basis | CPU seconds per decoded GiB |
| --- | ---: | ---: | ---: | ---: |
| Before the writer fix | 30.077 s | 32.960 s | 109.59% | 16.480 |
| First run with the writer fix | 1.790 s | 1.580 s | 88.27% | 0.790 |
| Current direct fork v0.1.5 | 1.862 s | 1.650 s | 88.62% | 0.825 |

| Counter | Recorded value |
| --- | ---: |
| elapsed_seconds | 1.862 |
| cpu_seconds | 1.65 |
| average_cpu_percent_one_core | 88.62 |
| peak_rss_bytes | 146669568 |
| initial_rss_bytes | 146620416 |
| peak_rss_growth_bytes | 49152 |
| rss_sample_interval_seconds | 0.05 |
| bytes | 2938662361 |
| mib_per_second | 1505.12 |
| attachment_mib_per_second | 1099.89 |

The historical pre-fix run used 32.96 CPU s over 30.077 wall s (109.59% of one
core), peak RSS 181.125 MiB. The v0.1.5 run used about 95.0% less CPU time.
Against the first fixed run, CPU time was 4.4% higher, wall time 4.0% longer and
average CPU only 0.35 percentage points higher. These unpaired historical samples
do not establish regression or isolate RSS effects from application/GC history.
88.62% is 0.886 logical cores on this 10-logical-CPU host. Throttling can lengthen
a transfer without reducing CPU seconds per GiB. Concurrent/peak CPU is unmeasured.
The 48 KiB sampled RSS growth is not allocated memory; the isolated 2 GiB/op to
4,248 B/op change measures cumulative allocations, not a 2 GiB RSS reduction.
The harness is scripts/test_streaming.py and retains the exact module and counters.

## Follow-up: vector validation of POP3 body spans (2026-09-10)

The bulk writer still searched for each LF separately. An isolated CPU profile
of five 2 GiB iterations attributed 61% of sampled CPU to `bytes.IndexByte`
and its bytealg implementation, including 54% directly in bytealg. This is a
body-transformation profile, not a breakdown of the complete mail server.

The follow-up implementation adds an ARM64 NEON validator that checks 16 current
bytes and their preceding bytes for bare LF and line-leading dot. It skips only
validated ordinary data, leaves a boundary byte for the existing scalar scanner,
and never reads outside the input slice. First-byte state, normalization,
stuffing, downstream writes and partial-error accounting remain in Go.

Other architectures retain the portable scanner. `-tags=pop3scalar` disables only
this POP3 vector path for controlled comparisons; `-tags=purego` also selects the
portable path but must not be used as the end-to-end baseline because it changes
cryptography and other dependencies too. No new buffer, allocation, exported API,
or dependency is introduced.

Two initial SWAR prototypes were rejected: approximately 0.445 s and 0.292 s per
2 GiB, slower than the existing roughly 0.25 s writer. The first NEON version also
regressed repeated bare-LF / leading-dot inputs by about 30%. The final version
stops attempting vector scans after an exceptional block within each Write, and
preserves the standard-library fast search for newline-free spans. Shape
benchmarks explicitly cover these cases rather than testing only ideal CRLF MIME.

The full RETR diagnostic profile placed most samples under network system calls
through TLS writes and S3 reads. A short profile is diagnostic, not a precise
allocation of end-to-end latency; it nevertheless motivates keeping isolated
encoding throughput separate from actual download measurements. The preliminary
vector run used 1.56 server CPU seconds over 1.896 wall seconds, versus a historical
v0.1.5 run of 1.65 CPU seconds over 1.862 wall seconds. This does not demonstrate
an end-to-end speedup. Final matched-build results follow below.

### Matched isolated writer results

Same performance commit `5a8a1ebd43ca`, Apple M4, Go 1.25.3. The two builds differ
only in the `pop3scalar` build tag. Runs were sequential, without concurrent
server benchmarks, profiling or race instrumentation. Each row reports the median
of three samples, with five operations per sample.

| Input | Bytes per operation | Scalar (ms) | ARM64 vector (ms) | Throughput ratio |
| --- | ---: | ---: | ---: | ---: |
| Main CRLF benchmark | 2 GiB | 267.945 | 131.334 | 2.040x |
| CRLF | 32 MiB | 4.146 | 1.957 | 2.119x |
| BareLF | 32 MiB | 7.739 | 7.631 | 1.014x |
| LeadingDot | 32 MiB | 8.115 | 7.989 | 1.016x |
| NoNewline | 32 MiB | 0.950 | 0.921 | 1.031x |

The main CRLF benchmark improves **2.04x** over the portable bulk scanner.
Against the historical upstream median of 16.839746 s, the new 0.131334 s
median is approximately **128x**, with architecture and measurement-date limits.
Median allocation remains **4,248 B/op and 6 allocs/op**. No buffer was added.
The other input shapes show why the adaptive fallback and newline-free fast path
are necessary; small differences in those rows should not be treated as reliable gains.

Exact samples: series 1 is scalar; series 2 is ARM64 vector.

| Series / benchmark | Iterations per sample | ns/op | MB/s (decimal) | B/op | allocs/op |
| --- | --- | --- | --- | --- | --- |
| 1: BenchmarkDotStuffWriter-10 | 5, 5, 5 | 266024858, 269532792, 267945275 | 8072.49, 7967.43, 8014.64 | 4248, 4248, 4248 | 6, 6, 6 |
| 1: BenchmarkDotStuffWriterShapes/CRLF-10 | 5, 5, 5 | 4146358, 4126117, 4148675 | 8092.51, 8132.21, 8087.99 | 4248, 4248, 4248 | 6, 6, 6 |
| 1: BenchmarkDotStuffWriterShapes/BareLF-10 | 5, 5, 5 | 7746342, 7636708, 7739150 | 4331.65, 4393.83, 4335.67 | 4248, 4248, 4251 | 6, 6, 6 |
| 1: BenchmarkDotStuffWriterShapes/LeadingDot-10 | 5, 5, 5 | 8115358, 8001792, 8287300 | 4134.68, 4193.36, 4048.90 | 4248, 4248, 4248 | 6, 6, 6 |
| 1: BenchmarkDotStuffWriterShapes/NoNewline-10 | 5, 5, 5 | 978817, 919583, 950183 | 34280.61, 36488.74, 35313.64 | 4248, 4257, 4248 | 6, 6, 6 |
| 2: BenchmarkDotStuffWriter-10 | 5, 5, 5 | 131455083, 131334250, 129193617 | 16336.25, 16351.28, 16622.21 | 4248, 4251, 4248 | 6, 6, 6 |
| 2: BenchmarkDotStuffWriterShapes/CRLF-10 | 5, 5, 5 | 1957075, 1906200, 1968667 | 17145.19, 17602.79, 17044.24 | 4248, 4248, 4248 | 6, 6, 6 |
| 2: BenchmarkDotStuffWriterShapes/BareLF-10 | 5, 5, 5 | 7630658, 7584883, 7668842 | 4397.32, 4423.86, 4375.42 | 4248, 4248, 4248 | 6, 6, 6 |
| 2: BenchmarkDotStuffWriterShapes/LeadingDot-10 | 5, 5, 5 | 7958958, 8002542, 7989475 | 4215.93, 4192.97, 4199.83 | 4248, 4248, 4248 | 6, 6, 6 |
| 2: BenchmarkDotStuffWriterShapes/NoNewline-10 | 5, 5, 5 | 913375, 921317, 954558 | 36736.75, 36420.09, 35151.79 | 4248, 4248, 4248 | 6, 6, 6 |

All samples passed. Package test durations: github.com/migadu/go-pop3/pop3server 5.367s; github.com/migadu/go-pop3/pop3server 2.861s.

## Updated native Fals3y: SMTP latency investigation (2026-09-10)

The local Fals3y executable was replaced during investigation. New measurements
are kept separate from historical storage runs. The harness now records the
resolved executable, version output, SHA-256 and Go build flags, and verifies
that the executable fingerprint has not changed by the end of the run.

- Requested executable: `/Users/jw238/.local/bin/fals3y`
- Resolved executable: `/Users/jw238/.local/share/fals3y/0.3.1-rc.1/fals3y`
- CLI version: `fals3y version 0.3.1-rc.1`
- SHA-256: `edbb3bca75da4f536509ffde2918ae1d0aabd90211773bad3aacee342b7578ec`

### Priority finding: SMTP still takes approximately seven seconds

For a 2 GiB decoded attachment, the SMTP DATA body is 2,938,662,361 bytes after
Base64 expansion and MIME headers. The client prepares the repeated Base64 block
before the timed phase. Timing covers the SMTP operation through its success
response; server stage logs split body reception/storage, commit and delivery.

| Run | SMTP wall (s) | Read/store stream (s) | Commit (s) | Inbox import/delivery (s) |
| --- | ---: | ---: | ---: | ---: |
| scalar-1 | 7.412 | 3.383 | 2.893 | 1.128 |
| vector-1 | 6.397 | 2.969 | 2.333 | 1.088 |
| scalar-2 | 6.704 | 3.095 | 2.442 | 1.159 |

These stages are sequential: **the remaining time is not explained solely by
SMTP DATA parsing**. In the first run, 3.383 s + 2.893 s + 1.128 s accounts for
7.404 s of the 7.412 s operation. The approximately 8 ms remainder covers other
protocol/bookkeeping work and timing boundaries. The storage commit still adds
almost three seconds after reception, then import adds about one second.

The current `complete_seconds` metric includes flushing the last part, waiting
for outstanding part uploads, and the CompleteMultipartUpload request; it is
not yet an isolated measurement of the remote complete request. The complete
path uploads 351 parts, writes a small reference index and does not perform an
application-level whole-object CopyObject. A storage server may still assemble
or copy multipart data internally; that needs source/measurement confirmation.
Read/store includes the protocol reader, hashing, gzip, buffering and concurrent
part-upload backpressure. Delivery includes the JMAP import path. Further
profiling must separate these costs before changing protocol or storage code.

The first new-storage raw-attachment HTTP upload/download measured **2.736 s /
0.899 s**, totaling **3.635 s**; the combined three-second target is still unmet.
Storage encoding was gzip even for the low-compressibility payload. These figures
are separate from SMTP's larger MIME wire representation and its import work.
The new POP3 vector path cannot explain SMTP improvements: it is not executed
by SMTP. Differences among these sequential SMTP samples reflect run variation.

The first report's CLI-version field is empty because Fals3y prints its version
to stderr. Its SHA-256 is recorded correctly; later reports capture both output
streams. An earlier `purego` end-to-end attempt is excluded because that global
tag also disables cryptographic acceleration and is not a valid POP3-only control.

### POP3 resources with the updated storage binary

All rows transfer the same 2 GiB decoded size / 2,938,662,361 MIME bytes.
The first four runs use regenerated low-compressibility fixtures and were run
sequentially in scalar/vector/scalar/vector order; the last uses compressible data.
The POP3-only `pop3scalar` tag leaves TLS and storage dependencies unchanged.
All five complete runs passed their hashes, protocol checks and 256 MiB server
RSS ceiling. The scalar/vector sources differ only by the scanner build tag.

| Run | RETR wall (s) | Server CPU (s) | Average CPU (one core = 100%) | Initial RSS (MiB) | Peak RSS (MiB) | Sampled increase (KiB) |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| scalar-1 | 2.055 | 1.780 | 86.60% | 131.109 | 131.141 | 32 |
| vector-1 | 1.849 | 1.560 | 84.36% | 71.359 | 71.719 | 368 |
| scalar-2 | 1.866 | 1.660 | 88.95% | 37.109 | 37.547 | 448 |
| vector-2 | 2.269 | 1.770 | 78.02% | 56.703 | 57.031 | 336 |
| compressible | 2.205 | 2.280 | 103.38% | 103.297 | 103.312 | 16 |

**These end-to-end results do not establish a download speedup.** The second
vector run is slower than its scalar counterpart. The isolated 2.04x encoding
gain remains valid, but does not remove network/storage work or guarantee lower
whole-process CPU in every run. Two samples per variant are insufficient for a
statistical regression claim. RSS includes memory retained by earlier phases;
the large differences in starting RSS cannot be attributed to this scanner.
No new buffer is allocated by the vector implementation.
CPU is summed user+system time across all server threads; RSS is sampled every
50 ms. The separate Fals3y and benchmark-client processes are excluded.

### Full updated-storage timings

Times are seconds; all rows use 2 GiB decoded attachments. Raw upload/download
are HTTP blob transfers through gzip storage. SMTP and IMAP APPEND additionally
store and import the full Base64 MIME message. JMAP attachment download includes
MIME/base64 processing and remains a distinct, slower path.

| Run | Raw upload | Raw download | SMTP DATA | IMAP download | IMAP APPEND | POP3 | JMAP metadata | JMAP attachment download |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| scalar-1 | 2.736 | 0.899 | 7.412 | 2.203 | 6.864 | 2.055 | 5.340 | 15.276 |
| vector-1 | 2.481 | 0.875 | 6.397 | 1.911 | 6.567 | 1.849 | 5.188 | 14.074 |
| scalar-2 | 2.526 | 0.919 | 6.704 | 2.022 | 6.232 | 1.866 | 5.190 | 14.494 |
| vector-2 | 2.647 | 0.913 | 7.025 | 2.509 | 9.189 | 2.269 | 6.047 | 16.353 |
| compressible | 1.566 | 1.117 | 9.159 | 2.349 | 4.949 | 2.205 | 5.512 | 19.780 |

Low-compressibility raw blobs stored 2,147,647,513 gzip bytes; the compressible
raw blob stored 8,389,108 bytes. In the compressible SMTP run, the 9.159 s total
breaks down into 6.473 s read/store, 0.068 s commit and 2.609 s import/delivery.
A smaller stored object does not guarantee a faster SMTP operation: gzip and
subsequent decompression/parsing can dominate when storage work becomes small.
These figures identify an unresolved performance problem, not a three-second SLA.

### Confirmed Fals3y multipart implementation

`go version -m` identifies the installed binary as Go 1.26.8, darwin/arm64,
revision `3a3ee5eca1c4aa8e3bee5d801beef9607a8c89b8`, with `vcs.modified=false`.
The email server and writer benchmarks use Go 1.25.3. The local Fals3y checkout's
`internal/store` has no diff from that binary's revision.

In `internal/store/multipart.go`, `CompleteMultipartUpload` validates cached
part ETags, then opens every part and calls `copyToFile(dst, part, nil)` to build
one temporary object before an atomic rename. Thus the newer release removes
repeated digest work but still performs an entire-object assembly pass.
In `internal/store/stream_mmap.go`, each copy call grows the destination by a
64 MiB mapping, copies the source, then truncates to its actual end. This server
receives 8 MiB parts from the email application, so completion invokes that
mapping/truncation path separately for each of its 351 MIME parts. This is a
concrete optimization candidate; the current timing does not isolate how much
of commit belongs to mapping, copying, upload waits or final publication.

No change has been made to the installed Fals3y binary in this investigation.
The report does not attribute all SMTP latency to the SMTP library, nor does it
claim that the storage implementation alone explains the compressible case.

Reproduce the complete path with the installed storage binary:

The measured comparison used a temporary `-modfile` replacing only the POP3 module
with the independent fork checkout containing upstream patch `5a8a1ebd43ca`
(cherry-picked as `6e1b336` on fork main). Add `-tags=pop3scalar` for the portable
scanner control. Published v0.1.6 contains that same runtime change. Raw reports
are `/tmp/fma-fals3y-new-{scalar-1,vector-1,scalar-2,vector-2,compressible}.json`;
these temporary paths are local evidence, while the tables and source details
above preserve the findings in this existing Markdown file.

### POP3 publication and application validation

The independent fork published `v0.1.6` at `b70bb165ca14`, and the application
now directly requires `github.com/Jabberwocky238/go-pop3 v0.1.6`. Upstream
[PR #3](https://github.com/migadu/go-pop3/pull/3) uses `Jabberwocky238:pr` at
`5a8a1ebd43ca` and contains only six performance/test files. Fork branding and
module-path changes remain on main. The PR includes the new input-shape
benchmarks, updated-storage CPU/RSS table and the lack of consistent end-to-end
speedup. Application `make test` and module verification passed after selecting
the published version, including native S3 protocol integration and the 32 MiB
streaming check. The larger measurements above used the identical POP3 runtime
patch in a local checkout; they are not relabeled as new published-module runs.


## SMTP and JMAP bottleneck isolation after the storage update

Application baseline: commit **077b404**. All measurements in this section use a
2,147,483,648-byte low-compressibility attachment / 2,938,662,361-byte MIME message,
Apple M4/macOS arm64, Go 1.25.3 and the same Fals3y shared-file binary
0.3.1-dev.b8e48bf (SHA-256 1298e405c1631fbded92892eadc7252d8e2e319dfdf42f48c188ae3fd82ffb4b).
This is a new investigation; the historical seven-second SMTP / three-second
commit results are not the current baseline.

| Component | Repository / version | Role in the measured path | PR relationship |
| --- | --- | --- | --- |
| Application | [Jabberwocky238/fma](https://github.com/Jabberwocky238/fma), 077b404 | serveJMAP, materializePart, findPart, openPart, streaming storage | Temporary diagnostic overlays only; no new runtime patch/PR |
| SMTP | [Jabberwocky238/go-smtp](https://github.com/Jabberwocky238/go-smtp), b0673510e580 / v0.25.1-0.20260910174640-b0673510e580 | DATA reader plus original per-byte line-length checks | Existing [PR #312](https://github.com/emersion/go-smtp/pull/312) is reference only; not edited |
| JMAP core | [naust-mail/naust-jmap](https://github.com/naust-mail/naust-jmap), core v0.4.2 | Blob authorization and HTTP download using streamed copy | No PR filed |
| JMAP mail | [naust-mail/naust-jmap](https://github.com/naust-mail/naust-jmap), datatypes/mail v0.3.3 | Email/get attachment identity calculation, MIME and Base64 parsing | No PR filed |
| POP3 | [Jabberwocky238/go-pop3](https://github.com/Jabberwocky238/go-pop3), v0.1.6 | Unchanged control path | Existing [PR #3](https://github.com/migadu/go-pop3/pull/3); no additional POP3 fix in this investigation |

### Unprofiled single-change comparisons

Temporary Go overlays and a temporary SMTP checkout isolate one change at a time.
The production checkout and pinned dependencies remain unchanged. All four full
runs passed protocol/content hashes and the 256 MiB email-server RSS ceiling.
Samples were sequential; one sample per variant cannot establish significance.

| Variant | SMTP DATA (s) | JMAP attachment metadata (s) | JMAP attachment download (s) | Changed behavior |
| --- | ---: | ---: | ---: | --- |
| baseline | 5.430 | 5.167 | 14.067 | None |
| coalesce-result | 5.500 | 5.209 | 8.779 | Aggregate decoded attachment reads, bounded at 128 KiB; no extra whole-object buffer |
| smtp128 | 5.776 | 5.196 | 14.189 | SMTP text input buffer 4 KiB to 128 KiB only; line checks retained |
| smtp-bulk-limit | 4.512 | 5.200 | 16.508 | Batch line-length checks at LF boundaries; original 4 KiB input buffer retained |

| Variant / affected phase | Server CPU (s) | Average CPU, one core = 100% | Initial RSS (bytes) | Peak RSS (bytes) | Sampled increase (bytes) |
| --- | ---: | ---: | ---: | ---: | ---: |
| baseline / smtp_mime_upload | 7.530 | 138.68% | 136134656 | 149585920 | 13451264 |
| smtp128 / smtp_mime_upload | 7.240 | 125.35% | 135905280 | 157777920 | 21872640 |
| smtp-bulk-limit / smtp_mime_upload | 6.270 | 138.97% | 118407168 | 147980288 | 29573120 |
| baseline / jmap_attachment_metadata | 5.410 | 104.70% | 184909824 | 185090048 | 180224 |
| baseline / jmap_attachment_download | 16.990 | 120.78% | 185090048 | 187449344 | 2359296 |
| coalesce-result / jmap_attachment_download | 9.350 | 106.50% | 176603136 | 176685056 | 81920 |

CPU/RSS cover the entire email server, exclude the separate Fals3y/client processes,
and include prior phases' retained buffers. RSS sampling is every 0.05 s. The
aggregation experiment adds no whole-attachment allocation; its bounded reader
still requires additional error/cancellation/short-read tests before production use.

### SMTP: the bottleneck moved away from multipart commit

| Variant | Receive / gzip / stream (s) | Commit including outstanding uploads (s) | Inbox import (s) |
| --- | ---: | ---: | ---: |
| baseline | 4.245561 | 0.025131 | 1.150662 |
| smtp128 | 4.105414 | 0.518050 | 1.142987 |
| smtp-bulk-limit | 3.331336 | 0.019617 | 1.152079 |

The baseline's 4.245561 s reception, 0.025131 s commit and 1.150662 s import account
for almost all 5.430 s SMTP latency. Continuing to optimize the historical 2.893 s
commit would target a bottleneck already removed by the storage update.
The 128 KiB buffer alone did not improve total latency in this sample; its commit
also varied to 0.518050 s, so the test does not isolate a buffer slowdown either.
The line-check experiment reduced SMTP to 4.512 s (16.9% less wall time) and CPU
from 7.53 to 6.27 s (16.7% less CPU). It preserves positive line-length enforcement
and passed the existing DataReader/LineLimit tests; it is not a merged library fix.

### JMAP: repeated full scans and small decoded output chunks

An instrumented diagnostic build measured the following actual gzip-decoded MIME
consumption. These counters are logical decompressed bytes, not physical S3 bytes
or socket-read counts. The CPU-profile run is separate from the unprofiled table.

| Diagnostic stage | Stage elapsed (s) | MIME bytes read | OpenStream calls | Recorded completion UTC |
| --- | ---: | ---: | ---: | --- |
| Email/get attachments | 5.183364 | 2,938,662,361 | 1 | 2026-09-10T18:45:10.259Z |
| First-download materializePart subset | 4.481955 | 2,938,662,361 | 1 | 2026-09-10T18:45:14.876Z |
| Entire first attachment download | 14.105453 | 5,877,324,722 | 3 | 2026-09-10T18:45:24.499Z |

The subset is included in the whole download; do not add it again. Metadata plus
first download reads **8,815,987,083 MIME bytes**, exactly three full message passes.
Additional source opens can consume headers or fail the physical-blob lookup;
three OpenStream calls do not mean three full-body reads during download.

Email/get enables identity capture for attachment blobId/size and hashes decoded
content each time. The application then lacks a part locator: materializePart
scans account messages, findPart decodes/hashes candidates, and only then saves
Source/Path/Size. openPart subsequently opens the MIME again for actual delivery.
For a late attachment or an account with many messages, the uncached search can
scan still more data. This is an algorithmic cost, not just gzip speed.

The standard Base64 stream decoder buffers 1,024 encoded bytes and can yield only
768 decoded bytes per call. JMAP core copies that reader to HTTP; small writes
increase transport/dispatch overhead. Aggregating reads reduced download from
14.067 to 8.779 s (37.6% less time) and CPU from 16.99 to 9.35 s (45.0% less CPU),
while metadata remained 5.167 versus 5.209 s. No scan was removed in this experiment,
so it independently demonstrates an output-chunking cost. The diagnostic first
locator scan was 4.482 s; the remainder was approximately 9.623 s before aggregation.

Profiles showed most samples under system-call stacks reached from SMTP buffered
reads and from JMAP gzip/MIME/Base64 reads and HTTP writes. SMTP lineLimitReader
had 3.82/7.04 s cumulative samples; the metadata decoder 5.06/5.32 s; first-download
system calls 11.68/15.00 s. These are nested sampled CPU stacks, not additive wall
stages or proof that pure decoder arithmetic owns those times. The logical read
counters and isolated changes provide stronger causal evidence than flat symbols.

### Optimization order and limits

| Priority | Proposed change | Why / required safeguards |
| --- | --- | --- |
| 1 | Aggregate decoded HTTP attachment output in bounded chunks | Measured reduction; preserve partial data, EOF, cancellation, source closure and read errors |
| 2 | Persist MIME part identity and locator together during an existing parse | Reuse account + immutable source blob + parser-version metadata; avoid searching/rehashing the MIME again for the first download |
| 3 | Cache immutable computed attachment metadata | Repeated Email/get should not hash all attachment bytes again; keep request projection, malformed-MIME semantics and account authorization correct |
| 4 | Batch SMTP line validation while keeping limits | Measured improvement in a temporary checkout; do not disable line checks or broaden the upstream DATA-only PR silently |
| 5 | Reuse or overlap mail import parsing with reception | Import still rereads the full MIME; simply moving extra hashing into SMTP can make uploads slower, so measure the combined pipeline |

Locators and hashes must never bypass account/message access checks. A single gzip
object also prevents ordinary S3 byte-range access to decoded MIME offsets; offset
indexing alone cannot make late parts randomly accessible without an appropriate
seekable or independently stored representation. These are follow-up designs,
not performance gains claimed as implemented.

### UTC recording intervals and artifacts

These intervals are measurement-file creation through last modification, including
setup/other phases; they are not exact per-operation UTC start/end instants.
No console logs or source snippets are embedded here.

| Measurement | Recording start UTC | Recording end UTC |
| --- | --- | --- |
| profile | 2026-09-10T18:44:27.666Z | 2026-09-10T18:45:25.517Z |
| baseline | 2026-09-10T18:47:22.911Z | 2026-09-10T18:48:02.168Z |
| coalesce-result | 2026-09-10T18:48:02.364Z | 2026-09-10T18:48:37.045Z |
| smtp128 | 2026-09-10T18:49:08.300Z | 2026-09-10T18:49:48.495Z |
| smtp-bulk-limit | 2026-09-10T18:50:27.918Z | 2026-09-10T18:51:10.002Z |

Local evidence uses /tmp/fma-bottleneck-{profile,baseline,coalesce-result,smtp128,smtp-bulk-limit}.json;
profiles and temporary overlays are outside the repository. The final profile
run passed hashes but is not mixed into the unprofiled speedup comparisons.
This pass isolates low-compressibility data; the previous compressible results
remain historical and do not acquire a new paired speedup claim.


### Repeated-request confirmation with aggregated output

A second aggregation run repeated Email/get and download for the same attachment
within one server process. Both requests still verify the decoded size and hash.
Only the first download needs a new locator; metadata is requested twice with
identical properties. This tests an existing locator, not a full-body memory cache.

| Request | Wall (s) | Server CPU (s) | Average one-core CPU | Initial RSS (bytes) | Peak RSS (bytes) | Sampled increase (bytes) |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| jmap_attachment_metadata | 5.185 | 5.450 | 105.12% | 199262208 | 199278592 | 16384 |
| jmap_attachment_download | 8.681 | 9.180 | 105.75% | 199278592 | 199311360 | 32768 |
| jmap_repeat_metadata | 5.178 | 5.430 | 104.87% | 199311360 | 199327744 | 16384 |
| jmap_repeat_download | 4.226 | 4.470 | 105.76% | 199327744 | 199327744 | 0 |

Metadata remained **5.185 / 5.178 s**, confirming the absence of effective computed
attachment-identity caching. Download fell from **8.681 to 4.226 s**, a 4.455 s
difference consistent with the independently counted 4.482 s locator scan.
The repeat download remains expensive even after locator reuse and aggregation;
it still streams and decodes the attachment. This experiment does not measure
eliminating gzip or Base64 processing.

Recording interval: **2026-09-10T18:53:00.127Z–2026-09-10T18:53:43.408Z**,
from the measurement artifact's creation/last-write metadata. Evidence:
/tmp/fma-bottleneck-repeat.json. All hashes and the 256 MiB RSS ceiling passed.
The only repository change from this investigation is this documentation;
experimental runtime and library changes remain in temporary files for follow-up.


## JMAP attachment output batching: implemented and paired 2 GiB results

This follow-up implements bounded decoded-output aggregation in `main.go`.
`openPart` now returns `jmapDownloadReader`: short Base64 reads fill the caller's
buffer, and its `WriteTo` uses the existing shared 128 KiB copy-buffer pool.
Explicit Read/Write-only wrappers prevent recursive `io.Copy` dispatch and stop
HTTP's `ReaderFrom` from selecting a smaller output buffer. The reader preserves
partial data and terminal errors, checks cancellation between reads, bounds
consecutive empty reads, and delegates Close to the original source. It does not
retain a decoded attachment. The pool has 32 slots shared with existing copy
operations; JMAP HTTP admission remains four concurrent requests.

Baseline runtime is the `main.go` at commit `6c9db2c` (unchanged since `077b404`),
compiled through a Go overlay pointing at its saved source. The after variant
contains this output change. Both use Go 1.25.3 on the same Apple M4 darwin/arm64
host and Fals3y 0.3.1-dev.b8e48bf. Dependencies are unchanged: naust-jmap core
v0.4.2, mail v0.3.3, POP3 fork v0.1.6, SMTP fork
v0.25.1-0.20260910174640-b0673510e580, and klauspost/compress v1.20.0.
Repository and upstream PR links are retained in the earlier version tables.

The updated `scripts/test_streaming.py` supports `--repeat-jmap` and `--seed`.
Reproduce the after runs with `python3 scripts/test_streaming.py --mail
--repeat-jmap --seed 20260910 --report /tmp/fma-jmap.json` (one shell command;
default size is 2048 MiB). Add `--data compressible` for the compressed fixture.
The low-compressibility fixture repeats a seeded random 1 MiB block, whose period
exceeds gzip's window; each before/after pair has identical attachment and MIME
SHA-256 values. Each run starts a fresh server and storage fixture, runs
sequentially without a CPU profiler or concurrent test suite, then downloads the
same attachment twice. First download includes locator discovery; repeat download
reuses the locator. Both still read, decode, transfer and hash all 2 GiB.

CPU is fma process CPU time, excluding Fals3y and the Python client; 100% average
CPU means one logical core. RSS is the whole fma process sampled every 50 ms,
not an allocation count or an attributable per-buffer measurement. Every run
passed the 256 MiB sampled peak ceiling, complete content hashes, protocol checks,
interrupted-upload checks, and the no-local-spool assertion. Two low-compressibility
pairs and one compressible pair establish the observed improvement, not a broad
statistical latency guarantee.

### Paired JMAP results

All durations are seconds. Metadata precedes the first download and is **not**
included in the download column; the combined column includes both.

| Run | Metadata | First download | Metadata + first download | Repeat metadata | Repeat download |
| --- | ---: | ---: | ---: | ---: | ---: |
| before-1 | 5.128 | 13.863 | 18.991 | 5.153 | 9.558 |
| after-1 | 5.125 | 8.422 | 13.547 | 5.123 | 3.976 |
| before-2 | 5.228 | 13.830 | 19.058 | 5.134 | 9.563 |
| after-2 | 5.176 | 8.505 | 13.681 | 5.150 | 3.981 |
| before-compressed | 5.520 | 15.630 | 21.150 | 6.413 | 15.897 |
| after-compressed | 5.576 | 9.417 | 14.993 | 5.596 | 4.498 |

| Run | Download | CPU (s) | Average one-core CPU | Initial RSS (bytes) | Peak RSS (bytes) | Sampled increase (bytes) |
| --- | --- | ---: | ---: | ---: | ---: | ---: |
| before-1 | first | 16.880 | 121.76% | 190480384 | 190578688 | 98304 |
| before-1 | repeat | 12.340 | 129.11% | 190611456 | 190611456 | 0 |
| after-1 | first | 9.040 | 107.34% | 185057280 | 187858944 | 2801664 |
| after-1 | repeat | 4.330 | 108.89% | 188760064 | 190234624 | 1474560 |
| before-2 | first | 17.010 | 122.99% | 198590464 | 198623232 | 32768 |
| before-2 | repeat | 12.480 | 130.51% | 198656000 | 198672384 | 16384 |
| after-2 | first | 9.150 | 107.59% | 185384960 | 188022784 | 2637824 |
| after-2 | repeat | 4.360 | 109.53% | 188891136 | 190316544 | 1425408 |
| before-compressed | first | 18.630 | 119.19% | 108068864 | 108068864 | 0 |
| before-compressed | repeat | 20.380 | 128.20% | 108101632 | 108118016 | 16384 |
| after-compressed | first | 9.470 | 100.56% | 108347392 | 108445696 | 98304 |
| after-compressed | repeat | 4.590 | 102.05% | 108478464 | 108658688 | 180224 |

First download, low-compressibility two-run mean: **13.8465 → 8.4635 s** (38.9% less wall time); CPU **16.945 → 9.095 s** (46.3% less CPU time).

Repeat download, low-compressibility two-run mean: **9.5605 → 3.9785 s** (58.4% less wall time); CPU **12.410 → 4.345 s** (65.0% less CPU time).

The 3-second target is still unmet, including for repeat attachment downloads.
Metadata parsing and the first-download `findPart` full decode/hash pass are
unchanged. Eliminating that extra scan needs trusted attachment identity and MIME
locator information shared from the metadata parser; the current mail library's
public EmailConfig has no hook exposing that parse result. Metadata caching also
needs account/source scoping and parser-version invalidation. The output change
makes no claim to have implemented either cache or to have eliminated Base64/gzip
work. The earlier temporary aggregation measurements remain historical and are
not substituted for the implemented reader's paired results.

### Other protocol measurements from the same runs

These are controls, not speedup claims for unchanged protocols. Durations are
seconds; the complete reports retain CPU/RSS and stream-stage timings.

| Run | Raw upload | Raw download | SMTP DATA | IMAP download | IMAP APPEND | POP3 |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| before-1 | 1.925 | 0.873 | 5.586 | 2.041 | 4.161 | 1.884 |
| after-1 | 1.828 | 0.887 | 5.457 | 2.018 | 3.884 | 1.854 |
| before-2 | 1.813 | 0.869 | 5.433 | 1.957 | 3.937 | 1.842 |
| after-2 | 1.818 | 0.886 | 5.420 | 1.951 | 3.885 | 1.789 |
| before-compressed | 1.429 | 0.857 | 7.062 | 2.096 | 4.891 | 2.236 |
| after-compressed | 1.428 | 0.868 | 7.063 | 2.103 | 4.921 | 2.254 |

### UTC and executable provenance

The following bounds are actual UTC timestamps captured at each JMAP measurement
boundary, from the first metadata request through the end of repeat download.
They are not inferred from log creation times. Individual phase timestamps,
monotonic elapsed durations, fixture hashes and executable hashes are in
`/tmp/fma-jmap-paired-{before,after}-{1,2,compressed}.json`. Console output is kept
outside the document in the matching `.log` files. These local artifacts are
supplementary; the measured tables remain here.

| Run | JMAP start UTC | JMAP finish UTC |
| --- | --- | --- |
| before-1 | 2026-09-10T19:30:36.423225+00:00 | 2026-09-10T19:31:10.134084+00:00 |
| after-1 | 2026-09-10T19:31:39.730065+00:00 | 2026-09-10T19:32:02.384072+00:00 |
| before-2 | 2026-09-10T19:32:35.149339+00:00 | 2026-09-10T19:33:08.913006+00:00 |
| after-2 | 2026-09-10T19:33:28.781198+00:00 | 2026-09-10T19:33:51.600964+00:00 |
| before-compressed | 2026-09-10T19:34:14.402681+00:00 | 2026-09-10T19:34:57.871185+00:00 |
| after-compressed | 2026-09-10T19:36:39.784611+00:00 | 2026-09-10T19:37:04.880286+00:00 |

| Artifact | SHA-256 |
| --- | --- |
| Baseline main.go | c36bf5d7f995eab8e0a2f86cc4168e83134124af3aca144df91e57f524f0657d |
| Optimized main.go | d651e78e444f2148ad70aeb0b205dd36bc2a531ff5cd5854d87c81980fe4359c |
| before executable | ce46a4736155d1fb04383755ab6d94a77f817df42a82dba70e9e785685b61543 |
| after executable | 9a3705bcf108091cf9bc5d207e3c6ff598b109a29eb8169bf31958c6c3074eac |
| Fals3y executable | 1298e405c1631fbded92892eadc7252d8e2e319dfdf42f48c188ae3fd82ffb4b |
| incompressible attachment | 70ee1bb08fc817e52cdde811bfd8860d092fdefef4cd1a07038510a71da71acc |
| incompressible MIME | a3096ef28f2bb06b8fc473710cc19bbe046a14bcd2f94c6dd889fad10c866e59 |
| compressible attachment | 36a084c480d42b87482e186b191c8d0a26e088205a6278d100c092e9bc166f65 |
| compressible MIME | 4636132035f0177dc958563b65a003ba7bf8af10f57015e75c0f157796edcb2b |

One after/compressible attempt failed before measurements because Fals3y could
not bind port 61894. Its startup failure is retained at
`/tmp/fma-jmap-paired-after-compressed-startup-failure.log`; the table contains
the completed retry, not a selectively discarded measured sample.

Validation: `make test` and `make build` passed after the implementation. The
new Go regressions check decoded byte equality and 128 KiB write batching even
when the destination exposes ReaderFrom, partial EOF/corruption errors,
cancellation and source closure, bounded read-ahead, empty-reader termination,
and short/failed writes with pooled-buffer return. The full suite also passed
race detection, JMAP exact binary/empty attachment downloads, account isolation,
restart and cross-protocol checks, plus the existing 32 MiB streaming test.
Validation output remains outside the document in
`/tmp/fma-jmap-optimized-tests.log` and `/tmp/fma-jmap-optimized-build.log`.


## Single-request MIME and S3 streaming optimization (2026-09-10)

Implementation: fma `b62287c`; MIME parser fork
[86f12015507c](https://github.com/Jabberwocky238/naust-jmap/commit/86f12015507c),
based on naust-jmap mail v0.3.3 and pinned through `go.mod replace` as
`github.com/Jabberwocky238/naust-jmap/datatypes/mail v0.3.4-0.20260910200323-86f12015507c`.
This change adds no metadata cache, decoded-body cache or duplicate-object speedup.
The MIME walker borrows consumed spans until drained; Base64 filtering compacts
owned input in place, decoding writes into the caller's buffer, and temporary
buffers return to a pool. The application's strict MIME decoder applies the same
block-processing approach while preserving strict Base64 validation.

The prior CPU profile's 90.28% under `io.copyBuffer` was cumulative, including
filtering, decoding and hashing, not 90.28% spent copying bytes. Its directly
sampled redundant newline scan was 28.74%; removing that second scan is the main
parser gain. In the later profile SHA-256's relative share rose as other work
shrank; it still uses ARM SHA2 instructions. S3 checksums cover stored objects or
parts, not a decoded MIME attachment, so they cannot replace this attachment ID's
SHA-256 without changing what is stored or adding MIME processing to storage.
See [S3 checksum semantics](https://docs.aws.amazon.com/AmazonS3/latest/userguide/checking-object-integrity-upload.html).

S3 write buffers now consist of reusable **1 MiB blocks**, with a global bound of
**1 GiB of active part capacity**, allocated on demand. Eight blocks form one
8 MiB request body with Read/Seek support for retries; they are not concatenated
into another 8 MiB allocation. Each upload admits four parts including the part
being filled. Global admission reserves complete part capacity before allocating
individual blocks, avoiding deadlock among partially assembled parts. This is a
buffer admission bound, not a whole-process RSS bound or a 1 GiB preallocation.
[S3 requires non-final multipart parts of at least 5 MiB](https://docs.aws.amazon.com/AmazonS3/latest/userguide/qfacts.html).

`Write` sends completed parts using UploadPart while bytes are still arriving;
`Commit` sends any final partial part, waits for active uploads, completes the
multipart object and publishes its small reference. Commit does not retain or
upload the entire message at once. Cross-account CopyStream now uses CopyObject
or UploadPartCopy after reserving the destination; only small references pass
through fma. Tests reject any GET of the source body and cover both normal and
6 GiB multipart-copy paths, with compression metadata and destination references
checked. Same-account immutable references retain their existing behavior.

The first two matched full-protocol runs used seed 20260910, 2,147,483,648 decoded
attachment bytes and 2,938,662,361 MIME bytes. Each variant starts a fresh process
and S3 fixture and requests JMAP metadata and attachment download only once.
The full-protocol sequence reads through IMAP/POP3 before JMAP; these are first
JMAP requests, not an assertion of cold OS page caches. Additional JMAP-first
measurements below isolate the first client fetch after SMTP delivery.

Matched controls use the original runtime from `9429b8d`, with the same AWS S3
SDK v1.97.3, AWS core SDK v1.41.5, smithy v1.24.2 and x/text v0.39.0 as the new
build; the mail parser is v0.3.3 in the control. Other protocol versions remain
unchanged. This excludes unrelated dependency-version changes from the comparison.
Fals3y remains 0.3.1-dev.b8e48bf; Go is 1.25.3, darwin/arm64, Apple M4.

| Run | Metadata (s) | First download (s) | Combined (s) | Metadata CPU (s) | Download CPU (s) |
| --- | ---: | ---: | ---: | ---: | ---: |
| control-1 | 5.196 | 8.514 | 13.710 | 5.440 | 9.130 |
| after-1 | 3.716 | 6.008 | 9.724 | 4.000 | 6.370 |
| control-2 | 5.265 | 8.529 | 13.794 | 5.520 | 9.230 |
| after-2 | 3.701 | 5.967 | 9.668 | 3.990 | 6.340 |

Two-run means: metadata **5.2305 → 3.7085 s (29.1% less)**; first download
**8.5215 → 5.9875 s (29.7% less)**; combined **13.7520 → 9.6960 s (29.5% less)**.
The first-download locator still scans the MIME; this change makes each pass
faster, rather than hiding it behind a cache. The 3-second attachment target is
still unmet.

### Additional first-request results and resources

All times are seconds. The compressible and JMAP-first comparisons each contain one matched pair; they are supporting measurements, not statistically established averages. JMAP-first moves the requests ahead of IMAP/POP3 reads, but SMTP import and OS page caches still exist. No repeated JMAP requests or application cache are used.

| Run | Metadata | First download | Combined | Metadata CPU | Download CPU |
| --- | ---: | ---: | ---: | ---: | ---: |
| control-compressed | 5.580 | 9.361 | 14.941 | 5.570 | 9.450 |
| after-compressed | 4.108 | 6.966 | 11.074 | 4.100 | 7.040 |
| first-control | 5.093 | 8.352 | 13.445 | 5.380 | 9.000 |
| first-after | 3.637 | 5.893 | 9.530 | 3.920 | 6.300 |

JMAP-first metadata falls **28.6%**, download **29.4%**, and their combined time **13.445 → 9.530 s (29.1%)**. The compressible pair improves from **14.941 → 11.074 s (25.9%)** combined. For the two standard matched pairs, total JMAP CPU falls **14.660 → 10.350 CPU-seconds (29.4%)**. This reduces work rather than trading latency for higher CPU consumption.

Resource figures measure the fma process only, excluding Fals3y and the benchmark client. CPU 100% means one logical core. RSS is sampled every 50 ms; peak RSS includes existing process memory, while growth is relative to that phase's initial RSS. MiB values below are rounded; they are not whole-system resource totals.

| Run | Metadata CPU % | Download CPU % | Metadata peak / growth MiB | Download peak / growth MiB | Whole-run peak MiB |
| --- | ---: | ---: | ---: | ---: | ---: |
| control-1 | 104.70 | 107.23 | 170.06 / 0.05 | 170.27 / 0.20 | 170.27 |
| after-1 | 107.65 | 106.03 | 169.47 / 0.09 | 169.50 / 0.03 | 169.50 |
| control-2 | 104.84 | 108.22 | 171.81 / 0.06 | 172.03 / 0.22 | 172.03 |
| after-2 | 107.81 | 106.26 | 168.70 / 0.08 | 168.70 / 0.00 | 168.70 |
| control-compressed | 99.81 | 100.95 | 102.11 / 0.06 | 102.11 / 0.00 | 102.11 |
| after-compressed | 99.80 | 101.06 | 97.97 / 0.12 | 98.09 / 0.12 | 98.09 |
| first-control | 105.64 | 107.76 | 148.91 / 0.27 | 149.19 / 0.28 | 168.33 |
| first-after | 107.79 | 106.92 | 148.75 / 0.84 | 151.31 / 2.56 | 176.59 |
| before-1 | 102.29 | 107.76 | 181.83 / 0.06 | 181.83 / 0.00 | 181.83 |

### Full protocol results and write-side evidence

Each row retains the same run's other protocol measurements, in seconds. These operations have different wire sizes and processing requirements: raw and attachment downloads contain 2 GiB, while SMTP/IMAP/POP3 transfer the 2.737 GiB MIME message. The compressible fixture has the same logical sizes. The initial `before-1` pilot used older AWS dependencies and is **excluded from matched improvement calculations**.

| Run | Raw upload | Raw download | SMTP DATA | IMAP download | IMAP APPEND | POP3 | JMAP metadata | JMAP attachment download |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| control-1 | 1.858 | 0.884 | 5.321 | 1.981 | 4.036 | 1.871 | 5.196 | 8.514 |
| after-1 | 1.935 | 0.904 | 5.468 | 2.078 | 3.865 | 1.834 | 3.716 | 6.008 |
| control-2 | 2.082 | 0.893 | 5.558 | 1.958 | 4.321 | 1.958 | 5.265 | 8.529 |
| after-2 | 2.042 | 0.890 | 5.380 | 1.941 | 3.814 | 1.881 | 3.701 | 5.967 |
| control-compressed | 1.470 | 0.855 | 7.111 | 2.135 | 4.936 | 2.293 | 5.580 | 9.361 |
| after-compressed | 1.490 | 0.865 | 6.979 | 2.212 | 4.844 | 2.283 | 4.108 | 6.966 |
| first-control | 1.817 | 0.852 | 5.447 | 1.946 | 3.923 | 1.748 | 5.093 | 8.352 |
| first-after | 1.827 | 0.869 | 5.290 | 1.912 | 3.777 | 1.856 | 3.637 | 5.893 |
| before-1 | 2.182 | 0.989 | 6.033 | 2.466 | 4.902 | 2.138 | 5.895 | 8.547 |

In `first-after`, raw upload plus download takes **2.696 s**, meeting the 3-second raw round-trip goal in that run; this does not mean the JMAP goal is met. The raw gzip object contains 2,147,647,513 stored bytes and 257 parts. Its CompleteMultipartUpload takes **0.027985 s**, reference publication **0.000745 s**, and copy **0 s**. SMTP receives and writes the MIME stream in **4.265265 s**, final storage commit takes **0.014873 s**, and delivery/import takes **1.001266 s**, within **5.290 s** SMTP DATA overall. The current final commit is milliseconds; reception/processing and import remain the larger costs. These internal stages are diagnostic timings, not independently summed benchmarks across runs.

### UTC provenance and reproducibility

The following are recorded wall-clock timestamps converted to UTC, not inferred dates or local solar time. Each interval runs from metadata start to attachment-download completion. The run labels map to `/tmp/fma-cold-<run>.json` and `.log`; those local diagnostic files are not embedded in this document or committed and may be removed by temporary-directory cleanup. The tables retain the comparison results independently of those files.

| Run | JMAP start UTC | JMAP end UTC |
| --- | --- | --- |
| control-1 | 2026-09-10T20:09:48.155209Z | 2026-09-10T20:10:01.867853Z |
| after-1 | 2026-09-10T20:08:28.084693Z | 2026-09-10T20:08:37.811449Z |
| control-2 | 2026-09-10T20:10:52.444833Z | 2026-09-10T20:11:06.242920Z |
| after-2 | 2026-09-10T20:10:22.038938Z | 2026-09-10T20:10:31.709393Z |
| control-compressed | 2026-09-10T20:11:29.259126Z | 2026-09-10T20:11:44.203208Z |
| after-compressed | 2026-09-10T20:12:06.500435Z | 2026-09-10T20:12:17.578124Z |
| first-control | 2026-09-10T20:13:31.499840Z | 2026-09-10T20:13:44.947270Z |
| first-after | 2026-09-10T20:14:04.679916Z | 2026-09-10T20:14:14.211803Z |
| before-1 | 2026-09-10T19:55:11.451426Z | 2026-09-10T19:55:25.896615Z |

Control execution uses the `9429b8d` runtime through a Go source overlay, with the current dependencies except the unmodified mail parser v0.3.3. The JSON `commit` field records the checkout HEAD, so it alone does not identify an overlay build. The pilot instead uses AWS core v1.36.3, S3 v1.78.2, smithy v1.22.2 and x/text v0.34.0. Optimized execution uses runtime `b62287c` and parser `86f12015507c`.

Shared libraries: [POP3 v0.1.6](https://github.com/Jabberwocky238/go-pop3/tree/v0.1.6), [SMTP b0673510e580](https://github.com/Jabberwocky238/go-smtp/commit/b0673510e580) (v0.25.1-0.20260910174640-b0673510e580), go-imap v1.2.1 / v2.0.0-beta.8, naust-jmap core v0.4.2 and klauspost/compress v1.20.0. The MIME changes are published on the fork's [perf/streaming-mime branch](https://github.com/Jabberwocky238/naust-jmap/tree/perf/streaming-mime); no upstream MIME PR has been opened in this round. Historical SMTP/POP3 PR links and results above remain unchanged.

Fals3y executable SHA-256: `1298e405c1631fbded92892eadc7252d8e2e319dfdf42f48c188ae3fd82ffb4b`.

| Build / run | fma executable SHA-256 |
| --- | --- |
| control-1, control-2, control-compressed, first-control | `77afba2a67ddfce0dd33a3a71baa0bbf2faf38332144440080d6717e8d04e511` |
| after-1, after-2, after-compressed, first-after | `9f3b2185b108a10ffbeb872f5da4e01baefc3861fee8fb5d7d5494ebd32a1336` |
| before-1 | `38d8ea1633a9833987c291a20d55a47b09117abc3384e22699b030a418c8530a` |

Reproduce the optimized first-fetch order with `python3 scripts/test_streaming.py --mail --jmap-first --seed 20260910 --report /tmp/fma-jmap-first.json`. Omit `--jmap-first` for the original full-protocol order; add `--data compressible` for the compressible fixture. The default transfer is 2 GiB. The fixed-seed incompressible attachment SHA-256 is `70ee1bb08fc817e52cdde811bfd8860d092fdefef4cd1a07038510a71da71acc`; MIME SHA-256 is `a3096ef28f2bb06b8fc473710cc19bbe046a14bcd2f94c6dd889fad10c866e59`. Raw fixture and MIME attachment hashes differ because mail framing uses a 57-byte-aligned fixture block; the initial raw upload therefore does not pre-create the requested MIME attachment.

### Isolated decoder benchmarks and validation

The parser benchmark is upstream `BenchmarkParseWithDigest_1MBAttachment`; each sample runs for 2 seconds on the same M4. Its throughput uses encoded MIME bytes, whereas the application decoder benchmark uses decoded bytes. Both include digest work. The table preserves all three samples per implementation; allocations are per operation, not peak live memory.

| Parser variant | ns/op (three samples) | B/op (three samples) | allocs/op |
| --- | --- | --- | ---: |
| v0.3.3 | 2160872, 2163847, 2196584 | 163800, 163560, 163566 | 137 |
| Direct block decoder | 1425069, 1446461, 1450894 | 158059, 157742, 157724 | 135 |
| In-place filter | 1493504, 1499015, 1513043 | 145401, 144998, 144924 | 125 |
| Borrowed segments | 1481256, 1468511, 1465247 | 99615, 99390, 99447 | 124 |
| Final pooled buffers | 1433353, 1453578, 1456583 | 58954, 58641, 58709 | 124 |

Parser median: **2.163847 → 1.453578 ms (32.8% less)**, allocated bytes **163,566 → 58,709 (64.1% less)**, allocations **137 → 124**. Intermediate in-place filtering alone did not outperform the first block-decoder variant; the final version combines reduced copies with buffer reuse.

| Application decoder | ns/op (three samples) | B/op (three samples) | allocs/op |
| --- | --- | --- | ---: |
| Standard library | 1580079, 1593125, 1600782 | 2256, 2256, 2256 | 5 |
| Pooled block decoder | 1049422, 1050793, 1049904 | 302, 288, 288 | 4 |

The corrected application benchmark constructs only the decoder being measured.
Median time falls **1.593125 → 1.049904 ms (34.1%)**, and median allocated bytes
fall **2,256 → 288 (87.2%)**. An earlier exploratory benchmark constructed an
unused standard decoder in the block case, inflating its allocation counts;
those exploratory counts are not used here. The benchmark remains in
`main_test.go` as `BenchmarkMIMEBase64`; the parser benchmark is in the linked
mail fork. No standalone benchmark source or raw log is embedded in this report.

Validation passed: `make test`, `make build`, the fork's `go test -race ./...`,
and a 15-second differential fuzz run (10,158 executions). New regression checks
cover Base64 boundaries, corruption and source errors, buffer reuse, S3 block
boundaries and retry seeks, and server-side copy without source-body GETs.
All reported end-to-end runs passed content SHA-256 checks, the 256 MiB fma RSS
threshold, interruption handling and the no-local-spool assertion. The new
`--jmap-first` execution order passed both 2 GiB control and optimized runs.
Validation and profile evidence remains outside the repository under
`/tmp/fma-cold-*` and `/tmp/fma-mime-parser-*`; all historical measurements above
are retained.


## Persisted MIME parts, concurrent ingestion and task workers (2026-09-10)

This supersedes the earlier virtual-attachment design for newly ingested mail. Original MIME remains available for IMAP/POP3; each decoded leaf is also stored as a named S3 object, with a separate versioned JSON record containing MIME headers/structure, part hashes and sizes, and preview. A small parent-message record supports attachment authorization. These records are produced at write time, not populated by repeated downloads. First metadata access reads JSON; first attachment download reads the decoded object without scanning the original MIME. Text body values read the stored text part with charset conversion and the requested truncation limit. Legacy objects without metadata retain the previous read path.

The parser fork is [ea6016819f55](https://github.com/Jabberwocky238/naust-jmap/commit/ea6016819f557258bc1adf95488dca474c37e846), pinned as `github.com/Jabberwocky238/naust-jmap/datatypes/mail v0.3.4-0.20260910204519-ea6016819f55`. Its optional `MessageStore`, `PartWriter` and `IngestMessage` APIs stream parts and propagate required storage-write/commit errors. No SMTP or POP3 library version or PR was changed in this round. AWS S3 v1.97.3, AWS core v1.41.5, smithy v1.24.2, Go 1.25.3, Apple M4 and Fals3y 0.3.1-dev.b8e48bf remain as recorded above. Intermediate runs used a temporary local module replacement for this fork; final runs use the published version.

Physical parts use `<account>/mail/<escaped-subject>_<timestamp>/attachments/<part-id>/<escaped-filename>`. Part-number directories disambiguate duplicate filenames. Raw MIME and all parts must commit before the final JSON metadata is published. Interrupted or failed publication can leave unreferenced objects, matching the existing no-automatic-GC policy; it does not publish a successful Email with unfinished attachments. Keeping original MIME plus decoded parts consumes additional S3 storage. Attachment identities are hashed once by the parser and passed to the writer, avoiding a second application-level hash of decoded bytes.

### Why the first implementation took eight seconds

The first persisted-parts prototype still received/stored MIME and then read it back from S3 for import. In its unprofiled 2 GiB run, reception/write took **4.370775 s**, raw commit **0.038265 s**, and import/part storage **4.144295 s**, yielding **8.562 s SMTP DATA**. It made JMAP metadata **0.004 s** and first attachment download **0.898 s**, but moved work into a serial upload stage. The full SMTP-plus-JMAP sequence was **9.464 s**, compared with **14.820 s** for the prior first-request run. This prototype is retained as evidence, not presented as the final upload result.

Concurrent ingestion removes that S3 reread. The receiver writes the original object and transfers ownership of three reusable 1 MiB blocks to the parser; blocks return only after consumption. The parser decodes/hashes each leaf and writes its object while reception continues. Completed parts upload incrementally. This is bounded streaming with backpressure, not full-message buffering. The first parallel run reduced SMTP to **4.525 s**, final import to **0.012954 s**, with **0.004 s** metadata and **0.904 s** first download: **5.433 s** for SMTP plus JMAP. SMTP CPU changed **11.92 → 11.94 CPU-seconds** while peak SMTP RSS changed **170.73 → 209.59 MiB**: concurrency reduced elapsed time, not total CPU work.

### TCP read-count experiment and mmap assessment

A 1 MiB reusable TCP reader below TLS batches the SMTP library's existing 4 KiB requests without changing its line limits, DATA state machine or TLS detection. Two otherwise matched diagnostic builds counted calls immediately around the underlying `net.Conn.Read`; the small-read control bypassed the added buffer. These are underlying Read calls, not a count of every kernel retry/EAGAIN. Both transferred 2,938,662,512 bytes including SMTP commands/framing, with identical attachment content.

| Receive buffer | Underlying Read calls | Mean bytes/read | SMTP wall s | SMTP CPU s | SMTP peak MiB |
| --- | ---: | ---: | ---: | ---: | ---: |
| Existing 4 KiB protocol reads | 717,453 | 4,096.0 | 4.423 | 11.790 | 219.72 |
| Added 1 MiB TCP buffer | 2,818 | 1,042,818.5 | 4.278 | 11.620 | 220.39 |

Read calls fall **99.6%**, but wall time falls only **3.3% (4.423 → 4.278 s)**. Small reads were real overhead; their count alone does not explain the remaining critical path. The block queue already supplies bounded buffer rotation. Merely allocating it with anonymous mmap does not change the socket-read batching policy; mmap is a memory/file mapping API, not a replacement for receiving this TCP stream. No mmap or file-backed spool was introduced. See [POSIX mmap](https://pubs.opengroup.org/onlinepubs/9799919799/functions/mmap.html) and [Go buffered reads](https://go.dev/src/bufio/bufio.go).

An earlier buffered pilot retained a 10 MiB decoded-part compression probe and failed the existing 256 MiB whole-run RSS assertion: peak **258.34375 MiB** after POP3, despite correct content transfers. Its SMTP was **4.305 s**, metadata **0.004 s**, and download **0.908 s**; it is excluded from passing-run comparisons. Decoded-part probes now use 1 MiB and begin gzip above that threshold; original MIME/raw uploads retain their 10 MiB threshold. Raw and part compression/probe pools are separate, preventing dependent stages from exhausting one shared pool. The two read-count runs and subsequent profile use the smaller part probe and passed the RSS assertion.

### SMTP CPU hot functions

CPU profiling was enabled only during SMTP DATA in temporary Go source overlays, with no profiling endpoint added to the application. Samples are CPU time across all fma threads, not elapsed stage time or syscall counts. The first serial profile lasted **8.61 s**, with **11.20 CPU-seconds** sampled: `syscall.syscall` flat **7.83 s (69.91%)**, SHA-256 `blockSHA2` flat **1.04 s (9.29%)**. The receiving `dataReader.Read → bufio.ReadByte` chain accumulated **3.92 s**; import `base64Filter.fill` accumulated **3.48 s**, mostly including its downstream reads. These cumulative figures do not mean those functions themselves spent that time computing, and the read-count experiment above is a stronger test of the syscall-count hypothesis.

The concurrent buffered profile lasted **4.41 s**, with **10.82 CPU-seconds** sampled. Its principal flat samples were:

| Function | Flat CPU s | Flat % | Interpretation |
| --- | ---: | ---: | --- |
| syscall.syscall | 3.19 | 29.48 | Socket/system work across concurrent paths |
| sha256.blockSHA2 | 1.75 | 16.17 | SHA2-accelerated content hashing |
| runtime.pthread_cond_signal | 1.67 | 15.43 | Thread wakeups |
| runtime.pthread_cond_wait | 1.02 | 9.43 | Runtime synchronization samples |
| message.(*base64Filter).fill | 0.84 | 7.76 | Filtering; cumulative 1.16 s including callees |
| base64.(*Encoding).Decode | 0.48 | 4.44 | Decode; cumulative 0.55 s |
| smtp.(*lineLimitReader).Read | 0.48 | 4.44 | Protocol reader; cumulative 2.61 s including reads |
| runtime.memmove | 0.29 | 2.68 | Memory copies |
| crc32.ieeeUpdate | 0.16 | 1.48 | CRC work |

The same diagnostic run timed decoded-part `Write` calls: **0.418693 s** accumulated over 2 GiB during SMTP (and **0.403204 s** during IMAP APPEND). This includes synchronous gzip/write work and any backpressure in those calls; it excludes final Commit and independently running S3 workers. Therefore treating the remaining four seconds as gzip time is also unsupported. Profiling runs are retained below but excluded from unprofiled latency comparisons. Profiles and diagnostic logs remain under `/tmp/fma-ingest-*`, `/tmp/fma-pipeline-*` and `/tmp/fma-smtp-count-*`, outside the repository.

### Startup task pool

The final implementation starts **8 worker goroutines**. `FMA_STREAM_WORKERS` or `-stream-workers` changes the count (1–128), with the command-line flag taking precedence. Workers pull MIME parsing tasks from a single shared queue; queue capacity equals worker count. Busy workers do not retain private queues, so the next idle worker takes the next waiting message. A full queue applies backpressure, and the three block allocations occur only after admission. Canceled queued work is skipped; shutdown cancels running task contexts and drains pending results. Part compression pools scale with the parser-worker count; raw upload pools stay independent. S3 multipart bodies retain their separate 1 GiB active-capacity bound. This controls concurrent messages, not eight-way decoding of one Base64 stream, and does not replace protocol or SDK network goroutines.

Configuration and scheduling regressions verify the default, environment/flag precedence, invalid counts, eight simultaneous active tasks, bounded-queue cancellation, shutdown cancellation, post-shutdown rejection, buffer ownership/reuse and cancellation of blocked producers. Storage regressions deny complete-MIME reads after import and account reopen while exercising metadata, first attachment authorization/download and cross-account rejection. Library regressions cover create/write/short-write/commit/metadata failures, mandatory sink error propagation, decoded identity equality and stored text-value truncation.

### Intermediate results and exact UTC provenance

All runs use a fresh fixture and process, seed 20260910, a 2 GiB decoded attachment and 2,938,662,361 MIME bytes. JMAP requests occur immediately after SMTP, before IMAP/POP3 reads, once each. CPU/RSS describe fma only; Fals3y and client resources are excluded. 100% CPU is one logical core. These are first application requests, not a claim of cold OS page caches. The `cold-first-after` row is the previously recorded streaming-decoder baseline. The other rows precede the startup worker-pool change; final-version results follow separately.

| Run | Raw up | Raw down | SMTP | IMAP down | APPEND | POP3 | JMAP metadata | JMAP first down | Whole-run peak MiB |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| cold-first-after | 1.827 | 0.869 | 5.290 | 1.912 | 3.777 | 1.856 | 3.637 | 5.893 | 176.59 |
| ingest-after-1 | 1.828 | 0.867 | 8.562 | 1.901 | 2.905 | 1.814 | 0.004 | 0.898 | 188.44 |
| ingest-profile | 1.813 | 0.866 | 8.620 | 1.943 | 2.759 | 1.819 | 0.004 | 0.890 | 192.23 |
| pipeline-after-1 | 1.826 | 0.877 | 4.525 | 1.920 | 4.326 | 1.867 | 0.004 | 0.904 | 221.81 |
| smtp-count-small | 1.825 | 0.855 | 4.423 | 1.964 | 4.319 | 1.827 | 0.004 | 0.886 | 228.98 |
| smtp-count-buffered | 1.791 | 0.885 | 4.278 | 1.926 | 4.313 | 1.826 | 0.005 | 0.880 | 251.20 |
| pipeline-profile | 1.835 | 0.880 | 4.417 | 1.974 | 4.334 | 1.858 | 0.006 | 0.888 | 243.38 |

| Run | SMTP start UTC | First JMAP download end UTC | SMTP CPU s | JMAP download CPU s |
| --- | --- | --- | ---: | ---: |
| cold-first-after | 2026-09-10T20:13:59.385619Z | 2026-09-10T20:14:14.211803Z | 7.460 | 6.300 |
| ingest-after-1 | 2026-09-10T20:28:31.189259Z | 2026-09-10T20:28:40.660257Z | 11.920 | 0.600 |
| ingest-profile | 2026-09-10T20:31:43.619118Z | 2026-09-10T20:31:53.140911Z | 12.090 | 0.600 |
| pipeline-after-1 | 2026-09-10T20:34:58.653505Z | 2026-09-10T20:35:04.092502Z | 11.940 | 0.610 |
| smtp-count-small | 2026-09-10T20:38:05.961465Z | 2026-09-10T20:38:11.281544Z | 11.790 | 0.600 |
| smtp-count-buffered | 2026-09-10T20:38:26.318019Z | 2026-09-10T20:38:31.488412Z | 11.620 | 0.620 |
| pipeline-profile | 2026-09-10T20:39:37.863227Z | 2026-09-10T20:39:43.182892Z | 11.780 | 0.600 |

Each run maps to `/tmp/<run>.json` and `.log` with the `fma-` prefix retained; no log dump or standalone benchmark source is embedded here. Reproduce using `python3 scripts/test_streaming.py --mail --jmap-first --seed 20260910 --stream-workers 8 --report /tmp/fma-workers.json`; add `--data compressible` for the high-compression fixture. The report records worker count, UTC timestamps and executable SHA-256. Historical results above have not been removed.

### Final published-library, eight-worker results

These three unprofiled runs use the published `ea6016819f55` parser, the final application source and `--stream-workers 8`, with no Go source overlay or local module replacement. They are single-message throughput measurements; the eight-way scheduling property is separately covered by concurrent worker tests. All passed SHA-256 content verification, the 256 MiB fma RSS limit, interrupted-upload handling and no-local-spool checks.

| Run | Raw up s | Raw down s | SMTP s | IMAP down s | APPEND s | POP3 s | JMAP metadata s | First attachment down s |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| workers-8-1 | 1.828 | 0.865 | 4.280 | 1.934 | 4.367 | 1.821 | 0.004 | 0.888 |
| workers-8-2 | 2.044 | 0.885 | 4.289 | 1.898 | 4.315 | 1.766 | 0.004 | 0.889 |
| workers-8-compressed | 1.477 | 0.844 | 5.045 | 2.146 | 3.746 | 2.257 | 0.004 | 0.849 |

| Run | SMTP CPU s / average % | SMTP peak MiB | JMAP download CPU s / average % | JMAP download peak MiB | Whole-run peak MiB |
| --- | ---: | ---: | ---: | ---: | ---: |
| workers-8-1 | 11.660 / 272.45% | 190.48 | 0.590 / 66.45% | 190.55 | 217.42 |
| workers-8-2 | 11.650 / 271.64% | 184.53 | 0.590 / 66.39% | 186.16 | 240.11 |
| workers-8-compressed | 8.990 / 178.20% | 79.64 | 0.510 / 60.09% | 82.69 | 116.59 |

Two-run incompressible means: SMTP **4.2845 s**, metadata **0.004 s**, first attachment download **0.8885 s**, and SMTP plus metadata plus download **5.1770 s**. Against the serial persisted-parts prototype, SMTP falls **8.562 → 4.2845 s (50.0%)**; against the prior streaming-decoder first-request run it falls **5.290 → 4.2845 s (19.0%)**, despite now also storing decoded parts. First download falls **5.893 → 0.8885 s (84.9%)** and metadata-plus-download **9.530 → 0.8925 s (90.6%)**. The full SMTP-plus-JMAP sequence falls **14.820 → 5.1770 s (65.1%)** relative to that prior run. The older comparison points are individual preserved runs, not new multi-run confidence estimates.

Final SMTP CPU averages **11.655 CPU-seconds**, versus **11.920** in the serial persisted-parts prototype, while average utilization is about **272% of one core**. This is parallel work over a shorter interval, not an eightfold throughput claim. Raw upload-plus-download means **2.811 s**; JMAP metadata-plus-download is under one second. **SMTP itself and SMTP-plus-download remain above the 3-second goal.**

| Run | SMTP start UTC | SMTP end UTC | First attachment download end UTC |
| --- | --- | --- | --- |
| workers-8-1 | 2026-09-10T20:50:38.087133Z | 2026-09-10T20:50:42.366938Z | 2026-09-10T20:50:43.265287Z |
| workers-8-2 | 2026-09-10T20:50:58.571384Z | 2026-09-10T20:51:02.860164Z | 2026-09-10T20:51:03.759382Z |
| workers-8-compressed | 2026-09-10T20:51:18.271031Z | 2026-09-10T20:51:23.315904Z | 2026-09-10T20:51:24.175748Z |

Final `main.go` SHA-256: `8d0e9a5adbb4f661633326862d1e9b8f1c1c65dc9b477dae0d91775c30920bc4`. The benchmark JSON checkout field records base commit `3e488709981b365027fe2f50b9dab33bdb29a964` plus the then-uncommitted changes; it is not an assertion that the base commit alone contains this implementation.

| Run | Executable SHA-256 |
| --- | --- |
| workers-8-1 | `0bbc387e5ae7fd1fb402cf4f5e5d77032c54a4fecf4a0b5bf7ad73ff0bb668e6` |
| workers-8-2 | `0bbc387e5ae7fd1fb402cf4f5e5d77032c54a4fecf4a0b5bf7ad73ff0bb668e6` |
| workers-8-compressed | `0bbc387e5ae7fd1fb402cf4f5e5d77032c54a4fecf4a0b5bf7ad73ff0bb668e6` |

Final validation: **`make test` and `make build` passed**, including race, native S3, SMTP STARTTLS/SMTPS, IMAP/POP3, JMAP account isolation and restart checks. The published fork passed `go test -race ./...`; its additional required-sink and metadata tests are committed with the library. Local validation records are `/tmp/fma-workers-full-tests.log`, `/tmp/fma-workers-full-build.log` and `/tmp/fma-pipeline-library-tests.log`. Final reports are `/tmp/fma-workers-final-{1,2,compressed}.json`. No upstream SMTP PR was modified.

## Four default workers and the remaining SMTP critical path (2026-09-10)

The default MIME worker count is now **4**, including startup workers and their dependent part pools. `FMA_STREAM_WORKERS` and `-stream-workers` still accept 1–128, with flags taking precedence. The benchmark script and both READMEs use the same default; the earlier eight-worker measurements above remain historical results. The configurable eight-way dispatch regression is retained.

Two fresh, unprofiled 2 GiB runs used the default without an explicit worker flag: SMTP **4.313 / 4.343 s**, mean **4.328 s**; metadata **0.004 / 0.004 s**; first attachment download **0.904 / 0.895 s**, mean **0.8995 s**. The earlier eight-worker SMTP mean was 4.2845 s. This roughly 1% difference is not evidence that worker count controls single-message latency. Both normal runs passed content checks, the 256 MiB RSS assertion and interruption checks.

### Measured wall-clock breakdown

Temporary Go overlays instrumented the existing receiver, parser and writers; profiling hooks were not added to production code. The successful timing-only repeat measured **4.306 s SMTP DATA**. The receiver spends its time in the following sequential operations; gzip includes lower-level object writes and backpressure, and its child waits must not be added again. These are elapsed intervals around calls, including scheduling delays, not isolated CPU costs.

| Receiver operation | Elapsed s |
| --- | ---: |
| Protocol input `Read` (network, SMTP DATA framing and line checks) | 2.646418 |
| Original MIME SHA-256 in `jmapBlobWriter.Write` | 1.030694 |
| Original MIME `gzip.Write`, including lower-level writes | 0.544498 |
| Wait for a reusable receiver block | 0.014864 |
| Send a filled block to the parser queue | 0.002526 |
| Receiver total, including remaining overhead | 4.241502 |

The parser runs **concurrently** for **4.274211 s**, including **0.224711 s** waiting for input. Task admission/start waits only **0.000017 s**. Attachment Write calls accumulate **0.426282 s**, with **0.029883 s** for final part Commit. Once the parser finishes, final original-MIME commit plus metadata publication takes **0.009679 s**. The earlier CPU-profile run independently measured reception **4.260854 s**, parser **4.287204 s**, original commit **0.009824 s** and delivery/import **0.012389 s**. Adding parser duration to receiver duration would double-count overlapping work.

S3 backpressure is small in the timing repeat: original-MIME part-slot waits total **0.025666 s** and global-buffer waits **0.000245 s**; attachment part-slot waits **0.000082 s**, global-buffer waits **0.000168 s**. The first timing attempt measured original slot waits **0.004622 s**. This does not support increasing worker count, multipart concurrency or the 1 GiB active buffer budget as the main fix for this single transfer.

The physical work is larger than a raw 2 GiB upload: original MIME stores **2,938,601,966 bytes**, decoded attachment **2,147,647,513 bytes**, plus the small text part and metadata—about **4.74 GiB** written to S3. BestSpeed gzip saves only about 0.002% of this high-entropy MIME fixture. This byte count explains why comparing SMTP directly with one raw-object upload is incomplete; it does not, by itself, establish a storage-hardware throughput limit.

### Hot functions and separate hash consumers

The four-worker SMTP CPU profile covers **4.41 s** and **10.93 CPU-seconds** of samples. Receiver-labeled tasks and their children account for 5.89 sampled CPU-seconds; parser-labeled tasks and children account for 3.27. The labels include concurrent S3 workers and therefore are not each stage's elapsed duration.

| Function / consumer | Flat CPU s | Cumulative CPU s | Meaning |
| --- | ---: | ---: | --- |
| `smtp.(*lineLimitReader).Read` | 0.75 | 2.99 | Its own byte-wise line checks plus downstream reads |
| `smtp.(*dataReader).Read` | 0.00 sampled | 3.16 | Framing chain, including the line reader and socket |
| `message.(*base64Filter).fill` | 0.70 | 1.03 | MIME Base64 filtering and its callees |
| `base64.(*Encoding).Decode` | 0.49 | 0.55 | Base64 decoding |
| `sha256.blockSHA2` | 1.80 | 1.80 | All SHA-256 consumers combined |
| `runtime.memmove` | 0.37 | 0.37 | Actual sampled copy work |
| `syscall.syscall` | 3.27 | 3.27 | System-call samples across all paths, not call counts |

SHA-256 has **three different consumers**: original MIME identity contributes about **0.41 sampled CPU-seconds**, parser part identity **0.35**, and hashing reached through `io.copyBuffer` **1.04**. The AWS SDK's `ComputePayloadSHA256.HandleFinalize` chain accumulates **1.17 CPU-seconds**, including its callees. Inspection of AWS core v1.41.5 confirms that the HTTP S3 path hashes each request body and rewinds it before transmission; its dynamic signing middleware chooses unsigned payloads for HTTPS. These request hashes are distinct from the content IDs, so the earlier general statement that all SHA-256 work belongs to attachment identity was incomplete. The per-part signing work occurs in concurrent upload workers: 1.17 CPU-seconds cannot simply be subtracted from 4.3 seconds of latency. Request-signing semantics were not changed in this investigation. Source: [AWS SigV4 middleware v1.41.5](https://github.com/aws/aws-sdk-go-v2/blob/v1.41.5/aws/signer/v4/middleware.go).

The next code-level targets are the SMTP line-limit scan and original-MIME hashing on the receiver path, together with filtering/decoding on the parallel parser path. Both paths remain busy until near the end. Queue startup, final storage commit and Email import are already too small to explain the remaining seconds; optimizing only one branch may expose the other as the limiting branch.

### Results, failures and provenance

All runs below use seed 20260910, 2,147,483,648 decoded attachment bytes, 2,938,662,361 MIME bytes, and JMAP before IMAP/POP3 reads. Dependencies remain mail fork `v0.3.4-0.20260910204519-ea6016819f55`, SMTP fork `b0673510e580`, POP3 v0.1.6, AWS core v1.41.5 / S3 v1.97.3, Go 1.25.3, Apple M4, and Fals3y 0.3.1-dev.b8e48bf. CPU/RSS measure fma alone.

| Run | Raw up s | Raw down s | SMTP s | IMAP down s | APPEND s | POP3 s | JMAP metadata s | JMAP down s | Peak RSS MiB |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| workers4-1 | 1.878 | 0.868 | 4.313 | 1.916 | 4.442 | 1.816 | 0.004 | 0.904 | 239.60938 |
| workers4-2 | 1.879 | 0.877 | 4.343 | 2.004 | 4.420 | 1.786 | 0.004 | 0.895 | 233.56250 |
| workers4-profile | 1.994 | 0.885 | 4.419 | 1.915 | 4.392 | 1.870 | 0.005 | 0.894 | 241.96875 |
| workers4-timings-failed | 1.985 | 0.870 | 4.292 | 1.999 | 4.413 | 1.844 | 0.005 | 0.886 | 256.15625 |
| workers4-timings-retry | 1.896 | 0.878 | 4.306 | 1.998 | 4.395 | 1.767 | 0.004 | 0.890 | 240.67188 |

The first timing-only attempt failed the **256 MiB** whole-run RSS assertion at **256.15625 MiB** in the later POP3 phase; SMTP itself peaked at **211.53125 MiB**. Its content-transfer stages finished, but the subsequent interruption check was not reached. It is retained as a failed diagnostic, not included in passing latency means. Repeating the same instrumented source passed at **240.671875 MiB** peak. These samples do not establish a hard whole-process RSS bound. The existing 1 GiB limit applies to active S3 part capacity.

| Run | SMTP start UTC | SMTP end UTC | SMTP CPU s / average % | SMTP peak MiB |
| --- | --- | --- | ---: | ---: |
| workers4-1 | 2026-09-10T20:56:18.798383Z | 2026-09-10T20:56:23.111817Z | 11.760 / 272.64% | 189.54688 |
| workers4-2 | 2026-09-10T20:57:00.098039Z | 2026-09-10T20:57:04.441436Z | 11.810 / 271.91% | 206.04688 |
| workers4-profile | 2026-09-10T20:56:39.606076Z | 2026-09-10T20:56:44.025338Z | 11.830 / 267.69% | 184.60938 |
| workers4-timings-failed | 2026-09-10T20:59:08.515542Z | 2026-09-10T20:59:12.807108Z | 11.720 / 273.09% | 211.53125 |
| workers4-timings-retry | 2026-09-10T21:01:07.016840Z | 2026-09-10T21:01:11.322727Z | 11.690 / 271.49% | 203.01562 |

The first two rows have no source overlay. The CPU profile is `/tmp/fma-workers4-smtp.cpu`; other diagnostics are `/tmp/fma-workers4-profile.json`, `/tmp/fma-workers4-timings-failed.json` and `/tmp/fma-workers4-timings-retry.json`, with corresponding logs. The failed JSON was reconstructed from the benchmark's emitted assertion data and server log because its normal success-report path was not reached. Tables retain the results without embedding logs or probe code.

Normal-run application SHA-256: `6dc9fa3776c7ee308703f543f51f57fb3ab7b3874ed21cb0e4e4f0a3a55acd3c`. Benchmarks record base checkout `ed6a3d57be9d39aab46604a49f98c16b17855d67` plus the then-uncommitted default-worker change.

| Run | Executable SHA-256 |
| --- | --- |
| workers4-1 | `ab301cb39fd761e9daec21563a09507650456549246f576eb095d8729e8941b8` |
| workers4-2 | `ab301cb39fd761e9daec21563a09507650456549246f576eb095d8729e8941b8` |
| workers4-profile | `a5734ae3dc0039655c1c98a3cdcef43f0dea1b76fcb2d6dc554f10d7ed85b4a7` |
| workers4-timings-failed | `ff0ad55e167409df634bbf0d530d74a744fd98785c1823ddea3e26c7d38ec8b5` |
| workers4-timings-retry | `ff0ad55e167409df634bbf0d530d74a744fd98785c1823ddea3e26c7d38ec8b5` |

**`make test` and `make build` passed** for the default-four change, including the configuration regression, task-pool race tests and native protocol suite. Records: `/tmp/fma-workers4-tests.log` and `/tmp/fma-workers4-build.log`. To reproduce the default configuration, run `python3 scripts/test_streaming.py --mail --jmap-first --seed 20260910 --report /tmp/fma-workers4.json` without a worker override. **SMTP remains above the 3-second target.**


## Parallel original-MIME digest and gzip consumers (2026-09-10)

Previously, the receiver synchronously hashed original MIME for 1.030694 s and wrote gzip/downstream storage for 0.544498 s in the timing diagnostic above. `mimeStreamBlocks.receive` now fans each immutable block out to three ordered consumers: the MIME parser, a gzip/object writer, and a SHA-256 writer. The receiver only reads and dispatches. `jmapBlobWriter.parallelDigest` suppresses its inline hash; Commit waits for all consumers before deriving the raw blob ID and publishing metadata. Failed or short writes cancel reception and abort publication. S3 multipart requests already upload concurrently as full parts become available; gzip retains its ordered single-stream state.

The three 1 MiB allocations are shared, not copied for each consumer. Atomic reference counts return a block only after every consumer has finished. The default remains four ingestion workers. Each admitted ingestion owns two auxiliary goroutines, started only after its parser actually begins; admission is held until both helpers exit. Helpers do not submit dependent jobs to the same pool, allowing `-stream-workers 1` without pool starvation. Queued jobs do not allocate the rotating blocks. The existing global 1 GiB active S3 part-capacity limit and separate raw/part compression pools remain in force.

Two fresh ordinary 2 GiB runs measured SMTP **4.107 / 3.993 s**, mean **4.050 s**, versus the preceding default-four mean **4.328 s**: **6.4% less elapsed time / 6.9% higher throughput**. SMTP CPU rose from **11.785 to 12.860 CPU s**, **9.1% more CPU work**; average occupancy rose from approximately 272% to 318% (100% = one core). This is a latency/CPU tradeoff, not a reduction in total computation. SMTP alone and SMTP plus first attachment download still miss three seconds. First JMAP download remains direct persisted-part streaming, measured before IMAP/POP3; no repeated-read cache is introduced.

### Complete transfer measurements

All rows use 2,147,483,648 decoded attachment bytes and 2,938,662,361 MIME bytes, seed 20260910. The first three use four workers and incompressible data; the last uses compressible data and **one worker** to validate the minimum configuration. The profile row includes CPU-profiler start/stop overhead and is excluded from ordinary means. All four runs passed content hashes, the 256 MiB sampled RSS assertion, no-local-spool and interrupted-upload checks. RSS is sampled every 50 ms; CPU and RSS cover fma, excluding the client and Fals3y.

| Run | Raw up s | Raw down s | SMTP s | IMAP down s | APPEND s | POP3 s | JMAP metadata s | First JMAP down s | Whole-run peak MiB |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| fanout-1 | 1.831 | 0.904 | 4.107 | 1.977 | 4.222 | 1.772 | 0.004 | 0.888 | 249.67188 |
| fanout-2 | 1.800 | 0.884 | 3.993 | 1.984 | 4.143 | 1.848 | 0.004 | 0.902 | 243.82812 |
| fanout-profile | 1.823 | 0.900 | 4.220 | 1.919 | 4.214 | 1.857 | 0.007 | 0.896 | 254.46875 |
| fanout-compressed-one | 1.462 | 0.849 | 3.630 | 2.140 | 3.971 | 2.254 | 0.004 | 0.852 | 131.34375 |

| Run | SMTP start UTC | SMTP end UTC | SMTP CPU s / average % | SMTP peak MiB |
| --- | --- | --- | ---: | ---: |
| fanout-1 | 2026-09-10T21:12:04.601121Z | 2026-09-10T21:12:08.707933Z | 12.920 / 314.60% | 190.95312 |
| fanout-2 | 2026-09-10T21:12:32.404133Z | 2026-09-10T21:12:36.396915Z | 12.800 / 320.58% | 201.00000 |
| fanout-profile | 2026-09-10T21:13:24.286090Z | 2026-09-10T21:13:28.506131Z | 12.930 / 306.40% | 214.14062 |
| fanout-compressed-one | 2026-09-10T21:13:57.064567Z | 2026-09-10T21:14:00.694137Z | 9.010 / 248.24% | 96.51562 |

### Remaining critical path

The temporary profile/timing overlay measured the following overlapping branches during SMTP. Writer call time includes downstream waits and scheduling; it is not pure CPU time and must not be added to receiver/parser wall time.

| Branch / interval | Elapsed s |
| --- | ---: |
| Receiver, including consumer completion | 3.982449 |
| Protocol Read calls within receiver | 3.283252 |
| Wait for a reusable block within receiver | 0.690395 |
| Raw gzip/object Write calls in auxiliary consumer | 1.060034 |
| Raw SHA-256 Write calls in auxiliary consumer | 1.198349 |
| Parser | 4.008562 |
| Parser input wait, included above | 0.145980 |
| Final raw commit and metadata publication | 0.010369 |

The parser still takes about four seconds while the receiver catches up with it. Removing receiver-side hashing does not subtract its old elapsed duration from the end-to-end critical path: consumers now compete for CPU and each shared block waits for its slowest reader. In this profile, protocol Read calls also take longer than before. More detailed scheduling traces would be needed to assign that increase to specific contention sources.

CPU profiling sampled **11.93 CPU s** over **4.21 s**. Flat costs include `syscall.syscall` **2.43 CPU s**, `runtime.pthread_cond_signal` **2.42**, `runtime.pthread_cond_wait` **1.13**, SHA-256 `blockSHA2` **1.88**, SMTP `lineLimitReader.Read` **0.99** (2.41 cumulative), MIME `base64Filter.fill` **0.73** (1.05 cumulative), and Base64 Decode **0.40** (0.44 cumulative). The prior four-worker profile had signal/wait costs **1.62 / 0.82 CPU s**: more consumer wakeups are a material cost of this implementation. SHA-256 samples still include raw identity, decoded-part identity and AWS HTTP payload signing. No checksums or protocol line checks were disabled. The next optimization should reduce parser/framing work and wakeup overhead before increasing worker counts again.

### Provenance and verification

This is an application change on base commit `7b1471753883de288b52d01151a7aa873d262212` plus the working-tree patch recorded by each report. No dependency or upstream SMTP PR was modified. Versions remain Go 1.25.3 on Apple M4/macOS arm64; [MIME fork ea6016819f55](https://github.com/Jabberwocky238/naust-jmap/commit/ea6016819f557258bc1adf95488dca474c37e846), module `v0.3.4-0.20260910204519-ea6016819f55`; [SMTP fork b0673510e580](https://github.com/Jabberwocky238/go-smtp/commit/b0673510e580); [POP3 fork v0.1.6](https://github.com/Jabberwocky238/go-pop3/tree/v0.1.6); AWS core v1.41.5 / S3 v1.97.3; klauspost/compress v1.20.0; Fals3y 0.3.1-dev.b8e48bf (same executable SHA-256 as the preceding section). This application-only change has no new upstream PR.

Production `main.go` SHA-256: `d453f5dd131d5ca4dad82c9da4b8180b88099b29ba4ff4f657489aea244d5348`.

| Run | Executable SHA-256 |
| --- | --- |
| fanout-1 | `30e0eda55ced639b670e01746ed60ed90cc597185d7e981877e2f0d9cc33cdd4` |
| fanout-2 | `30e0eda55ced639b670e01746ed60ed90cc597185d7e981877e2f0d9cc33cdd4` |
| fanout-profile | `73a9b238de69bb3adcef21cc7711daf525d762c153fda75a790cc7e070090423` |
| fanout-compressed-one | `30e0eda55ced639b670e01746ed60ed90cc597185d7e981877e2f0d9cc33cdd4` |

Reports are `/tmp/fma-fanout-{1,2,profile}.json` and `/tmp/fma-fanout-compressed-one.json`; the temporary CPU profile is `/tmp/fma-fanout-smtp.cpu`. Probe code and logs are not committed. Reproduce normal runs with `python3 scripts/test_streaming.py --mail --jmap-first --seed 20260910 --report /tmp/fma-fanout.json`; add `--data compressible --stream-workers 1` for the minimum-worker compression check. Tests cover genuinely overlapping writers, byte ordering/ownership across multiple blocks, cancellation, failed and short writes, and the existing first-read/restart/account-isolation checks.

**`make test` and `make build` passed** for the final implementation, including race tests and native protocol integration. Validation records are `/tmp/fma-fanout-final-tests.log` and `/tmp/fma-fanout-build.log`.


## Receive-branch timing and buffer-pool experiments (2026-09-10)

Five new 2 GiB runs compared larger shared rings, independent rings and separately timed receive branches. These are temporary source-overlay experiments on application commit `8b774f6ce1eab0ed4364948c4a9fdac3ba562d33`; production remains at three shared 1 MiB blocks pending a demonstrated latency/memory tradeoff. No repeated-read cache was used. The independent-ring variant gives the parser 16 private blocks and the raw hash/gzip consumers 16 shared blocks, requiring one extra full-MIME copy into the parser ring. Merely allocating separate pools while retaining the same shared-block lifetime would not remove the dependency on the slowest consumer.

### A–F branch measurements

The two timing runs use the same instrumentation, without CPU sampling. A is the complete protocol-facing Read, not isolated socket time. E is measured around decoder Read calls and separately around its underlying MIME body Read calls. F's hash is measured inside the library identity writer; F's gzip/object Write and Commit are measured at the application part writer. C and D are measured in their separate ordered consumers. All values are elapsed call durations including preemption; none should be presented as pure CPU work.

| Branch / interval | Shared 3 blocks, s | Shared 16 blocks, s |
| --- | ---: | ---: |
| A: protocol Read calls (TCP + SMTP framing/line checks) | 3.231492 | 3.309177 |
| B: receiver wait for a reusable block | 0.668871 | 0.535851 |
| A+B: receiver overall, including writer completion | 3.909045 | 3.853964 |
| C: raw MIME SHA-256 Write calls | 1.175537 | 1.214616 |
| C: wait for input / queue closure | 2.732749 | 2.638575 |
| D: raw MIME gzip/object Write calls | 1.019026 | 1.174804 |
| D: wait for input / queue closure | 2.888406 | 2.677674 |
| E: decoded Read calls, including boundary/input reads | 2.738991 | 2.687303 |
| E child: boundary/input Read calls | 0.625965 | 0.539812 |
| E remainder: decode/filter calls excluding body Read | 2.113025 | 2.147491 |
| F: decoded attachment SHA-256 calls | 0.759396 | 0.765358 |
| F: decoded attachment gzip/object Write calls | 0.396406 | 0.401813 |
| F: attachment Commit | 0.026989 | 0.027210 |
| Parser child: wait for input blocks / EOF | 0.096581 | 0.000562 |
| Parser child: input Read calls including wait and copy | 0.180882 | 0.092572 |
| E+F: complete parser task before admission-release wait | 3.938597 | 3.900574 |
| Final raw Commit plus metadata publication | 0.009179 | 0.009847 |

The E and F operations run sequentially within the attachment parser: for the three-block run, **2.738991 + 0.759396 + 0.396406 + 0.026989 = 3.921782 s**, compared with **3.935741 s** measured for the complete attachment leaf. Remaining time includes other sinks, loop/timing overhead and finalization. The 21-byte text leaf adds 0.002649 s. Do not add the boundary/input child duration to the decoder parent, or add C/D to E+F: the raw consumers overlap parsing. The final application parser duration also includes MIME headers, structure handling and the epilogue/EOF drain. The ready channel closes only after the raw writers finish, so parser duration alone cannot identify the slowest consumer.

The raw hash and gzip streams overlap almost completely. In timed3 their first/last Write timestamps are **2026-09-10T21:24:20.138174Z–21:24:24.047059Z** and **21:24:20.138131Z–21:24:24.047061Z** respectively. Each spans approximately 3.909 s, but most of that span is waiting for input. In timed16 both span approximately 3.854 s, starting around **21:24:47.48764Z** and ending around **21:24:51.34147Z**.

Increasing the shared ring from 3 to 16 blocks reduced parser input wait from **0.096581 to 0.000562 s** and receiver free-block wait from **0.668871 to 0.535851 s**. Yet SMTP changed only **3.968 → 3.932 s**, a **0.9%** difference in this instrumented pair. The parser is essentially no longer starved for incoming blocks, while its decode/filter, identity hash and attachment-write chain still runs for nearly 3.9 s. This supports a modest buffering benefit; it does not support the claim that buffer capacity alone explains the remaining seconds. Receiver waits are not yet attributed to the identity of the last consumer releasing each block.

### Capacity comparison and resource results

The uninstrumented larger-ring runs measured SMTP **3.887 s** (shared16), **3.867 s** (shared32) and **3.917 s** (split16+16), against the preceding three-block mean of 4.050 s. Each new capacity/layout currently has one uninstrumented sample, so the 20–50 ms differences do not establish a reliable winner. Splitting the rings did not demonstrate a throughput advantage and added a 2,938,662,361-byte copy. Increasing capacity did not materially reduce SMTP CPU work.

The shared16 run failed the existing **256 MiB** RSS assertion at **285.203125 MiB** during the later whole-protocol sequence, after transfer/content checks but before interruption checks. Its JSON was reconstructed from the emitted assertion; it is retained as a failed run. Subsequent diagnostic runs explicitly used `--max-rss-mib 512` to complete the measurements and interruption checks; this changes the diagnostic assertion only, not production limits. Passing those diagnostics must not be described as passing the original 256 MiB criterion. Per-phase and whole-run memory values follow. The existing 1 GiB limit applies to active S3 part capacity, not total process RSS.

| Run | Raw up s | Raw down s | SMTP s | IMAP down s | APPEND s | POP3 s | JMAP metadata s | First JMAP down s | Whole-run peak MiB |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| shared16 | 1.838 | 0.875 | 3.887 | 1.950 | 4.139 | 1.840 | 0.004 | 0.909 | 285.20312 |
| shared32 | 1.830 | 0.898 | 3.867 | 2.024 | 4.180 | 1.852 | 0.004 | 0.884 | 285.81250 |
| split16+16 | 1.850 | 0.875 | 3.917 | 1.917 | 4.222 | 1.854 | 0.004 | 0.883 | 288.84375 |
| timed3 | 1.813 | 0.876 | 3.968 | 1.894 | 4.168 | 1.857 | 0.004 | 0.904 | 234.20312 |
| timed16 | 1.793 | 0.877 | 3.932 | 1.976 | 4.157 | 1.868 | 0.004 | 0.888 | 271.59375 |

| Run | SMTP start UTC | SMTP end UTC | SMTP CPU s / average % | SMTP peak MiB |
| --- | --- | --- | ---: | ---: |
| shared16 | 2026-09-10T21:21:03.834092Z | 2026-09-10T21:21:07.720947Z | 12.890 / 331.63% | 214.60938 |
| shared32 | 2026-09-10T21:21:43.444300Z | 2026-09-10T21:21:47.311469Z | 12.900 / 333.58% | 252.68750 |
| split16+16 | 2026-09-10T21:22:17.336345Z | 2026-09-10T21:22:21.249402Z | 13.160 / 335.97% | 265.17188 |
| timed3 | 2026-09-10T21:24:20.133287Z | 2026-09-10T21:24:24.101604Z | 12.790 / 322.30% | 198.93750 |
| timed16 | 2026-09-10T21:24:47.481893Z | 2026-09-10T21:24:51.413870Z | 12.900 / 328.08% | 214.59375 |

All runs use four workers, incompressible seed 20260910, 2,147,483,648 decoded attachment bytes, 2,938,662,361 MIME bytes and JMAP first, before IMAP/POP3. Go 1.25.3, Apple M4/macOS arm64, Fals3y 0.3.1-dev.b8e48bf, SMTP b0673510e580, POP3 v0.1.6, AWS core v1.41.5 / S3 v1.97.3 and klauspost/compress v1.20.0 are unchanged. CPU/RSS measure fma only, with 100% = one core and RSS sampled every 50 ms.

The timing overlay uses a temporary modfile selecting the local [MIME fork ea6016819f55](https://github.com/Jabberwocky238/naust-jmap/commit/ea6016819f557258bc1adf95488dca474c37e846), with its `sinks.go` verified byte-identical to the pinned published module before instrumentation. Go prohibits overlays beneath GOMODCACHE, hence the local module selection. No production dependency files or upstream PRs were changed. The first timing launch was rejected by that overlay restriction before any benchmark ran; no timing result is assigned to it.

| Run | Executable SHA-256 |
| --- | --- |
| shared16 | `25e6a00b203d0fe1a3ed54df740b9c4e6f64c6fb519a2e9ead66ded4b1690163` |
| shared32 | `300ea5f69b5f2a2398bd8166b4336effe953eb9b094cc971676bd7c1f12903b6` |
| split16+16 | `7a7dba6596b0eb1004bbe3e0b8d5153670aadf18ca9d7707a26a121cf1eee86c` |
| timed3 | `5f90fdd555d48f0390d624993a2c3529f68c1959859923028db31106e5e343e0` |
| timed16 | `a662c780f2fdf6a23569e9062afb9744e4f1331a162124ad35a29e6c0d87ee29` |

Reports: `/tmp/fma-buffer-16-failed.json`, `/tmp/fma-buffer-32.json`, `/tmp/fma-buffer-split.json`, `/tmp/fma-branches.json`, `/tmp/fma-branches-16.json`. Each successful report retains SMTP and IMAP APPEND timing diagnostics; tables above isolate SMTP. The failed shared16 report retains the emitted transfer data. Timer code, modfiles and full logs remain temporary and are not embedded or committed. Application production source remains unchanged; this update records experiments and measurements.

The timing overlay passed targeted race tests for MIME streaming and stored-part first reads (`/tmp/fma-branches-race.log`). All four diagnostics with the explicit 512 MiB assertion completed content, protocol and interruption checks. This commit changes documentation only; production code remains the previously tested `8b774f6` implementation.


## Single-account load slowdown, stopped diagnostic (2026-09-10)

The intended workload was 5,000 SMTP DATA deliveries, each with a distinct 2 MiB attachment, initially targeting one account with 16 persistent SMTP connections and four ingestion workers. Every subject, Message-ID and attachment payload was distinct; no automatic retries or repeated-file cache was used. The user then selected 16 accounts and subsequently paused further testing. The original single-account run was stopped, and **no 16-account run started**.

The stopped run recorded **859 successful SMTP acknowledgements**. Its last complete progress sample was **856 successes in 371.73 s**, with zero observed errors during normal operation. Stopping fma generated 16 connection/disconnection errors and terminated the harness; these are shutdown artifacts, not a measured steady-state failure rate. The full 5,000-message target and post-load mailbox/content verification were not completed. Do not describe 859 SMTP acknowledgements as independently verified stored-email count.

### Throughput decay

Rows represent ranges of completed requests, not particular submission IDs; requests finish out of order. Throughput is calculated from client progress timestamps. Payload throughput counts decoded attachment bytes only.

| Completion range | Interval s | Messages/s | Attachment MiB/s |
| --- | ---: | ---: | ---: |
| 1–250 | 18.24 | 13.706 | 27.412 |
| 251–500 | 66.17 | 3.778 | 7.556 |
| 501–750 | 178.57 | 1.400 | 2.800 |
| 751–856 | 108.75 | 0.975 | 1.949 |

The last interval is **92.9% slower in throughput** than the first, approximately a **14.1-fold** decline. Attachment size and configured concurrency stayed fixed. These are completion rates, not individual message latency percentiles. The interrupted harness did not finish persisting its latency distribution or integrated CPU/RSS measurements, so no p95/p99, whole-run CPU average or sampled peak is claimed.

Read-only process observations during the run showed fma at roughly 680–782% CPU (100% = one core) and approximately 304–342 MiB RSS; one simultaneous observation showed Fals3y at 51.3% CPU and 21.1 MiB RSS. These are snapshots, not averages or maxima. A HEAD request during the run measured `alice/.jmap/state.json` at **2,092,645 bytes**. Its exact associated completed-message count was not sampled atomically.

### Confirmed amplification in the metadata adapter

The application stores an account's JMAP logical key/value database in one `state.json`. In `main.go`, `jmapBackend.snapshot` downloads that whole object and unmarshals the entire map; `Get` does this even for a single logical record. `Scan` also downloads/parses the entire map, sorts all keys, and only then filters the requested range. `WriteBatch` reads/parses the whole map, applies a small batch, marshals the whole map and conditionally replaces the object. Conflicts can repeat this work, with a maximum of 128 attempts; this run did **not** measure the actual retry count.

There is an additional concrete integration gap: core v0.4.2's `objectdb.getManyRaw` supports the optional `backend.MultiGetter` interface, but the application's `jmapBackend` does not implement it. The library therefore falls back to sequential `Get` calls. Reading K records can cause K full account snapshot downloads/parses. Also, `deliverMailStream` obtains the mailbox catalog and then calls `mailbox`, which obtains the catalog again; `catalog → all → GetMany` repeats metadata reads before import. Normal incoming SMTP passes `unique=false` to `importStoredMail`, so its optional full-email deduplication scan is **not** the explanation for this particular workload.

These verified code paths provide a strong explanation for degradation as account metadata grows. They do not constitute a CPU profile of this run: JSON time, S3 metadata read time, lease waiting, GC and conflict retries were not separately timed. The data do not establish an exact complexity exponent or prove that every lost second belongs to JSON processing. Buffer expansion alone cannot remove the demonstrated full-account read/write amplification.

Next fixes to consider, without restarting load tests yet: implement request-local batch reads through `MultiGetter` (one fresh snapshot per batch, no cross-request cache); eliminate duplicate catalog reads; then replace monolithic account storage with indexed/sharded persistent records while preserving atomic batch visibility and account isolation. Simply mapping each key to an unrelated S3 object would lose existing multi-key transaction semantics and is not a complete replacement.

### Version and data provenance

This run used application `3ce6cb3` and a temporary modfile selecting the local MIME fork based on `ea6016819f55`, with **uncommitted encoded-read-ahead changes**. Base64 attachment ingestion reads ahead through four independently owned 1 MiB blocks and decodes concurrently; this path is restricted to required storage sinks. Generic identity-only parsing retains the earlier bounded path. The candidate passed the library's full race suite after this restriction. It was not published or selected by the production go.mod at the time of the load run. Go 1.25.3, AWS core v1.41.5 / S3 v1.97.3, SMTP b0673510e580, POP3 v0.1.6 and Fals3y 0.3.1-dev.b8e48bf remain unchanged.

Measured executable SHA-256: `ccc1d2464dd8cdb0572008a86e0022c010b9d9e9e95b590595c3d1fa433f46e1`. Candidate `sinks.go` SHA-256: `9b1508a56f270e0f70d3caefe5a1beea7114ad1537b74eacbe35cd2f6baeea01`.

The retained reports are `/tmp/fma-stress-single-partial.json` and `/tmp/fma-stress-single-partial.log`; `/tmp/fma-stress-single-server.log` records server messages before shutdown. Temporary fixture storage was cleaned up by the harness. No new benchmark, stress run or production code change was performed for this analysis.


## Mail-owned state and memory-only indexes (2026-09-10)

This change addresses the whole-account amplification recorded in the stopped
single-account diagnostic above. Runtime metadata no longer uses the old
whole-image adapter. That adapter remains solely for private, in-memory bootstrap
and as a benchmark comparison. Bootstrap publishes the new owner format directly;
its temporary indexes are never uploaded.

`<account>/.jmap/state.json` now contains folders, identities, UID/state counters,
the account lease, and the current transaction decision. Email, Thread,
EmailSubmission, upload ownership and change history records live under `mail/`.
Each mutation prepares only changed authoritative records and conditionally
publishes the account decision. The previous decision is materialized before the
next write; periodic and shutdown flushes also finish committed records. A failed
head write does not publish its prepared records. Concurrent writers use the
account ETag, and record materialization also uses conditional writes.

Property, membership, blob-reference, account/worklist and collection-hint indexes
are rebuilt in memory from records using the registered descriptors and the
library's public sort codec. Get and MultiGet use the shared account cache; Scan
uses a sorted key range rather than decoding and sorting every record on each
call. Value-only updates retain the ordered keys. Index-only and unchanged batches
do not write S3. Idle accounts with no pending materialization do not generate
background version reads. Foreground reads check the account version at most once
per second; writes force a version check.

Blob identity/encoding descriptors and MIME metadata are stored beside the mail
bytes. The blob ID lookup map is reconstructed from S3 object names, not persisted
as an account index. Attachment parent lookups derive from the directory layout;
new `.origin.json` and legacy `.part` indexes are not written. Legacy part locators
are memory-only. Submission scheduling discovers account prefixes using delimiter
listing on S3 and reconstructs its in-memory worklist after restart, without
`.jmap-queue` markers. Old blobs and old metadata remain readable.

### Adapter read microbenchmark

Command, Apple M4 / darwin arm64, two 100 ms samples per case:

```sh
go test -run '^$' -bench BenchmarkJMAPMetadataGet -benchmem -benchtime=100ms -count=2
```

Each account contains the stated number of small Thread records. Both adapters
use the same in-memory object store, so this isolates JSON/metadata adapter work:
there is **no S3 network latency**. Setup, migration, cold recovery and periodic
version checks are outside these short warm-read measurements. These figures are
not SMTP throughput or production latency estimates.

| Records | Old whole-image Get, ns/op (two runs) | Owner-cache Get, ns/op (two runs) | Old B/op, approximately | Owner B/op | Old allocations/op | Owner allocations/op |
| ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| 100 | 56,580 / 56,846 | 65.56 / 67.06 | 37,480 | 112 | 320 | 2 |
| 1,000 | 595,880 / 593,091 | 66.08 / 64.97 | 473,450 | 128 | 3,032–3,033 | 2 |
| 10,000 | 5,961,842 / 5,913,375 | 67.71 / 66.99 | 4,351,200 | 128 | 30,091 | 2 |

The measured result is removal of per-Get whole-account parsing/allocation.
Regression tests separately count storage operations: 100 warm MultiGet/Scan
pairs perform no S3 reads or lists, an index-only batch performs no S3 writes,
and a small owner update does not list unrelated records. Fault injection checks
that a rejected account commit cannot reveal its prepared mail or user changes.
Four independent node caches concurrently commit 80 counter/mail-record updates;
a cold reader must observe both final values together. Cold-recovery tests compare
property/reference index keys with the library's live writes and verify account
and submission worklist reconstruction.

### Remaining costs and compatibility

Cold starts and externally changed account versions still list/read owner records
and rebuild indexes. Same-account writes remain serialized; this change does not
remove account-lease contention. Ordered-key membership changes merge in memory,
so they still have a cost proportional to the key count. All accessed account and
blob indexes remain resident; there is no eviction policy. Prepared transaction
objects, deletion tombstones and unreferenced blobs are retained without automatic
cleanup, so long-running S3 listing/recovery cost can grow. The current account
file is proportional to folder/user state and the latest changed-record batch,
rather than all stored emails and derived query indexes.

Stop every old-version node before upgrading. The previous writer cannot read the
new format; old-format account snapshots migrate on access. This change preserves
single-file runtime/tests and introduces no database, local index or spool.

Final verification: `make test`, `make build` and `git diff --check` passed.
The full suite includes race tests, native S3, SMTP/IMAP/POP3/JMAP, queued
submission recovery after restart, deployment/installer tests, and the 32 MiB
streaming content/compression/RSS checks. Local logs are
`/tmp/fma-owner-complete-tests.log`, `/tmp/fma-owner-build.log` and
`/tmp/fma-owner-benchmark.log`. The four-node ownership regressions also passed
three consecutive race-enabled runs.


## Post-state-refactor 2 GiB protocol measurements (2026-09-10)

Tested the committed `ea11b06` application with a clean working tree before this documentation update. The runtime source was not changed for these tests. Apple M4, macOS arm64, Go 1.25.3, four default ingestion workers, native Fals3y `0.3.1-dev.b8e48bf`, loopback networking. Five runs executed sequentially, each with a newly built process and isolated temporary S3 storage. These are local end-to-end protocol measurements, not remote AWS S3 throughput or cold OS page-cache measurements.

Every run transfers **2,147,483,648 attachment bytes (2 GiB)**. SMTP, IMAP and POP3 transfer **2,938,662,361 MIME bytes (about 2.737 GiB)** after Base64/MIME framing. The seed is 20260910; incompressible fixtures repeat a random 1 MiB block whose period exceeds the gzip window. JMAP attachment metadata/download are measured immediately after SMTP, before IMAP/POP3 reads. SMTP includes the connection/envelope and final storage acknowledgement; IMAP APPEND includes the command and final acknowledgement.

### Per-run elapsed time

All times below are seconds. `DATA` and `BDAT` rows are separate SMTP transfer modes.

| Run | Raw JMAP up | Raw JMAP down | SMTP write | IMAP down | IMAP APPEND | POP3 down | JMAP metadata | First attachment down | Peak RSS MiB |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| data-1 | 1.863 | 0.850 | 4.075 | 1.978 | 4.157 | 1.855 | 0.003 | 0.912 | 239.34 |
| data-2 | 1.848 | 0.863 | 3.932 | 1.940 | 4.048 | 1.849 | 0.004 | 0.871 | 238.97 |
| bdat-1 | 1.971 | 0.866 | 4.051 | 1.926 | 4.075 | 1.831 | 0.003 | 0.892 | 233.38 |
| bdat-2 | 1.791 | 0.865 | 4.088 | 1.918 | 4.096 | 1.817 | 0.003 | 0.887 | 231.84 |
| compressible | 1.447 | 0.842 | 3.626 | 2.069 | 3.922 | 2.248 | 0.003 | 0.834 | 121.00 |

### Incompressible averages and write throughput

Shared paths average all four incompressible runs; each SMTP mode averages its two runs. Payload throughput uses the 2 GiB decoded size for every path. MIME wire throughput uses the larger MIME size and must not be confused with payload throughput.

| Write path | Samples | Mean seconds | Payload MiB/s | MIME wire MiB/s |
| --- | ---: | ---: | ---: | ---: |
| JMAP raw upload | 4 | 1.8682 | 1096.2 | — |
| SMTP DATA | 2 | 4.0035 | 511.6 | 700.0 |
| SMTP BDAT | 2 | 4.0695 | 503.3 | 688.7 |
| IMAP APPEND | 4 | 4.0940 | 500.2 | 684.5 |

SMTP DATA averages **4.0035 s** and BDAT **4.0695 s**. This pair does not demonstrate a BDAT advantage. DATA is close to the preceding production three-block mean of 4.050 s; the roughly 1% difference is not evidence of a significant single-message speedup from the state refactor. This workload uses an almost empty account, so it does not measure the removed whole-account metadata amplification at high message counts. Both SMTP modes and IMAP APPEND remain above three seconds.

The compressible fixture is one additional sample, not a matched two-run mean: raw upload **1.447 s**, SMTP DATA **3.626 s**, IMAP APPEND **3.922 s**. Compression reduces stored bytes and memory, but does not eliminate MIME parsing, hashing or transfer work. Some compressed download timings are slower; do not treat a single sample as a regression finding.

All five runs exited successfully and passed content/hash, storage-encoding, mail-prefix and no-local-spool assertions, as well as the original **256 MiB** sampled fma RSS criterion. Incompressible whole-run peaks were **231.84–239.34 MiB**; the compressible peak was **121.00 MiB**. The harness also exercised interrupted upload handling. CPU/RSS measure fma only, excluding Fals3y and the Python client; RSS sampling is every 50 ms.

### Reproduction and provenance

```sh
python3 scripts/test_streaming.py --mail --jmap-first --seed 20260910 --size-mib 2048 --report /tmp/fma-data.json
python3 scripts/test_streaming.py --mail --jmap-first --seed 20260910 --size-mib 2048 --smtp-transfer bdat --report /tmp/fma-bdat.json
python3 scripts/test_streaming.py --mail --jmap-first --seed 20260910 --size-mib 2048 --data compressible --report /tmp/fma-compressible.json
```

Reports and full logs are retained locally in `/tmp/fma-2gib-20260910T222508Z` as `<run>.json` and `<run>.log`; `summary.json` contains calculated means. Each report records phase UTC timestamps, fixture hashes, executable identity, CPU, RSS and throughput. Temporary test buckets/processes were cleaned by the harness.

| Run | First upload start UTC | Last measured phase end UTC |
| --- | --- | --- |
| data-1 | 2026-09-10T22:25:09.898889+00:00 | 2026-09-10T22:25:27.137661+00:00 |
| data-2 | 2026-09-10T22:25:29.721193+00:00 | 2026-09-10T22:25:46.709390+00:00 |
| bdat-1 | 2026-09-10T22:25:49.256838+00:00 | 2026-09-10T22:26:06.548272+00:00 |
| bdat-2 | 2026-09-10T22:26:09.058219+00:00 | 2026-09-10T22:26:26.203159+00:00 |
| compressible | 2026-09-10T22:26:28.715879+00:00 | 2026-09-10T22:26:45.386663+00:00 |

All five runs used executable SHA-256 `a19d4aa7dc2bfb16a7d9fdde695f4eab1c63b52c42ce9f3ad98ed8c2f2bf6bb5`. This update records measurements only; no runtime code or tuning parameters changed.
