package nntp

import (
	"os"
	"testing"

	"github.com/sirrobot01/decypharr/internal/config"
)

// NewClient reads the global config; point it at a temp dir (as the
// reader package's tests do) so tests never touch a real config.json.
func TestMain(m *testing.M) {
	configDir, err := os.MkdirTemp("", "decypharr-nntp-test-")
	if err != nil {
		panic(err)
	}
	config.SetConfigPath(configDir)
	code := m.Run()
	_ = os.RemoveAll(configDir)
	os.Exit(code)
}
