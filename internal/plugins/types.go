// Package plugins holds the vocabulary shared by the plugin platform: plugin
// types, installation modes and lifecycle states. Everything else lives in
// sub-packages (manifest, secrets, registry, protocol, contract, dynamic,
// manager, controller).
package plugins

import "fmt"

// Type names one pluggable component. The nine values below are the complete
// set the platform can install.
type Type string

const (
	TypeLLM              Type = "llm"
	TypeEmbedder         Type = "embedder"
	TypeVectorStore      Type = "vector-store"
	TypePersistence      Type = "persistence"
	TypeQueue            Type = "queue"
	TypePolicy           Type = "policy"
	TypeEmbeddingModel   Type = "embedding-model"
	TypeVectorIndex      Type = "vector-index"
	TypeSimilarityMetric Type = "similarity-metric"
)

// AllTypes lists every supported type in a stable order.
var AllTypes = []Type{
	TypeLLM, TypeEmbedder, TypeVectorStore, TypePersistence, TypeQueue, TypePolicy,
	TypeEmbeddingModel, TypeVectorIndex, TypeSimilarityMetric,
}

// ContractVersion is the only remote-protocol version this release speaks.
const ContractVersion = "v1"

// ParseType validates a type name.
func ParseType(s string) (Type, error) {
	for _, t := range AllTypes {
		if string(t) == s {
			return t, nil
		}
	}
	return "", fmt.Errorf("unknown plugin type %q (supported: %v)", s, AllTypes)
}

// IsPython reports whether the type is a Python registry plugin. Those run
// inside a specialised runner image (the embedding or vector-store service with
// the developer's package added), never as a free-standing HTTP server.
func (t Type) IsPython() bool {
	return t == TypeEmbeddingModel || t == TypeVectorIndex || t == TypeSimilarityMetric
}

// Slot names the orchestrator seam a type is activated into. The three Python
// types do not have a slot of their own: they are activated by replacing the
// service behind the embedder or vector-store slot.
func (t Type) Slot() Type {
	switch t {
	case TypeEmbeddingModel:
		return TypeEmbedder
	case TypeVectorIndex, TypeSimilarityMetric:
		return TypeVectorStore
	}
	return t
}

// Mode is how the plugin is supplied.
type Mode string

const (
	ModeEndpoint   Mode = "endpoint"   // developer already runs it
	ModeImage      Mode = "image"      // prebuilt OCI image
	ModeRepository Mode = "repository" // GitHub repository built by us
)

// State is the installation state shown to the administrator.
type State string

const (
	StateDraft       State = "draft"
	StateValidating  State = "validating_manifest"
	StateTesting     State = "testing_contract"
	StateBuilding    State = "building"
	StateScanning    State = "scanning"
	StateStarting    State = "starting_candidate"
	StateMigrating   State = "migrating"
	StateChecking    State = "checking_health"
	StateActivating  State = "activating"
	StateVerified    State = "verified" // ready to activate
	StateActive      State = "active"
	StateInactive    State = "inactive"
	StateFailed      State = "failed"
	StateRollingBack State = "rolling_back"
	StateRolledBack  State = "rolled_back"
)

// InProgress reports whether the state is a transient working state.
func (s State) InProgress() bool {
	switch s {
	case StateValidating, StateTesting, StateBuilding, StateScanning, StateStarting,
		StateMigrating, StateChecking, StateActivating, StateRollingBack:
		return true
	}
	return false
}

// transitions is the lifecycle state machine. A plugin only ever moves along
// these edges; the manager refuses anything else, so a bug cannot, for example,
// mark a plugin active without passing through activation.
var transitions = map[State][]State{
	StateDraft:       {StateValidating, StateFailed},
	StateValidating:  {StateBuilding, StateScanning, StateStarting, StateTesting, StateFailed},
	StateBuilding:    {StateScanning, StateFailed},
	StateScanning:    {StateStarting, StateFailed},
	StateStarting:    {StateTesting, StateFailed},
	StateTesting:     {StateChecking, StateFailed},
	StateChecking:    {StateVerified, StateActivating, StateMigrating, StateFailed},
	StateVerified:    {StateMigrating, StateChecking, StateActivating, StateValidating, StateInactive, StateFailed},
	StateMigrating:   {StateChecking, StateActivating, StateVerified, StateFailed},
	StateActivating:  {StateActive, StateVerified, StateFailed},
	StateActive:      {StateInactive, StateRollingBack, StateActive},
	StateInactive:    {StateValidating, StateChecking, StateMigrating, StateActivating, StateFailed, StateVerified},
	StateFailed:      {StateValidating, StateChecking, StateMigrating, StateActivating, StateInactive, StateVerified, StateFailed},
	StateRollingBack: {StateRolledBack, StateActive, StateFailed},
	StateRolledBack:  {StateValidating, StateChecking, StateMigrating, StateActivating, StateInactive, StateVerified},
}

// ValidTransition reports whether from -> to is an allowed lifecycle edge.
func ValidTransition(from, to State) bool {
	if from == to && from.InProgress() {
		return true // progress detail updates
	}
	for _, t := range transitions[from] {
		if t == to {
			return true
		}
	}
	return false
}
