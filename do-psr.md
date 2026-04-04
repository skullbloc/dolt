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

## 9. Recommended Next Steps

1. **Immediate**: Set `GOMEMLIMIT=150MiB` and `--pprof-server` on next Dolt restart
2. **This week**: Implement the `membench` baseline test to establish measurement
3. **First code change**: Make prolly node cache size configurable via env var (highest impact, lowest risk)
4. **Then**: Profile with pprof under Gas Town load to identify remaining hot spots
5. **Ongoing**: Integrate membench into CI to catch regressions
