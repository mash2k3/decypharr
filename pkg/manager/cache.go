package manager

import (
	"strings"

	"github.com/puzpuzpuz/xsync/v4"
	"golang.org/x/sync/singleflight"
)

const (
	torrentEntryCachePrefix = "torrent::"
)

func (m *Manager) initEntryCache() {
	m.entry = NewEntryCache(m)
}

type EntryCacheItem struct {
	current  *FileInfo
	children []FileInfo
}

type EntryCache struct {
	manager    *Manager
	entries    *xsync.Map[string, EntryCacheItem]
	refreshing singleflight.Group
}

func NewEntryCache(manager *Manager) *EntryCache {
	return &EntryCache{
		manager: manager,
		entries: xsync.NewMap[string, EntryCacheItem](),
	}
}

func (e *EntryCache) Get(name string) (*FileInfo, []FileInfo) {
	item, ok := e.entries.Load(name)
	if !ok {
		item = e.refreshEntry(name)
	}
	return item.current, item.children
}

func (e *EntryCache) refreshEntry(name string) EntryCacheItem {
	result, _, _ := e.refreshing.Do(name, func() (interface{}, error) {
		return e._refreshEntry(name), nil
	})
	return result.(EntryCacheItem)
}

func (e *EntryCache) _refreshEntry(name string) EntryCacheItem {
	if strings.HasPrefix(name, torrentEntryCachePrefix) {
		// This is a torrent folder
		torrentName := strings.TrimPrefix(name, torrentEntryCachePrefix)
		current, children := e.manager.getTorrentChildren(torrentName)
		item := EntryCacheItem{
			current:  current,
			children: children,
		}
		e.entries.Store(name, item)
		return item
	}

	// This is either a __all__, __bad__ or custom folder
	current, children := e.manager.getEntryChildren(name)
	item := EntryCacheItem{
		current:  current,
		children: children,
	}
	e.entries.Store(name, item)
	return item
}

// Refresh drops every cached listing so the next read rebuilds it from storage.
//
// The cache only holds group listings (__all__, __bad__, torrents, nzbs, custom
// folders, per-provider folders) and torrent:: listings, and all of them can be
// changed by a sync. Deleting group names one by one missed the per-provider
// folders (e.g. "RealDebrid"), which then stayed frozen until restart while
// __all__ picked up new content.
func (e *EntryCache) Refresh() {
	e.entries.Clear()
}
