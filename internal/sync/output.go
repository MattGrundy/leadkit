package sync

// Op is one thing an Output's Plan says to do against the tracker — computed
// locally, executed elsewhere (by an agent holding the tracker's own MCP token).
type Op struct {
	Action          string            `json:"action"` // "create" | "update" | "close"
	Level           string            `json:"level"`  // the OUTPUT's own level name (e.g. Linear's "initiative")
	LocalID         string            `json:"local_id"`
	ParentTrackerID string            `json:"parent_tracker_id,omitempty"`
	Fields          map[string]string `json:"fields,omitempty"`
}

// Output is one pluggable tracker target. leadkit computes Plan (pure, read-only).
// Who EXECUTES it is the adapter's own business and is deliberately not part of this
// interface: Linear's ops are applied by an agent holding the Linear MCP (or the
// opt-in DirectExecutor), GitHub's by GitHubExecutor in process through gh. Each
// calls RecordID to write a created id back locally.
//
// An Output may also implement FlatTickets, to be handed the flat ticket model
// (BuildTicketModel) instead of the hierarchy tree (BuildSyncModel).
type Output interface {
	Name() string
	Plan(model Model) ([]Op, error)
}

// Registry maps an `--output` name to its Output. Plan, Op, Node and RecordID are
// generic; two things are not and live outside this map: which MODEL an output is
// built from (BuildModelFor) and how its ops are EXECUTED (cmd/leadkit's `sync run`
// branches on the output name).
var Registry = map[string]Output{
	"linear": LinearAdapter{},
	"github": GitHubAdapter{},
}
