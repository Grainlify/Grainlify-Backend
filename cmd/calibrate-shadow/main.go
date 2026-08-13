// Command calibrate-shadow runs models over a labelled calibration sample and
// reports how often each agrees with the human labels.
//
// Local only, like the rest of the tool. It reads labels and writes model
// verdicts; it never writes to calibration_labels.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jagadeesh/grainlify/backend/internal/calibration"
)

// Pricing as published, per million tokens. Recorded on each run so a cost can
// be recomputed later against the prices that were actually in effect.
var pricing = map[string]calibration.ModelPricing{
	"gpt-5.6-luna": {InputPerMTok: 0.20, OutputPerMTok: 1.20},
	"gpt-5-mini":   {InputPerMTok: 0.25, OutputPerMTok: 2.00},
	"gpt-5":        {InputPerMTok: 1.25, OutputPerMTok: 10.00},
}

func main() {
	if os.Getenv("CALIBRATION_ENABLED") != "true" {
		fmt.Fprintln(os.Stderr, "refusing to run: CALIBRATION_ENABLED is not 'true'")
		os.Exit(2)
	}
	name := flag.String("name", "set-1", "sample name")
	models := flag.String("models", "gpt-5.6-luna,gpt-5-mini,gpt-5", "comma-separated models, run in this order")
	reportOnly := flag.Bool("report", false, "re-report the most recent run per model without calling anything")
	scope := flag.String("scope", "main", `which rows to score: "main" (the originally labelled set) or "held-back" (the released hold-back, labelled under the judge's own question)`)
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()

	local, err := pgxpool.New(ctx, os.Getenv("CALIBRATION_DB_URL"))
	if err != nil {
		fatal("connect: %v", err)
	}
	defer local.Close()

	var sampleID uuid.UUID
	if err := local.QueryRow(ctx, `SELECT id FROM calibration_samples WHERE name = $1`, *name).Scan(&sampleID); err != nil {
		fatal("sample %q: %v", *name, err)
	}

	sc := calibration.Scope(*scope)
	if sc != calibration.ScopeMain && sc != calibration.ScopeHeldBack {
		fatal("scope must be \"main\" or \"held-back\", not %q", *scope)
	}
	prs, err := calibration.LabelledPRs(ctx, local, *name, sc)
	if err != nil {
		fatal("load labelled: %v", err)
	}
	if len(prs) == 0 {
		fatal("no labelled pull requests in %q", *name)
	}
	baseline, baseAccepts, baseTotal := calibration.AlwaysAcceptBaseline(prs)
	version, sha := calibration.PromptFingerprint()

	fmt.Printf("sample      %s (%s)\n", *name, sampleID)
	fmt.Printf("scope       %s\n", sc)
	if sc == calibration.ScopeHeldBack {
		fmt.Println("            these rows were withheld until after the prompt was settled, and are")
		fmt.Println("            labelled under the judge's own question - coordination assumed passed")
	}
	fmt.Printf("labelled    %d pull requests\n", len(prs))
	fmt.Printf("prompt      %s  sha256 %s\n", version, sha[:16])
	fmt.Printf("threshold   %d input tokens (long-context pricing above this)\n", calibration.LongContextThresholdTokens)
	fmt.Printf("run at      %s\n\n", time.Now().Format(time.RFC3339))

	apiKey := os.Getenv("OPENAI_API_KEY")
	var scores []calibration.RunScore

	for _, model := range strings.Split(*models, ",") {
		model = strings.TrimSpace(model)
		if model == "" {
			continue
		}
		price, ok := pricing[model]
		if !ok {
			fatal("no pricing recorded for %q; refusing to run a model whose cost cannot be reported", model)
		}

		var runID uuid.UUID
		if *reportOnly {
			if err := local.QueryRow(ctx, `
SELECT id FROM calibration_model_runs WHERE sample_id = $1 AND model = $2
ORDER BY started_at DESC LIMIT 1`, sampleID, model).Scan(&runID); err != nil {
				fatal("no recorded run for %s: %v", model, err)
			}
		} else {
			if apiKey == "" {
				fatal("OPENAI_API_KEY is not set")
			}
			fmt.Printf("--- %s\n", model)
			judge := calibration.NewOpenAIJudge(apiKey, model)
			runID, err = calibration.RunShadow(ctx, local, sampleID, *name, judge, price, prs,
				func(i int, p calibration.LabelledPR, r calibration.JudgeResponse, jerr error) {
					status := r.Verdict
					if jerr != nil {
						status = "FAILED: " + jerr.Error()
					}
					flag := ""
					if r.PromptTokens > calibration.LongContextThresholdTokens {
						flag = "  [LONG CONTEXT]"
					}
					fmt.Printf("  %2d/%d %-36s #%-6d %-8s %6d in / %5d out%s\n",
						i+1, len(prs), p.Project, p.Number, status, r.PromptTokens, r.CompletionTokens, flag)
				})
			if err != nil {
				fatal("run %s: %v", model, err)
			}
			fmt.Println()
		}

		score, err := calibration.ScoreRun(ctx, local, runID)
		if err != nil {
			fatal("score %s: %v", model, err)
		}
		scores = append(scores, score)
	}

	views, err := calibration.PrefilterViews(ctx, local, *name, sc == calibration.ScopeHeldBack)
	if err != nil {
		fatal("prefilter view: %v", err)
	}
	report(scores, baseline, baseAccepts, baseTotal, version, sha)
	reportPrefilter(views)
}

