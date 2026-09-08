package app

import (
	"os"
	"testing"
)

// TestMain pins AIMESH_HOME to an isolated empty temp dir so config.LoadLayered
// never resolves the developer's real ~/.aimesh/adapters.yaml.
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "app-aimesh-home")
	if err != nil {
		panic(err)
	}
	_ = os.Setenv("AIMESH_HOME", home)
	code := m.Run()
	_ = os.RemoveAll(home)
	os.Exit(code)
}
