package pii

import (
	"container/list"
	"sync"
)

// DefaultCacheCapacity is the default entry bound. Sized for a modest host,
// not tuned.
const DefaultCacheCapacity = 10_000

// Cache holds decrypted values in this process's memory ("option A" of the
// read-side design), so a warm read makes no round trip to the key service
// and keeps working while it is down.
//
// Keyed by (subject, ciphertext), so an entry can only ever answer for the
// exact ciphertext the facade decrypted. Holds raw plaintext bytes, not a
// typed value: the same ciphertext decodes the same way for any Value[T], so
// one cache serves every field type. Bounded by entry count, evicted least
// recently used.
//
// Per-process, and never shared or persisted: what can't forget holds
// ciphertext. One per process; never back it with a distributed cache.
//
// Erasure: MarkErased drops the subject's entries and records the subject
// in an erased set. Later fills for it are ignored and reveals answer
// Redacted without a round trip. The set closes the fill-after-evict race (a
// flush that started before the erasure and finishes after it would
// otherwise put the plaintext straight back). It only grows; erasures are
// rare and subject ids small, so it is deliberately unbounded. Fed by
// CacheEvictor on SubjectErased, and by Buffer when the facade reports a
// destroyed key.
type Cache struct {
	capacity int

	mu        sync.Mutex
	entries   map[cacheKey]*list.Element
	bySubject map[string]map[string]struct{}
	lru       *list.List // front = most recently used
	erased    map[string]struct{}
	onErased  []func(subjectID string)
}

type cacheKey struct{ subject, cipher string }

type cacheEntry struct {
	key   cacheKey
	plain []byte
}

// NewCache returns an empty cache bounded at capacity entries (< 1 means
// DefaultCacheCapacity).
func NewCache(capacity int) *Cache {
	if capacity < 1 {
		capacity = DefaultCacheCapacity
	}
	return &Cache{
		capacity:  capacity,
		entries:   map[cacheKey]*list.Element{},
		bySubject: map[string]map[string]struct{}{},
		lru:       list.New(),
		erased:    map[string]struct{}{},
	}
}

// Len is the number of cached entries.
func (c *Cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

// IsErased reports whether this process has learned the subject is erased.
func (c *Cache) IsErased(subjectID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.erased[subjectID]
	return ok
}

// Get returns the cached plaintext for exactly this ciphertext, marking it
// most recently used. Always misses for an erased subject.
func (c *Cache) Get(subjectID, ciphertext string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.entries[cacheKey{subjectID, ciphertext}]
	if !ok {
		return nil, false
	}
	c.lru.MoveToFront(el)
	return el.Value.(*cacheEntry).plain, true
}

// Put caches a plaintext the facade just returned (or that was just
// encrypted). Ignored for an erased subject — how a fill that lost the race
// with an erasure is dropped.
func (c *Cache) Put(subjectID, ciphertext string, plaintext []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, gone := c.erased[subjectID]; gone {
		return
	}
	k := cacheKey{subjectID, ciphertext}
	if el, ok := c.entries[k]; ok {
		c.lru.MoveToFront(el)
		return
	}
	c.entries[k] = c.lru.PushFront(&cacheEntry{key: k, plain: append([]byte(nil), plaintext...)})
	set := c.bySubject[subjectID]
	if set == nil {
		set = map[string]struct{}{}
		c.bySubject[subjectID] = set
	}
	set[ciphertext] = struct{}{}
	for len(c.entries) > c.capacity {
		c.remove(c.lru.Back())
	}
}

// OnErased calls fn, outside the cache's lock, the first time a subject is
// marked erased: a reader whose result held that subject's values (a live
// view) can recompute it as redacted. fn must not block.
func (c *Cache) OnErased(fn func(subjectID string)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onErased = append(c.onErased, fn)
}

// MarkErased drops every entry for the subject and refuses future fills for
// it. Idempotent: listeners hear of a subject once.
func (c *Cache) MarkErased(subjectID string) {
	c.mu.Lock()
	_, already := c.erased[subjectID]
	c.markErasedLocked(subjectID)
	listeners := c.onErased
	c.mu.Unlock()
	if !already {
		for _, fn := range listeners {
			fn(subjectID)
		}
	}
}

func (c *Cache) markErasedLocked(subjectID string) {
	c.erased[subjectID] = struct{}{}
	for cipher := range c.bySubject[subjectID] {
		if el, ok := c.entries[cacheKey{subjectID, cipher}]; ok {
			c.lru.Remove(el)
			delete(c.entries, cacheKey{subjectID, cipher})
		}
	}
	delete(c.bySubject, subjectID)
}

func (c *Cache) remove(el *list.Element) {
	e := el.Value.(*cacheEntry)
	c.lru.Remove(el)
	delete(c.entries, e.key)
	if set := c.bySubject[e.key.subject]; set != nil {
		delete(set, e.key.cipher)
		if len(set) == 0 {
			delete(c.bySubject, e.key.subject)
		}
	}
}
