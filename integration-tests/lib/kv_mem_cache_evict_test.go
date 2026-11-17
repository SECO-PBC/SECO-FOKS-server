package lib

import (
	"fmt"
	"testing"

	"github.com/foks-proj/go-foks/client/libclient"
	"github.com/foks-proj/go-foks/proto/lcl"
)

// TestKVMemCacheEviction runs KV traffic with a one-entry memory tier, so
// nearly every read misses memory and falls through to the disk tier, which
// returns the box without its plaintext. Overwrites seen from a second device
// also drive the stale-cache path (ClearBefore) on entries memory no longer
// holds. SECO fork: the memory tier became a bounded LRU.
func TestKVMemCacheEviction(t *testing.T) {
	mdt := setupMultiDeviceTest(t, 2, libclient.CacheSettings{UseMem: true, UseDisk: true, MemCap: 1})
	x, y := mdt.dev[0], mdt.dev[1]
	x.mkdir(t, "/a/b")

	// One path overwritten again and again, read on both devices.
	for i := range 5 {
		body := fmt.Sprintf("v%d", i)
		x.echo(t, body, "/a/b/c.txt")
		x.cat(t, "/a/b/c.txt", body)
		y.cat(t, "/a/b/c.txt", body)
	}

	// More files than the memory tier holds, read back after all are written.
	for i := range 5 {
		x.echo(t, fmt.Sprintf("f%d", i), fmt.Sprintf("/a/b/f%d.txt", i))
	}
	for i := range 5 {
		x.cat(t, fmt.Sprintf("/a/b/f%d.txt", i), fmt.Sprintf("f%d", i))
		y.cat(t, fmt.Sprintf("/a/b/f%d.txt", i), fmt.Sprintf("f%d", i))
	}

	// A multi-chunk file: its key seed lives only in memory after the write.
	big := x.pathify("/a/b/big.bin")
	data := writeRandomFileWithConfig(t, x.kvm, x.mc, big, 3*testChunkSize+7, lcl.KVConfig{}, 0)
	readFile(t, x.kvm, x.mc, big, data)
	readFile(t, y.kvm, y.mc, big, data)
	readFile(t, x.kvm, x.mc, big, data)
}
