package policy

import "context"

// Env is what the host hands each tool call: who to ask, where to log,
// and the user's task for the Critic. A host whose Gate is per turn (such
// as Quartermaster's card gate) builds a new Env for each call.
type Env struct {
	Gate   Gate
	Audit  Auditor
	Critic Critic
	// Task is the user's request in their own words, for the Critic.
	Task string
	// Owner names who is using the browser (a chat). A Session lets one
	// owner at a time drive it.
	Owner string
}

// gate is e.Gate, or NoGate.
func (e Env) gate() Gate {
	if e.Gate == nil {
		return NoGate
	}
	return e.Gate
}

// Ask asks through the Gate.
func (e Env) Ask(ctx context.Context, a Ask) (Answer, error) {
	return e.gate().Decide(ctx, a)
}

// Log records an audit event when there is an Auditor.
func (e Env) Log(ev Event) {
	if e.Audit != nil {
		e.Audit.Record(ev)
	}
}
