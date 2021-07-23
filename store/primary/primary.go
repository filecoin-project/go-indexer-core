package primary

import (
	"github.com/filecoin-project/go-indexer-core/store"
	"github.com/ipfs/go-cid"
	peer "github.com/libp2p/go-libp2p-core/peer"
)

// concurrrency is the lock granularity for radixtree. Must be power of two.
// This can be adjusted, but testing seems to indicate 16 is a good balance.
const concurrency = 16

var _ store.Storage = &rtStorage{}

// rtStorage is Adaptive Radix Tree based primary storage
type rtStorage struct {
	cacheSet   []*syncCache
	rotateSize int
}

// cidToKey gets the multihash from a CID to be used as a cache key
func cidToKey(c cid.Cid) string { return string(c.Hash()) }

// New creates a new Adaptive Radix Tree storage
func New(size int) *rtStorage {
	cacheSetSize := concurrency
	if size < 256 {
		cacheSetSize = 1
	}
	cacheSet := make([]*syncCache, cacheSetSize)
	for i := range cacheSet {
		cacheSet[i] = newSyncCache()
	}
	return &rtStorage{
		cacheSet:   cacheSet,
		rotateSize: size / (cacheSetSize * 2),
	}
}

func (s *rtStorage) Get(c cid.Cid) ([]store.IndexEntry, bool, error) {
	// Keys indexed as multihash
	k := cidToKey(c)
	cache := s.getCache(k)

	ents, found := cache.get(k)
	if !found {
		return nil, false, nil
	}

	ret := make([]store.IndexEntry, len(ents))
	for i, v := range ents {
		ret[i] = *v
	}
	return ret, true, nil
}

func (s *rtStorage) Put(c cid.Cid, entry store.IndexEntry) (bool, error) {
	return s.PutCheck(c, entry), nil
}

// PutCheck stores an IndexEntry for a CID if the entry is not already
// stored.  New entries are added to the entries that are already there.
// Returns true if a new entry was added to the cache.
//
// Only rotate one cache at a time. This may leave older entries in other
// caches, but if CIDs are dirstributed evenly over the cache set then over
// time all members should be rotated the same amount on average.  This is done
// so that it is not necessary to lock all caches in order to perform a
// rotation.  This also means that items age out more incrementally.
func (s *rtStorage) PutCheck(c cid.Cid, entry store.IndexEntry) bool {
	k := cidToKey(c)

	cache := s.getCache(k)
	return cache.put(k, entry, s.rotateSize)
}

func (s *rtStorage) PutMany(cids []cid.Cid, entry store.IndexEntry) error {
	s.PutManyCount(cids, entry)
	return nil
}

// PutManyCount stores an IndexEntry for multiple CIDs.  Returns the
// number of new entries stored.  A new entry is counted whenever an IndexEntry
// is added to the list of entries for a CID, whether or not that CID was
// already in the cache.
//
// This is more efficient than using Put to store individual values, becase
// PutMany allows the same IndexEntry to be reues across all sub-caches.
func (s *rtStorage) PutManyCount(cids []cid.Cid, entry store.IndexEntry) uint64 {
	if len(s.cacheSet) == 1 {
		keys := make([]string, len(cids))
		for i := range cids {
			keys[i] = cidToKey(cids[i])
		}
		return uint64(s.cacheSet[0].putMany(keys, entry, s.rotateSize))
	}
	var stored uint64
	var reuseEnt *store.IndexEntry
	interns := make(map[*syncCache]*store.IndexEntry, len(s.cacheSet))

	for i := range cids {
		k := cidToKey(cids[i])
		cache := s.getCache(k)
		ent, ok := interns[cache]
		if !ok {
			// Intern the entry once for this cache to avoid repeared lookups
			// on every call to cache.put().  If the entry is not already
			// interned for the cache, then reuse an entry that is already
			// interned elsewhere.
			cache.mutex.Lock()
			if reuseEnt == nil {
				ent = cache.internEntry(&entry)
				reuseEnt = ent
			} else {
				ent = cache.internEntry(reuseEnt)
			}
			cache.mutex.Unlock()
			interns[cache] = ent
		}
		if cache.putInterned(k, ent, s.rotateSize) {
			stored++
		}
	}

	return stored
}

func (s *rtStorage) Remove(c cid.Cid, entry store.IndexEntry) (bool, error) {
	return s.RemoveCheck(c, entry), nil
}

// RemoveCheck removes an IndexEntry for a CID.  Returns true if an
// entry was removed from cache.
func (s *rtStorage) RemoveCheck(c cid.Cid, entry store.IndexEntry) bool {
	k := cidToKey(c)

	cache := s.getCache(k)
	return cache.remove(k, &entry)
}

func (s *rtStorage) RemoveMany(cids []cid.Cid, entry store.IndexEntry) error {
	s.RemoveManyCount(cids, entry)
	return nil
}

// RemoveManyCount removes an IndexEntry from multiple CIDs.  Returns
// the number of entries removed.
func (s *rtStorage) RemoveManyCount(cids []cid.Cid, entry store.IndexEntry) uint64 {
	var removed uint64

	for i := range cids {
		k := cidToKey(cids[i])
		cache := s.getCache(k)
		if cache.remove(k, &entry) {
			removed++
		}
	}

	return removed
}

func (s *rtStorage) RemoveProvider(providerID peer.ID) error {
	s.RemoveProviderCount(providerID)
	return nil
}

// RemoveProvider removes all enrties for specified provider.  Returns the
// total number of entries removed from the cache.
func (s *rtStorage) RemoveProviderCount(providerID peer.ID) uint64 {
	countChan := make(chan uint64)
	for _, cache := range s.cacheSet {
		go func(c *syncCache) {
			countChan <- uint64(c.removeProvider(providerID))
		}(cache)
	}
	var total uint64
	for i := 0; i < len(s.cacheSet); i++ {
		total += <-countChan
	}
	return total
}

func (s *rtStorage) Stats() CacheStats {
	statsChan := make(chan CacheStats)
	for _, cache := range s.cacheSet {
		go func(cache *syncCache) {
			statsChan <- cache.stats()
		}(cache)
	}

	var totalStats CacheStats
	for i := 0; i < len(s.cacheSet); i++ {
		stats := <-statsChan
		totalStats.Cids += stats.Cids
		totalStats.Values += stats.Values
		totalStats.UniqueValues += stats.UniqueValues
		totalStats.InternedValues += stats.InternedValues
		totalStats.Rotations += stats.Rotations
	}

	return totalStats
}

// getCache returns the cache that stores the given key.  This function must
// evenly distribute keys over the set of caches.
func (s *rtStorage) getCache(k string) *syncCache {
	var idx int
	if k != "" {
		// Use last bits of key for good distribution
		//
		// bitwise modulus requires that size of cache set is power of 2
		idx = int(k[len(k)-1]) & (len(s.cacheSet) - 1)
	}
	return s.cacheSet[idx]
}

func (c *rtStorage) Size() (int64, error) {
	panic("not implemented")
}
