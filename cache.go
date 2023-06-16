package cache

import (
	"encoding/gob"
	"fmt"
	"io"
	"os"
	"reflect"
	"runtime"
	"sync"
	"time"
)

type Item[T any] struct {
	Object     T
	Expiration int64
}

// Expired Returns true if the item has expired.
func (item Item[T]) Expired() bool {
	if item.Expiration == 0 {
		return false
	}
	return time.Now().UnixNano() > item.Expiration
}

const (
	// NoExpiration For use with functions that take an expiration time.
	NoExpiration time.Duration = -1
	// DefaultExpiration For use with functions that take an expiration time. Equivalent to
	// passing in the same expiration duration as was given to New() or
	// NewFrom() when the cache was created (e.g. 5 minutes.)
	DefaultExpiration time.Duration = 0
)

type Cache[K comparable, T any] struct {
	*cache[K, T]
	// If this is confusing, see the comment at the bottom of New()
}

type cache[K comparable, T any] struct {
	defaultExpiration time.Duration
	items             map[K]Item[T]
	mu                sync.RWMutex
	onEvicted         func(K, T)
	janitor           *janitor[K, T]
}

// Set an item to the cache, replacing any existing item. If the duration is 0
// (DefaultExpiration), the cache's default expiration time is used. If it is -1
// (NoExpiration), the item never expires.
func (c *cache[K, T]) Set(k K, x T, d time.Duration) {
	// "Inlining" of set
	var e int64
	if d == DefaultExpiration {
		d = c.defaultExpiration
	}
	if d > 0 {
		e = time.Now().Add(d).UnixNano()
	}
	c.mu.Lock()
	_, found := c.get(k)
	if found {
		v, evicted := c.delete(k)
		c.items[k] = Item[T]{
			Object:     x,
			Expiration: e,
		}
		c.mu.Unlock()
		if evicted {
			c.onEvicted(k, v)
		}
		return
	}
	c.items[k] = Item[T]{
		Object:     x,
		Expiration: e,
	}
	// TODO: Calls to mu.Unlock are currently not deferred because defer
	// adds ~200 ns (as of go1.)
	c.mu.Unlock()
}

func (c *cache[K, T]) set(k K, x T, d time.Duration) {
	var e int64
	if d == DefaultExpiration {
		d = c.defaultExpiration
	}
	if d > 0 {
		e = time.Now().Add(d).UnixNano()
	}
	c.items[k] = Item[T]{
		Object:     x,
		Expiration: e,
	}
}

// SetDefault an item to the cache, replacing any existing item, using the default
// expiration.
func (c *cache[K, T]) SetDefault(k K, x T) {
	c.Set(k, x, DefaultExpiration)
}

// Add an item to the cache only if an item doesn't already exist for the given
// key, or if the existing item has expired. Returns an error otherwise.
func (c *cache[K, T]) Add(k K, x T, d time.Duration) error {
	c.mu.Lock()
	_, found := c.get(k)
	if found {
		c.mu.Unlock()
		return fmt.Errorf("item %v already exists", k)
	}
	c.set(k, x, d)
	c.mu.Unlock()
	return nil
}

// Replace a new value for the cache key only if it already exists, and the existing
// item hasn't expired. Returns an error otherwise.
func (c *cache[K, T]) Replace(k K, x T, d time.Duration) error {
	c.mu.Lock()
	_, found := c.get(k)
	if !found {
		c.mu.Unlock()
		return fmt.Errorf("item %v doesn't exist", k)
	}
	c.set(k, x, d)
	c.mu.Unlock()
	return nil
}

// Get an item from the cache. Returns the item or nil, and a bool indicating
// whether the key was found.
func (c *cache[K, T]) Get(k K) (T, bool) {
	c.mu.RLock()
	// "Inlining" of get and Expired
	item, found := c.items[k]
	if !found {
		c.mu.RUnlock()
		return *new(T), false
	}
	if item.Expiration > 0 {
		if time.Now().UnixNano() > item.Expiration {
			c.mu.RUnlock()
			return *new(T), false
		}
	}
	c.mu.RUnlock()
	return item.Object, true
}

