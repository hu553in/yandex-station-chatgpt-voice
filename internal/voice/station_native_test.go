package voice

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func TestNativeSpotterConstructorReportsVendorFailure(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "spotter-abi")
	args := []string{"-std=c11", "-Wall", "-Wextra", "-Werror", "testdata/spotter_abi.c", "-o", binary}
	if runtime.GOOS == "linux" {
		args = append(args, "-ldl")
	}
	if output, err := exec.CommandContext(t.Context(), "cc", args...).CombinedOutput(); err != nil {
		t.Fatalf("cannot build native ABI fixture: %v\n%s", err, output)
	}
	if output, err := exec.CommandContext(t.Context(), binary).CombinedOutput(); err != nil {
		t.Fatalf("native constructor did not return the vendor error safely: %v\n%s", err, output)
	}
}
