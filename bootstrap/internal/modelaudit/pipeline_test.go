package modelaudit

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jdwlabs/infrastructure/bootstrap/internal/modelaudit/audittest"
	"github.com/jdwlabs/infrastructure/bootstrap/internal/modelaudit/hub"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	shaA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	shaB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	shaC = "cccccccccccccccccccccccccccccccccccccccc"
)

func days(n int) time.Time { return audittest.Now.Add(-time.Duration(n) * 24 * time.Hour) }

// fits is a new Qwen3-Coder variant enriched with the incumbent's recorded
// answers, so it fits and resolves to qwen3_xml.
func fits(id, sha string, dl int64) audittest.Model {
	return audittest.Model{ID: id, SHA: sha, CreatedAt: days(2), Downloads: dl,
		PipelineTag: "text-generation", License: "apache-2.0"}
}

func runConfig(t *testing.T, orgs ...string) Config {
	t.Helper()
	c := testConfig(t)
	c.Orgs = orgs
	c.MaxRequests = 60
	c.DiscoveryRequests = 20
	return c
}

// testCurrent is the fixture incumbent, not the committed serving.yaml: these
// tests assert its repo, quantization and flags by value.
func testCurrent(t *testing.T) Current {
	t.Helper()
	cur, err := LoadCurrent(fixtureSpec)
	require.NoError(t, err)
	return cur
}

func deps(h *audittest.Hub, cfg Config) Deps {
	return Deps{
		Hub: &hub.Client{
			HubBase: h.Server.URL, RawBase: h.Server.URL, HTTP: h.Server.Client(),
			Budget: hub.NewBudget(cfg.MaxRequests, cfg.DiscoveryRequests),
			Sleep:  func(context.Context, time.Duration) error { return nil },
		},
		Now:           audittest.Now,
		DedupeSkipped: "test",
	}
}

func repos(rows []CandidateRow) []string {
	out := []string{}
	for _, r := range rows {
		out = append(out, r.Repo)
	}
	return out
}

func rejected(r Report) map[string]string {
	out := map[string]string{}
	for _, x := range r.Rejected {
		out[x.Repo] = x.Reason
	}
	return out
}

func TestRunReportsAFittingCandidateWithItsTrialSteps(t *testing.T) {
	h := audittest.NewHub(t)
	h.Orgs["Qwen"] = [][]audittest.Model{{fits("Qwen/Qwen3-Coder-Next-Instruct", shaA, 500)}}
	cfg := runConfig(t, "Qwen")

	r, err := Run(context.Background(), cfg, testCurrent(t), deps(h, cfg))

	require.NoError(t, err)
	require.Len(t, r.Candidates, 1)
	c := r.Candidates[0]
	assert.Equal(t, "Qwen/Qwen3-Coder-Next-Instruct", c.Repo)
	assert.Equal(t, PoolAllowListed, c.Pool)
	assert.Equal(t, "qwen3_xml", c.Parser)
	assert.Equal(t, "awq", c.Quant)
	assert.Equal(t, 15.66, c.WeightsGiB)
	assert.Equal(t, 21.66, c.TotalGiB)
	assert.Equal(t, 7.00, c.MarginGiB, "28.66 GiB budget - 21.66 GiB")
	assert.Equal(t, "https://huggingface.co/Qwen/Qwen3-Coder-Next-Instruct/tree/"+shaA, c.URL)
	assert.Equal(t, []string{
		"set model.repo=Qwen/Qwen3-Coder-Next-Instruct and model.revision=" + shaA,
		"keep servedName=local-chat: consumers request the model by it",
		"keep servedAliases=[qwen/qwen3-coder-30b-a3b]: consumers not yet moved to servedName request the model by them",
		"set --tool-call-parser=qwen3_xml",
		"remove --quantization=awq_marlin: vLLM detects awq from quantization_config",
		"keep --max-model-len=32768",
		"open a PR; run talops vllm plan; run talops vllm apply --confirm in an announced window",
	}, c.TrialSteps)
	for _, s := range c.TrialSteps {
		assert.NotContains(t, s, "set servedName", "servedName must never change")
	}
	assert.Equal(t, "2026-W40", r.Summary.Week)
	assert.Equal(t, 28.66, r.Summary.BudgetGiB)
	assert.Equal(t, 7, r.Summary.Requests, "two registries, one org page, trending, three enrich requests")
	assert.Contains(t, r.Skipped, Skip{Source: "jira", Error: "dedupe skipped: test"})
}

