package review

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadRegistryLegacyFallback(t *testing.T) {
	r, err := LoadRegistry("", []string{"tsetso-review", "dimoreview"}, "/casino-review")
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Reviews) != 2 || r.Reviews[0].Engine != "dispatch" || r.Reviews[1].Name != "dimoreview" {
		t.Fatalf("unexpected legacy registry: %+v", r.Reviews)
	}
}

func TestLoadRegistryFile(t *testing.T) {
	dir := t.TempDir()
	persona := filepath.Join(dir, "p.md")
	os.WriteFile(persona, []byte("be the law"), 0o644)
	path := filepath.Join(dir, "reviews.json")
	os.WriteFile(path, []byte(`{
	  "reviews": [
	    {"name":"tsetso-review","engine":"dispatch"},
	    {"name":"barbie-review","engine":"dispatch"},
	    {"name":"eslint","engine":"analyzer","cmd":["npx","eslint",".","--format","json"],"parser":"eslint"}
	  ],
	  "judges": [{"name":"judge-dread","prompt_file":"`+persona+`"}]
	}`), 0o644)

	r, err := LoadRegistry(path, nil, "/casino-review")
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Names(); strings.Join(got, ",") != "tsetso-review,barbie-review,eslint" {
		t.Fatalf("names = %v", got)
	}
	if len(r.Judges) != 1 {
		t.Fatalf("judges = %+v", r.Judges)
	}
}

// LLM reviewers are sunset: the registry must refuse claude engines with a
// message pointing at the foundations, until the gate is deliberately removed.
func TestLoadRegistrySunsetsClaude(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "r.json")
	os.WriteFile(p, []byte(`{"reviews":[{"name":"paranoid","engine":"claude","prompt_file":"x.md"}]}`), 0o644)
	_, err := LoadRegistry(p, nil, "/casino-review")
	if err == nil || !strings.Contains(err.Error(), "sunset") {
		t.Fatalf("expected sunset error, got %v", err)
	}
}

func TestLoadRegistryRejectsBadSpecs(t *testing.T) {
	dir := t.TempDir()
	for name, content := range map[string]string{
		"no-reviews":   `{"reviews":[]}`,
		"dup-names":    `{"reviews":[{"name":"a","engine":"dispatch"},{"name":"a","engine":"dispatch"}]}`,
		"bad-engine":   `{"reviews":[{"name":"a","engine":"quantum"}]}`,
		"claude-no-pf": `{"reviews":[{"name":"a","engine":"claude"}]}`,
		"analyzer-cmd": `{"reviews":[{"name":"a","engine":"analyzer"}]}`,
		"bad-parser":   `{"reviews":[{"name":"a","engine":"analyzer","cmd":["x"],"parser":"pylint"}]}`,
		"bad-timeout":  `{"reviews":[{"name":"a","engine":"dispatch","timeout":"soon"}]}`,
	} {
		p := filepath.Join(dir, name+".json")
		os.WriteFile(p, []byte(content), 0o644)
		if _, err := LoadRegistry(p, nil, "/casino-review"); err == nil {
			t.Errorf("%s: expected error, got none", name)
		}
	}
}

func TestLoadRegistryAddon(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.json")
	os.WriteFile(good, []byte(`{
	  "reviews":[{"name":"a","engine":"dispatch"}],
	  "addon":{"name":"static","chance":0.25,"analyzers":[
	    {"cmd":["npx","eslint",".","--format","json"],"parser":"eslint"},
	    {"cmd":["npx","tsc","--noEmit"],"parser":"tsc","timeout":"2m"}]}
	}`), 0o644)
	r, err := LoadRegistry(good, nil, "/casino-review")
	if err != nil {
		t.Fatal(err)
	}
	if r.Addon == nil || r.Addon.Chance != 0.25 || len(r.Addon.Analyzers) != 2 {
		t.Fatalf("addon = %+v", r.Addon)
	}

	for name, content := range map[string]string{
		"bad-chance":   `{"reviews":[{"name":"a","engine":"dispatch"}],"addon":{"name":"s","chance":1.5,"analyzers":[{"cmd":["x"]}]}}`,
		"no-steps":     `{"reviews":[{"name":"a","engine":"dispatch"}],"addon":{"name":"s","chance":0.5,"analyzers":[]}}`,
		"dup-name":     `{"reviews":[{"name":"s","engine":"dispatch"}],"addon":{"name":"s","chance":0.5,"analyzers":[{"cmd":["x"]}]}}`,
		"trigger-name": `{"reviews":[{"name":"a","engine":"dispatch"}],"addon":{"name":"casino-review","chance":0.5,"analyzers":[{"cmd":["x"]}]}}`,
	} {
		p := filepath.Join(dir, name+".json")
		os.WriteFile(p, []byte(content), 0o644)
		if _, err := LoadRegistry(p, nil, "/casino-review"); err == nil {
			t.Errorf("%s: expected error, got none", name)
		}
	}
}

