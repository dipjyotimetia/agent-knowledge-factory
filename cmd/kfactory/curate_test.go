package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestCurateRefusesWithoutWorkspaceDatabase(t *testing.T) {
	t.Setenv("AKF_DATABASE_URL", "")
	t.Setenv("AKF_CONFIG", "")
	cmd := newCurate()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--gap", "gap-1"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "AKF_DATABASE_URL") {
		t.Fatalf("err=%v out=%s", err, out.String())
	}
	if strings.Contains(out.String(), "started ") {
		t.Fatalf("claimed a workflow start: %s", out.String())
	}
}