func TestRunWindowsOnCreationAndStopsPagingOutsideIt(t *testing.T) {
	h := audittest.NewHub(t)
	old := fits("Qwen/Qwen3-Old-Instruct", shaB, 900)
	old.CreatedAt = days(8)
	h.Orgs["Qwen"] = [][]audittest.Model{{fits("Qwen/Qwen3-Coder-Next-Instruct", shaA, 500), old}, {fits("Qwen/Never-Read-Instruct", shaC, 1)}}
	cfg := runConfig(t, "Qwen")

	r, err := Run(context.Background(), cfg, testCurrent(t), deps(h, cfg))

	require.NoError(t, err)
	assert.Equal(t, []string{"Qwen/Qwen3-Coder-Next-Instruct"}, repos(r.Candidates))
	assert.Equal(t, 1, r.Summary.Checked)
	for _, req := range h.Requests() {
		assert.NotContains(t, req, "cursor=1", "the page ended outside the window")
	}
}

func TestRunFollowsTheCursorWhileThePageIsInsideTheWindow(t *testing.T) {
	h := audittest.NewHub(t)
	h.Orgs["Qwen"] = [][]audittest.Model{{fits("Qwen/A-Instruct", shaA, 5)}, {fits("Qwen/B-Instruct", shaB, 9)}}
	cfg := runConfig(t, "Qwen")

	r, err := Run(context.Background(), cfg, testCurrent(t), deps(h, cfg))

	require.NoError(t, err)
	assert.Equal(t, []string{"Qwen/B-Instruct", "Qwen/A-Instruct"}, repos(r.Candidates), "ranked by downloads")
}

func TestRunRanksAllowListedBeforeUnvettedAndCaps(t *testing.T) {
	h := audittest.NewHub(t)
	h.Orgs["Qwen"] = [][]audittest.Model{{fits("Qwen/Low-Instruct", shaA, 1), fits("Qwen/Tie-B-Instruct", shaB, 50), fits("Qwen/Tie-A-Instruct", shaC, 50)}}
	h.Trending = []audittest.Model{fits("Stranger/Huge-Instruct", shaA, 1_000_000), fits("Qwen/Low-Instruct", shaA, 1)}
	cfg := runConfig(t, "Qwen")
	cfg.MaxCandidates = 3

	r, err := Run(context.Background(), cfg, testCurrent(t), deps(h, cfg))

	require.NoError(t, err)
	assert.Equal(t, []string{"Qwen/Tie-A-Instruct", "Qwen/Tie-B-Instruct", "Qwen/Low-Instruct"}, repos(r.Candidates))
	assert.Equal(t, "over maxCandidates (3)", rejected(r)["Stranger/Huge-Instruct"])
	assert.Equal(t, 4, r.Summary.Checked, "a repo in both pools is counted once, as allow-listed")
}

