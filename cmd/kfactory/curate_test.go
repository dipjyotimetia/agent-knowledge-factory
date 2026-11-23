package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestCurateRefusesWhenTemporalIsUnset(t *testing.T) {
	t.Setenv("TEMPORAL_ADDRESS", "")
	t.Setenv("AKF_CONFIG", "")
	cmd := newCurate()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--workspace", "banking-demo", "--gap", "gap-1"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "TEMPORAL_ADDRESS") {
		t.Fatalf("err=%v out=%s", err, out.String())
	}
	if strings.Contains(out.String(), "started ") {
		t.Fatalf("claimed a workflow start: %s", out.String())
	}
}
