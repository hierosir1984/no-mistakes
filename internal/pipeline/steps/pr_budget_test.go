package steps

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/scm/plugin/fakeplugin"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func productionBudgetStep(t *testing.T, name types.StepName) pipeline.Step {
	t.Helper()
	for _, step := range AllSteps() {
		if step.Name() == name {
			return step
		}
	}
	t.Fatalf("missing step %s", name)
	return nil
}

func TestPRBudgetProductionStepsRefuseBeforeExpensiveWork(t *testing.T) {
	for _, name := range []types.StepName{types.StepIntent, types.StepReview, types.StepTest} {
		t.Run(string(name), func(t *testing.T) {
			sctx := newIntentStepContext(t)
			intent := strings.Repeat("large public intent ", 4000)
			sctx.Run.Intent = &intent
			_, err := productionBudgetStep(t, name).Execute(sctx)
			if err == nil || !strings.Contains(err.Error(), "largest sections: Intent=") || !strings.Contains(err.Error(), "63488") || !strings.Contains(err.Error(), "linked file") {
				t.Fatalf("did not refuse before needing an agent/git/test command: %v", err)
			}
		})
	}
}

func TestPRBudgetRechecksRecordedEvidenceBeforeReviewAndTest(t *testing.T) {
	for _, name := range []types.StepName{types.StepReview, types.StepTest} {
		t.Run(string(name), func(t *testing.T) {
			sctx := newIntentStepContext(t)
			intent := "A short intent"
			sctx.Run.Intent = &intent
			if _, err := productionBudgetStep(t, types.StepIntent).Execute(sctx); err != nil {
				t.Fatal(err)
			}
			insertCompletedStep(t, sctx, types.StepRebase, "", strings.Repeat("recorded failure evidence ", 4000))
			_, err := productionBudgetStep(t, name).Execute(sctx)
			if err == nil || !strings.Contains(err.Error(), "largest sections: Pipeline=") {
				t.Fatalf("recorded evidence did not stop expensive work: %v", err)
			}
		})
	}
}

func TestPRBudgetUnderLimitStillRunsReviewAndTest(t *testing.T) {
	for _, name := range []types.StepName{types.StepReview, types.StepTest} {
		t.Run(string(name), func(t *testing.T) {
			dir, base, head := setupGitRepo(t)
			output := passingScenarioFindingsJSON
			if name == types.StepReview {
				output = `{"findings":[],"summary":"clean","reviewed_paths":["feature.txt"],"risk_level":"low","risk_rationale":"bounded","risk_scope":"source-or-external"}`
			}
			ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
				return &agent.Result{Output: json.RawMessage(output)}, nil
			}}
			sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
			outcome, err := productionBudgetStep(t, name).Execute(sctx)
			if err != nil || outcome == nil || outcome.NeedsApproval || len(ag.calls) == 0 {
				t.Fatalf("under-budget step did not run normally: %+v %v, agent calls=%d", outcome, err, len(ag.calls))
			}
		})
	}
}

func TestPRBudgetNearLimitOnlyWarnsAndOmittedIntentIsExcluded(t *testing.T) {
	sctx := newIntentStepContext(t)
	intent := strings.Repeat("public text ", 4700)
	sctx.Run.Intent = &intent
	var logs []string
	sctx.Log = func(line string) { logs = append(logs, line) }
	if _, err := productionBudgetStep(t, types.StepIntent).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(logs, "\n"), "warning: near PR body budget") {
		t.Fatalf("missing warning: %v", logs)
	}
	intent = strings.Repeat("private text ", 10000)
	sctx.Run.OmitIntent = true
	logs = nil
	if _, err := productionBudgetStep(t, types.StepIntent).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(logs, "\n"), "PR body budget") {
		t.Fatalf("omitted intent affected budget: %v", logs)
	}
}