func TestRunRejectsWithTheFirstFailingReason(t *testing.T) {
	h := audittest.NewHub(t)
	base := fits("Qwen/Base-Model", shaA, 1)
	base.Revision = []byte(`{"id":"Qwen/Base-Model","tags":[],"config":{"model_type":"qwen3_moe","tokenizer_config":{}}}`)
	image := fits("Qwen/Image-Instruct", shaB, 1)
	image.PipelineTag = "text-to-image"
	gemma := fits("Qwen/Gemma-Like-Instruct", shaC, 1)
	gemma.Config = []byte(`{"model_type":"gemma4"}`)
	forbidden := fits("Qwen/Forbidden-Instruct", shaC, 1)
	forbidden.EnrichStatus = http.StatusForbidden
	flaky := fits("Qwen/Flaky-Instruct", shaC, 1)
	flaky.EnrichStatus = http.StatusBadGateway
	mistral := fits("Qwen/Too-Big-Instruct", shaC, 1)
	mistral.Config = audittest.Fixture("mistral-small-3.2-config.json")
	mistral.Tree = audittest.Fixture("mistral-small-3.2-tree.json")
	gguf := fits("Qwen/Only-GGUF-Instruct", shaC, 1)
	gguf.ConfigStatus = http.StatusNotFound
	incumbent := fits("QuantTrio/Qwen3-Coder-30B-A3B-Instruct-AWQ", shaA, 1)
	h.Orgs["Qwen"] = [][]audittest.Model{{base, image, gemma, forbidden, flaky, mistral, gguf}}
	h.Orgs["QuantTrio"] = [][]audittest.Model{{incumbent}}
	cfg := runConfig(t, "Qwen", "QuantTrio")
	cfg.Parsers = append(cfg.Parsers, Rule{ModelType: "mistral3", Parser: "mistral"})

	r, err := Run(context.Background(), cfg, testCurrent(t), deps(h, cfg))

	require.NoError(t, err)
	assert.Empty(t, r.Candidates)
	assert.Equal(t, map[string]string{
		"Qwen/Base-Model":                            "not instruction-tuned",
		"Qwen/Image-Instruct":                        "pipeline_tag text-to-image",
		"Qwen/Gemma-Like-Instruct":                   "no parser rule for gemma4",
		"Qwen/Forbidden-Instruct":                    "enrich failed: 403 (access)",
		"Qwen/Flaky-Instruct":                        "enrich failed: 502",
		"Qwen/Too-Big-Instruct":                      "does not fit: 52.72 GiB > 28.66 GiB budget",
		"Qwen/Only-GGUF-Instruct":                    "enrich failed: 404 (no config.json)",
		"QuantTrio/Qwen3-Coder-30B-A3B-Instruct-AWQ": "incumbent",
	}, rejected(r))
	assert.Equal(t, 8, r.Summary.Rejected)
}

func TestRunDropsAlreadyReportedRepos(t *testing.T) {
	h := audittest.NewHub(t)
	h.Orgs["Qwen"] = [][]audittest.Model{{fits("Qwen/Old-News-Instruct", shaA, 1), fits("Qwen/New-Instruct", shaB, 1)}}
	cfg := runConfig(t, "Qwen")
	d := deps(h, cfg)
	d.Reported = map[string]bool{"Qwen/Old-News-Instruct": true}

	r, err := Run(context.Background(), cfg, testCurrent(t), d)

	require.NoError(t, err)
	assert.Equal(t, []string{"Qwen/New-Instruct"}, repos(r.Candidates))
	assert.Equal(t, "already reported", rejected(r)["Qwen/Old-News-Instruct"])
	for _, s := range r.Skipped {
		assert.NotEqual(t, "jira", s.Source, "dedupe ran")
	}
	for _, req := range h.Requests() {
		assert.NotContains(t, req, "Old-News-Instruct", "a reported repo is never enriched")
	}
}

func TestRunNeverEnrichesTheIncumbent(t *testing.T) {
	h := audittest.NewHub(t)
	h.Orgs["QuantTrio"] = [][]audittest.Model{{fits("QuantTrio/Qwen3-Coder-30B-A3B-Instruct-AWQ", shaA, 1)}}
	cfg := runConfig(t, "QuantTrio")

	r, err := Run(context.Background(), cfg, testCurrent(t), deps(h, cfg))

	require.NoError(t, err)
	assert.Equal(t, "incumbent", rejected(r)["QuantTrio/Qwen3-Coder-30B-A3B-Instruct-AWQ"])
	assert.Equal(t, 4, r.Summary.Requests, "two registries, one org page, trending; no enrich request")
}

func TestRunSpendsATightEnrichmentBudgetOnlyOnUnreportedRepos(t *testing.T) {
	h := audittest.NewHub(t)
	h.Orgs["Qwen"] = [][]audittest.Model{{fits("Qwen/Old-News-Instruct", shaA, 9), fits("Qwen/New-Instruct", shaB, 1)}}
	cfg := runConfig(t, "Qwen")
	cfg.MaxRequests = 7 // two registries, org page, trending, three enrich requests for one repo
	d := deps(h, cfg)
	d.Reported = map[string]bool{"Qwen/Old-News-Instruct": true}

	r, err := Run(context.Background(), cfg, testCurrent(t), d)

	require.NoError(t, err)
	assert.Equal(t, []string{"Qwen/New-Instruct"}, repos(r.Candidates))
	assert.Equal(t, map[string]string{"Qwen/Old-News-Instruct": "already reported"}, rejected(r))
	for _, s := range r.Skipped {
		assert.NotEqual(t, "enrichment", s.Source, "the budget covered every repo that needed it")
	}
}

