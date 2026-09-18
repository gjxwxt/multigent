package main

import (
	"os/exec"
	"testing"
)

func TestCIReadyCheckToolOnTemplate(t *testing.T) {
	cmd := exec.Command("go", "run", ".", "../../internal/projecttemplate/files/react_go_fullstack")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("expected success on react_go_fullstack template, got error: %v, output: %s", err, string(out))
	}
}

func TestCIReadyCheckToolFailsOnEmpty(t *testing.T) {
	tmp := t.TempDir()
	cmd := exec.Command("go", "run", ".", tmp)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected error on empty dir, got success. Output: %s", string(out))
	}
}
