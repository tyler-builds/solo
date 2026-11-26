// Package claudesdk is a minimal, unofficial Go client for Claude Code, ported from the
// open-source Python Claude Agent SDK (github.com/anthropics/claude-agent-sdk-python, MIT).
// It is not made or endorsed by Anthropic.
//
// It starts the user's installed `claude` CLI in stream-json mode and speaks its control protocol
// over stdin and stdout, one JSON object per line:
//
//   - Messages the CLI emits (system, assistant, user, stream_event, result, …) are delivered as
//     [Message] values from [Client.Messages], with their raw JSON kept for the caller to decode.
//   - Requests from the SDK to the CLI are control_request frames (initialize, interrupt, set_model,
//     set_permission_mode, stop_task, …), answered by control_response frames with the same
//     request_id. [Client.Control] sends any of them.
//   - Requests from the CLI to the SDK are control_request frames too: can_use_tool goes to
//     [Options.CanUseTool], hook_callback to [Options.Hooks], and mcp_message to the in-process MCP
//     servers of [Options.MCPServers].
//
// One [Client] is one claude process and one session. Follow-up turns are sent into the same
// process with [Client.Send], each carrying a uuid that the CLI echoes back on the turn that answers
// it. [Complete] is a one-shot helper for small model calls (titles, summaries) with no tools, no
// MCP servers and no saved session.
//
// [Client.BackgroundTasks] moves a running subagent to the background, so a steer (a message sent with
// Priority "now") stops the turn without losing the subagent's work.
package claudesdk