// GetWithExpiration returns an item and its expiration time from the cache.
// It returns the item or nil, the expiration time if one is set (if the item
// never expires a zero value for time.Time is returned), and a bool indicating
// whether the key was found.
func (c *cache[K, T]) GetWithExpiration(k K) (T, time.Time, bool) {
	c.mu.RLock()
	// "Inlining" of get and Expired
	item, found := c.items[k]
	if !found {
		c.mu.RUnlock()
		return *new(T), time.Time{}, false
	}

	if item.Expiration > 0 {
		if time.Now().UnixNano() > item.Expiration {
			c.mu.RUnlock()
			return *new(T), time.Time{}, false
		}

		// Return the item and the expiration time
		c.mu.RUnlock()
		return item.Object, time.Unix(0, item.Expiration), true
	}

	// If expiration <= 0 (i.e. no expiration time set) then return the item
	// and a zeroed time.Time
	c.mu.RUnlock()
	return item.Object, time.Time{}, true
}

func (c *cache[K, T]) get(k K) (T, bool) {
	item, found := c.items[k]
	if !found {
		return *new(T), false
	}
	// "Inlining" of Expired
	if item.Expiration > 0 {
		if time.Now().UnixNano() > item.Expiration {
			return *new(T), false
		}
	}
	return item.Object, true
}

// Delete an item from the cache. Does nothing if the key is not in the cache.
func (c *cache[K, T]) Delete(k K) {
	c.mu.Lock()
	v, evicted := c.delete(k)
	c.mu.Unlock()
	if evicted {
		c.onEvicted(k, v)
	}
}

func (c *cache[K, T]) delete(k K) (T, bool) {
	if c.onEvicted != nil {
		if v, found := c.items[k]; found {
			delete(c.items, k)
			return v.Object, true
		}
	}
	delete(c.items, k)
	return *new(T), false
}

type keyAndValue[K comparable, T any] struct {
	key   K
	value T
}

// DeleteExpired Deletes all expired items from the cache.
func (c *cache[K, T]) DeleteExpired() {
	var evictedItems []keyAndValue[K, T]
	now := time.Now().UnixNano()
	c.mu.Lock()
	for k, v := range c.items {
		// "Inlining" of expired
		if v.Expiration > 0 && now > v.Expiration {
			ov, evicted := c.delete(k)
			if evicted {
				evictedItems = append(evictedItems, keyAndValue[K, T]{k, ov})
			}
		}
	}
	c.mu.Unlock()
	for _, v := range evictedItems {
		c.onEvicted(v.key, v.value)
	}
}

// OnEvicted sets an (optional) function that is called with the key and value when an
// item is evicted from the cache. (Including when it is deleted manually, but
// not when it is overwritten.) Set to nil to disable.
func (c *cache[K, T]) OnEvicted(f func(K, T)) {
	c.mu.Lock()
	c.onEvicted = f
	c.mu.Unlock()
}

// Save writes the cache's items (using Gob) to an io.Writer.
//
// NOTE: This method is deprecated in favor of c.Items() and NewFrom() (see the
// documentation for NewFrom().)
func (c *cache[K, T]) Save(w io.Writer) (err error) {
	enc := gob.NewEncoder(w)
	c.mu.RLock()
	defer c.mu.RUnlock()

	var t T
	switch reflect.TypeOf(t).Kind() {
	case reflect.Func:
		return fmt.Errorf("can't encode functions")
	case reflect.Chan:
		return fmt.Errorf("can't encode channels")
	}

	for _, v := range c.items {
		gob.Register(v.Object)
	}
	err = enc.Encode(&c.items)
	return err
}

// SaveFile saves the cache's items to the given filename, creating the file if it
// doesn't exist, and overwriting it if it does.
//
// NOTE: This method is deprecated in favor of c.Items() and NewFrom() (see the
// documentation for NewFrom().)
func (c *cache[K, T]) SaveFile(fname string) error {
	fp, err := os.Create(fname)
	if err != nil {
		return err
	}
	err = c.Save(fp)
	if err != nil {
		_ = fp.Close()
		return err
	}
	return fp.Close()
}

// Load adds (Gob-serialized) cache items from an io.Reader, excluding any items with
// keys that already exist (and haven't expired) in the current cache.
//
// NOTE: This method is deprecated in favor of c.Items() and NewFrom() (see the
// documentation for NewFrom().)
func (c *cache[K, T]) Load(r io.Reader) error {
	dec := gob.NewDecoder(r)
	items := map[K]Item[T]{}
	err := dec.Decode(&items)
	if err == nil {
		c.mu.Lock()
		defer c.mu.Unlock()
		for k, v := range items {
			ov, found := c.items[k]
			if !found || ov.Expired() {
				c.items[k] = v
			}
		}
	}
	return err
}

