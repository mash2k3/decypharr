package stats

import (
	"bytes"
	"encoding/json"
)

// redactedFields are dropped from debrid profiles and accounts before a
// snapshot leaves the machine: shared logs go to a public paste site.
var (
	redactedProfileFields = []string{"username", "email", "id"}
	redactedAccountFields = []string{"username", "token_masked"}
)

// ShareJSON returns the /debug/stats snapshot as indented JSON with account
// identifiers removed, for inclusion in shared logs.
func (c *Collector) ShareJSON() ([]byte, error) {
	snap := c.Snapshot()
	if snap == nil {
		snap = c.Refresh()
	}
	return redactSnapshotJSON(snap)
}

func redactSnapshotJSON(snap *Snapshot) ([]byte, error) {
	raw, err := json.Marshal(snap)
	if err != nil {
		return nil, err
	}
	var doc map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber() // keep byte counts exact instead of round-tripping through float64
	if err := dec.Decode(&doc); err != nil {
		return nil, err
	}
	debrids, _ := doc["debrids"].([]any)
	for _, d := range debrids {
		debrid, ok := d.(map[string]any)
		if !ok {
			continue
		}
		if profile, ok := debrid["profile"].(map[string]any); ok {
			for _, k := range redactedProfileFields {
				delete(profile, k)
			}
		}
		accounts, _ := debrid["accounts"].([]any)
		for _, a := range accounts {
			if account, ok := a.(map[string]any); ok {
				for _, k := range redactedAccountFields {
					delete(account, k)
				}
			}
		}
	}
	return json.MarshalIndent(doc, "", "  ")
}
