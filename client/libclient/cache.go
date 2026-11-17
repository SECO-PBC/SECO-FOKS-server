package libclient

import (
	"errors"
	"sync"

	"github.com/foks-proj/go-foks/lib/core"
	"github.com/foks-proj/go-foks/proto/lcl"
	proto "github.com/foks-proj/go-foks/proto/lib"
	lru "github.com/hashicorp/golang-lru/v2"
)

type Versioner interface {
	GetVersion() proto.Version
}

type memNode[V any, M any] struct {
	v *V
	m *M
}

// DefaultMemCacheCap bounds each Cache's memory tier when
// CacheSettings.MemCap is unset.
//
// SECO fork: the memory tier used to be a plain map that nothing evicted.
// Keys like SmallFileID are minted fresh on every write, so a long-lived
// client that overwrites the same KV paths (the SECO daemon's cursors) kept
// every version it ever wrote or read, plaintext and box both, and grew
// without bound. A miss falls through to the disk tier and then the
// server, the same path a cold start takes, so evicting is always safe.
const DefaultMemCacheCap = 4096

type CacheSettings struct {
	UseMem  bool
	UseDisk bool
	// MemCap is the most entries the memory tier holds before evicting the
	// least recently used. Zero means DefaultMemCacheCap.
	MemCap int
}

func (s CacheSettings) memCap() int {
	if s.MemCap > 0 {
		return s.MemCap
	}
	return DefaultMemCacheCap
}

type Cache[
	S Scoper, // Scoper for DB cache, usually an FQParty
	K comparable, // The DB Key, like a DirID, etc
	V Versioner, // The value to store
	VP interface { // Pointer to the value to store, must implement core.Codecable
		*V
		core.Codecable
	},
	MV any, // A MemVal, something that we only store in memory (like decrypted data)
] struct {
	sync.RWMutex
	typ   lcl.DataType
	Scope S
	// m is the memory tier, made on the first PutMem.
	m        *lru.Cache[K, memNode[V, MV]]
	dbt      DbType
	Settings CacheSettings
}

func NewCache[
	S Scoper,
	K comparable,
	V Versioner,
	VP interface {
		*V
		core.Codecable
	},
	MV any,
](typ lcl.DataType, s S, settings CacheSettings) *Cache[S, K, V, VP, MV] {
	return &Cache[S, K, V, VP, MV]{
		typ:      typ,
		Scope:    s,
		dbt:      DbTypeSoft,
		Settings: settings,
	}
}

func (c *Cache[S, K, V, VP, MV]) PutMem(k K, v V, mv *MV) {
	c.Lock()
	defer c.Unlock()
	if !c.Settings.UseMem {
		return
	}
	if c.m == nil {
		// Errors only on a size <= 0, which memCap never returns.
		c.m, _ = lru.New[K, memNode[V, MV]](c.Settings.memCap())
	}
	c.m.Add(k, memNode[V, MV]{v: &v, m: mv})
}

func (c *Cache[S, K, V, VP, MV]) clearMem(k K) {
	c.Lock()
	defer c.Unlock()
	if c.m == nil {
		return
	}
	c.m.Remove(k)
}

func (c *Cache[S, K, V, VP, MV]) putDb(m MetaContext, k K, v V) error {
	if !c.Settings.UseDisk {
		return nil
	}
	return m.DbPut(
		c.dbt,
		PutArg{
			Scope: c.Scope,
			Typ:   c.typ,
			Key:   k,
			Val:   (VP)(&v),
		},
	)
}

func (c *Cache[S, K, V, VP, MV]) Put(m MetaContext, k K, v V, memVal *MV) error {
	err := c.putDb(m, k, v)
	if err != nil {
		return err
	}
	c.PutMem(k, v, memVal)
	return nil
}

func (c *Cache[S, K, V, VP, MV]) getMem(k K) (*V, *MV) {
	c.RLock()
	defer c.RUnlock()
	if c.m == nil {
		return nil, nil
	}
	n, ok := c.m.Get(k)
	if !ok {
		return nil, nil
	}
	return n.v, n.m
}

func (c *Cache[S, K, V, VP, MV]) getDb(m MetaContext, k K) (*V, error) {
	if !c.Settings.UseDisk {
		return nil, nil
	}
	var ret V
	getSlot := (VP)(&ret)
	_, err := m.DbGet(getSlot, c.dbt, c.Scope, c.typ, k)
	if err != nil && errors.Is(err, core.RowNotFoundError{}) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &ret, nil
}

func (c *Cache[S, K, V, VP, MV]) Get(m MetaContext, k K) (*V, *MV, error) {
	v, memVal := c.getMem(k)
	if v != nil {
		return v, memVal, nil
	}
	ret, err := c.getDb(m, k)
	if err != nil {
		return nil, nil, err
	}
	if ret == nil {
		return nil, nil, nil
	}
	c.PutMem(k, *ret, nil)

	return ret, nil, nil
}

func (c *Cache[S, K, V, VP, MV]) ClearBefore(m MetaContext, k K, vers Versioner) error {
	if vers.GetVersion() == 0 {
		return nil
	}
	v, _ := c.getMem(k)
	if v != nil {
		cacheVers := (*v).GetVersion()
		if cacheVers == 0 || cacheVers == vers.GetVersion() {
			return nil
		}
		if cacheVers != 0 && cacheVers != vers.GetVersion() {
			c.clearMem(k)
		}
	}
	v, err := c.getDb(m, k)
	if err != nil {
		return err
	}
	if v == nil {
		return nil
	}
	err = m.DbDelete(c.dbt, c.Scope, c.typ, k)
	if err != nil {
		return err
	}
	return nil
}
