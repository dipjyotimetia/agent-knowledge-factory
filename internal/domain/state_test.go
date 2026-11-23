package domain

import "testing"

func TestCanTransition(t *testing.T) {
	if !CanTransition(StateNew, StateTriaged) {
		t.Fatal("new to triaged")
	}
	if CanTransition(StateNew, StatePublished) {
		t.Fatal("new must not jump to published")
	}
	if CanTransition(StateVerified, StateNew) {
		t.Fatal("verified is terminal")
	}
	if CanTransition(StateNew, StateNew) {
		t.Fatal("same state is not a transition")
	}
}
