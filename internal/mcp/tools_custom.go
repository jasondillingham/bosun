package mcp

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/jasondillingham/bosun/internal/config"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// CustomToolArgs is the input schema for every operator-defined tool.
// One shape covers them all so operators don't have to define per-tool
// argument schemas in config — too much rope for too little benefit
// (any tool that needs structured input is better written as a real
// Go tool in this package). The optional `args` slice is appended to
// the def's command before exec, so an operator can ship a
// "bosun_lint" tool with command=["./scripts/lint.sh"] and the agent
// can call it with args=["--fix"] when appropriate.
type CustomToolArgs struct {
	Args []string `json:"args,omitempty" jsonschema:"optional positional arguments appended to the configured command"`
}

// CustomToolResult is the structured output every operator-defined
// tool returns. Stdout is the primary signal — the agent reads it as
// the tool's "answer." Stderr surfaces in the result too, partitioned
// off so it doesn't pollute the answer when the tool is chatty.
// ExitCode is informational; non-zero is already reflected in the
// CallToolResult's IsError flag.
type CustomToolResult struct {
	Stdout   string `json:"stdout" jsonschema:"the tool's stdout (always present; empty when the tool emitted nothing)"`
	Stderr   string `json:"stderr,omitempty" jsonschema:"the tool's stderr (omitted when empty)"`
	ExitCode int    `json:"exit_code" jsonschema:"the tool's exit code; non-zero implies failure"`
}

// RegisterCustomTools wires each MCPToolDef from cfg as an MCP tool on
// the server. Called by the production daemon (cmd/bosun/cmd_mcp.go)
// after NewServer; in-process tests can call it directly to exercise
// the path. Idempotent failure: an invalid def can't reach here
// because Validate runs at Load — but we still defensively skip empty
// commands so a corrupt config can't crash registration.
//
// Returns the list of tool names actually registered, so cmd_mcp.go
// can log "registered N custom tools" without re-grepping the config.
func RegisterCustomTools(s *Server, defs []config.MCPToolDef) []string {
	registered := make([]string, 0, len(defs))
	for _, def := range defs {
		if len(def.Command) == 0 || strings.TrimSpace(def.Command[0]) == "" {
			continue
		}
		// Bind def into the closure — capturing by value avoids the
		// classic range-variable-aliasing bug where every tool ends up
		// invoking the last def's command.
		def := def
		timeout := def.TimeoutSeconds
		if timeout == 0 {
			timeout = config.DefaultCustomToolTimeoutSeconds
		}
		handler := makeCustomToolHandler(def, timeout)
		mcp.AddTool(s.mcp, &mcp.Tool{
			Name:        def.Name,
			Description: def.Description,
		}, handler)
		registered = append(registered, def.Name)
	}
	return registered
}

// makeCustomToolHandler builds the handler function for one
// MCPToolDef. Split out so the closure shape is the same as the
// built-in tool handlers (test-friendly, signature-compatible with
// mcp.AddTool's generic).
func makeCustomToolHandler(def config.MCPToolDef, timeoutSec int) func(context.Context, *mcp.CallToolRequest, CustomToolArgs) (*mcp.CallToolResult, CustomToolResult, error) {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args CustomToolArgs) (*mcp.CallToolResult, CustomToolResult, error) {
		argv := append([]string(nil), def.Command...)
		// Operator-supplied args are appended verbatim — no shell
		// interpretation. The bosun_ name-prefix rule + this no-shell
		// rule are the security floor. An operator-malicious def
		// could still do anything the bosun process can, but a
		// malicious BRIEF (which an LLM might inject from a web page
		// it just read) can't get `; rm -rf /` past us.
		argv = append(argv, args.Args...)

		ctx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
		defer cancel()

		//nolint:gosec // G204: argv[0] is an operator-defined binary path; argv tail comes from the operator's config + agent-supplied positional args. This is the documented extension surface.
		cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		runErr := cmd.Run()

		// Timeout is checked BEFORE the ExitError branch because
		// ctx.Cancel kills the child, which returns *exec.ExitError
		// with ExitCode=-1 — indistinguishable from a real signal
		// kill unless we look at ctx.Err(). Without this check, a
		// timed-out tool would surface as "exited -1" with no
		// timeout context, leaving the operator wondering whether
		// their tool crashed or hit the cap.
		if ctx.Err() == context.DeadlineExceeded {
			summary := fmt.Sprintf("bosun_custom_tool %q exceeded the %ds timeout", def.Name, timeoutSec)
			return errResult(fmt.Errorf("%s", summary)), CustomToolResult{
				Stdout:   stdout.String(),
				Stderr:   stderr.String(),
				ExitCode: -1,
			}, nil
		}

		exitCode := 0
		if exitErr, ok := runErr.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else if runErr != nil {
			// Non-ExitError failure (couldn't fork, binary missing).
			// Treat as a hard tool failure so the agent gets IsError
			// back, not a silently empty result.
			summary := fmt.Sprintf("bosun_custom_tool %q failed to run: %v", def.Name, runErr)
			return errResult(fmt.Errorf("%s", summary)), CustomToolResult{
				Stdout:   stdout.String(),
				Stderr:   stderr.String(),
				ExitCode: -1,
			}, nil
		}

		result := CustomToolResult{
			Stdout:   stdout.String(),
			Stderr:   stderr.String(),
			ExitCode: exitCode,
		}

		// Non-zero exit: flag IsError so the agent's tool-call
		// machinery treats it the same way it treats a built-in tool
		// returning an error. The stdout/stderr still come through —
		// the agent often wants the failure detail to decide what to
		// do next.
		if exitCode != 0 {
			text := strings.TrimSpace(stderr.String())
			if text == "" {
				text = fmt.Sprintf("%s exited %d", def.Name, exitCode)
			}
			return &mcp.CallToolResult{
				IsError: true,
				Content: []mcp.Content{&mcp.TextContent{Text: text}},
			}, result, nil
		}

		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{Text: strings.TrimRight(stdout.String(), "\n")},
			},
		}, result, nil
	}
}
