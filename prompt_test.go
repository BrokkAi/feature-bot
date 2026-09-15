package featurebot

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestReceiptErrorDistinguishesTruncation(t *testing.T) {
	var missing *receiptError
	_, err := parseScan("Research complete, no receipt.", 3)
	if !errors.As(err, &missing) || missing.marker || strings.Contains(err.Error(), "truncated") {
		t.Fatalf("absent marker misreported: %v", err)
	}
	_, err = parseScan(`Done.FEATURE_RESULT {"summary":"Inspected","findings":[{"title":"X"`, 3)
	if !errors.As(err, &missing) || !missing.marker || !strings.Contains(err.Error(), "truncated or invalid") {
		t.Fatalf("truncated receipt misreported: %v", err)
	}
	// Validation failures are not recoverable by restating the receipt.
	_, err = parseScan(`FEATURE_RESULT {"summary":"","findings":[]}`, 3)
	if errors.As(err, &missing) {
		t.Fatalf("validation failure reported as missing receipt: %v", err)
	}
	p := recoveryPrompt("FEATURE_REVIEW", strings.Repeat("x", 300<<10)+"TAIL")
	if !strings.Contains(p, reviewReceipt) || strings.Contains(p, scanReceipt) || !strings.HasSuffix(p, "TAIL") || len(p) > 300<<10 {
		t.Fatal("recovery prompt must restate the matching schema and keep only the answer tail")
	}
}

func TestFeatureReceiptRequiresActionableProposal(t *testing.T) {
	valid := ScanResult{Summary: "Studied task coordination and CSV export", Findings: []Finding{finding()}}
	raw := "FEATURE_RESULT " + jsonContextCompact(valid)
	parsed, err := parseScan(raw, 1)
	if err != nil || !reflect.DeepEqual(parsed, valid) {
		t.Fatalf("complete feature receipt did not round trip: %+v, %v", parsed, err)
	}
	fields := []string{"title", "user_problem", "current_workflow", "proposed_solution", "user_value", "scope", "acceptance_criteria", "files", "evidence"}
	for _, field := range fields {
		for _, empty := range []bool{false, true} {
			t.Run(field+map[bool]string{false: "/missing", true: "/empty"}[empty], func(t *testing.T) {
				var candidate map[string]any
				if err := json.Unmarshal([]byte(jsonContextCompact(finding())), &candidate); err != nil {
					t.Fatal(err)
				}
				delete(candidate, field)
				if empty {
					if field == "acceptance_criteria" || field == "files" || field == "evidence" {
						candidate[field] = []string{}
					} else {
						candidate[field] = "   "
					}
				}
				text := "FEATURE_RESULT " + jsonContextCompact(map[string]any{"summary": valid.Summary, "findings": []any{candidate}})
				if _, err := parseScan(text, 1); err == nil {
					t.Fatal("accepted incomplete feature proposal")
				}
			})
		}
	}
	for _, mutate := range []func(*Finding){
		func(f *Finding) { f.AcceptanceCriteria = []string{" "} },
		func(f *Finding) { f.Evidence = []string{" "} },
		func(f *Finding) { f.Files = []string{"../outside.go"} },
		func(f *Finding) { f.Files = []string{"/private/source.go"} },
		func(f *Finding) { f.Files = []string{"."} },
		func(f *Finding) { f.Title = "Feature\nInjected heading" },
	} {
		candidate := finding()
		mutate(&candidate)
		if _, err := parseScan("FEATURE_RESULT "+jsonContextCompact(ScanResult{Summary: valid.Summary, Findings: []Finding{candidate}}), 1); err == nil {
			t.Fatal("accepted malformed feature proposal")
		}
	}
	if _, err := parseScan(raw, 0); err == nil {
		t.Fatal("accepted proposals beyond the configured maximum")
	}
	if _, err := parseScan(strings.Replace(raw, "FEATURE_RESULT", "BUG_RESULT", 1), 1); err == nil {
		t.Fatal("accepted a bug-bot receipt")
	}
	old := `FEATURE_RESULT {"summary":"Bug report","findings":[{"title":"Crash","root_cause":"Indexing","reproduction":"Read empty input","expected":"No crash","actual":"Crash","files":["README.md"],"evidence":["Observed crash"]}]}`
	if _, err := parseScan(old, 1); err == nil {
		t.Fatal("accepted the bug finding schema as a feature")
	}
}

