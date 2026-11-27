package pii

import (
	"container/list"
	"crypto/cipher"
	"sync"
	"time"
)

// subjectKey is a decrypted subject key in the cache. aead == nil is a
// tombstone: the version version is destroyed by forget.
type subjectKey struct {
	version uint32
	aead    cipher.AEAD
	expires time.Time
}

// lru is a cache of subject keys that evicts the least recently used ones
// and has an entry lifetime. The key is the tenant, type and id of the subject.
type lru struct {
	mu    sync.Mutex
	max   int
	ttl   time.Duration
	order *list.List // *entry, the freshest first
	items map[string]*list.Element
}

type entry struct {
	key string
	val subjectKey
}

func newLRU(max int, ttl time.Duration) *lru {
	return &lru{max: max, ttl: ttl, order: list.New(), items: map[string]*list.Element{}}
}

func (c *lru) get(key []byte, now time.Time) (subjectKey, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[string(key)]
	if !ok {
		return subjectKey{}, false
	}
	e := el.Value.(*entry)
	if now.After(e.val.expires) {
		c.order.Remove(el)
		delete(c.items, e.key)
		return subjectKey{}, false
	}
	c.order.MoveToFront(el)
	return e.val, true
}

// put remembers a key; an older version does not replace a newer one.
func (c *lru) put(key string, v subjectKey, now time.Time) {
	if c.max <= 0 {
		return
	}
	v.expires = now.Add(c.ttl)
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		e := el.Value.(*entry)
		if e.val.version > v.version {
			return
		}
		e.val = v
		c.order.MoveToFront(el)
		return
	}
	c.items[key] = c.order.PushFront(&entry{key: key, val: v})
	for c.order.Len() > c.max {
		last := c.order.Back()
		c.order.Remove(last)
		delete(c.items, last.Value.(*entry).key)
	}
}

func (c *lru) drop(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		c.order.Remove(el)
		delete(c.items, key)
	}
}
