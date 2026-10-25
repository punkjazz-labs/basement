package fleet

// RuntimeHolder names a service that occupies a node from outside this
// manager: an attached service its operator declared (ADR 0025). basement can
// neither stop nor switch away from such a service, so a node it names is
// never given another model's runtime claim, by this node's own allocator or
// by fleet placement. held is false when nothing outside holds the node.
type RuntimeHolder func(nodeID, displayName string) (holder string, held bool)

// HeldNodeError is the refusal of a runtime claim on a node an attached
// service holds. It is a Go error, so it carries no capital and no full stop,
// and the console shows it verbatim.
type HeldNodeError struct{ Holder string }

func (failure HeldNodeError) Error() string {
	return "the attached service " + failure.Holder + " has this Spark reserved outside Basement, so no other model can start here; stop that service where it is managed and remove its declaration first"
}

// heldNodeReason is the same fact written as a plan reason, in the register
// the planner's other reasons use (see failures.go): a whole sentence that
// names something about the Spark rather than the Spark itself.
func heldNodeReason(holder string) string {
	return "The attached service " + holder + " has this Spark reserved outside Basement, so it cannot take a model now."
}

// SetRuntimeHolder installs the attached-service source for this node's
// allocator and for the placements this manager plans. Called once at
// startup, before any job can be admitted. nil means nothing outside holds
// any node, which is also what a manager that never calls this gets.
func (m *Manager) SetRuntimeHolder(holder RuntimeHolder) {
	m.catalogueMu.Lock()
	m.holder = holder
	m.catalogueMu.Unlock()
	if holder == nil {
		m.allocator.SetRuntimeHolder(nil)
		return
	}
	nodeID, displayName := m.identity.NodeID, m.displayName
	m.allocator.SetRuntimeHolder(func() (string, bool) { return holder(nodeID, displayName) })
}

func (m *Manager) runtimeHolder(nodeID, displayName string) (string, bool) {
	m.catalogueMu.RLock()
	holder := m.holder
	m.catalogueMu.RUnlock()
	if holder == nil {
		return "", false
	}
	return holder(nodeID, displayName)
}
