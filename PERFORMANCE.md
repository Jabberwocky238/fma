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
