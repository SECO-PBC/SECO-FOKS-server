package libclient

import (
	"testing"

	"github.com/foks-proj/go-foks/proto/lcl"
	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/stretchr/testify/require"
)

func newTestMemCache(cap int) *Cache[*proto.FQParty, int, proto.SmallFileBox, *proto.SmallFileBox, string] {
	var p proto.FQParty
	return NewCache[*proto.FQParty, int, proto.SmallFileBox, *proto.SmallFileBox, string](
		lcl.DataType_KVSymlink, &p, CacheSettings{UseMem: true, MemCap: cap},
	)
}

func TestCacheMemTierIsBounded(t *testing.T) {
	c := newTestMemCache(3)
	for i := range 10 {
		s := "v"
		c.PutMem(i, proto.SmallFileBox{}, &s)
	}
	require.Len(t, c.m, 3)
	require.Equal(t, 3, c.ll.Len())
	for i := range 7 {
		v, _ := c.getMem(i)
		require.Nil(t, v, "key %d should have been evicted", i)
	}
	for i := 7; i < 10; i++ {
		v, mv := c.getMem(i)
		require.NotNil(t, v)
		require.Equal(t, "v", *mv)
	}
}

func TestCacheMemTierEvictsLeastRecentlyUsed(t *testing.T) {
	c := newTestMemCache(2)
	a, b := "a", "b"
	c.PutMem(1, proto.SmallFileBox{}, &a)
	c.PutMem(2, proto.SmallFileBox{}, &b)
	// Touch 1, so 2 is now the least recently used.
	v, _ := c.getMem(1)
	require.NotNil(t, v)
	c.PutMem(3, proto.SmallFileBox{}, &b)
	v, _ = c.getMem(2)
	require.Nil(t, v)
	_, mv := c.getMem(1)
	require.Equal(t, "a", *mv)

	// Overwriting a key replaces it in place and does not grow the tier.
	c.PutMem(1, proto.SmallFileBox{}, &b)
	require.Len(t, c.m, 2)
	_, mv = c.getMem(1)
	require.Equal(t, "b", *mv)

	c.clearMem(1)
	require.Len(t, c.m, 1)
	require.Equal(t, 1, c.ll.Len())
}

func TestCacheMemTierDefaultCap(t *testing.T) {
	c := newTestMemCache(0)
	for i := range DefaultMemCacheCap + 100 {
		c.PutMem(i, proto.SmallFileBox{}, nil)
	}
	require.Len(t, c.m, DefaultMemCacheCap)
}
