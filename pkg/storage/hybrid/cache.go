package hybrid

import (
	"container/list"
	"sync"
)

// lruCache is a thread-safe LRU cache for decoded entries
type lruCache struct {
	mu       sync.RWMutex
	capacity int
	items    map[string]*list.Element
	order    *list.List
}

type cacheItem struct {
	key   string
	value []byte
}

// newLRUCache creates a new LRU cache with the given capacity
func newLRUCache(capacity int) *lruCache {
	return &lruCache{
		capacity: capacity,
		items:    make(map[string]*list.Element),
		order:    list.New(),
	}
}

// Get retrieves an item from the cache
func (c *lruCache) Get(key string) ([]byte, bool) {
	c.mu.RLock()
	elem, ok := c.items[key]
	c.mu.RUnlock()

	if !ok {
		return nil, false
	}

	// Move to front (most recently used)
	c.mu.Lock()
	c.order.MoveToFront(elem)
	c.mu.Unlock()

	return elem.Value.(*cacheItem).value, true
}

// Put adds an item to the cache
func (c *lruCache) Put(key string, value []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// If key exists, update and move to front
	if elem, ok := c.items[key]; ok {
		c.order.MoveToFront(elem)
		elem.Value.(*cacheItem).value = value
		return
	}

	// Evict oldest if at capacity
	for c.order.Len() >= c.capacity && c.order.Len() > 0 {
		c.evictOldest()
	}

	// Add new item at front
	item := &cacheItem{key: key, value: value}
	elem := c.order.PushFront(item)
	c.items[key] = elem
}

// Remove removes an item from the cache
func (c *lruCache) Remove(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if elem, ok := c.items[key]; ok {
		c.order.Remove(elem)
		delete(c.items, key)
	}
}

// Clear removes all items from the cache
func (c *lruCache) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.items = make(map[string]*list.Element)
	c.order = list.New()
}

// Len returns the number of items in the cache
func (c *lruCache) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.order.Len()
}

// SetCapacity changes the cache capacity and evicts if necessary
func (c *lruCache) SetCapacity(capacity int) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.capacity = capacity
	for c.order.Len() > c.capacity {
		c.evictOldest()
	}
}

// MemoryUsage returns approximate memory usage
func (c *lruCache) MemoryUsage() int64 {
	c.mu.RLock()
	defer c.mu.RUnlock()

	var total int64
	for elem := c.order.Front(); elem != nil; elem = elem.Next() {
		item := elem.Value.(*cacheItem)
		total += int64(len(item.key)) + int64(len(item.value)) + 64 // overhead
	}
	return total
}

// evictOldest removes the least recently used item (must hold write lock)
func (c *lruCache) evictOldest() {
	oldest := c.order.Back()
	if oldest != nil {
		item := oldest.Value.(*cacheItem)
		delete(c.items, item.key)
		c.order.Remove(oldest)
	}
}

// Keys returns all cached keys (for debugging)
func (c *lruCache) Keys() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()

	keys := make([]string, 0, c.order.Len())
	for elem := c.order.Front(); elem != nil; elem = elem.Next() {
		keys = append(keys, elem.Value.(*cacheItem).key)
	}
	return keys
}
