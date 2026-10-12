# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- Conversation → DAG planning: turn a natural-language request into a directed acyclic graph (DAG) of sub-tasks, with `depends_on` expressing dependencies.
- Parallel sub-task scheduling: independent sub-tasks run in parallel, and upstream node outputs are piped into downstream nodes.
- Three task types with independent lifecycles: conversation / workflow / workflow sub-task.
- Multiple workflows per conversation: append nodes to an existing DAG or start a new one, with switching, archived failed branches, and user-controlled `force_new_workflow`.
- Context engineering: rolling compaction with five-block input accounting (system prompt / tools / workflow summary / compacted history / raw detail), plus a `recall_context` tool to retrieve compacted history on demand.
- MCP tool protocol: integrate external tools over stdio / SSE / streamable_http transports.
- Built-in skill system: first-party skills (docx / pptx / shell) and bundled third-party skills (pdfkit-py, minimax-xlsx), with prompt-type and command-type skills.
- Built-in native tools sandboxed to the workspace: write_file / read_file / list_dir / create_dir / file_exists / run_command / get_current_time / deep_search / recall_context (plus conditional analyze_image for multimodal models).
- Multi-provider LLM abstraction: OpenAI-compatible / Gemini / Azure OpenAI / Claude, with per-conversation model resolution.
- React frontend: chat, DAG visualization, debug page (turn-level traces, context blocks, token tree), and model / MCP / skill management.
- Three-process architecture: agent-server (HTTP API), agent-scheduler (stage scheduler), agent-supervisor (process guard with an escape-hatch restart endpoint).
- SQLite (zero-dependency) and MySQL backends, with hot database switching.
- Observability: per-turn execution trace, tool-call records (input / output / latency), token usage with input/output split, context-block accounting, and built-in pprof endpoints (block / mutex profiling).

### Fixed

- Tool results within a single assistant turn are now replayed in declaration order — concurrent execution used to assign `turn_seq` by completion order, which could misalign tool arguments (e.g. `read_file` paths) with their outputs and drive models into a dead loop.
- The functional_call circuit breakers (dead-loop detection, consecutive failures, consecutive low-value results, round/budget exhaustion) now mark the task as failed instead of completed, so workflow status and the final summary correctly surface the failure.
- Stopping a conversation task that has delegated to a workflow now forwards the stop to the bound workflow task (previously the workflow kept running and re-synced the task back to running).
- Streaming LLM calls now carry an idle timeout, so a gateway that never sends the `[DONE]` marker no longer hangs the task until the 15-minute watchdog fires.
