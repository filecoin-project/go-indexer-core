package radixcache

import (
	"github.com/filecoin-project/go-indexer-core"
	"github.com/filecoin-project/go-indexer-core/cache"
	peer "github.com/libp2p/go-libp2p-core/peer"
	"github.com/multiformats/go-multihash"
)

// concurrrency is the lock granularity for radixtree. Must be power of two.
// This can be adjusted, but testing seems to indicate 16 is a good balance.
const concurrency = 16

// multiCache is a set of multiple radixCache instances
type multiCache struct {
	cacheSet []*radixCache
}

var _ cache.Interface = &multiCache{}

// New creates a new multiCache
func New(size int) *multiCache {
	cacheSetSize := concurrency
	if size < 256 {
		cacheSetSize = 1
	}
	cacheSet := make([]*radixCache, cacheSetSize)
	rotateSize := size / (cacheSetSize * 2)
	for i := range cacheSet {
		cacheSet[i] = newRadixCache(rotateSize)
	}
	return &multiCache{
		cacheSet: cacheSet,
	}
}

func (s *multiCache) Get(m multihash.Multihash) ([]indexer.Value, bool, error) {
	// Keys indexed as multihash
	k := string(m)
	cache := s.getCache(k)

	ents, found := cache.get(k)
	if !found {
		return nil, false, nil
	}

	ret := make([]indexer.Value, len(ents))
	for i, v := range ents {
		ret[i] = *v
	}
	return ret, true, nil
}

func (s *multiCache) Put(m multihash.Multihash, value indexer.Value) (bool, error) {
	return s.PutCheck(m, value), nil
}

// PutCheck stores an indexer.Value for a multihash if the value is not already
// stored.  New values are added to the values that are already there.  Returns
// true if a new value was added to the cache.
//
// Only rotate one cache at a time. This may leave older values in other
// caches, but if multihashes are dirstributed evenly over the cache set then
// over time all members should be rotated the same amount on average.  This is
// done so that it is not necessary to lock all caches in order to perform a
// rotation.  This also means that items age out more incrementally.
func (s *multiCache) PutCheck(m multihash.Multihash, value indexer.Value) bool {
	k := string(m)
	cache := s.getCache(k)
	return cache.put(k, value)
}

func (s *multiCache) PutMany(mhashes []multihash.Multihash, value indexer.Value) error {
	s.PutManyCount(mhashes, value)
	return nil
}

// PutManyCount stores an indexer.Value for multiple multihashess.  Returns the
// number of new values stored.  A new value is counted whenever a value is
// added to the list of values for a multihash, whether or not that multihash
// was already in the cache.
//
// This is more efficient than using Put to store individual values, becase
// PutMany allows the same indexer.Value to be reused across all sub-caches.
func (s *multiCache) PutManyCount(mhashes []multihash.Multihash, value indexer.Value) uint64 {
	if len(s.cacheSet) == 1 {
		keys := make([]string, len(mhashes))
		for i := range mhashes {
			keys[i] = string(mhashes[i])
		}
		return uint64(s.cacheSet[0].putMany(keys, value))
	}
	var stored uint64
	var reuseEnt *indexer.Value
	interns := make(map[*radixCache]*indexer.Value, len(s.cacheSet))

	for i := range mhashes {
		k := string(mhashes[i])
		cache := s.getCache(k)
		ent, ok := interns[cache]
		if !ok {
			// Intern the value once for this cache to avoid repeared lookups
			// on every call to cache.put().  If the value is not already
			// interned for the cache, then reuse an value that is already
			// interned elsewhere.
			cache.mutex.Lock()
			if reuseEnt == nil {
				ent = cache.internValue(&value)
				reuseEnt = ent
			} else {
				ent = cache.internValue(reuseEnt)
			}
			cache.mutex.Unlock()
			interns[cache] = ent
		}
		if cache.putInterned(k, ent) {
			stored++
		}
	}

	return stored
}

func (s *multiCache) Remove(m multihash.Multihash, value indexer.Value) (bool, error) {
	return s.RemoveCheck(m, value), nil
}

// RemoveCheck removes an indexer.Value for a multihash.  Returns true if a
// value was removed from cache.
func (s *multiCache) RemoveCheck(m multihash.Multihash, value indexer.Value) bool {
	k := string(m)
	cache := s.getCache(k)
	return cache.remove(k, &value)
}

func (s *multiCache) RemoveMany(mhashes []multihash.Multihash, value indexer.Value) error {
	s.RemoveManyCount(mhashes, value)
	return nil
}

// RemoveManyCount removes an indexer.Value from multiple multihashes.  Returns
// the number of values removed.
func (s *multiCache) RemoveManyCount(mhashes []multihash.Multihash, value indexer.Value) uint64 {
	var removed uint64

	for i := range mhashes {
		k := string(mhashes[i])
		cache := s.getCache(k)
		if cache.remove(k, &value) {
			removed++
		}
	}

	return removed
}

func (s *multiCache) RemoveProvider(providerID peer.ID) error {
	s.RemoveProviderCount(providerID)
	return nil
}

// RemoveProvider removes all enrties for specified provider.  Returns the
// total number of values removed from the cache.
func (s *multiCache) RemoveProviderCount(providerID peer.ID) uint64 {
	countChan := make(chan uint64)
	for _, cache := range s.cacheSet {
		go func(c *radixCache) {
			countChan <- uint64(c.removeProvider(providerID))
		}(cache)
	}
	var total uint64
	for i := 0; i < len(s.cacheSet); i++ {
		total += <-countChan
	}
	return total
}

func (s *multiCache) Stats() CacheStats {
	statsChan := make(chan CacheStats)
	for _, cache := range s.cacheSet {
		go func(cache *radixCache) {
			statsChan <- cache.stats()
		}(cache)
	}

	var totalStats CacheStats
	for i := 0; i < len(s.cacheSet); i++ {
		stats := <-statsChan
		totalStats.Indexes += stats.Indexes
		totalStats.Values += stats.Values
		totalStats.UniqueValues += stats.UniqueValues
		totalStats.InternedValues += stats.InternedValues
		totalStats.Rotations += stats.Rotations
	}

	return totalStats
}

// getCache returns the cache that stores the given key.  This function must
// evenly distribute keys over the set of caches.
func (s *multiCache) getCache(k string) *radixCache {
	var idx int
	if k != "" {
		// Use last bits of key for good distribution
		//
		// bitwise modulus requires that size of cache set is power of 2
		idx = int(k[len(k)-1]) & (len(s.cacheSet) - 1)
	}
	return s.cacheSet[idx]
}

func (c *multiCache) Size() (int64, error) {
	panic("not implemented")
}
