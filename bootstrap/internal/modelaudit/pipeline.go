package modelaudit

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"time"

	"github.com/jdwlabs/infrastructure/bootstrap/internal/modelaudit/hub"
)

// Deps is everything Run reads from outside the two YAML files.
type Deps struct {
	Hub      *hub.Client
	HasToken bool
	Now      time.Time
	DryRun   bool
	// Reported is every repo already listed on a model-audit ticket. Nil
	// means dedupe could not run, and DedupeSkipped says why.
	Reported      map[string]bool
	DedupeSkipped string
}

type candidate struct {
	item    hub.ListItem
	pool    Pool
	license string
}

// Run discovers, filters and ranks candidates. It never writes anywhere. A
// returned *Failure means nothing may be filed; the Report is still printable.
func Run(ctx context.Context, cfg Config, cur Current, d Deps) (Report, error) {
	r := Report{
		Summary: Summary{
			Week:       Week(d.Now),
			Model:      cur.Repo + "@" + cur.Revision,
			ServedName: cur.ServedName,
			VLLMTag:    cur.VLLMTag,
			BudgetGiB:  round2(BudgetGiB(cfg.GPUMemMiB, cur.GPUMemoryUtilization)),
			DryRun:     d.DryRun,
		},
		Candidates: []CandidateRow{},
		Reasons:    []ReasonCount{},
		Rejected:   []Rejection{},
		Skipped:    []Skip{},
		Jira:       JiraOutcome{Action: "not-filed"},
	}
	fail := func(code, msg string) (Report, error) {
		r.Summary.Requests = d.Hub.Budget.Used()
		r.Failure = &Failure{Code: code, Msg: msg}
		return r, r.Failure
	}
	// A signal turns every remaining read into an error that would otherwise
	// be recorded as a skipped org or a rejected candidate, so a cancelled run
	// must fail rather than produce a report that looks complete.
	cancelled := func() (Report, error) {
		return fail("cancelled", "run cancelled: "+context.Cause(ctx).Error())
	}

	src, err := d.Hub.ToolParserRegistry(ctx, cur.VLLMTag)
	if err != nil {
		return fail("registry_unreadable", fmt.Sprintf("read the vLLM %s tool-parser registry: %v", cur.VLLMTag, err))
	}
	reg, err := ParseRegistry(src, cur.ToolCallParser)
	if err != nil {
		return fail("registry_unusable", fmt.Sprintf("vLLM %s: %v", cur.VLLMTag, err))
	}

	items, orgsRead, budgetStopped := discoverOrgs(ctx, cfg, d, &r)
	if ctx.Err() != nil {
		return cancelled()
	}
	if orgsRead == 0 {
		if budgetStopped {
			return fail("discovery_budget_spent", fmt.Sprintf("discovery budget (discoveryRequests=%d) spent before any allow-listed org was read", cfg.DiscoveryRequests))
		}
		return fail("discovery_failed", "every allow-listed org query failed")
	}
	items = append(items, discoverTrending(ctx, cfg, d, &r, items)...)
	if ctx.Err() != nil {
		return cancelled()
	}
	r.Summary.Checked = len(items)

	var survivors []candidate
	for _, c := range items {
		lic, reason := PreFilter(c.item, cfg, d.HasToken)
		if reason != "" {
			r.reject(c.item.ID, reason)
			continue
		}
		c.license = lic
		survivors = append(survivors, c)
	}
	rank(survivors)

	budget := BudgetGiB(cfg.GPUMemMiB, cur.GPUMemoryUtilization)
	var rows []CandidateRow
	notEnriched := 0
	for _, c := range survivors {
		// Dedupe needs nothing from the Hub, so it runs before enrichment: a
		// reported repo stays in the trending window for weeks, and spending
		// the budget on it would push a genuinely new repo out of the report.
		switch {
		case c.item.ID == cur.Repo:
			r.reject(c.item.ID, "incumbent")
			continue
		case d.Reported[c.item.ID]:
			r.reject(c.item.ID, "already reported")
			continue
		}
		if notEnriched > 0 {
			r.reject(c.item.ID, "not enriched: request budget spent")
			notEnriched++
			continue
		}
		row, reason, err := enrich(ctx, cfg, cur, d, reg, c, budget)
		if ctx.Err() != nil {
			return cancelled()
		}
		if errors.Is(err, hub.ErrBudget) {
			r.reject(c.item.ID, "not enriched: request budget spent")
			notEnriched++
			continue
		}
		if reason != "" {
			r.reject(c.item.ID, reason)
			continue
		}
		rows = append(rows, row)
	}
	if notEnriched > 0 {
		r.skip("enrichment", fmt.Sprintf("%d candidates not enriched: request budget spent", notEnriched))
	}

	if d.Reported == nil {
		r.skip("jira", "dedupe skipped: "+d.DedupeSkipped)
	}
	for i, row := range rows {
		if i >= cfg.MaxCandidates {
			r.reject(row.Repo, fmt.Sprintf("over maxCandidates (%d)", cfg.MaxCandidates))
			continue
		}
		r.Candidates = append(r.Candidates, row)
	}

	r.Summary.Candidates = len(r.Candidates)
	r.Summary.Rejected = len(r.Rejected)
	r.Summary.Requests = d.Hub.Budget.Used()
	r.Reasons = countReasons(r.Rejected)
	return r, nil
}