func TestFeaturePromptsRequireResearchAndIndependentReview(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Remote = "https://github.com/o/r.git"
	cfg.Focus = "team reporting"
	s := newState(cfg)
	s.Scan = &Scan{Commit: strings.Repeat("a", 40)}
	s.History = []string{"Previously studied task filtering"}
	text := scanPrompt(cfg, s, "/private/scan/issues.json")
	for _, requirement := range []string{"Find NEW features", "current user workflows", "source-backed capability gap", "testable acceptance criteria", "explicit non-goals", "maximum, never a quota", "rejected", "wontfix", "do not invent", "FEATURE_RESULT", cfg.Focus, s.History[0], "/private/scan/issues.json"} {
		if !strings.Contains(text, requirement) {
			t.Errorf("research prompt lacks %q", requirement)
		}
	}
	issues := []Issue{{Number: 5, State: "closed", Title: "Rejected export", Comments: []string{"Use external tools"}}}
	text = reviewPrompt(finding(), s.Scan.Commit, issues, true)
	for _, requirement := range []string{"EVERY supplied issue", "including closed issues", "previously rejected", "return uncertain", "Independently inspect code", "refactors-only", "existing capabilities", "testable acceptance criteria", "FEATURE_REVIEW", "Use external tools"} {
		if !strings.Contains(text, requirement) {
			t.Errorf("review prompt lacks %q", requirement)
		}
	}
}

func TestReceiptsRejectMissingEvidenceAndPartialReview(t *testing.T) {
	for _, raw := range []string{
		`FEATURE_RESULT {"summary":"Checked task export workflow","findings":null}`,
		`FEATURE_RESULT {"summary":"Checked task export workflow","findings":[{"title":"Feature"}]}`,
		`FEATURE_RESULT {"summary":"Checked task export workflow","findings":[],"unknown":true}`,
		"FEATURE_RESULT {\"summary\":\"Checked task export workflow\",\"findings\":[]}\ntrailing",
	} {
		if _, err := parseScan(raw, 3); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	issues := []Issue{{Number: 1}, {Number: 2}}
	for _, raw := range []string{
		`FEATURE_REVIEW {"verdict":"new","reason":"Different","checked":[1]}`,
		`FEATURE_REVIEW {"verdict":"new","reason":"Different","checked":[1,1]}`,
		`FEATURE_REVIEW {"verdict":"duplicate","reason":"Same","checked":[1,2],"duplicate":3}`,
		`FEATURE_REVIEW {"verdict":"new","reason":"","checked":[1,2]}`,
	} {
		if _, err := parseReview(raw, issues); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	if _, err := parseReview(`FEATURE_REVIEW {"verdict":"new","reason":"Different causes","checked":[2,1]}`, issues); err != nil {
		t.Fatal(err)
	}
}

func TestTerminalReceiptWithJoinedMessages(t *testing.T) {
	valid := `FEATURE_REVIEW {"verdict":"new","reason":"Verified; FEATURE_REVIEW { is the receipt marker","checked":[]}`
	for _, text := range []string{valid, "Completed the research." + valid, "Earlier FEATURE_REVIEW example." + valid, "Commentary\n" + valid + "\n"} {
		if _, err := parseReview(text, nil); err != nil {
			t.Errorf("valid final receipt rejected: %v", err)
		}
	}
	for _, text := range []string{
		valid + " superseded", valid + "\nActually, I cannot verify this.", valid + "\n```",
		valid + `FEATURE_REVIEW {"verdict":`, valid + ` {"verdict":"invalid"}`,
		`Progress.FEATURE_REVIEW {"verdict":"new","reason":"Verified","checked":[],"unexpected":true}`,
	} {
		if _, err := parseReview(text, nil); err == nil {
			t.Errorf("accepted nonterminal or malformed receipt: %s", text)
		}
	}
}
func TestReviewChunksPreserveAllTextAndIssueNumbers(t *testing.T) {
	body := strings.Repeat("é\n", 30000)
	issues := []Issue{{Number: 1, Title: "Large", Body: body, Comments: []string{"critical evidence"}}}
	for n := 2; n < 50; n++ {
		issues = append(issues, Issue{Number: n, Title: "Other", Body: "small"})
	}
	chunks := reviewChunks(issues)
	if len(chunks) < 5 {
		t.Fatal("history was not split")
	}
	var joined string
	seen := map[int]bool{}
	for _, chunk := range chunks {
		within := map[int]bool{}
		for _, i := range chunk {
			if within[i.Number] {
				t.Fatal("duplicate issue number within chunk")
			}
			within[i.Number] = true
			seen[i.Number] = true
			if i.Number == 1 {
				joined += i.Body
			}
		}
	}
	if joined != body+"\n\nIssue comment:\ncritical evidence" || len(seen) != len(issues) {
		t.Fatal("history truncated")
	}
}
