package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestJevProcessBuilds(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "jev")
	build := exec.Command("go", "build", "-o", binary, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v: %s", err, out)
	}
	if info, err := os.Stat(binary); err != nil || info.Size() == 0 {
		t.Fatalf("built process missing or empty: %v", err)
	}
}
