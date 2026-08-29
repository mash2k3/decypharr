package config

import (
	"encoding/json"
	"testing"
)

func TestInteractivePoolReserveDefaultsFalse(t *testing.T) {
	var cfg Config
	if err := json.Unmarshal([]byte(`{"usenet":{}}`), &cfg); err != nil {
		t.Fatalf("unmarshal config: %v", err)
	}
	if cfg.Usenet.InteractivePoolReserveEnabled {
		t.Fatal("interactive_pool_reserve_enabled must default to false when omitted")
	}
	cfg.updateUsenetConfig()
	if cfg.Usenet.InteractivePoolReservePercent != 15 {
		t.Fatalf("percent default = %d, want 15", cfg.Usenet.InteractivePoolReservePercent)
	}
	if cfg.Usenet.InteractivePoolReserveMin != 6 {
		t.Fatalf("min default = %d, want 6", cfg.Usenet.InteractivePoolReserveMin)
	}
	if cfg.Usenet.InteractivePoolReserveMax != 40 {
		t.Fatalf("max default = %d, want 40", cfg.Usenet.InteractivePoolReserveMax)
	}
	if cfg.Usenet.InteractiveDetectBytes != "4MB" {
		t.Fatalf("detect bytes default = %q, want 4MB", cfg.Usenet.InteractiveDetectBytes)
	}
}

func TestInteractivePoolReserveJSON(t *testing.T) {
	raw := `{"usenet":{"interactive_pool_reserve_enabled":true,"interactive_pool_reserve_percent":20,"interactive_pool_reserve_min":8,"interactive_pool_reserve_max":50,"interactive_detect_bytes":"8MB","interactive_detect_window":"10s","interactive_idle_timeout":"60s"}}`
	var cfg Config
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatalf("unmarshal config: %v", err)
	}
	if !cfg.Usenet.InteractivePoolReserveEnabled {
		t.Fatal("interactive_pool_reserve_enabled=true was not decoded")
	}
	if cfg.Usenet.InteractivePoolReservePercent != 20 {
		t.Fatalf("percent = %d, want 20", cfg.Usenet.InteractivePoolReservePercent)
	}
	if cfg.Usenet.InteractiveDetectBytes != "8MB" {
		t.Fatalf("detect bytes = %q, want 8MB", cfg.Usenet.InteractiveDetectBytes)
	}
}

func TestInteractivePoolReserveEnv(t *testing.T) {
	t.Setenv("DECYPHARR_USENET__INTERACTIVE_POOL_RESERVE_ENABLED", "true")
	t.Setenv("DECYPHARR_USENET__INTERACTIVE_POOL_RESERVE_PERCENT", "25")
	var cfg Config
	cfg.applyUsenetEnvVars()
	if !cfg.Usenet.InteractivePoolReserveEnabled {
		t.Fatal("DECYPHARR_USENET__INTERACTIVE_POOL_RESERVE_ENABLED=true was not applied")
	}
	if cfg.Usenet.InteractivePoolReservePercent != 25 {
		t.Fatalf("percent = %d, want 25", cfg.Usenet.InteractivePoolReservePercent)
	}
}

func TestComputeInteractiveReserve(t *testing.T) {
	tests := []struct {
		total, percent, min, max, want int
	}{
		{20, 15, 6, 40, 6},
		{40, 15, 6, 40, 6},
		{100, 15, 6, 40, 15},
		{310, 15, 6, 40, 40},
		{0, 15, 6, 40, 0},
	}
	for _, tc := range tests {
		got := ComputeInteractiveReserve(tc.total, tc.percent, tc.min, tc.max)
		if got != tc.want {
			t.Fatalf("ComputeInteractiveReserve(%d, %d, %d, %d) = %d, want %d",
				tc.total, tc.percent, tc.min, tc.max, got, tc.want)
		}
	}
}

func TestComputeDynamicInteractiveReserve(t *testing.T) {
	reserved, perStream := ComputeDynamicInteractiveReserve(310, 2, 15, 6, 100, 0)
	if perStream != 47 {
		t.Fatalf("perStream = %d, want 47", perStream)
	}
	if reserved != 94 {
		t.Fatalf("reserved = %d, want 94", reserved)
	}
	reserved, _ = ComputeDynamicInteractiveReserve(310, 3, 15, 6, 100, 0)
	if reserved != 100 {
		t.Fatalf("capped reserved = %d, want 100", reserved)
	}
}

func TestInteractiveDetectBytesValue(t *testing.T) {
	u := Usenet{InteractiveDetectBytes: "8MB"}
	if got := u.InteractiveDetectBytesValue(); got != 8<<20 {
		t.Fatalf("InteractiveDetectBytesValue() = %d, want %d", got, 8<<20)
	}
}
