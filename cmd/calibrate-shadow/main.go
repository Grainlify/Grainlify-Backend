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

	prs, err := calibration.LabelledPRs(ctx, local, *name)
	if err != nil {
		fatal("load labelled: %v", err)
	}
	if len(prs) == 0 {
		fatal("no labelled pull requests in %q", *name)
	}
	baseline := calibration.AlwaysAcceptBaseline(prs)
	version, sha := calibration.PromptFingerprint()

	fmt.Printf("sample      %s (%s)\n", *name, sampleID)
	fmt.Printf("labelled    %d pull requests, held-back excluded\n", len(prs))
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

	report(scores, baseline, len(prs), version, sha)
}

func report(scores []calibration.RunScore, baseline float64, total int, version, sha string) {
	line := strings.Repeat("=", 78)
	fmt.Printf("\n%s\nSHADOW COMPARISON - SINGLE-LABELLER BENCHMARK\n%s\n", line, line)
	fmt.Println(`
This scores each model against ONE person's labels. It is NOT inter-rater
agreement: there is no second human verdict in this set, so no claim of the
form "humans agreed" is available at any confidence.

The system prompt and output contract are the production judging ones, read
from the same constants the hackathon pipeline uses. The USER CONTENT is
assembled by the calibration harness from frozen snapshots, because production
builds it from hackathon tables that do not exist here. So this is the
production prompt, not the production code path.`)

	fmt.Printf("\nprompt %s (sha256 %s)\n", version, sha[:16])
	fmt.Printf("always-accept baseline: %.0f%% - a model that answers \"accept\" every time scores this\n", baseline)

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
