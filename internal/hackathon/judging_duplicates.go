package hackathon

import (
	"sort"
	"strings"
)

// Similarity scores two normalized diffs in [0,1].
//
// A function type rather than a hardcoded implementation because AI-specs.md
// §5.2 specifies embeddings, which are an external model call. The default
// below is lexical and needs no API, so stage 2 runs and is testable with
// the rest of the deterministic pipeline; an embedding-backed implementation
// can be substituted behind the same judging flag as stages 3-5.
//
// The two catch different things. Lexical similarity catches near-identical
// submissions, which is the farming vector §5.2 actually names ("near-
// identical submissions across accounts"). Embeddings additionally catch
// paraphrased-but-equivalent code, which is the subtler case.
type Similarity func(a, b string) float64

// normalizeDiff strips a patch down to its added content, lowercased and
// whitespace-collapsed, so that reindenting or renaming a file does not
// disguise an otherwise identical submission.
func normalizeDiff(patch string) []string {
	var tokens []string
	for _, line := range strings.Split(patch, "\n") {
		if !strings.HasPrefix(line, "+") || strings.HasPrefix(line, "+++") {
			continue
		}
		content := strings.TrimSpace(strings.ToLower(line[1:]))
		if content == "" {
			continue
		}
		tokens = append(tokens, strings.Fields(content)...)
	}
	return tokens
}

// shingles builds overlapping k-token windows. Comparing windows rather than
// individual tokens is what makes this measure structure instead of
// vocabulary - two files can share every keyword and still be unrelated.
func shingles(tokens []string, k int) map[string]struct{} {
	set := map[string]struct{}{}
	if len(tokens) < k {
		if len(tokens) > 0 {
			set[strings.Join(tokens, " ")] = struct{}{}
		}
		return set
	}
	for i := 0; i+k <= len(tokens); i++ {
		set[strings.Join(tokens[i:i+k], " ")] = struct{}{}
	}
	return set
}

// LexicalSimilarity is the default Similarity: Jaccard overlap of 5-token
// shingles over each diff's added lines.
func LexicalSimilarity(a, b string) float64 {
	const k = 5
	sa := shingles(normalizeDiff(a), k)
	sb := shingles(normalizeDiff(b), k)
	if len(sa) == 0 || len(sb) == 0 {
		return 0
	}
	intersection := 0
	// Iterate the smaller set for the cheaper pass.
	small, large := sa, sb
	if len(sb) < len(sa) {
		small, large = sb, sa
	}
	for s := range small {
		if _, ok := large[s]; ok {
			intersection++
		}
	}
	union := len(sa) + len(sb) - intersection
	if union == 0 {
		return 0
	}
	return float64(intersection) / float64(union)
}

// DuplicateCandidate is one submission entering duplicate detection.
type DuplicateCandidate struct {
	VerdictID string
	Login     string
	Diff      string
}

// DuplicatePair is a flagged pair, ordered so Later is the one that gets
// flagged - the earlier submission is left alone.
type DuplicatePair struct {
	EarlierVerdictID string  `json:"earlier_verdict_id"`
	LaterVerdictID   string  `json:"later_verdict_id"`
	Similarity       float64 `json:"similarity"`
	SameAuthor       bool    `json:"same_author"`
}

// FindDuplicates implements AI-specs.md §5.2's pairwise comparison.
//
// §5.2: "In-memory embeddings with pairwise cosine comparison is sufficient
// at this scale. Do not build a vector database - there is nothing to
// retrieve." Same reasoning applies here: this is O(n^2) over one event's
// merged PRs, which is hundreds, not millions.
//
// Flagged pairs are for human review, never auto-rejection - "two people
// independently fixing the same obvious bug happens." Nothing in this
// function rejects anything; it only marks pairs worth a look.
//
// candidates must already be in submission order; the later of each pair is
// the one flagged.
func FindDuplicates(candidates []DuplicateCandidate, threshold float64, sim Similarity) []DuplicatePair {
	if sim == nil {
		sim = LexicalSimilarity
	}
	var pairs []DuplicatePair
	for i := 0; i < len(candidates); i++ {
		for j := i + 1; j < len(candidates); j++ {
			score := sim(candidates[i].Diff, candidates[j].Diff)
			if score < threshold {
				continue
			}
			pairs = append(pairs, DuplicatePair{
				EarlierVerdictID: candidates[i].VerdictID,
				LaterVerdictID:   candidates[j].VerdictID,
				Similarity:       score,
				// Same-author near-duplicates are a different problem from
				// cross-account ones - one person splitting work across PRs
				// versus a farm. Recorded so a reviewer can tell them apart.
				SameAuthor: strings.EqualFold(candidates[i].Login, candidates[j].Login),
			})
		}
	}
	// Highest similarity first: the most likely duplicates lead the queue.
	sort.SliceStable(pairs, func(a, b int) bool { return pairs[a].Similarity > pairs[b].Similarity })
	return pairs
}
