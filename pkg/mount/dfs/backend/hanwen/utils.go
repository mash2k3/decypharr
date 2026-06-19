//go:build linux || (darwin && amd64)

package hanwen

import (
	"hash"
	"hash/fnv"
	"sync"
)

var hasherPool = sync.Pool{
	New: func() interface{} {
		return fnv.New64a()
	},
}

func hashPath(path string) uint64 {
	h := hasherPool.Get().(hash.Hash64)
	defer hasherPool.Put(h)

	h.Reset()
	_, _ = h.Write([]byte(path))
	hs := h.Sum64()
	if hs <= 1 {
		hs = 2
	}
	return hs
}