func TestRunNamesEveryOrgTheDiscoveryBudgetStopped(t *testing.T) {
	h := audittest.NewHub(t)
	h.OrgStatus["Broken"] = http.StatusInternalServerError
	h.Orgs["Deep"] = [][]audittest.Model{{fits("Deep/A-Instruct", shaA, 1)}, {fits("Deep/B-Instruct", shaB, 1)}}
	h.Orgs["Unread"] = [][]audittest.Model{{fits("Unread/C-Instruct", shaC, 1)}}
	cfg := runConfig(t, "Broken", "Deep", "Unread")
	cfg.DiscoveryRequests = 7 // two registries, four attempts at Broken, Deep's first page

	r, err := Run(context.Background(), cfg, testCurrent(t), deps(h, cfg))

	require.NoError(t, err, "one org was read, so the report is filed")
	assert.Equal(t, []string{"Deep/A-Instruct"}, repos(r.Candidates))
	skipped := map[string]string{}
	for _, s := range r.Skipped {
		skipped[s.Source] = s.Error
	}
	assert.Contains(t, skipped["Broken"], "HTTP 500")
	assert.Equal(t, "truncated at 1", skipped["Deep"])
	assert.Equal(t, "not read: request budget spent", skipped["Unread"])
	assert.Equal(t, "not read: request budget spent", skipped["trending"])
}

func TestRunFailsWhenTheBudgetStopsDiscoveryBeforeAnyOrg(t *testing.T) {
	h := audittest.NewHub(t)
	h.Orgs["Qwen"] = [][]audittest.Model{{fits("Qwen/A-Instruct", shaA, 1)}}
	cfg := runConfig(t, "Qwen")
	cfg.DiscoveryRequests = 2 // the two registries

	r, err := Run(context.Background(), cfg, testCurrent(t), deps(h, cfg))

	var f *Failure
	require.ErrorAs(t, err, &f)
	assert.Equal(t, "discovery_budget_spent", f.Code)
	assert.Contains(t, f.Msg, "discoveryRequests=2")
	assert.Same(t, f, r.Failure)
}

func TestRunFailsWhenEveryOrgFails(t *testing.T) {
	h := audittest.NewHub(t)
	h.OrgStatus["Qwen"] = http.StatusNotFound
	cfg := runConfig(t, "Qwen")

	_, err := Run(context.Background(), cfg, testCurrent(t), deps(h, cfg))

	var f *Failure
	require.ErrorAs(t, err, &f)
	assert.Equal(t, "discovery_failed", f.Code)
}

func TestRunFailsOnAnUnusableRegistry(t *testing.T) {
	for name, mut := range map[string]func(*audittest.Hub){
		"unreachable": func(h *audittest.Hub) { h.RegistryStatus = http.StatusNotFound },
		"parser missing": func(h *audittest.Hub) {
			h.Registry = []byte("_TOOL_PARSERS_TO_REGISTER = {\n    \"hermes\": (\"a\", \"B\"),\n}\n")
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := audittest.NewHub(t)
			mut(h)
			cfg := runConfig(t, "Qwen")
			_, err := Run(context.Background(), cfg, testCurrent(t), deps(h, cfg))
			var f *Failure
			require.ErrorAs(t, err, &f)
			assert.Contains(t, []string{"registry_unreadable", "registry_unusable"}, f.Code)
		})
	}
}

