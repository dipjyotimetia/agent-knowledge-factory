package domain

// Gap lifecycle states. duplicate, insufficient-evidence, cancelled, failed,
// rejected, and verified are terminal. verified is the only state that means
// the published revision was checked after merge.
const (
	StateNew                  = "new"
	StateTriaged              = "triaged"
	StateInvestigating        = "investigating"
	StateProposalReady        = "proposal-ready"
	StateAwaitingReview       = "awaiting-review"
	StateAccepted             = "accepted"
	StateRejected             = "rejected"
	StatePublished            = "published"
	StateVerified             = "verified"
	StateDuplicate            = "duplicate"
	StateInsufficientEvidence = "insufficient-evidence"
	StateCancelled            = "cancelled"
	StateFailed               = "failed"
)

var transitions = map[string]map[string]bool{
	StateNew:            {StateTriaged: true, StateDuplicate: true, StateRejected: true, StateCancelled: true, StateFailed: true},
	StateTriaged:        {StateInvestigating: true, StateCancelled: true, StateFailed: true, StateInsufficientEvidence: true},
	StateInvestigating:  {StateProposalReady: true, StateInsufficientEvidence: true, StateCancelled: true, StateFailed: true},
	StateProposalReady:  {StateAwaitingReview: true, StateInsufficientEvidence: true, StateFailed: true, StateCancelled: true},
	StateAwaitingReview: {StateAccepted: true, StateRejected: true, StateFailed: true, StateProposalReady: true},
	StateAccepted:       {StatePublished: true, StateRejected: true, StateAwaitingReview: true},
	StatePublished:      {StateVerified: true, StateFailed: true},
}

// CanTransition reports whether a gap may move from one state to another.
// Same-state calls are rejected so a no-op cannot look like progress.
func CanTransition(from, to string) bool {
	return transitions[from][to]
}
