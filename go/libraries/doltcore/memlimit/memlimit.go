// Copyright 2026 Dolthub, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package memlimit

import (
	"math"
	"runtime/debug"
	"sync"
)

// Default cache sizes matching current Dolt hardcoded values.
const (
	DefaultNodeCacheSize     = 256 * 1024 * 1024 // 256 MiB
	DefaultMemtableSize      = 256 * 1024 * 1024 // 256 MiB
	DefaultDecodedChunksSize = 1 << 25            // 32 MiB

	// Minimum sizes below which caches become pathologically small.
	minNodeCacheSize     = 16 * 1024 * 1024 // 16 MiB
	minMemtableSize      = 4 * 1024 * 1024  // 4 MiB
	minDecodedChunksSize = 4 * 1024 * 1024  // 4 MiB
)

// Budget holds computed byte sizes for Dolt's major caches.
type Budget struct {
	NodeCache     int
	Memtable      uint64
	DecodedChunks uint64
}

var (
	once    sync.Once
	current Budget
)

// Init reads GOMEMLIMIT and partitions the memory budget across caches.
// When GOMEMLIMIT is not set, all sizes remain at their current defaults.
// Safe to call multiple times; only the first call takes effect.
func Init() {
	once.Do(func() {
		current = compute()
	})
}

func compute() Budget {
	limit := debug.SetMemoryLimit(-1)
	if limit == math.MaxInt64 || limit <= 0 {
		return defaults()
	}

	// Reserve 25% for GC heap, goroutine stacks, and working memory.
	usable := float64(limit) * 0.75

	b := Budget{
		NodeCache:     int(usable * 0.50),     // 50% — dominant read cache
		Memtable:      uint64(usable * 0.30),  // 30% — write buffer
		DecodedChunks: uint64(usable * 0.10),  // 10% — decoded value cache
		// remaining 10% of usable = headroom within the 75%
	}

	// Clamp to minimums.
	if b.NodeCache < minNodeCacheSize {
		b.NodeCache = minNodeCacheSize
	}
	if b.Memtable < minMemtableSize {
		b.Memtable = minMemtableSize
	}
	if b.DecodedChunks < minDecodedChunksSize {
		b.DecodedChunks = minDecodedChunksSize
	}

	// Don't exceed defaults — no benefit to larger caches than current behavior.
	if b.NodeCache > DefaultNodeCacheSize {
		b.NodeCache = DefaultNodeCacheSize
	}
	if b.Memtable > uint64(DefaultMemtableSize) {
		b.Memtable = uint64(DefaultMemtableSize)
	}
	if b.DecodedChunks > uint64(DefaultDecodedChunksSize) {
		b.DecodedChunks = uint64(DefaultDecodedChunksSize)
	}

	return b
}

func defaults() Budget {
	return Budget{
		NodeCache:     DefaultNodeCacheSize,
		Memtable:      uint64(DefaultMemtableSize),
		DecodedChunks: uint64(DefaultDecodedChunksSize),
	}
}

// NodeCacheSize returns the byte size for the prolly tree node cache.
func NodeCacheSize() int {
	Init()
	return current.NodeCache
}

// MemtableSize returns the byte size for the NBS memtable write buffer.
func MemtableSize() uint64 {
	Init()
	return current.Memtable
}

// DecodedChunksSize returns the byte size for the ValueStore decoded chunks cache.
func DecodedChunksSize() uint64 {
	Init()
	return current.DecodedChunks
}
