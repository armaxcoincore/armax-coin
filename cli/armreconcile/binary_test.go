package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/armaxcoincore/armax-coin/core"
)

var (
	binPath string
	binDir  string
	binOnce sync.Once
	binErr  error
)

// TestMain removes the per-process build directory after the package runs.
func TestMain(m *testing.M) {
	code := m.Run()
	if binDir != "" {
		_ = os.RemoveAll(binDir)
	}
	os.Exit(code)
}

// productionBinary builds armreconcile without test hooks, exactly as release builds do, into a
// directory private to this test process.
func productionBinary(t *testing.T) string {
	t.Helper()
	binOnce.Do(func() {
		dir, err := os.MkdirTemp("", "armreconcile-test-")
		if err != nil {
			binErr = err
			return
		}
		binDir = dir
		name := "armreconcile"
		if runtime.GOOS == "windows" {
			name += ".exe"
		}
		binPath = filepath.Join(dir, name)
		if out, err := exec.Command("go", "build", "-o", binPath, ".").CombinedOutput(); err != nil {
			binErr = &buildFailure{err: err, out: string(out)}
		}
	})
	if binErr != nil {
		t.Fatal(binErr)
	}
	return binPath
}

type buildFailure struct {
	err error
	out string
}

func (e *buildFailure) Error() string { return "build armreconcile: " + e.err.Error() + ": " + e.out }

func TestBinary_VersionMatchesCore(t *testing.T) {
	out, err := exec.Command(productionBinary(t), "version").CombinedOutput()
	if err != nil {
		t.Fatalf("version: %v: %s", err, out)
	}
	if got, want := strings.TrimSpace(string(out)), "armreconcile v"+core.Version; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestBinary_MainnetTrancheRefusedBeforeLaunch(t *testing.T) {
	if core.MainnetLaunched {
		t.Skip("Mainnet is launched")
	}
	dir := t.TempDir()
	cmd := exec.Command(productionBinary(t), "tranche", "-network", "mainnet",
		"-db", filepath.Join(dir, "blockchain.db"), "-manifest", filepath.Join(dir, "m.json"),
		"-treasury", core.MainnetGenesisMiner, "-target-total", "1", "-out", filepath.Join(dir, "plan.json"))
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("a production build must refuse Mainnet before launch: %s", out)
	}
	if !strings.Contains(string(out), "Mainnet has not been launched") {
		t.Fatalf("unexpected refusal: %s", out)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "plan.json")); statErr == nil {
		t.Fatal("no plan may be written when Mainnet is locked")
	}
}
