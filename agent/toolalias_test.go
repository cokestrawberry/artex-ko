package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/guard"
	"github.com/Autumn-27/artex/intercept"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
	actool "github.com/Autumn-27/norma/tool"
)

func TestCamelToSnake(t *testing.T) {
	cases := map[string]string{
		"Bash":            "bash",
		"WebFetch":        "web_fetch",
		"TodoWrite":       "todo_write",
		"TaskList":        "task_list",
		"MultiEdit":       "multi_edit",
		"AskUserQuestion": "ask_user_question",
		"LS":              "ls",
		"sleep":           "sleep",
	}
	for in, want := range cases {
		if got := camelToSnake(in); got != want {
			t.Errorf("camelToSnake(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestToolAliasNames(t *testing.T) {
	bash := toolAliasNames("Bash")
	if !contains(bash, "bash") || !contains(bash, "BASH") || contains(bash, "Bash") {
		t.Errorf("Bash aliases = %v", bash)
	}
	wf := toolAliasNames("WebFetch")
	for _, want := range []string{"webfetch", "web_fetch", "WEBFETCH"} {
		if !contains(wf, want) {
			t.Errorf("WebFetch aliases %v missing %q", wf, want)
		}
	}
	if a := toolAliasNames("sleep"); len(a) != 0 {
		t.Errorf("lowercase name should have no aliases, got %v", a)
	}
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// TestAddToolAliasesDelegates proves an alias is dispatchable by its drifted name
// and that Call/Description/Schema delegate to the wrapped handler.
func TestAddToolAliasesDelegates(t *testing.T) {
	var called bool
	orig := actool.Build(actool.Spec{
		Name:        "Bash",
		Description: "run a shell command",
		Run: func(_ context.Context, _ json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			called = true
			return actool.Text("ok"), nil
		},
	})
	out, deferred, aliases := addToolAliases([]actool.CoreTool{orig}, nil, nil)
	if aliases["bash"] != "Bash" || aliases["BASH"] != "Bash" {
		t.Errorf("alias map = %v, want bash/BASH → Bash", aliases)
	}
	byName := map[string]actool.CoreTool{}
	for _, tl := range out {
		byName[tl.Name()] = tl
	}
	if _, ok := byName["Bash"]; !ok {
		t.Fatal("original Bash dropped")
	}
	alias, ok := byName["bash"]
	if !ok {
		t.Fatalf("bash alias not registered; have %v", keys(byName))
	}
	if !contains(deferred, "bash") {
		t.Errorf("bash alias not deferred: %v", deferred)
	}
	if alias.Description() != "run a shell command" {
		t.Errorf("alias description = %q", alias.Description())
	}
	if _, err := alias.Call(context.Background(), json.RawMessage(`{}`), nil); err != nil || !called {
		t.Fatalf("alias Call did not reach handler (called=%v err=%v)", called, err)
	}
}

// TestAddToolAliasesSkipsTaken ensures an alias never shadows an existing tool.
func TestAddToolAliasesSkipsTaken(t *testing.T) {
	camel := actool.Build(actool.Spec{Name: "Bash", Run: func(context.Context, json.RawMessage, *actool.ToolContext) (actool.Result, error) {
		return actool.Text(""), nil
	}})
	lower := actool.Build(actool.Spec{Name: "bash", Run: func(context.Context, json.RawMessage, *actool.ToolContext) (actool.Result, error) {
		return actool.Text(""), nil
	}})
	out, _, _ := addToolAliases([]actool.CoreTool{camel, lower}, nil, nil)
	n := 0
	for _, tl := range out {
		if tl.Name() == "bash" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("bash registered %d times, want 1 (alias must not shadow)", n)
	}
}

// TestWithToolAliasesDefersAliases: aliases of opts.Tools and of agentcore-added
// tools are registered with schemas withheld, while the official names stay
// visible and WebFetch/TodoWrite are still left for agentcore to add.
func TestWithToolAliasesDefersAliases(t *testing.T) {
	opts := agentcore.Options{Tools: actool.DefaultTools(), EnableWebFetch: true, Todos: actool.NewTodoStore()}
	withToolAliases(context.Background(), &opts)

	byName := map[string]bool{}
	for _, tl := range opts.Tools {
		byName[tl.Name()] = true
	}
	deferredSet := map[string]bool{}
	for _, n := range opts.DeferredTools {
		deferredSet[n] = true
	}
	for _, n := range []string{"bash", "read", "ls", "web_fetch", "todo_write"} {
		if !byName[n] || !deferredSet[n] {
			t.Errorf("alias %q registered=%v deferred=%v", n, byName[n], deferredSet[n])
		}
	}
	if deferredSet["Bash"] || deferredSet["WebFetch"] || deferredSet["TodoWrite"] {
		t.Errorf("official names must NOT be deferred: %v", opts.DeferredTools)
	}
	if byName["WebFetch"] || byName["TodoWrite"] {
		t.Error("only aliases of agentcore-added tools belong in opts.Tools")
	}
}

func keys(m map[string]actool.CoreTool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// chatCallingTool runs one ChatAgent turn in which the model calls the tool name
// with input, so the call goes through the real chat assembly and hooks. It
// returns the tool_result activities.
func chatCallingTool(t *testing.T, g *guard.Guard, name string, in any) []db.Activity {
	t.Helper()
	oldPolicy, oldResolve, oldAugment := FindingTrafficBindingEnabled, ToolResolve, ToolAugment
	t.Cleanup(func() { FindingTrafficBindingEnabled, ToolResolve, ToolAugment = oldPolicy, oldResolve, oldAugment })
	FindingTrafficBindingEnabled, ToolResolve, ToolAugment = nil, nil, nil
	input, _ := json.Marshal(in)
	turn := 0
	provider := captureUsageProvider{stream: func(_ context.Context, yield func(llm.StreamEvent, error) bool) {
		turn++
		events := []llm.StreamEvent{{Type: llm.SETextDelta, Text: "done"}, {Type: llm.SEMessageDelta, StopReason: "end_turn"}}
		if turn == 1 {
			events = []llm.StreamEvent{
				{Type: llm.SEToolUseStart, ToolID: "alias-call", ToolName: name},
				{Type: llm.SEToolInputJSON, Text: string(input)}, {Type: llm.SEMessageDelta, StopReason: "tool_use"},
			}
		}
		for _, ev := range events {
			if !yield(ev, nil) {
				return
			}
		}
	}}
	c := NewChatAgent(provider, "test-model", t.TempDir(), nil, 0)
	c.SetGuard(g)
	var results []db.Activity
	if _, err := c.Chat(context.Background(), "alias-probe", "alias-probe", "run it", 2, 0, false, func(a db.Activity) {
		if a.Kind == "tool_result" {
			results = append(results, a)
		}
	}); err != nil {
		t.Fatal(err)
	}
	return results
}

// TestChatAliasReachesSessionTools: tools agentcore.NewSession adds on its own
// (TodoWrite, WebFetch, background-task tools) must answer to drifted names too.
func TestChatAliasReachesSessionTools(t *testing.T) {
	calls := map[string]any{
		"todo_write": map[string]any{"todos": []any{}},
		"web_fetch":  map[string]any{"url": "http://127.0.0.1:1/"},
		"webfetch":   map[string]any{"url": "http://127.0.0.1:1/"},
	}
	if !actool.BackgroundTasksDisabled() {
		calls["task_list"] = map[string]any{}
	}
	for name, in := range calls {
		t.Run(name, func(t *testing.T) {
			results := chatCallingTool(t, guard.New(), name, in)
			if len(results) != 1 || strings.Contains(results[0].Detail, "unknown tool") {
				t.Fatalf("%s was not dispatched: %+v", name, results)
			}
		})
	}
}

// TestChatAliasCallIsGuardedAsCanonical: a drifted bash/BASH call must reach the
// guard as Bash, so the audit keeps its command and the Observer counts its result.
func TestChatAliasCallIsGuardedAsCanonical(t *testing.T) {
	for _, name := range []string{"bash", "BASH"} {
		t.Run(name, func(t *testing.T) {
			g := guard.New()
			chatCallingTool(t, g, name, map[string]string{"command": "echo alias-probe"})
			if audit := g.Audit(); len(audit) != 1 || audit[0].Tool != "Bash" || audit[0].Command != "echo alias-probe" {
				t.Fatalf("audit = %+v, want one Bash entry with the command", audit)
			}
			if got := g.Attributions(); got["ok"] != 1 {
				t.Fatalf("attributions = %v, want ok=1", got)
			}
		})
	}
}

// TestChatAliasCallHitsInterceptRule: with Bash enabled for interception, a deny
// rule must block a drifted bash/BASH call before the command runs.
func TestChatAliasCallHitsInterceptRule(t *testing.T) {
	dsn, _, err := db.DSN()
	if err != nil {
		t.Skip("no test database configured")
	}
	d, err := db.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	rule, err := d.CreateInterceptRule("alias-probe deny", "tool_input", "string", "alias-probe-deny", "deny", "alias probe", 1000, true, false, 60, "deny")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = d.Exec(`DELETE FROM intercept_pending WHERE rule_id=$1`, rule.ID)
		_ = d.DeleteInterceptRule(rule.ID)
	})
	ic := intercept.New(d)
	priorTools, err := ic.GetEnabledTools()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ic.SetEnabledTools(priorTools) })
	if err := ic.SetEnabledTools([]string{"Bash"}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"bash", "BASH"} {
		t.Run(name, func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "ran")
			g := guard.NewWithInterceptor(ic)
			chatCallingTool(t, g, name, map[string]string{"command": "touch " + marker + " # alias-probe-deny"})
			if _, err := os.Stat(marker); err == nil {
				t.Fatal("aliased Bash call ran despite the deny rule on Bash")
			}
			audit := g.Audit()
			if len(audit) == 0 || audit[len(audit)-1].Tool != "Bash" || audit[len(audit)-1].Action != "block" {
				t.Fatalf("audit = %+v, want a Bash block", audit)
			}
		})
	}
}