// LoadFile loads and add cache items from the given filename, excluding any items with
// keys that already exist in the current cache.
//
// NOTE: This method is deprecated in favor of c.Items() and NewFrom() (see the
// documentation for NewFrom().)
func (c *cache[K, T]) LoadFile(fname string) error {
	fp, err := os.Open(fname)
	if err != nil {
		return err
	}
	err = c.Load(fp)
	if err != nil {
		_ = fp.Close()
		return err
	}
	return fp.Close()
}

// Items copies all unexpired items in the cache into a new map and returns it.
func (c *cache[K, T]) Items() map[K]Item[T] {
	c.mu.RLock()
	defer c.mu.RUnlock()
	m := make(map[K]Item[T], len(c.items))
	now := time.Now().UnixNano()
	for k, v := range c.items {
		// "Inlining" of Expired
		if v.Expiration > 0 {
			if now > v.Expiration {
				continue
			}
		}
		m[k] = v
	}
	return m
}

// ItemCount returns the number of items in the cache. This may include items that have
// expired, but have not yet been cleaned up.
func (c *cache[K, T]) ItemCount() int {
	c.mu.RLock()
	n := len(c.items)
	c.mu.RUnlock()
	return n
}

// Flush deletes all items from the cache.
func (c *cache[K, T]) Flush() {
	c.mu.Lock()
	c.items = map[K]Item[T]{}
	c.mu.Unlock()
}

type janitor[K comparable, T any] struct {
	Interval time.Duration
	stop     chan bool
}

func (j *janitor[K, T]) Run(c *cache[K, T]) {
	ticker := time.NewTicker(j.Interval)
	for {
		select {
		case <-ticker.C:
			c.DeleteExpired()
		case <-j.stop:
			ticker.Stop()
			return
		}
	}
}

func stopJanitor[K comparable, T any](c *Cache[K, T]) {
	c.janitor.stop <- true
}

func runJanitor[K comparable, T any](c *cache[K, T], ci time.Duration) {
	j := &janitor[K, T]{
		Interval: ci,
		stop:     make(chan bool),
	}
	c.janitor = j
	go j.Run(c)
}

func newCache[K comparable, T any](de time.Duration, m map[K]Item[T]) *cache[K, T] {
	if de == 0 {
		de = -1
	}
	c := &cache[K, T]{
		defaultExpiration: de,
		items:             m,
	}
	return c
}

func newCacheWithJanitor[K comparable, T any](de time.Duration, ci time.Duration, m map[K]Item[T]) *Cache[K, T] {
	c := newCache[K, T](de, m)
	// This trick ensures that the janitor goroutine (which--granted it
	// was enabled--is running DeleteExpired on c forever) does not keep
	// the returned C object from being garbage collected. When it is
	// garbage collected, the finalizer stops the janitor goroutine, after
	// which c can be collected.
	C := &Cache[K, T]{c}
	if ci > 0 {
		runJanitor(c, ci)
		runtime.SetFinalizer(C, stopJanitor[K, T])
	}
	return C
}

// New returns a new cache with a given default expiration duration and cleanup
// interval. If the expiration duration is less than one (or NoExpiration),
// the items in the cache never expire (by default), and must be deleted
// manually. If the cleanup interval is less than one, expired items are not
// deleted from the cache before calling c.DeleteExpired().
func New[K comparable, T any](defaultExpiration, cleanupInterval time.Duration) *Cache[K, T] {
	items := make(map[K]Item[T])
	return newCacheWithJanitor[K, T](defaultExpiration, cleanupInterval, items)
}

// NewFrom returns a new cache with a given default expiration duration and cleanup
// interval. If the expiration duration is less than one (or NoExpiration),
// the items in the cache never expire (by default), and must be deleted
// manually. If the cleanup interval is less than one, expired items are not
// deleted from the cache before calling c.DeleteExpired().
//
// NewFrom() also accepts an items map which will serve as the underlying map
// for the cache. This is useful for starting from a deserialized cache
// (serialized using e.g. gob.Encode() on c.Items()), or passing in e.g.
// make(map[K]Item, 500) to improve startup performance when the cache
// is expected to reach a certain minimum size.
//
// Only the cache's methods synchronize access to this map, so it is not
// recommended to keep any references to the map around after creating a cache.
// If need be, the map can be accessed at a later point using c.Items() (subject
// to the same caveat.)
//
// Note regarding serialization: When using e.g. gob, make sure to
// gob.Register() the individual types stored in the cache before encoding a
// map retrieved with c.Items(), and to register those same types before
// decoding a blob containing an items map.
func NewFrom[K comparable, T any](defaultExpiration, cleanupInterval time.Duration, items map[K]Item[T]) *Cache[K, T] {
	return newCacheWithJanitor(defaultExpiration, cleanupInterval, items)
}