// A dispatch engine named after the bot's own trigger would post a comment
// that re-triggers the bot forever — the registry must refuse it, on both the
// file path and the legacy REVIEWS fallback.
func TestLoadRegistryRejectsTriggerCollision(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "r.json")
	os.WriteFile(p, []byte(`{"reviews":[{"name":"casino-review","engine":"dispatch"}]}`), 0o644)
	if _, err := LoadRegistry(p, nil, "/casino-review"); err == nil {
		t.Fatal("file registry: expected trigger-collision error")
	}
	if _, err := LoadRegistry("", []string{"casino-review"}, "/casino-review"); err == nil {
		t.Fatal("legacy registry: expected trigger-collision error")
	}
}

// tsc exiting non-zero with no parseable diagnostics is a broken analyzer
// (bad tsconfig, crash) and must be an error — not a "clean, 0 findings" pass.
func TestParseAnalyzerTSCFailure(t *testing.T) {
	if _, err := parseAnalyzerOutput("tsc", []byte("error TS5083: Cannot read file 'tsconfig.json'"), nil, errExit(1)); err == nil {
		t.Fatal("expected error for tsc failure with no diagnostics")
	}
	// ...but non-zero WITH diagnostics is a normal findings run.
	out := []byte("src/a.ts(1,1): error TS2304: Cannot find name 'x'.")
	findings, err := parseAnalyzerOutput("tsc", out, nil, errExit(2))
	if err != nil || len(findings) != 1 {
		t.Fatalf("findings=%v err=%v", findings, err)
	}
}

func TestParseClaudeOutput(t *testing.T) {
	out := []byte(`{"type":"result","subtype":"success","is_error":false,"total_cost_usd":0.42,
	  "result":"Here you go:\n{\"findings\":[{\"path\":\"src/a.ts\",\"line\":3,\"severity\":\"high\",\"title\":\"bug\"}],\"summary\":\"one bug\"}"}`)
	report, cost, err := parseClaudeOutput(out)
	if err != nil {
		t.Fatal(err)
	}
	if cost != 0.42 || len(report.Findings) != 1 || report.Findings[0].Path != "src/a.ts" || report.Summary != "one bug" {
		t.Fatalf("report=%+v cost=%v", report, cost)
	}
}

func TestParseClaudeOutputErrors(t *testing.T) {
	for name, out := range map[string]string{
		"not-json":     `plain text`,
		"error-result": `{"type":"result","is_error":true,"result":"limit reached"}`,
		"no-report":    `{"type":"result","is_error":false,"result":"I could not produce JSON, sorry"}`,
		"bad-report":   `{"type":"result","is_error":false,"result":"{\"findings\": 42}"}`,
	} {
		if _, _, err := parseClaudeOutput([]byte(out)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestParseESLint(t *testing.T) {
	out := []byte(`[
	  {"filePath":"/work/o__r/src/a.ts","messages":[
	    {"ruleId":"no-unused-vars","severity":2,"message":"x is unused","line":10},
	    {"ruleId":"eqeqeq","severity":1,"message":"expected ===","line":20}]},
	  {"filePath":"/work/o__r/src/b.ts","messages":[]}
	]`)
	// eslint exits 1 when errors exist — must still parse as findings.
	findings, err := parseESLint(out, errExit(1))
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 2 {
		t.Fatalf("findings = %+v", findings)
	}
	if findings[0].Severity != "medium" || findings[0].Path != "src/a.ts" || findings[0].Line != 10 {
		t.Fatalf("first finding = %+v", findings[0])
	}
	if findings[1].Severity != "low" || !strings.Contains(findings[1].Title, "eqeqeq") {
		t.Fatalf("second finding = %+v", findings[1])
	}
}

func TestParseESLintRealFailure(t *testing.T) {
	if _, err := parseESLint([]byte("Oops, config not found"), errExit(2)); err == nil {
		t.Fatal("expected error for non-JSON output with non-zero exit")
	}
}

func TestParseTSC(t *testing.T) {
	out := []byte(`src/a.ts(12,5): error TS2304: Cannot find name 'foo'.
src/b.ts(3,1): warning TS6133: 'x' is declared but never used.
some unrelated line`)
	findings := parseTSC(out)
	if len(findings) != 2 {
		t.Fatalf("findings = %+v", findings)
	}
	if findings[0].Path != "src/a.ts" || findings[0].Line != 12 || findings[0].Severity != "medium" {
		t.Fatalf("first = %+v", findings[0])
	}
	if findings[1].Severity != "low" {
		t.Fatalf("second = %+v", findings[1])
	}
}

func TestParseGeneric(t *testing.T) {
	if f := parseGeneric([]byte("all good"), nil, nil); f != nil {
		t.Fatalf("clean run should have no findings: %+v", f)
	}
	f := parseGeneric([]byte("boom"), []byte("stack"), errExit(3))
	if len(f) != 1 || !strings.Contains(f[0].Body, "boom") {
		t.Fatalf("failure run = %+v", f)
	}
}

type errExit int

func (e errExit) Error() string { return "exit status " + string(rune('0'+int(e))) }
