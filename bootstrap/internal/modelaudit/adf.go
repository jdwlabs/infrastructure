package modelaudit

import (
	"fmt"
	"strings"
	"time"

	"github.com/jdwlabs/infrastructure/bootstrap/internal/modelaudit/jira"
)

// DescriptionADF is a new weekly ticket's body. Its audit-candidates block
// is what later runs dedupe against, so it lists every candidate reported.
func DescriptionADF(r Report) jira.Node {
	s := r.Summary
	content := []jira.Node{
		jira.Heading(2, "Current"),
		jira.BulletList(
			[]jira.Node{jira.Text("model: " + s.Model)},
			[]jira.Node{jira.Text("servedName: " + s.ServedName)},
			[]jira.Node{jira.Text("vLLM: " + s.VLLMTag)},
			[]jira.Node{jira.Text(fmt.Sprintf("budget: %.2f GiB", s.BudgetGiB))},
		),
	}
	content = append(content, skippedSection(r.Skipped)...)
	content = append(content, jira.Heading(2, "Allow-listed candidates"))
	content = append(content, candidateSections(r.Candidates, PoolAllowListed, s.BudgetGiB)...)
	content = append(content, jira.Heading(2, "Unvetted (trending)"))
	content = append(content, candidateSections(r.Candidates, PoolUnvetted, s.BudgetGiB)...)
	content = append(content, rejectionSection(r)...)
	content = append(content, jira.CandidatesBlock(candidateRepos(r.Candidates)))
	return jira.Doc(content...)
}

// CandidatesCommentADF is a same-week re-run's addition to the week's ticket.
// It carries its own block, so next week's dedupe sees these repos too.
func CandidatesCommentADF(rows []CandidateRow, week string, budgetGiB float64) jira.Node {
	content := []jira.Node{
		jira.Paragraph(jira.Text(fmt.Sprintf("Candidates found by a re-run in %s: %d.", week, len(rows)))),
		jira.Heading(2, "Allow-listed candidates"),
	}
	content = append(content, candidateSections(rows, PoolAllowListed, budgetGiB)...)
	content = append(content, jira.Heading(2, "Unvetted (trending)"))
	content = append(content, candidateSections(rows, PoolUnvetted, budgetGiB)...)
	content = append(content, jira.CandidatesBlock(candidateRepos(rows)))
	return jira.Doc(content...)
}

// NoChangeCommentADF names the week label verbatim: a later run in the same
// week finds it in this comment and does not post a second one.
func NoChangeCommentADF(r Report, label string) jira.Node {
	content := []jira.Node{jira.Paragraph(jira.Text(fmt.Sprintf(
		"No new candidates in %s (%s); %d repos checked, %d rejected.", r.Summary.Week, label, r.Summary.Checked, r.Summary.Rejected)))}
	content = append(content, rejectionSection(r)...)
	content = append(content, skippedSection(r.Skipped)...)
	return jira.Doc(content...)
}

func candidateRepos(rows []CandidateRow) []string {
	out := make([]string, 0, len(rows))
	for _, c := range rows {
		out = append(out, c.Repo)
	}
	return out
}

func skippedSection(skipped []Skip) []jira.Node {
	if len(skipped) == 0 {
		return nil
	}
	var items [][]jira.Node
	for _, s := range skipped {
		items = append(items, []jira.Node{jira.Text(s.Source + ": " + s.Error)})
	}
	return []jira.Node{jira.Heading(2, "Sources skipped"), jira.BulletList(items...)}
}

func rejectionSection(r Report) []jira.Node {
	if len(r.Reasons) == 0 {
		return nil
	}
	var items [][]jira.Node
	for _, rc := range r.Reasons {
		items = append(items, []jira.Node{jira.Text(fmt.Sprintf("%s: %d", rc.Reason, rc.Count))})
	}
	return []jira.Node{jira.Heading(2, "Rejections"), jira.BulletList(items...)}
}

func candidateSections(rows []CandidateRow, pool Pool, budgetGiB float64) []jira.Node {
	var out []jira.Node
	for _, c := range rows {
		if c.Pool != pool {
			continue
		}
		details := [][]jira.Node{
			{jira.Link(c.Repo+" @ "+shortSHA(c.SHA), c.URL), jira.Text(" (" + string(c.Pool) + ")")},
			{jira.Text("created: " + c.CreatedAt.UTC().Format(time.RFC3339))},
			{jira.Text("quantization: " + c.Quant)},
			{jira.Text(fmt.Sprintf("weights %.2f GiB; estimated total %.2f GiB of %.2f GiB budget; margin %.2f GiB", c.WeightsGiB, c.TotalGiB, budgetGiB, c.MarginGiB))},
			{jira.Text("parser: " + c.Parser + "; license: " + c.License)},
		}
		if c.Gated != "" {
			details = append(details, []jira.Node{jira.Text("gated (" + c.Gated + "): the host needs the same HF_TOKEN")})
		}
		details = append(details, []jira.Node{jira.Text("source: "), jira.Link(c.URL, c.URL)})
		var steps [][]jira.Node
		for _, st := range c.TrialSteps {
			steps = append(steps, []jira.Node{jira.Text(st)})
		}
		out = append(out,
			jira.Heading(3, c.Repo),
			jira.BulletList(details...),
			jira.Paragraph(jira.Text(strings.ToUpper(TrialStepsNote[:1])+TrialStepsNote[1:]+":")),
			jira.BulletList(steps...),
		)
	}
	if len(out) == 0 {
		return []jira.Node{jira.Paragraph(jira.Text("None."))}
	}
	return out
}

func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
