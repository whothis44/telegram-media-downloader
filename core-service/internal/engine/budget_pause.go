package engine

// PauseBudget stops the attempt's time limit while the sink waits for
// something that is not the transfer (room in the local staging folder).
// Call the returned function when the wait is over.
func (t trackedTransport) PauseBudget() func() {
	t.budget.pause()
	return t.budget.resume
}