func TestPRBudgetUsesForgeUnitsAndFinalGuardStillCatchesGrowth(t *testing.T) {
	sctx := newIntentStepContext(t)
	sctx.Repo.UpstreamURL = "https://dev.azure.com/org/project/_git/repo"
	intent := strings.Repeat("😀", 2100)
	sctx.Run.Intent = &intent
	_, err := productionBudgetStep(t, types.StepIntent).Execute(sctx)
	if err == nil || !strings.Contains(err.Error(), "/4000 UTF-16 units (forge limit)") {
		t.Fatalf("ignored forge limit: %v", err)
	}
	intent = "Small public intent"
	if _, err := productionBudgetStep(t, types.StepIntent).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	_, appendix := ownedFixture(t)
	if _, err := composeOwnedPRContent(prOwnedBody{before: "Small narrative"}, "", appendix+strings.Repeat("later evidence ", 6000), 0); err == nil {
		t.Fatal("final publication guard accepted later evidence growth")
	}
}

func TestPRBudgetEstimateKeepsRecoveryAndOverrideInterfaces(t *testing.T) {
	if _, ok := productionBudgetStep(t, types.StepReview).(pipeline.ApprovalGateResumer); !ok {
		t.Fatal("review lost its gate resumer")
	}
	if _, ok := productionBudgetStep(t, types.StepTest).(pipeline.ApprovalOverrideVerifier); !ok {
		t.Fatal("test lost its override verifier")
	}
}

func TestPRBudgetMinimalAppendixDoesNotCountExcludedEvidence(t *testing.T) {
	sctx := newIntentStepContext(t)
	insertCompletedStep(t, sctx, types.StepRebase, "", strings.Repeat("recorded evidence ", 5000))
	sctx.Config.PR.Appendix = config.PRAppendixMinimal
	body, _, err := estimatePRBody(sctx, scm.ProviderGitHub)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(body, "recorded evidence") {
		t.Fatal("minimal estimate included excluded full evidence")
	}
	if err := checkEarlyPRBudget(sctx); err != nil {
		t.Fatal(err)
	}
}

func TestPRBudgetCountsTrustedTemplateAndUnclaimedClosingReferences(t *testing.T) {
	sctx, ag, _ := templateTestContext(t)
	if err := sctx.DB.UpdateRunClosingIssueRefs(sctx.Run.ID, []string{"1471"}); err != nil {
		t.Fatal(err)
	}
	body, _, err := estimatePRBody(sctx, scm.ProviderGitHub)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(body, testPRTemplate) || strings.Contains(body, "CONTRIBUTOR TEMPLATE MUST NOT WIN") || !strings.Contains(body, "Closes #1471") || !hasPRAppendixMarkers(body) {
		t.Fatalf("estimate lost trusted template, appendix or closing references: %s", body)
	}
	if err := checkEarlyPRBudget(sctx); err != nil {
		t.Fatal(err)
	}
	if len(ag.calls) != 0 {
		t.Fatal("preflight invoked a drafting agent")
	}
	if err := sctx.DB.UpdateRunClosingIssueRefs(sctx.Run.ID, []string{"1471", "1472"}); err != nil {
		t.Fatalf("preflight claimed the closing references: %v", err)
	}
}

func TestPRBudgetUsesPluginDeclaredLimitWithoutPublishing(t *testing.T) {
	sctx := newIntentStepContext(t)
	_, logPath := fakeProviderPlugin(t, sctx, fakeplugin.State{MaxPRBodyChars: 4000})
	intent := strings.Repeat("intent ", 700)
	sctx.Run.Intent = &intent
	_, err := productionBudgetStep(t, types.StepIntent).Execute(sctx)
	if err == nil || !strings.Contains(err.Error(), "/4000 UTF-16 units (forge limit)") {
		t.Fatalf("ignored plugin limit: %v", err)
	}
	if ops := pluginOperations(t, logPath); len(ops) != 1 || ops[0] != "status" {
		t.Fatalf("preflight performed operations beyond the budget handshake: %v", ops)
	}
}
