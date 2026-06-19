package storage

import "github.com/sirrobot01/decypharr/internal/config"

// HandleExistingEntryMerge merges an incoming entry with an existing one that
// shares the same infohash. This preserves placements, files, and tags from
// the existing entry that the incoming entry may not know about.
func HandleExistingEntryMerge(existing, incoming *Entry) *Entry {
	// If NZB entry, ignore merging - just return incoming
	if incoming.Protocol == config.ProtocolNZB {
		return incoming
	}
	incoming.Files = mergeFiles(existing.Files, incoming.Files)
	incoming.ActiveProvider = selectActivePlacement(existing, incoming)
	incoming.Providers = mergeProviders(existing.Providers, incoming.Providers)
	incoming.Tags = mergeTags(existing.Tags, incoming.Tags)

	return incoming
}

// mergeProviders merges two placement maps, preferring newer data for same debrid
func mergeProviders(existing, incoming map[string]*ProviderEntry) map[string]*ProviderEntry {
	if existing == nil {
		return incoming
	}
	if incoming == nil {
		return existing
	}

	merged := make(map[string]*ProviderEntry)

	// Copy existing placements
	for k, v := range existing {
		merged[k] = v
	}

	// Merge incoming placements (overwrites if same key)
	for k, v := range incoming {
		if existingPlacement, exists := merged[k]; exists {
			// Keep placement with more recent UpdatedAt
			if v.AddedAt.After(existingPlacement.AddedAt) {
				merged[k] = v
			}
		} else {
			merged[k] = v
		}
	}

	return merged
}

// mergeFiles merges two file maps, preferring files with newer AddedOn.
// When an existing file was renamed (different key, same size), the renamed
// version takes precedence over the incoming original-named version.
func mergeFiles(existing, incoming map[string]*File) map[string]*File {
	if existing == nil {
		return incoming
	}
	if incoming == nil {
		return existing
	}

	merged := make(map[string]*File)

	// Copy existing files
	for k, v := range existing {
		merged[k] = v
	}

	// Build a size index of already-merged files to detect same-file duplicates
	mergedSizes := make(map[int64]bool)
	for _, v := range merged {
		if v.Size > 0 {
			mergedSizes[v.Size] = true
		}
	}

	// Merge incoming files — skip if an existing file with the same size is
	// already present (it means the existing file was renamed and the incoming
	// key is the original RD filename; the renamed version takes precedence).
	for k, v := range incoming {
		if existingFile, exists := merged[k]; exists {
			// Same key — prefer newer AddedOn timestamp
			if v.AddedOn.After(existingFile.AddedOn) {
				merged[k] = v
			}
		} else if v.Size > 0 && mergedSizes[v.Size] {
			// Different key but same size — existing renamed file wins, skip
			continue
		} else {
			merged[k] = v
			if v.Size > 0 {
				mergedSizes[v.Size] = true
			}
		}
	}

	return merged
}

// selectActivePlacement selects the active debrid placement
func selectActivePlacement(existing, incoming *Entry) string {
	// Prefer incoming if it has an active placement
	if incoming.ActiveProvider != "" {
		return incoming.ActiveProvider
	}
	return existing.ActiveProvider
}

// mergeTags merges two tag slices, removing duplicates
func mergeTags(existing, incoming []string) []string {
	if len(existing) == 0 {
		return incoming
	}
	if len(incoming) == 0 {
		return existing
	}

	tagSet := make(map[string]bool)
	for _, tag := range existing {
		tagSet[tag] = true
	}
	for _, tag := range incoming {
		tagSet[tag] = true
	}

	merged := make([]string, 0, len(tagSet))
	for tag := range tagSet {
		merged = append(merged, tag)
	}
	return merged
}
