package resonate

import "fmt"

// This file is the fluent builder's coverage of MCP (Model Context Protocol).
// MCP is plain HTTP underneath, so Timeout, Insecure, CAFile, ClientCert, H2C,
// HTTP3, Feeder and Identity apply as they do for HTTP and SSE; Header sets
// headers sent on every request. See docs/mcp.md.

func (s *Scenario) requireMCP(name string) bool {
	if s.mcp == nil {
		s.deferErr(fmt.Errorf("resonate: %s only applies to an MCP scenario (call MCP first); this is %s", name, s.describeMode()))
		return false
	}
	return true
}

func (s *Scenario) requireMCPCall(name string) bool {
	if !s.requireMCP(name) {
		return false
	}
	if s.curMCP == nil {
		s.deferErr(fmt.Errorf("resonate: %s applies to an MCP call — call Tool, MCPList or MCPMethod first", name))
		return false
	}
	return true
}

// MCP switches the Scenario to build an MCPGenerator against url, the MCP
// server's streamable HTTP endpoint. Every virtual user opens its own session
// (initialize, then notifications/initialized — reported as the steps
// "initialize" and "initialized") and each iteration makes the calls added
// with Tool, MCPList and MCPMethod, in order, stopping at the first failure.
// Header sets headers sent on every request. Plain HTTP and event-stream
// replies are both handled.
func (s *Scenario) MCP(url string) *Scenario {
	s.mcp = &MCPTarget{URL: url}
	s.curMCP = nil
	return s
}

func (s *Scenario) addMCPCall(c MCPCall) *Scenario {
	s.mcp.Calls = append(s.mcp.Calls, c)
	s.curMCP = &s.mcp.Calls[len(s.mcp.Calls)-1]
	return s
}

// Tool appends a tool call (tools/call) to the scenario. arguments is a map
// (every string value may embed {{ }} expressions and renders as a JSON
// string), a string holding a raw JSON document that is itself a template (use
// that for a templated number or boolean), or nil for a tool with no
// arguments. Name, Extract, ExpectResult and ExpectToolError then apply to
// this call until the next one. The call is reported under the tool's name
// unless Name says otherwise.
func (s *Scenario) Tool(name string, arguments any) *Scenario {
	if !s.requireMCP("Tool") {
		return s
	}
	return s.addMCPCall(MCPCall{Tool: name, Arguments: arguments})
}

// MCPList appends a list request: kind is "tools", "resources" or "prompts"
// (tools/list, resources/list, prompts/list).
func (s *Scenario) MCPList(kind string) *Scenario {
	if !s.requireMCP("MCPList") {
		return s
	}
	return s.addMCPCall(MCPCall{List: kind})
}

// MCPMethod appends any other JSON-RPC method, with params in the same forms
// Tool takes arguments (a map, a raw JSON template string, or nil).
func (s *Scenario) MCPMethod(method string, params any) *Scenario {
	if !s.requireMCP("MCPMethod") {
		return s
	}
	return s.addMCPCall(MCPCall{Method: method, Params: params})
}

// ExpectResult checks the current MCP call's JSON-RPC result with an
// extract-style rule ("json:<path>", "yaml:<path>", "xml:<path>" or
// "regex:<pattern>"), keyed by rule and valued by the expected result ("" to
// only require the rule evaluates without error). MCP only.
func (s *Scenario) ExpectResult(rule, value string) *Scenario {
	if !s.requireMCPCall("ExpectResult") {
		return s
	}
	if s.curMCP.ExpectResult == nil {
		s.curMCP.ExpectResult = map[string]string{}
	}
	s.curMCP.ExpectResult[rule] = value
	return s
}

// ExpectToolError makes a tool-level error (result.isError: true) the expected
// outcome of the current Tool call: it passes, and a call that succeeds fails.
// MCP only.
func (s *Scenario) ExpectToolError() *Scenario {
	if !s.requireMCPCall("ExpectToolError") {
		return s
	}
	s.curMCP.ExpectToolError = true
	return s
}

// ProtocolVersion sets the MCP protocol version announced in initialize
// (default MCPDefaultProtocolVersion). MCP only.
func (s *Scenario) ProtocolVersion(version string) *Scenario {
	if !s.requireMCP("ProtocolVersion") {
		return s
	}
	s.mcp.ProtocolVersion = version
	return s
}

// ClientInfo sets the clientInfo announced in initialize (default "resonate",
// "1.0"). MCP only.
func (s *Scenario) ClientInfo(name, version string) *Scenario {
	if !s.requireMCP("ClientInfo") {
		return s
	}
	s.mcp.ClientName, s.mcp.ClientVersion = name, version
	return s
}

// mcpExtract is Extract after MCP: it pulls a value from the current call's
// JSON-RPC result into a variable for later calls in the same iteration.
func (s *Scenario) mcpExtract(name, rule string) *Scenario {
	if !s.requireMCPCall("Extract") {
		return s
	}
	if s.curMCP.Extract == nil {
		s.curMCP.Extract = map[string]string{}
	}
	s.curMCP.Extract[name] = rule
	return s
}
