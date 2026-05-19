# Per-session agent command + Ollama support — design proposal

**Scope.** Make the agent command configurable globally and per-session
so operators can route different sessions to different agents (Claude
Code, an Ollama-backed local model, a wrapper script that does
something else entirely). Plus a path to native Ollama integration if
the wrapper-script approach proves insufficient.

This is **forward-looking** — no implementation today. Recording the
slices so a future round can pick one with the full picture.

## 1. Motivation

1. **Local models for cheap work.** Routine tasks (rename a function
   across a file, write a unit test from a template, summarize a diff)
   don't need Claude-Opus-quality reasoning. A locally-hosted Ollama
   model (Llama 3.1 8B, Qwen 2.5 Coder, etc.) can handle them with no
   API spend, no network round-trip, and no rate limits.
2. **Mixed-fleet rounds.** A 4-session round where session-1
   (architecture refactor) uses Claude Opus, session-2 (test
   boilerplate) uses a local model, session-3 (docs) uses a cheaper
   Claude Haiku, session-4 (research) uses Claude Sonnet. Today this
   requires four separate `bosun launch` invocations with different
   `--command` values; init can't express it.
3. **Operator-owned agent choice.** Some operators have license
   constraints, latency requirements, or just preferences. The
   abstraction should be "bosun runs a command in a worktree"; what
   that command IS should be operator policy.
4. **Wrapper-script playground.** Once `agent_command` is a first-class
   knob, operators can wrap it with anything: a Docker-launching script
   ([sandbox-launcher-design.md](sandbox-launcher-design.md) Phase 1),
   an SSH-to-remote script, a model-routing script that picks between
   Ollama and Claude based on the brief content, etc.

Non-goals:

- Bosun deciding which model is best for which task. That's a product
  question and a research project. Operator policy stays operator
  policy.
- Built-in prompt templating for non-Claude agents. Out of scope for
  the cheap slice; revisit if native Ollama lands.

## 2. Current state

- [launcher.Options.Command](../internal/launcher/launcher.go) is already
  a string field, defaults to `"claude"`.
- [`bosun launch --command <cmd>`](../cmd/bosun/cmd_launch.go) exposes it.
- [`bosun init`](../cmd/bosun/cmd_init.go) hardcodes `Command: "claude"`
  at launch-spawn time (no flag, no config setting).
- No config field for the default command.
- No way to vary the command per session within a single `init`
  invocation.
- No way to plumb model-specific config (system prompt, temperature,
  etc.) — `Command` is just the binary name + args.

## 3. Candidate slices

### A. Minimum useful: per-session `--command` flag on `init`

- **Shape.** `bosun init --brief plan.md --command claude` (applies to
  all sessions) plus the brief's `(command: ollama-llama.sh)` clause
  for per-session overrides:
  ```
  ## session-1 (depends: session-2, command: ollama-coder.sh)
  body
  ```
- **Config field.** `config.AgentCommand` (default `"claude"`), reads
  from `.bosun/config.json` so operators don't retype `--command` per
  invocation.