func TestRunRejectsAndNamesCandidatesTheEnrichmentBudgetStopped(t *testing.T) {
	h := audittest.NewHub(t)
	h.Orgs["Qwen"] = [][]audittest.Model{{fits("Qwen/First-Instruct", shaA, 3), fits("Qwen/Second-Instruct", shaB, 2), fits("Qwen/Third-Instruct", shaC, 1)}}
	cfg := runConfig(t, "Qwen")
	cfg.MaxRequests = 7 // two registries, org page, trending, three enrich requests for First

	r, err := Run(context.Background(), cfg, testCurrent(t), deps(h, cfg))

	require.NoError(t, err)
	assert.Equal(t, []string{"Qwen/First-Instruct"}, repos(r.Candidates))
	assert.Equal(t, "not enriched: request budget spent", rejected(r)["Qwen/Second-Instruct"])
	assert.Equal(t, "not enriched: request budget spent", rejected(r)["Qwen/Third-Instruct"])
	assert.Contains(t, r.Skipped, Skip{Source: "enrichment", Error: "2 candidates not enriched: request budget spent"})
}

func TestRunSendsTheTokenOnlyToTheHubAndEnrichesGatedRepos(t *testing.T) {
	h := audittest.NewHub(t)
	gated := fits("Qwen/Gated-Instruct", shaA, 1)
	gated.Gated = "manual"
	h.Orgs["Qwen"] = [][]audittest.Model{{gated}}
	cfg := runConfig(t, "Qwen")
	d := deps(h, cfg)
	d.Hub.Token = "hf_test"
	d.HasToken = true

	r, err := Run(context.Background(), cfg, testCurrent(t), d)

	require.NoError(t, err)
	require.Len(t, r.Candidates, 1)
	assert.Equal(t, "manual", r.Candidates[0].Gated)
	auth := h.AuthHeaders()
	assert.Equal(t, []string{"", ""}, auth[:2], "both registry requests go to GitHub")
	for _, a := range auth[2:] {
		assert.Equal(t, "Bearer hf_test", a)
	}
}

func TestWeekUsesTheISOYearAndPadsTheWeek(t *testing.T) {
	assert.Equal(t, "2026-W01", Week(time.Date(2025, 12, 29, 12, 0, 0, 0, time.UTC)))
	assert.Equal(t, "2026-W53", Week(time.Date(2027, 1, 1, 12, 0, 0, 0, time.UTC)))
	assert.Equal(t, "2026-W09", Week(time.Date(2026, 2, 23, 0, 0, 0, 0, time.UTC)))
	assert.Equal(t, "2026-W40", Week(audittest.Now))
	// Sunday 23:30 in UTC-5 is already Monday in UTC: the week follows UTC.
	assert.Equal(t, "2026-W40", Week(time.Date(2026, 9, 27, 23, 30, 0, 0, time.FixedZone("EST", -5*3600))))
}

func TestTrialStepsForAnUnquantizedModelSayWhy(t *testing.T) {
	steps := TrialSteps(testCurrent(t), "Org/M", shaA, "none", Rule{Parser: "llama3_json", ExtraArgs: []string{"--chat-template=<model's tool chat template>"}}, nil)
	assert.Contains(t, steps, "remove --quantization=awq_marlin: this model has no quantization_config")
	assert.Contains(t, steps, "add --chat-template=<model's tool chat template>")
}

func TestRunFailsOnAnUnusableModelRegistry(t *testing.T) {
	for code, mut := range map[string]func(*audittest.Hub){
		"model_registry_unreadable": func(h *audittest.Hub) { h.ModelRegistryStatus = http.StatusNotFound },
		"model_registry_unusable":   func(h *audittest.Hub) { h.ModelRegistry = []byte("# moved\n") },
	} {
		t.Run(code, func(t *testing.T) {
			h := audittest.NewHub(t)
			mut(h)
			cfg := runConfig(t, "Qwen")
			_, err := Run(context.Background(), cfg, testCurrent(t), deps(h, cfg))
			var f *Failure
			require.ErrorAs(t, err, &f)
			assert.Equal(t, code, f.Code)
			assert.Contains(t, f.Msg, "v0.24.0")
		})
	}
}

