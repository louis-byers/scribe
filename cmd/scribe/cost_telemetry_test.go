package main

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// A synthetic composite envelope. The field names and shapes match real
// `claude -p --output-format json` results (one JSON line per run;
// permission_denials[].tool_input is an object; modelUsage is keyed by model
// id, and a run that used subagents lists their model too), but the values
// are made up and no real KB paths appear.
const telemetryEnvelope = `{"type":"result","subtype":"success","is_error":false,` +
	`"num_turns":45,"terminal_reason":"completed","result":"done",` +
	`"usage":{"input_tokens":12,"output_tokens":3400,"cache_read_input_tokens":900},` +
	`"total_cost_usd":0.46,` +
	`"modelUsage":{"claude-sonnet-5-5":{"inputTokens":12,"outputTokens":3400,"canonicalModel":"claude-sonnet-5-5"},` +
	`"claude-haiku-4-5":{"inputTokens":3,"outputTokens":40}},` +
	`"permission_denials":[` +
	`{"tool_name":"Bash","tool_use_id":"t1","tool_input":{"command":"cd /kb && ls; for f in wiki/*.md; do wc -l \"$f\"; done"}},` +
	`{"tool_name":"Bash","tool_use_id":"t2","tool_input":{"command":"python3 -c 'print(1)'"}},` +
	`{"tool_name":"Task","tool_use_id":"t3","tool_input":{"prompt":"sweep the KB"}},` +
	`{"tool_name":"Bash","tool_use_id":"t4","tool_input":{"command":"scribe lint"}}],` +
	`"subagent_stats":{"spawned":4,"completed":1,"failed":3}}`

func TestWithEnvelope_FillsTokensAndTelemetry(t *testing.T) {
	env, ok := parseClaudeResult(telemetryEnvelope)
	if !ok {
		t.Fatal("fixture envelope should parse")
	}
	base := CostEntry{Model: "sonnet", Op: "dream"}

	got := base.withEnvelope(env)

	if got.Model != "sonnet" || got.Op != "dream" {
		t.Errorf("identity fields changed: %+v", got)
	}
	if got.InputTokens == nil || *got.InputTokens != 12 || got.OutputTokens == nil || *got.OutputTokens != 3400 {
		t.Errorf("tokens not filled: in=%v out=%v", got.InputTokens, got.OutputTokens)
	}
	if got.CacheReadTokens == nil || *got.CacheReadTokens != 900 || got.CostUSD == nil || *got.CostUSD != 0.46 {
		t.Errorf("cache/cost not filled: cache=%v cost=%v", got.CacheReadTokens, got.CostUSD)
	}
	if want := []string{"claude-haiku-4-5", "claude-sonnet-5-5"}; !slices.Equal(got.ResolvedModels, want) {
		t.Errorf("ResolvedModels = %v, want %v (sorted)", got.ResolvedModels, want)
	}
	if got.NumTurns == nil || *got.NumTurns != 45 {
		t.Errorf("NumTurns = %v, want 45", got.NumTurns)
	}
	if got.TerminalReason != "completed" {
		t.Errorf("TerminalReason = %q", got.TerminalReason)
	}
	if got.Denials != 4 {
		t.Errorf("Denials = %d, want 4", got.Denials)
	}
	if got.DeniedTools["Bash"] != 3 || got.DeniedTools["Task"] != 1 {
		t.Errorf("DeniedTools = %v, want Bash:3 Task:1", got.DeniedTools)
	}
	if len(got.DeniedSamples) != maxDeniedSamples {
		t.Fatalf("DeniedSamples has %d entries, want cap %d: %v", len(got.DeniedSamples), maxDeniedSamples, got.DeniedSamples)
	}
	// The leading `cd <path> && ` is stripped: it carries no signal about
	// what was refused and is where most of the absolute path lives.
	if !strings.HasPrefix(got.DeniedSamples[0], "Bash: ls; for f in") {
		t.Errorf("first sample = %q, want the Bash command without its cd prefix", got.DeniedSamples[0])
	}
	if got.DeniedSamples[2] != "Task" {
		t.Errorf("non-Bash sample = %q, want the bare tool name", got.DeniedSamples[2])
	}
	if got.Subagents == nil || *got.Subagents != (SubagentCounts{Spawned: 4, Completed: 1, Failed: 3}) {
		t.Errorf("Subagents = %+v", got.Subagents)
	}
}

func TestWithEnvelope_DoesNotMutateReceiver(t *testing.T) {
	env, _ := parseClaudeResult(telemetryEnvelope)
	base := CostEntry{Model: "sonnet"}
	_ = base.withEnvelope(env)
	if base.InputTokens != nil || base.ResolvedModels != nil || base.Denials != 0 {
		t.Errorf("receiver was mutated: %+v", base)
	}
}

// A legacy envelope (no telemetry fields) must leave every new field at its
// zero value, so the ledger row serializes exactly as before.
func TestWithEnvelope_LegacyEnvelopeAddsNoKeys(t *testing.T) {
	env, ok := parseClaudeResult(`{"type":"result","subtype":"success","is_error":false,"result":"ok","usage":{"input_tokens":10,"output_tokens":20}}`)
	if !ok {
		t.Fatal("legacy envelope should parse")
	}
	got := CostEntry{Model: "haiku"}.withEnvelope(env)
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"resolved_models", "num_turns", "terminal_reason", "denials", "denied_tools", "denied_samples", "subagents"} {
		if strings.Contains(string(raw), `"`+key+`"`) {
			t.Errorf("legacy row gained key %q: %s", key, raw)
		}
	}
}

func TestDenialSample_TruncatesLongCommands(t *testing.T) {
	long := strings.Repeat("x", 500)
	d := claudeDenial{ToolName: "Bash", ToolInput: json.RawMessage(`{"command":"` + long + `"}`)}
	got := denialSample(d)
	if !strings.HasPrefix(got, "Bash: ") {
		t.Fatalf("sample = %q", got)
	}
	if body := strings.TrimPrefix(got, "Bash: "); len(body) > deniedSampleMaxBytes {
		t.Errorf("command part is %d bytes, want <= %d", len(body), deniedSampleMaxBytes)
	}
}

func TestDenialSample_UnparseableInputFallsBackToToolName(t *testing.T) {
	d := claudeDenial{ToolName: "Bash", ToolInput: json.RawMessage(`"not an object"`)}
	if got := denialSample(d); got != "Bash" {
		t.Errorf("sample = %q, want bare tool name", got)
	}
}

// Only Bash commands are previewed. A non-Bash tool (including an MCP tool)
// whose input happens to have a "command" key stores its name only.
func TestDenialSample_NonBashNeverStoresInput(t *testing.T) {
	d := claudeDenial{ToolName: "mcp__shell__run", ToolInput: json.RawMessage(`{"command":"cat ~/.secrets"}`)}
	if got := denialSample(d); got != "mcp__shell__run" {
		t.Errorf("sample = %q, want bare tool name", got)
	}
}

func TestDenialSample_RedactsURLTokens(t *testing.T) {
	d := claudeDenial{ToolName: "Bash", ToolInput: json.RawMessage(`{"command":"curl https://api.example.com/x?token=s3cr3t"}`)}
	if got := denialSample(d); strings.Contains(got, "s3cr3t") {
		t.Errorf("sample leaked a URL token: %q", got)
	}
}
