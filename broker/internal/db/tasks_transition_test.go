package db

import "testing"

// TestIsValidTransition_ExecutingToCancelled_Allowed covers the CP1 cancel edge:
// a user-initiated cancel must be acceptable while a task is EXECUTING.
func TestIsValidTransition_ExecutingToCancelled_Allowed(t *testing.T) {
	if !isValidTransition(TaskStateExecuting, TaskStateCancelled) {
		t.Error("expected EXECUTING→CANCELLED to be a valid transition")
	}
}

// TestIsValidTransition_ApprovedEdges pins the two ways a task leaves APPROVED
// and the one way it must not.
//
// Both edges exist because of a live failure: every task on the on-prem host was stuck in
// APPROVED (343 of them, zero COMPLETED or FAILED ever) because nothing wrote
// EXECUTING and APPROVED had no edge to a terminal state, so the caller's
// terminal EmitStatus was always rejected. Two distinct cases needed fixing:
//
//   - the tool ran → InvokeTool writes APPROVED→EXECUTING before dispatch, and
//     the outcome lands as EXECUTING→COMPLETED/FAILED.
//   - the tool was refused before dispatch (capability gate, cost-budget
//     pre-gate, rate limiter, org effect-class switch) → nothing ran, so the
//     task reports FAILED straight from APPROVED.
//
// APPROVED→COMPLETED stays invalid: a task cannot complete work it never began.
func TestIsValidTransition_ApprovedEdges(t *testing.T) {
	if !isValidTransition(TaskStateApproved, TaskStateExecuting) {
		t.Error("APPROVED→EXECUTING must be valid — InvokeTool writes it before running the tool")
	}
	if !isValidTransition(TaskStateApproved, TaskStateFailed) {
		t.Error("APPROVED→FAILED must be valid — a tool call refused before dispatch never becomes EXECUTING")
	}
	if isValidTransition(TaskStateApproved, TaskStateCompleted) {
		t.Error("APPROVED→COMPLETED must NOT be valid — nothing ran, so nothing completed")
	}
	for _, to := range []TaskState{TaskStateCompleted, TaskStateFailed} {
		if !isValidTransition(TaskStateExecuting, to) {
			t.Errorf("EXECUTING→%s must be valid", to)
		}
	}
}

// TestIsValidTransition_TerminalStatesStayClosed guards against the edit
// accidentally reopening any terminal state's outgoing transitions.
func TestIsValidTransition_TerminalStatesStayClosed(t *testing.T) {
	terminal := []TaskState{
		TaskStateCompleted,
		TaskStateFailed,
		TaskStateDenied,
		TaskStateCancelled,
		TaskStateTerminated,
		TaskStateTimeout,
	}
	targets := []TaskState{
		TaskStateCreated, TaskStatePlanning, TaskStateValidating, TaskStateAwaitingApproval,
		TaskStateApproved, TaskStateExecuting, TaskStateCompleted, TaskStateFailed,
		TaskStateDenied, TaskStateCancelled, TaskStateTerminated, TaskStateTimeout,
	}
	for _, from := range terminal {
		for _, to := range targets {
			if isValidTransition(from, to) {
				t.Errorf("terminal state %s must have no outgoing transitions, but %s→%s was allowed", from, from, to)
			}
		}
	}
}

// TestIsValidTransition_OtherExecutingEdgesUnaffected pins the pre-existing
// EXECUTING edges so the CP1 edit is additive, not a rewrite.
func TestIsValidTransition_OtherExecutingEdgesUnaffected(t *testing.T) {
	stillAllowed := []TaskState{
		TaskStatePlanning, TaskStateCompleted, TaskStateFailed, TaskStateTerminated, TaskStateTimeout,
	}
	for _, to := range stillAllowed {
		if !isValidTransition(TaskStateExecuting, to) {
			t.Errorf("expected EXECUTING→%s to remain valid", to)
		}
	}
	if isValidTransition(TaskStateExecuting, TaskStateDenied) {
		t.Error("EXECUTING→DENIED must not be a valid transition")
	}
}