func (r *Report) reject(repo, reason string) {
	r.Rejected = append(r.Rejected, Rejection{Repo: repo, Reason: reason})
}

func (r *Report) skip(source, msg string) {
	r.Skipped = append(r.Skipped, Skip{Source: source, Error: msg})
}

// discoverOrgs reads each allow-listed org newest-first until an item falls
// outside the window. Every org the budget or an error stops is named in
// Skipped, so a partial read never looks complete.
func discoverOrgs(ctx context.Context, cfg Config, d Deps, r *Report) (items []candidate, orgsRead int, budgetStopped bool) {
	since := d.Now.Add(-time.Duration(cfg.WindowDays) * 24 * time.Hour)
	for _, org := range cfg.Orgs {
		if budgetStopped {
			r.skip(org, "not read: request budget spent")
			continue
		}
		next := d.Hub.ListURL(url.Values{
			"author": {org}, "sort": {"createdAt"}, "direction": {"-1"}, "limit": {"100"},
		})
		read, pages := 0, 0
		for next != "" {
			page, link, err := d.Hub.ListPage(ctx, next)
			if err != nil {
				switch {
				case errors.Is(err, hub.ErrBudget) && pages == 0:
					budgetStopped = true
					r.skip(org, "not read: request budget spent")
				case errors.Is(err, hub.ErrBudget):
					budgetStopped = true
					r.skip(org, "truncated at "+strconv.Itoa(read))
				case pages == 0:
					r.skip(org, err.Error())
				default:
					r.skip(org, fmt.Sprintf("truncated at %d: %v", read, err))
				}
				break
			}
			if pages == 0 {
				orgsRead++
			}
			pages++
			next = ""
			inWindow := true
			for _, it := range page {
				read++
				if it.CreatedAt.Before(since) {
					inWindow = false
					break
				}
				items = append(items, candidate{item: it, pool: PoolAllowListed})
			}
			if inWindow {
				next = link
			}
		}
	}
	return items, orgsRead, budgetStopped
}

// discoverTrending reads one trending page. A repo already found through the
// allow-list keeps that pool.
func discoverTrending(ctx context.Context, cfg Config, d Deps, r *Report, have []candidate) []candidate {
	since := d.Now.Add(-time.Duration(cfg.TrendingWindowDays) * 24 * time.Hour)
	seen := map[string]bool{}
	for _, c := range have {
		seen[c.item.ID] = true
	}
	page, _, err := d.Hub.ListPage(ctx, d.Hub.ListURL(url.Values{
		"sort": {"trendingScore"}, "direction": {"-1"}, "limit": {strconv.Itoa(cfg.TrendingN)},
		"pipeline_tag": {"text-generation"},
	}))
	if errors.Is(err, hub.ErrBudget) {
		r.skip("trending", "not read: request budget spent")
		return nil
	}
	if err != nil {
		r.skip("trending", err.Error())
		return nil
	}
	var out []candidate
	for _, it := range page {
		if seen[it.ID] || it.CreatedAt.Before(since) {
			continue
		}
		seen[it.ID] = true
		out = append(out, candidate{item: it, pool: PoolUnvetted})
	}
	return out
}

// rank orders allow-listed before unvetted, then by downloads, then by id,
// so the budget is spent in the order candidates would be reported.
func rank(cs []candidate) {
	slices.SortStableFunc(cs, func(a, b candidate) int {
		if a.pool != b.pool {
			if a.pool == PoolAllowListed {
				return -1
			}
			return 1
		}
		if c := cmp.Compare(b.item.Downloads, a.item.Downloads); c != 0 {
			return c
		}
		return cmp.Compare(a.item.ID, b.item.ID)
	})
}

