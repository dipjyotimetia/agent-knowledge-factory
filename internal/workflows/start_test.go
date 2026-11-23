package workflows

import (
	"errors"
	"strings"
	"testing"

	"go.temporal.io/api/serviceerror"
)

func TestWorkflowIDIsStable(t *testing.T) {
	if WorkflowID("banking-demo", "gap") != "curate-banking-demo-gap" {
		t.Fatal(WorkflowID("banking-demo", "gap"))
	}
}

func TestTemporalOptions(t *testing.T) {
	_, err := (TemporalConfig{}).Options()
	if err == nil || !strings.Contains(err.Error(), "TEMPORAL_ADDRESS") {
		t.Fatal(err)
	}
	opts, err := TemporalFromLookup(func(key string) string {
		if key == "TEMPORAL_ADDRESS" {
			return "example.invalid:7233"
		}
		if key == "TEMPORAL_API_KEY" {
			return "key"
		}
		if key == "TEMPORAL_NAMESPACE" {
			return "akf"
		}
		return ""
	}).Options()
	if err != nil {
		t.Fatal(err)
	}
	if opts.HostPort != "example.invalid:7233" || opts.Namespace != "akf" || opts.Credentials == nil {
		t.Fatalf("options = %+v", opts)
	}
	if _, err := (TemporalConfig{Address: "example.invalid:7233", TLSCert: "cert.pem"}).Options(); err == nil {
		t.Fatal("expected a cert without a key to fail")
	}
}

func TestAlreadyStartedIsIdempotent(t *testing.T) {
	if err := ignoreAlreadyStarted(&serviceerror.WorkflowExecutionAlreadyStarted{}); err != nil {
		t.Fatal(err)
	}
	if err := ignoreAlreadyStarted(errors.New("down")); err == nil {
		t.Fatal("unexpected nil")
	}
}