- **Brief parser.** Extend the existing `(depends: ...)` clause shape
  to a multi-key form: `(depends: x, y, command: z)`. Single regex
  update in
  [brief.go](../internal/brief/brief.go#L70).
- **Lifecycle.** The chosen command is captured at init time and
  persisted in session state so cleanup/show/launch use the right
  command on resume. New field: `session.Session.AgentCommand`.
- **Resource cost.** ~50-100 lines spread across `internal/config`,
  `internal/brief`, `internal/session`, `cmd/bosun/cmd_init.go`. Test
  surface: brief parser unit tests (new clause), session state
  round-trip, scenario test that verifies a different command actually
  runs.

Pros: cheap, generalizes for Ollama AND Docker AND SSH AND anything
else. Operator owns the wrapper script. Bosun doesn't need to know
about Ollama specifically.

Cons: operator has to write the wrapper script. No prompt-template
help — if Ollama needs different prompting than Claude, the operator
has to handle that in their script.

### B. Medium: native `model` field + provider router

- **Shape.** Brief heading clause becomes `(model: ollama/llama3.1:8b)`
  or `(model: claude/opus-4.7)`. Bosun parses provider/model, picks the
  right command + env from a built-in registry.
- **Config field.** `config.AgentModels` map: `claude/opus-4.7` →
  `{command: "claude", env: {ANTHROPIC_MODEL: "opus-4.7"}}`,
  `ollama/llama3.1:8b` → `{command: "ollama-claude-wrapper.sh", env:
  {OLLAMA_MODEL: "llama3.1:8b"}}`.
- **Operator extensibility.** Custom entries land in
  `.bosun/config.json` and merge into the built-in registry.
- **Resource cost.** ~200-300 lines. New `agentmodel` package
  (or maybe `agent` — naming bikeshed), updates to brief parser, config
  schema migration.

Pros: cleaner brief syntax (`model: ollama/llama3.1:8b` vs
`command: ./my-ollama-wrapper.sh`). Discoverability (`bosun list-models`
becomes a real command).

Cons: bosun owns a registry it has to maintain. Adding a new model
provider becomes a bosun change instead of an operator-script change.
Smells like premature abstraction.

### C. Native Ollama integration with prompt templating

- **Shape.** Bosun ships a built-in Ollama agent that handles the
  protocol differences: Claude tool-use format vs Ollama's
  `tools` parameter format, system-prompt injection, context-window
  management, etc.
- **Reference implementation.** A Go package `internal/agentollama`
  with an `Agent` interface bosun owns. The Claude path becomes one
  implementation; Ollama is another.
- **Resource cost.** Easily 1000+ lines. Lots of prompt engineering.
  Maintenance burden on every Ollama model upgrade. Test fixtures for
  the prompt-template rendering. Real Ollama dependency for integration
  tests.

Pros: works out of the box for Ollama users with no wrapper script.
Mixed-fleet rounds become trivially expressible.

Cons: bosun is now a multi-agent runtime. Each new agent (Gemini,
GPT-4, local Llama) becomes a maintenance burden. Drift from upstream
Ollama API would require ongoing work.

## 4. Cross-cutting concerns

1. **MCP server compatibility.** Today's `bosun_*` MCP tools assume
   the agent supports MCP. Claude Code does. Ollama generally doesn't
   (varies by model + serving framework). For slice A, the operator's
   wrapper script can ignore MCP entirely or implement a stub. For
   slices B/C, bosun has to know which agents do/don't speak MCP and
   downgrade gracefully (CLI fallback for `bosun_claim`, etc.).

2. **`BOSUN_BRIEF.md` interpretation.** Today the workflow preamble in
   [brief.WorkflowPreamble](../internal/brief/brief.go) assumes Claude
   Code's behavior: it reads `BOSUN_BRIEF.md`, runs shell commands,
   commits, marks done. Non-Claude agents may need different
   onboarding text. Slice A: operator-written wrapper handles it.
   Slices B/C: bosun has to template per-agent.

3. **Tool-use protocol mismatch.** Claude Code's `Bash`, `Edit`, `Read`
   tools are Claude API tool-use. Ollama models that support tools
   (with frameworks like LiteLLM, Ollama's native `tools` param) use
   different schemas. Slice A punts; slices B/C have to translate.

4. **Resource detection.** The launcher's pre-flight load check
   (`preflight.CheckLoad`) assumes the agent runs on the host. An
   Ollama agent calling a remote Ollama server doesn't strain local
   CPU; a local Ollama agent definitely does (model inference IS the
   CPU bottleneck). Should the load check know which agent is
   running?

5. **Per-session `RunningPID`.** Today
   [proc.Running](../internal/proc/detect.go) matches `claude` /
   `claude-code` / `code-cli` basenames. A wrapper script changes
   the basename to something arbitrary. `IsAgent()` needs a
   config-driven allowlist (`config.AgentProcessNames []string`)
   so detection still works.

## 5. Recommended phasing

1. **Phase 1: Slice A** (per-session command + config default). Land
   this first. It unlocks all three cases (Ollama, Docker, SSH) via
   operator wrapper scripts. Most of the value at lowest cost.

2. **Phase 2: revisit.** If wrapper-script friction proves real
   (operators repeatedly hit the same bugs in their Ollama wrappers,
   tool-use translation becomes a community concern, etc.), consider
   Slice B's named-model registry.

3. **Phase 3: Slice C** lands only if there's product evidence that
   native Ollama is worth the maintenance burden. Default position:
   don't.

## 6. Decision points to settle before Phase 1

- **Brief-clause syntax.** `(command: x)` vs `(agent: x)` vs `(cmd: x)`.
  Pick one before parser work starts. Recommend `command:` for
  symmetry with `--command`.
- **Override precedence.** Operator-passed `--command` on the init CLI
  vs per-session brief clause vs config default. Recommend: brief
  clause wins (most specific), then CLI flag, then config default.
- **Persistence.** Where does the chosen command live for resume?
  `.bosun/state/<label>.json` is the natural home — already persists
  per-session state. Add `agent_command` field.
- **`bosun launch` parity.** Should `bosun launch <session> --command`
  also persist the change? Or is launch a one-shot that doesn't update
  state? Recommend one-shot — `launch` already documents itself as a
  spawn helper, not a state mutation.
- **`IsAgent()` allowlist generalization.** When `command` isn't
  `claude`, how does `proc.Running` find the agent? Recommend: derive
  the expected basename from the command at session-creation time and
  persist it alongside `agent_command`.

## 7. Out of scope for Phase 1

- Native prompt templating for non-Claude agents (Phase 3 territory).
- Tool-use protocol translation (Phase 3).
- Bosun-owned model registry / `bosun list-models` (Phase 2).
- GPU passthrough for containerized local models (see
  [sandbox-launcher-design.md](sandbox-launcher-design.md) section 6).
- Cross-session model selection by classifier ("hard task → Claude,
  easy task → local"). That's a research project, not v-next.

---

When Phase 1 starts, this doc gets pruned to "what shipped" + a
roadmap entry for any deferred phase.
