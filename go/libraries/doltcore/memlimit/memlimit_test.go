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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDefaults(t *testing.T) {
	b := defaults()
	assert.Equal(t, uint64(DefaultNodeCacheSize), b.NodeCache)
	assert.Equal(t, uint64(DefaultMemtableSize), b.Memtable)
	assert.Equal(t, uint64(DefaultDecodedChunksSize), b.DecodedChunks)
}

func TestComputeNoLimit(t *testing.T) {
	prev := debug.SetMemoryLimit(-1)
	defer debug.SetMemoryLimit(prev)

	debug.SetMemoryLimit(math.MaxInt64)
	b := compute()
	assert.Equal(t, uint64(DefaultNodeCacheSize), b.NodeCache)
	assert.Equal(t, uint64(DefaultMemtableSize), b.Memtable)
	assert.Equal(t, uint64(DefaultDecodedChunksSize), b.DecodedChunks)
}

func TestComputeZeroLimit(t *testing.T) {
	prev := debug.SetMemoryLimit(-1)
	defer debug.SetMemoryLimit(prev)

	debug.SetMemoryLimit(0)
	b := compute()
	// Zero is treated as "unset" — returns defaults
	assert.Equal(t, uint64(DefaultNodeCacheSize), b.NodeCache)
}

func TestCompute512MiB(t *testing.T) {
	prev := debug.SetMemoryLimit(-1)
	defer debug.SetMemoryLimit(prev)

	limit := int64(512 * 1024 * 1024) // 512 MiB
	debug.SetMemoryLimit(limit)

	b := compute()

	usable := float64(limit) * 0.75
	expectedNode := uint64(usable * 0.50)
	expectedMem := uint64(usable * 0.30)
	expectedDecoded := uint64(usable * 0.10)

	assert.Equal(t, expectedNode, b.NodeCache)
	assert.Equal(t, expectedMem, b.Memtable)
	assert.Equal(t, expectedDecoded, b.DecodedChunks)

	// Total allocated should be well under the limit
	total := int64(b.NodeCache) + int64(b.Memtable) + int64(b.DecodedChunks)
	assert.Less(t, total, limit, "total cache budget should be under GOMEMLIMIT")
}

func TestCompute128MiB(t *testing.T) {
	prev := debug.SetMemoryLimit(-1)
	defer debug.SetMemoryLimit(prev)

	limit := int64(128 * 1024 * 1024) // 128 MiB
	debug.SetMemoryLimit(limit)

	b := compute()

	// At 128 MiB, usable = 96 MiB
	// All above minimums, all below defaults
	assert.Greater(t, b.NodeCache, uint64(minNodeCacheSize))
	assert.Greater(t, b.Memtable, uint64(minMemtableSize))
	assert.Greater(t, b.DecodedChunks, uint64(minDecodedChunksSize))

	assert.Less(t, b.NodeCache, uint64(DefaultNodeCacheSize))
	assert.Less(t, b.Memtable, uint64(DefaultMemtableSize))
	assert.Less(t, b.DecodedChunks, uint64(DefaultDecodedChunksSize))
}

func TestComputeVerySmall(t *testing.T) {
	prev := debug.SetMemoryLimit(-1)
	defer debug.SetMemoryLimit(prev)

	limit := int64(32 * 1024 * 1024) // 32 MiB — very constrained
	debug.SetMemoryLimit(limit)

	b := compute()

	// Should hit minimums for node cache
	assert.Equal(t, uint64(minNodeCacheSize), b.NodeCache)
	assert.GreaterOrEqual(t, b.Memtable, uint64(minMemtableSize))
	assert.GreaterOrEqual(t, b.DecodedChunks, uint64(minDecodedChunksSize))
}

func TestComputeLarge(t *testing.T) {
	prev := debug.SetMemoryLimit(-1)
	defer debug.SetMemoryLimit(prev)

	limit := int64(4 * 1024 * 1024 * 1024) // 4 GiB
	debug.SetMemoryLimit(limit)

	b := compute()

	// With no cap, large GOMEMLIMIT should produce caches larger than defaults
	assert.Greater(t, b.NodeCache, uint64(DefaultNodeCacheSize))
	assert.Greater(t, b.Memtable, uint64(DefaultMemtableSize))
	assert.Greater(t, b.DecodedChunks, uint64(DefaultDecodedChunksSize))
}

func TestAccessorsCallInit(t *testing.T) {
	// Accessors should work even without explicit Init() — they self-init.
	require.Greater(t, NodeCacheSize(), uint64(0))
	require.Greater(t, MemtableSize(), uint64(0))
	require.Greater(t, DecodedChunksSize(), uint64(0))
}