func enrich(ctx context.Context, cfg Config, cur Current, d Deps, reg Registry, c candidate, budget float64) (CandidateRow, string, error) {
	id, sha := c.item.ID, c.item.SHA
	info, err := d.Hub.ModelInfo(ctx, id, sha)
	if err != nil {
		return enrichFailure(err, c)
	}
	tree, err := d.Hub.Tree(ctx, id, sha)
	if err != nil {
		return enrichFailure(err, c)
	}
	conf, err := d.Hub.ConfigJSON(ctx, id, sha)
	if err != nil {
		// GGUF-only and adapter repos have no config.json; that is what the
		// repo is, not a failed read, and vLLM cannot serve it from here.
		var se *hub.StatusError
		if errors.As(err, &se) && se.Status == http.StatusNotFound {
			return CandidateRow{}, "enrich failed: 404 (no config.json)", nil
		}
		return enrichFailure(err, c)
	}

	if !InstructionTuned(id, info, tree) {
		return CandidateRow{}, "not instruction-tuned", nil
	}
	rule, reason := ResolveCandidateParser(cfg, reg, cur.VLLMTag, id, conf)
	if reason != "" {
		return CandidateRow{}, reason, nil
	}
	fit := Estimate(conf, tree, cur.ContextTokens, cfg.OverheadGiB, budget)
	if !fit.Fits {
		return CandidateRow{}, fit.Reason, nil
	}

	quant := "none"
	if qc, ok := conf["quantization_config"].(map[string]any); ok {
		if m, ok := qc["quant_method"].(string); ok && m != "" {
			quant = m
		}
	}
	return CandidateRow{
		Repo:       id,
		SHA:        sha,
		Pool:       c.pool,
		CreatedAt:  c.item.CreatedAt.UTC(),
		Downloads:  c.item.Downloads,
		Quant:      quant,
		WeightsGiB: fit.WeightsGiB,
		TotalGiB:   fit.TotalGiB,
		Parser:     rule.Parser,
		License:    c.license,
		Gated:      string(c.item.Gated),
		URL:        fmt.Sprintf("%s/%s/tree/%s", PublicHubURL, id, sha),
		TrialSteps: TrialSteps(cur, id, sha, quant, rule),
	}, "", nil
}

func enrichFailure(err error, c candidate) (CandidateRow, string, error) {
	if errors.Is(err, hub.ErrBudget) {
		return CandidateRow{}, "", err
	}
	var se *hub.StatusError
	if errors.As(err, &se) {
		// A 401/403 on an open repo is not the structural gated case the
		// pre-filter already names, so it is called out as access.
		if (se.Status == http.StatusUnauthorized || se.Status == http.StatusForbidden) && c.item.Gated == "" {
			return CandidateRow{}, fmt.Sprintf("enrich failed: %d (access)", se.Status), nil
		}
		return CandidateRow{}, fmt.Sprintf("enrich failed: %d", se.Status), nil
	}
	return CandidateRow{}, "enrich failed: " + err.Error(), nil
}

// TrialSteps is the args delta against serving.yaml. servedName stays fixed:
// consumers request the model by it, and the health gate checks the new name,
// so renaming would pass the gate while every consumer broke.
func TrialSteps(cur Current, repo, sha, quant string, rule Rule) []string {
	steps := []string{
		fmt.Sprintf("set model.repo=%s and model.revision=%s", repo, sha),
		fmt.Sprintf("keep servedName=%s: consumers request the model by it", cur.ServedName),
		"set --tool-call-parser=" + rule.Parser,
	}
	switch {
	case cur.Quantization == "":
	case quant == "none":
		steps = append(steps, fmt.Sprintf("remove --quantization=%s: this model has no quantization_config", cur.Quantization))
	default:
		steps = append(steps, fmt.Sprintf("remove --quantization=%s: vLLM detects %s from quantization_config", cur.Quantization, quant))
	}
	for _, a := range rule.ExtraArgs {
		steps = append(steps, "add "+a)
	}
	return append(steps,
		fmt.Sprintf("keep --max-model-len=%d", cur.ContextTokens),
		"open a PR; run talops vllm plan; run talops vllm apply --confirm in an announced window",
	)
}

func countReasons(rs []Rejection) []ReasonCount {
	counts := map[string]int{}
	for _, r := range rs {
		counts[r.Reason]++
	}
	out := make([]ReasonCount, 0, len(counts))
	for reason, n := range counts {
		out = append(out, ReasonCount{Reason: reason, Count: n})
	}
	slices.SortFunc(out, func(a, b ReasonCount) int {
		if c := cmp.Compare(b.Count, a.Count); c != 0 {
			return c
		}
		return cmp.Compare(a.Reason, b.Reason)
	})
	return out
}
