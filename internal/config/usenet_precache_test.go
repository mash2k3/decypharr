package config

import (
	"encoding/json"
	"testing"
)

func TestUsenetPreCacheOnOpenDefaultsFalse(t *testing.T) {
	var cfg Config
	if err := json.Unmarshal([]byte(`{"usenet":{}}`), &cfg); err != nil {
		t.Fatalf("unmarshal config: %v", err)
	}
	if cfg.Usenet.PreCacheOnOpen {
		t.Fatal("pre_cache_on_open must default to false when omitted")
	}
}

func TestUsenetPreCacheOnOpenJSON(t *testing.T) {
	var cfg Config
	if err := json.Unmarshal([]byte(`{"usenet":{"pre_cache_on_open":true}}`), &cfg); err != nil {
		t.Fatalf("unmarshal config: %v", err)
	}
	if !cfg.Usenet.PreCacheOnOpen {
		t.Fatal("pre_cache_on_open=true was not decoded")
	}
}

func TestUsenetPreCacheOnOpenEnv(t *testing.T) {
	t.Setenv("DECYPHARR_USENET__PRE_CACHE_ON_OPEN", "true")
	var cfg Config
	cfg.applyUsenetEnvVars()
	if !cfg.Usenet.PreCacheOnOpen {
		t.Fatal("DECYPHARR_USENET__PRE_CACHE_ON_OPEN=true was not applied")
	}
}
