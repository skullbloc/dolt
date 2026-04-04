# PSR: Dolt RAM Usage Investigation & Improvement Test Harness

**Bead**: do-psr  
**Author**: harrison (crew)  
**Date**: 2026-04-04  

---

## 1. Executive Summary

Dolt sql-server serving Gas Town's 5 databases (~500 total rows, ~153MB on disk) consumes **~243MB RSS**. The primary culprits are fixed-size global caches that don't scale down for small workloads:

| Component | Size | Location |
|-----------|------|----------|
| Prolly node cache | **256MB** (hardcoded) | `go/store/prolly/tree/node_store.go:31` |
| memTable (write buffer) | **128MB** default | `go/store/nbs/store.go:69` |
| dbfactory memTableSize | **256MB** default | `go/libraries/doltcore/dbfactory/factory.go:69` |
| Manifest cache | **8MB** | `go/store/nbs/store.go:72` |
| hasCache | **2MB** (100k entries) | `go/store/nbs/store.go` |

The Go runtime, Dolt binary (~119MB mapped), and per-database metadata overhead account for the remainder. **No GOGC or GOMEMLIMIT tuning is applied.**

---

## 2. Live Process Profile

**Process**: PID 1097846, `dolt sql-server --config /home/admin/gt/.dolt-data/config.yaml`

