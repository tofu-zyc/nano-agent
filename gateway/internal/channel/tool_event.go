package channel

import "fmt"

// ToolEvent identifies one accepted call through queued, running and done.
// RunView remains compatible with channels that only support textual output.
type ToolEvent struct {
	RunID  string
	TurnID int
	CallID string
	Name   string
	State  string
	Detail string
	OK     bool
}

type ToolEventView interface{ ToolEvent(ToolEvent) }

func EmitToolEvent(view RunView, event ToolEvent) {
	if view == nil {
		return
	}
	if identified, ok := view.(ToolEventView); ok {
		identified.ToolEvent(event)
		return
	}
	switch event.State {
	case "accepted":
		view.Tool(event.Name, event.Detail)
	case "done":
		view.ToolResult(event.OK, fmt.Sprintf("%s [%d/%s] %s", event.Name, event.TurnID, event.CallID, event.Detail))
	}
}