func report(scores []calibration.RunScore, baseline float64, baseAccepts, baseTotal int, version, sha string) {
	line := strings.Repeat("=", 78)
	fmt.Printf("\n%s\nSHADOW COMPARISON - SINGLE-LABELLER BENCHMARK\n%s\n", line, line)
	fmt.Println(`
This scores each model against ONE person's labels. It is NOT inter-rater
agreement: there is no second human verdict in this set, so no claim of the
form "humans agreed" is available at any confidence.

It uses the COMBINED calibration prompt, which asks one model to do both jobs:
the coordination gates production enforces in deterministic code (Prefilter,
AI-specs §5.1) and the quality judgement production asks a model for (§5.3).

It therefore answers "could one model do the whole job?" - and says NOTHING
about how good production's judge is. In production those stages are separate
on purpose, and only pull requests that already passed coordination ever reach
a model.`)

	prodV, _ := calibration.ProductionJudgingFingerprint()
	fmt.Printf("\nprompt used     %s (sha256 %s)\n", version, sha[:16])
	fmt.Printf("prompt NOT used %s  <- production's judging prompt; this run says nothing about it\n", prodV)
	fmt.Printf("always-accept baseline: %.0f%% (%d accept of %d) - computed on THESE rows, not carried\n",
		baseline, baseAccepts, baseTotal)
	fmt.Printf("                       from another scope. A model answering \"accept\" every time scores this.\n")
	if baseTotal < 10 {
		fmt.Printf("                       NOTE: %d rows. Each row is %.0f points, so this figure is coarse.\n",
			baseTotal, 100/float64(baseTotal))
	}

	fmt.Printf("\n%-14s %8s %8s %10s %9s %10s %8s %10s\n", "model", "agree", "vs base", "cost USD", "in tok", "out tok", "failed", "effort")
	for _, s := range scores {
		fmt.Printf("%-14s %7.0f%% %+8.0f %10.4f %9d %10d %8d %10s\n",
			s.Model, s.AgreementPct(), s.AgreementPct()-baseline, s.CostUSD,
			s.PromptTokens, s.CompletionTokens, s.Failed, s.ReasoningEffort)
	}
	fmt.Println(`
NOTE ON "MODEL IS THE ONLY VARIABLE": it very nearly is, but not exactly.
gpt-5.6-luna refuses function tools unless reasoning_effort is "none"; gpt-5
and gpt-5-mini reject "none" and accept only minimal/low/medium/high. No single
value works for all three, so each ran at the minimum it supports, shown above.
The difference is forced by the API, not chosen - and it is recorded per run so
a later comparison can tell the two apart.`)

	fmt.Printf("\nlong-context calls (input above %d tokens):\n", calibration.LongContextThresholdTokens)
	for _, s := range scores {
		note := "none"
		if s.LongContextCalls > 0 {
			note = fmt.Sprintf("%d calls - PRICED AT THE LONG-CONTEXT RATE, cost above is understated", s.LongContextCalls)
		}
		fmt.Printf("  %-14s %s (largest single call %d input tokens)\n", s.Model, note, s.MaxPromptTokens)
	}

	bands := []calibration.SizeBand{calibration.BandTiny, calibration.BandSmall, calibration.BandMedium, calibration.BandLarge, calibration.BandHuge}
	fmt.Printf("\nby size band (agree/total):\n%-14s", "model")
	for _, b := range bands {
		fmt.Printf(" %9s", b)
	}
	fmt.Println()
	for _, s := range scores {
		fmt.Printf("%-14s", s.Model)
		for _, b := range bands {
			v := s.ByBand[b]
			if v[1] == 0 {
				fmt.Printf(" %9s", "-")
			} else {
				fmt.Printf(" %9s", fmt.Sprintf("%d/%d", v[0], v[1]))
			}
		}
		fmt.Println()
	}

	fmt.Printf("\nby outcome (agree/total):\n%-14s %12s %12s\n", "model", "merged", "unmerged")
	for _, s := range scores {
		m, u := s.ByMerged[true], s.ByMerged[false]
		fmt.Printf("%-14s %12s %12s\n", s.Model,
			fmt.Sprintf("%d/%d", m[0], m[1]), fmt.Sprintf("%d/%d", u[0], u[1]))
	}

	fmt.Printf("\nby stated confidence (agree/total). Borderline is a COUNT, not a rate:\n")
	fmt.Printf("%-14s %12s %14s\n", "model", "certain", "borderline")
	for _, s := range scores {
		c, b := s.ByConfidence["certain"], s.ByConfidence["borderline"]
		fmt.Printf("%-14s %12s %14s\n", s.Model,
			fmt.Sprintf("%d/%d", c[0], c[1]), fmt.Sprintf("%d of %d", b[0], b[1]))
	}
	fmt.Println("  (with 2 borderline rows in the set, no percentage computed from them would mean anything)")

	for _, s := range scores {
		fmt.Printf("\n%s\nDISAGREEMENTS - %s (%d of %d)\n%s\n", line, s.Model, len(s.Disagreements), s.Scored, line)
		if len(s.Disagreements) == 0 {
			fmt.Println("none")
			continue
		}
		sort.Slice(s.Disagreements, func(i, j int) bool { return s.Disagreements[i].Number < s.Disagreements[j].Number })
		for _, d := range s.Disagreements {
			state := "unmerged"
			if d.Merged {
				state = "merged"
			}
			fmt.Printf("\n%s#%d  (%s, %s)\n", d.Project, d.Number, d.SizeBand, state)
			fmt.Printf("  you   %-7s (%s): %s\n", d.Human, d.HumanConf, wrap(d.HumanReason, 68, "          "))
			fmt.Printf("  model %-7s: %s\n", d.Model, wrap(d.ModelReason, 68, "          "))
		}
	}
	fmt.Printf("\n%s\n", line)
}