| Metric | Value |
|--------|-------|
| VmRSS | 243MB |
| VmSize | 2.6GB |
| VmSwap | 27MB |
| Threads | 13 |
| Private Dirty (actual heap) | 174MB |
| File-backed (mmap'd journals) | ~65MB |
| Binary text/rodata | ~119MB |

### Databases Served

| Database | Rows | Commits | Disk |
|----------|------|---------|------|
| hq | ~400 | 320 | 86MB |
| ck3proj | ~0 | 621 | 53MB |
| gascity | ~9 | 10 | 13MB |
| doltdb | ~4 | 9 | 328K |
| do | ~0 | 5 | 252K |

**Data-to-RAM ratio**: ~2,400x more RAM than actual row data. The cost is **per-database**, not per-row — each database loads schema metadata, prolly tree roots, commit graph structures, and journal indexes regardless of content.

---

## 3. Memory Architecture Analysis

### 3.1 Prolly Node Cache (Dominant Consumer)

**File**: `go/store/prolly/tree/node_store.go:31`
```go
const cacheSize = 256 * 1024 * 1024  // 256MB, hardcoded
```

- 256-stripe LRU cache (`go/store/prolly/tree/node_cache.go`)
- Global singleton shared across all databases
- No configuration knob exists — requires code change to resize
- This single constant is the largest fixed memory allocation in Dolt

### 3.2 NBS MemTable (Write Buffer)

**File**: `go/store/nbs/store.go:69`
```go
defaultMemTableSize uint64 = (1 << 20) * 128  // 128MB
```

Also overridden in `go/libraries/doltcore/dbfactory/factory.go:69`:
```go
defaultMemTableSize = 256 * 1024 * 1024  // 256MB
```

This is a per-store write staging buffer. Chunks accumulate here before being flushed to disk.

### 3.3 Manifest Cache

**File**: `go/store/nbs/manifest_cache.go`
- 8MB LRU cache for database manifest metadata
- Shared globally, reasonable size

### 3.4 Memory Quota System

**File**: `go/store/nbs/quota.go`
- `MemoryQuotaProvider` interface exists but default is `UnlimitedQuotaProvider`
- Tracks allocations without enforcing limits

### 3.5 Go Runtime

- **GOGC**: Not set (defaults to 100 — heap can grow to 2x live data before GC)
- **GOMEMLIMIT**: Not set (no soft memory ceiling)
- Setting `GOMEMLIMIT=128MiB` would be the single highest-impact zero-code-change improvement

---

## 4. Enabling Profiling

Dolt supports pprof but it's **not enabled** on the current process.

**File**: `go/cmd/dolt/dolt.go:290`
```bash
dolt --pprof-server sql-server --config /path/to/config.yaml
```

This starts an HTTP pprof server on `:6060` with heap, allocs, goroutine, CPU, and trace profiles. On next restart, this should be enabled to get precise allocation breakdowns.

---

## 5. Quick Wins (No Code Changes)

1. **Set `GOMEMLIMIT=150MiB`** — pressures Go GC to return memory to OS more aggressively
2. **Set `GOGC=50`** — GC at 1.5x live data instead of 2x (trades CPU for memory)
3. **Enable `--pprof-server`** on next restart for ongoing profiling
4. **Consolidate databases** — if schemas allow, fewer databases = less per-database overhead
5. **Archive ck3proj** — 621 commits, 53MB disk, but appears to have ~0 active rows

---

## 6. Code Changes to Investigate

### 6.1 Make Prolly Node Cache Configurable

**Target**: `go/store/prolly/tree/node_store.go:31`

Replace the hardcoded `cacheSize` with an environment variable or config option:
```go
// Proposed: DOLT_NODE_CACHE_SIZE env var, default 256MB
cacheSize = envutil.GetInt64OrDefault("DOLT_NODE_CACHE_SIZE", 256*1024*1024)
```

For Gas Town's workload, 32MB would likely suffice (5 small databases, low query rate).

### 6.2 Make MemTable Size Configurable

**Target**: `go/store/nbs/store.go:69` and `go/libraries/doltcore/dbfactory/factory.go:69`

Same approach — an env var like `DOLT_MEMTABLE_SIZE` defaulting to current values.

### 6.3 Investigate Per-Database Overhead

Each of the 5 databases loads its own chunk store via `database_provider.go:55-93`. With 22 tables per database, that's 110 table descriptors. Profiling with pprof will reveal how much this costs.

---

## 7. Test Harness Design

### 7.1 Existing Infrastructure

| What | Where | Status |
|------|-------|--------|
| Server benchmarks | `go/performance/serverbench/` | Working, CPU only |
| Memory profiling | `go/performance/memprof/` | Skeleton (`b.SkipNow()`) |
| Benchmark runner | `go/performance/utils/benchmark_runner/` | External, CPU only |
| NBS benchmarks | `go/store/nbs/benchmarks/` | Working, no memory |
| Profile utilities | `go/store/util/profile/profile.go` | `--cpuprofile`, `--memprofile` flags |

### 7.2 Proposed Harness: `go/performance/membench/`

**Component A — Baseline Measurement** (`baseline_test.go`)

A Go test that:
1. Creates 5 databases in a temp dir matching Gas Town schema (22 tables, ~100 rows each)
2. Starts server in-process via `svcs.Controller` (same pattern as `serverbench`)
3. Runs representative queries (SELECT, INSERT, COMMIT)
4. After warmup, captures:
   - `runtime.MemStats` (HeapAlloc, HeapInuse, Sys, NumGC)
   - `pprof.WriteHeapProfile()` 
   - `/proc/self/status` VmRSS on Linux
5. Outputs JSON: `{rss_mb, heap_alloc_mb, heap_inuse_mb, sys_mb, num_gc, git_sha, timestamp}`

**Component B — Isolated Operation Benchmarks** (`operations_test.go`)

Standard `testing.B` with `b.ReportAllocs()`:
- `BenchmarkMultiDBStartup` — measure time/allocs to init N databases and start server
- `BenchmarkQueryMemory` — 1000 SELECTs, per-op allocations
- `BenchmarkCommitMemory` — INSERT + COMMIT cycle
- `BenchmarkDoltGC` — `CALL dolt_gc()`, measure peak memory delta

**Component C — Regression Detection** (`scripts/mem_bisect.sh`)

```bash
#!/bin/bash
# Usage: git bisect run ./scripts/mem_bisect.sh <threshold_mb>
THRESHOLD=${1:-150}
go build -o /tmp/dolt-test ./cmd/dolt
# Run baseline, extract RSS from JSON output
RSS=$(go test ./go/performance/membench/ -run TestBaseline -json | jq .rss_mb)
[ "$RSS" -lt "$THRESHOLD" ] && exit 0 || exit 1
```

**Component D — Before/After Comparison** (`scripts/mem_compare.sh`)

1. Stash changes, run baseline → `before.json`
2. Pop stash, rebuild, run baseline → `after.json`
3. Diff metrics, flag regressions >5%
4. Optionally: `go tool pprof -diff_base before.heap after.heap`

### 7.3 Key Design Decisions

- **In-process, not Docker**: Faster iteration (~seconds vs ~minutes). Docker for CI later.
- **Both RSS and heap**: RSS catches mmap/cgo; heap catches Go allocations. The gap reveals prolly cache and NBS overhead.
- **GOMEMLIMIT/GOGC as test parameters**: Run baseline under constrained memory to test behavior.
- **JSON output**: Machine-parseable for trending and CI integration.

---

## 8. Proposed Develop-Test-Evaluate Loop

```
┌─────────────┐     ┌──────────────┐     ┌─────────────┐
│  1. Change   │────▶│  2. Measure   │────▶│  3. Compare  │
│  (code edit) │     │  (membench)   │     │  (diff JSON) │
└─────────────┘     └──────────────┘     └─────────────┘
       ▲                                        │
       │            ┌──────────────┐            │
       └────────────│  4. Decide   │◀───────────┘
                    │  (keep/toss) │
                    └──────────────┘
```

1. **Change**: Edit cache sizes, add config knobs, or refactor memory-intensive paths
2. **Measure**: Run `go test ./go/performance/membench/ -run TestBaseline` → JSON output
3. **Compare**: `scripts/mem_compare.sh` diffs before/after, flags regressions
4. **Decide**: Keep changes if RSS decreases without performance regression (check serverbench too)

For regression hunting: `git bisect run ./scripts/mem_bisect.sh 150` identifies when RSS crossed a threshold.

---

---

## 9. Deep Analysis: GOMEMLIMIT

### What It Does

GOMEMLIMIT (Go 1.19+) sets a **soft limit** on total Go runtime memory (heap + stacks + GC metadata). It does not cap allocations — it changes GC scheduling. When heap approaches the limit, GC runs more aggressively and returns memory to the OS via `madvise(MADV_DONTNEED)`.

### Interaction with GOGC

Without GOMEMLIMIT, GOGC alone drives GC timing (GOGC=100 = GC at 2x live data). With GOMEMLIMIT, GC fires on **whichever trigger comes first**: the GOGC ratio or the memory limit approaching. This means GOMEMLIMIT can make GC run earlier than GOGC alone would dictate.

### Safety Valve

The GC CPU limiter caps GC CPU at ~50%. If a too-low GOMEMLIMIT would require more CPU, the runtime backs off and lets memory exceed the limit. This means a too-low value degrades throughput but **cannot freeze or OOM the process**.

### Why It Matters for Dolt

Without GOMEMLIMIT (Dolt's current state), the Go runtime holds onto freed heap pages after bursty workloads. Peak memory becomes the new RSS floor. For a long-running server with large caches, RSS appears bloated even when live data is small.

**Critical nuance**: GOMEMLIMIT controls only Go heap, not mmap'd files. Dolt's ~65MB of mmap'd journal files are invisible to it. A GOMEMLIMIT of 150MiB targets the ~174MB Go heap, not the total 243MB RSS.

### Industry Consensus

GOMEMLIMIT is considered **best practice** for long-running Go services:
- **CockroachDB**: Sets it programmatically based on `--cache` and `--max-sql-memory`
- **Weaviate**: Calls it a "game changer" for reducing OOM in containers
- **automemlimit** library (GitLab Runner, others): Auto-sets to 90% of cgroup limit

### Dolt's Current State

Zero references to `SetMemoryLimit`, `SetGCPercent`, `GOMEMLIMIT`, or `GOGC` anywhere in the Dolt codebase. No `--max-memory` flag exists. This is a gap.

### Recommended Value for Gas Town

`GOMEMLIMIT=150MiB` with `GOGC=100` (default). This targets the Go heap, leaves headroom for mmap, and the CPU limiter prevents thrashing. If we also shrink the prolly cache to 32MB, then `GOMEMLIMIT=100MiB` would be appropriate.

---

## 10. Deep Analysis: Prolly Node Cache

### What Prolly Trees Are

Prolly trees are content-addressed B-tree variants where chunk boundaries are determined by rolling hashes. Each **Node** is a flatbuffer message (4-16KB typical) containing key/value pairs. Trees are shallow: with ~50-200 branching factor, 1M rows needs only 3-4 levels. Every table, index, and schema map is a separate prolly tree.

**Every row access traverses root-to-leaf**, and a cache miss means disk I/O through the ChunkStore (index lookup + decompression). Caching is performance-critical.

### Cache Implementation

The cache (`node_store.go:72`) is a **process-global singleton** shared across all databases, tables, and connections. It's a 256-stripe LRU — first byte of the hash selects the stripe, each stripe has its own mutex and LRU list. This design optimizes for concurrent access on large servers.

### Why 256MB?

No documented rationale exists — the constant has no comment, no linked PR discussion, no benchmark reference. It's a pragmatic default for Dolt's primary deployment target: dedicated database servers with gigabytes of RAM running sysbench/TPCC workloads on millions of rows. At 4-16KB per node, 256MB holds tens of thousands of nodes, covering the hot working set of several large tables.

**For Gas Town**: 5 databases with a few hundred total rows. The entire prolly tree dataset is likely <5MB. We are reserving 256MB of address space for <5MB of useful data.

### Impact of Shrinking

| Size | Per-Stripe | Effect |
|------|-----------|--------|
| 8MB | 32KB (~2-8 nodes) | **Dangerous**: Interior nodes constantly evicted. Point lookups degrade 10-100x. |
| 32MB | 128KB (~8-32 nodes) | **Safe for small workloads**: Holds entire Gas Town working set with room to spare. |
| 64MB | 256KB | **Conservative**: Comfortable headroom for moderate growth. |
| 256MB (current) | 1MB | **Production default**: Tuned for large datasets on dedicated servers. |

### Upstream Acceptability: HIGH

**Direct precedent exists**: `DOLT_COMMIT_CACHE_SIZE` in `go/libraries/doltcore/doltdb/doltdb.go` already follows the exact pattern — env var overriding a hardcoded cache size. The `dconfig/envvars.go` file defines ~30 `DOLT_*` env vars. A `DOLT_NODE_CACHE_SIZE` env var with 256MB default is backwards-compatible, zero-risk, and follows established conventions.

---

## 11. Upstream Acceptability Assessment

Proposals ranked by likelihood of acceptance by Dolt maintainers:

### Tier 1: Near-Certain Accept

**(F) Make pprof easier to enable** — `--pprof-server` already exists. Adding a YAML config option is trivial, changes no defaults, benefits all operators.

**(B) Make prolly node cache configurable** — Follows the `DOLT_COMMIT_CACHE_SIZE` pattern exactly. ~3 files touched. Default unchanged. Benefits all memory-constrained deployments (containers, multi-tenant, embedded).

### Tier 2: Likely Accept

**(A) GOMEMLIMIT support** — Users can already set it externally, but surfacing in config improves discoverability. Alternatively, importing `automemlimit` (one line) auto-detects cgroup limits. CockroachDB and others do this.

### Tier 3: Needs Benchmarks

**(C) Configurable memTable size** — Moderate risk since it affects write performance and flush behavior. The parameter threading already exists through store constructors. Maintainers will want benchmark proof.

### Tier 4: Unlikely Accept

**(E) membench test harness** — Useful but perceived as niche test infrastructure. Better maintained in a fork unless it catches real upstream regressions.

**(D) Database consolidation** — Architectural change to Dolt's fundamental multi-database model. Wrong level of abstraction for this problem.

### Best Form Factor

**Environment variables** (`DOLT_*`) for cache sizes — matches ~30 existing env vars, requires no config schema changes, invisible to users who don't opt in. Server config YAML for pprof enablement.

---

## 12. Test Harness: First Cycle Results

### Test Created

`go/performance/membench/gastownbench_test.go` — a functional in-process benchmark that:
- Creates a temp Dolt database
- Initializes with `issues (id INT PRIMARY KEY, title TEXT, status TEXT)`
- Inserts 100 rows, reads them back
- Measures `runtime.MemStats` and `/proc/self/status` VmRSS at 5 phases

### Run Command

```bash
cd go && go test -v -tags gms_pure_go -run TestGasTownMemory -timeout 120s ./performance/membench/
```

(`-tags gms_pure_go` required — this machine lacks `libicu-devel`)

### Results

| Phase | Heap Alloc (MB) | VmRSS (KB) |
|-------|----------------|------------|
| baseline | 10.4 | 57,884 |
| after_init | 17.6 | 62,124 |
| after_engine_create | 17.7 | 62,796 |
| after_insert_100_rows | 17.9 | 66,532 |
| after_select_all | 17.7 | 66,948 |

**Delta**: +7.3MB heap, +9MB RSS for a complete init→insert→select cycle. Completed in 0.06s.

### Key Observations

1. **Repo initialization is the biggest memory event** (~7MB heap). Insert/select of 100 rows adds ~0.2MB.
2. **The test runs in-process without a server** — the 256MB prolly cache is allocated lazily, so it doesn't show up in a short test. A multi-database server test would reveal the full cache impact.
3. **Next iteration**: Extend to 5 databases with the full Gas Town schema to reproduce the production memory profile.

---

## 13. Recommended Next Steps

1. **Immediate (no code, no restart)**: Document `GOMEMLIMIT=150MiB` as a Gas Town config recommendation
2. **On next Dolt restart**: Add `--pprof-server` and `GOMEMLIMIT=150MiB` to the start command
3. **First upstream PR**: `DOLT_NODE_CACHE_SIZE` env var (follows `DOLT_COMMIT_CACHE_SIZE` precedent exactly)
4. **Second upstream PR**: YAML config option for pprof-server enablement
5. **Extend membench**: Multi-database test with full Gas Town schema to validate cache size impact
6. **Profile with pprof**: Once enabled, capture heap profiles under Gas Town load to confirm prolly cache dominance
7. **Longer term**: Propose `automemlimit` import or `--max-go-memory` flag for container-aware memory management