// The recorded Qwen3.8 repo is a Qwen3_5ForConditionalGeneration; the same
// config renamed to Qwen3_5ForCausalLM is the text-only class vLLM v0.24.0
// does not register.
func TestRunChecksTheArchitectureBeforeTheFit(t *testing.T) {
	h := audittest.NewHub(t)
	conf := audittest.Fixture("qwen3.8-27b-nvfp4-config.json")
	tree := audittest.Fixture("qwen3.8-27b-nvfp4-tree.json")
	wrapped := fits("Qwen/Qwen3.8-Instruct", shaA, 3)
	wrapped.Config, wrapped.Tree = conf, tree
	textOnly := fits("Qwen/Qwen3.8-Text-Instruct", shaB, 2)
	textOnly.Config = []byte(strings.Replace(string(conf), "Qwen3_5ForConditionalGeneration", "Qwen3_5ForCausalLM", 1))
	textOnly.Tree = tree
	bare := fits("Qwen/No-Architecture-Instruct", shaC, 1)
	bare.Config = []byte(`{"model_type":"qwen3_moe"}`)
	h.Orgs["Qwen"] = [][]audittest.Model{{wrapped, textOnly, bare}}
	cfg := runConfig(t, "Qwen")
	cfg.Parsers = append(cfg.Parsers, Rule{ModelType: "qwen3_5", Parser: "qwen3_xml"})

	r, err := Run(context.Background(), cfg, testCurrent(t), deps(h, cfg))

	require.NoError(t, err)
	assert.Equal(t, []string{"Qwen/Qwen3.8-Instruct"}, repos(r.Candidates))
	assert.Equal(t, 27.35, r.Candidates[0].TotalGiB)
	assert.Equal(t, 1.31, r.Candidates[0].MarginGiB)
	assert.Contains(t, r.Candidates[0].TrialSteps, languageModelOnlyStep,
		"a multimodal wrapper is trialled without the vision tower the estimate does not price")
	assert.Equal(t, map[string]string{
		"Qwen/Qwen3.8-Text-Instruct":    "architecture Qwen3_5ForCausalLM not in vLLM v0.24.0",
		"Qwen/No-Architecture-Instruct": "architecture missing",
	}, rejected(r))
}

func TestTrialStepsTurnOffTheVisionTowerOnlyForMultimodalWrappers(t *testing.T) {
	rule := Rule{Parser: "qwen3_xml"}
	wrapper := map[string]any{"architectures": []any{"Qwen3_5ForConditionalGeneration"}}
	textOnly := map[string]any{"architectures": []any{"Qwen3MoeForCausalLM"}}

	assert.Contains(t, TrialSteps(testCurrent(t), "Org/M", shaA, "none", rule, wrapper), languageModelOnlyStep)
	assert.NotContains(t, TrialSteps(testCurrent(t), "Org/M", shaA, "none", rule, textOnly), languageModelOnlyStep)

	cur := testCurrent(t)
	cur.LanguageModelOnly = true
	assert.NotContains(t, TrialSteps(cur, "Org/M", shaA, "none", rule, wrapper), languageModelOnlyStep,
		"serving.yaml already sets it")
}

// A trial swaps the model, not the names consumers request it by: an alias
// dropped in the same change would break a consumer not yet moved to
// servedName while the gate, which probes servedName, still passed.
func TestTrialStepsKeepTheServedAliases(t *testing.T) {
	cur := testCurrent(t)
	cur.ServedName = "local-chat"
	cur.ServedAliases = []string{"qwen/qwen3-coder-30b-a3b", "old-chat"}

	steps := TrialSteps(cur, "Org/M", shaA, "none", Rule{Parser: "qwen3_xml"}, nil)

	assert.Equal(t, "keep servedName=local-chat: consumers request the model by it", steps[1])
	assert.Equal(t, "keep servedAliases=[qwen/qwen3-coder-30b-a3b, old-chat]: consumers not yet moved to servedName request the model by them", steps[2])
	for _, s := range steps {
		assert.NotContains(t, s, "set servedAliases")
		assert.NotContains(t, s, "remove servedAliases")
	}
}

func TestTrialStepsSayNothingAboutAliasesWhenThereAreNone(t *testing.T) {
	cur := testCurrent(t)
	cur.ServedAliases = nil

	for _, s := range TrialSteps(cur, "Org/M", shaA, "none", Rule{Parser: "qwen3_xml"}, nil) {
		assert.NotContains(t, s, "servedAliases")
	}
}