func wrap(s string, width int, indent string) string {
	words := strings.Fields(s)
	if len(words) == 0 {
		return "(none given)"
	}
	var b strings.Builder
	line := 0
	for i, w := range words {
		if line+len(w)+1 > width && i > 0 {
			b.WriteString("\n" + indent)
			line = 0
		} else if i > 0 {
			b.WriteString(" ")
			line++
		}
		b.WriteString(w)
		line += len(w)
	}
	return b.String()
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

// reportPrefilter answers the question the agreement rate cannot: how many of
// the human's rejections were coordination calls that plain code already
// handles, and how many needed judgement about the work.
func reportPrefilter(views []calibration.PrefilterView) {
	line := strings.Repeat("=", 78)
	fmt.Printf("\n%s\nWHAT PLAIN CODE ALREADY DECIDES\n%s\n", line, line)
	fmt.Println(`
Production rejects on deterministic rules before any model call. Below is what
that code would say about these pull requests, using ONLY facts the frozen
snapshot contains. Rules whose inputs were never captured are listed as
unknowable and are NOT assumed either way.`)

	s := calibration.SummarisePrefilter(views)

	fmt.Printf("\nof %d labelled pull requests, you rejected %d:\n", s.Total, s.HumanRejected)
	fmt.Printf("  %2d  coordination - a knowable prefilter rule rejects these too, no judgement needed\n", s.CoordinationRejections)
	fmt.Printf("  %2d  judgement    - no knowable rule explains these; they required assessing the work\n", s.JudgementRejections)
	if s.PrefilterRejectsHumanAccepted > 0 {
		fmt.Printf("\n  %2d you ACCEPTED would be rejected by a knowable rule - worth reading, the code and you disagree\n", s.PrefilterRejectsHumanAccepted)
	}

	fmt.Println("\nby rule:")
	for _, r := range calibration.KnowableRules {
		fmt.Printf("  %-28s %d\n", r, s.ByRule[r])
	}

	fmt.Println("\nunknowable for this set, and therefore not applied:")
	for rule, why := range calibration.UnknowableRules {
		fmt.Printf("  %-42s %s\n", rule, why)
	}
	fmt.Println(`
Two caveats on the knowable rules. "documentation only" has an exemption in
production when the linked issue was itself a documentation issue, which the
snapshot cannot decide - so it is reported as a rejection here. And meaningful
lines are an upper bound, because per-file patches were not stored separately,
so whitespace-only and banner-generated lines are not subtracted; that makes
"no meaningful code" under-reject rather than over-reject.`)

	fmt.Printf("\n%-38s %-8s %-26s %s\n", "pull request", "you", "prefilter (knowable rules)", "meaningful")
	for _, v := range views {
		verdict := "-"
		if v.WouldReject {
			verdict = "REJECT: " + v.Rule
		} else {
			verdict = "passes knowable rules"
		}
		fmt.Printf("%-38s %-8s %-26s %d\n",
			fmt.Sprintf("%s#%d", shortProject(v.Project), v.Number), v.HumanVerdict, verdict, v.Stats.MeaningfulLines)
	}
	fmt.Printf("\n%s\n", line)
}

func shortProject(p string) string {
	if i := strings.Index(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}
