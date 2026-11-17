package libclient

import (
	"container/list"
	"errors"
	"sync"

	"github.com/foks-proj/go-foks/lib/core"
	"github.com/foks-proj/go-foks/proto/lcl"
	proto "github.com/foks-proj/go-foks/proto/lib"
)

type Versioner interface {
	GetVersion() proto.Version
}

type memNode[K comparable, V any, M any] struct {
	k K
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
	// m and ll are the memory tier: an LRU, front most recently used.
	m        map[K]*list.Element
	ll       *list.List
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
	node := &memNode[K, V, MV]{k: k, v: &v, m: mv}
	if c.m == nil {
		c.m = make(map[K]*list.Element)
		c.ll = list.New()
	}
	if el, ok := c.m[k]; ok {
		el.Value = node
		c.ll.MoveToFront(el)
		return
	}
	c.m[k] = c.ll.PushFront(node)
	for c.ll.Len() > c.Settings.memCap() {
		oldest := c.ll.Back()
		c.ll.Remove(oldest)
		delete(c.m, oldest.Value.(*memNode[K, V, MV]).k)
	}
}

func (c *Cache[S, K, V, VP, MV]) clearMem(k K) {
	c.Lock()
	defer c.Unlock()
	if c.m == nil {
		return
	}
	if el, ok := c.m[k]; ok {
		c.ll.Remove(el)
		delete(c.m, k)
	}
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

// getMem takes the write lock: a hit moves the entry to the front.
func (c *Cache[S, K, V, VP, MV]) getMem(k K) (*V, *MV) {
	c.Lock()
	defer c.Unlock()
	if c.m == nil {
		return nil, nil
	}
	el, ok := c.m[k]
	if !ok {
		return nil, nil
	}
	c.ll.MoveToFront(el)
	n := el.Value.(*memNode[K, V, MV])
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
