package filesystem_exfat

import "sync"

// entryCache keeps the parsed entries of directories that path resolution has
// read. Every Stat, ReadFile and ListDir resolves its path by reading and parsing
// the directory of each component, so a walk of N files parsed the same
// directories N times over (a metadata walk was 1.3x slower than the kernel,
// #44).
//
// The volume is writable, so the cache is dropped whole whenever a directory is
// written (writeDirBuf, which every directory change goes through) and when the
// volume is resized: it never serves what a write has since changed. It is
// bounded in bytes, estimated from the entries (a directory can be up to
// maxDirBytes of an attacker-controlled image); a directory costing more than
// the budget is not cached; eviction is FIFO. A change made to the image by
// anything but this exfatFS is not seen by an open one (as for any open image).
type entryCache struct {
	mu    sync.Mutex
	dirs  map[uint32][]rootDirEntry
	cost  map[uint32]int
	order []uint32
	bytes int
	// gen counts the drops: a reader that parsed a directory before a write
	// must not store it after the write's drop (a stale view would then stay
	// until the next directory write), so put takes the generation seen BEFORE
	// the read and refuses a changed one.
	gen uint64
}

const maxCachedEntryBytes = 16 << 20

// entrySlot is the size charged per entry: the struct (a string header, the
// numeric fields and the 17 time bytes), rounded up.
const entrySlot = 96

func entriesCost(es []rootDirEntry) int {
	n := len(es) * entrySlot
	for _, e := range es {
		n += len(e.name)
	}
	return n
}

func (c *entryCache) get(cluster uint32) ([]rootDirEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	es, ok := c.dirs[cluster]
	return es, ok
}

func (c *entryCache) generation() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.gen
}

func (c *entryCache) put(cluster uint32, es []rootDirEntry, gen uint64) {
	cost := entriesCost(es)
	if cost > maxCachedEntryBytes {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if gen != c.gen {
		return // a directory was written since the caller started reading
	}
	if c.dirs == nil {
		c.dirs = make(map[uint32][]rootDirEntry)
		c.cost = make(map[uint32]int)
	}
	if _, ok := c.dirs[cluster]; ok {
		return
	}
	for c.bytes+cost > maxCachedEntryBytes && len(c.order) > 0 {
		old := c.order[0]
		c.order = c.order[1:]
		c.bytes -= c.cost[old]
		delete(c.dirs, old)
		delete(c.cost, old)
	}
	c.dirs[cluster] = es[:len(es):len(es)]
	c.cost[cluster] = cost
	c.order = append(c.order, cluster)
	c.bytes += cost
}

// drop forgets everything: called before and after any directory write.
func (c *entryCache) drop() {
	c.mu.Lock()
	c.dirs, c.cost, c.order, c.bytes = nil, nil, nil, 0
	c.gen++
	c.mu.Unlock()
}

// dirEntries returns the parsed entries of the directory starting at cluster.
// The slice is shared with the cache and must not be modified.
func (fs *exfatFS) dirEntries(cluster uint32) ([]rootDirEntry, error) {
	if es, ok := fs.entries.get(cluster); ok {
		return es, nil
	}
	gen := fs.entries.generation()
	buf, err := fs.readDirBuf(cluster)
	if err != nil {
		return nil, err
	}
	es, err := parseRootDirMetadata(buf)
	if err != nil {
		return nil, err
	}
	fs.entries.put(cluster, es, gen)
	return es, nil
}
