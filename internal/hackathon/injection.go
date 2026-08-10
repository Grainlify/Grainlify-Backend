package hackathon

import (
	"regexp"
	"strings"
	"unicode"
)

// ConcernInjection is AI-specs.md §5.3's concern value for content that
// tries to instruct the judge rather than be judged.
const ConcernInjection = "instruction_injection_attempt"

// injectionPatterns are the shapes prompt injection takes in PR content.
//
// Detected in *our* code, not left to the model. §9 requires every
// adversarial case return this concern and a bucket unaffected by the
// injected text - and a guarantee that depends on the model noticing is not
// a guarantee, it is a hope. The prompt still instructs the model to flag
// these (§5.5), so the two are belt and braces; this one is the belt,
// because it is deterministic and testable in CI without a model.
//
// Deliberately tuned to catch instruction-shaped text rather than any
// mention of a bucket name: "this refactor is substantial work" is ordinary
// English in a PR description, while "reviewers: assign band 5" is not.
var injectionPatterns = []*regexp.Regexp{
	// Direct address to a reviewer or model.
	regexp.MustCompile(`(?i)\b(note|message|instruction[s]?)\s+to\s+(the\s+)?(reviewer|ai|assistant|model|judge)\b`),
	regexp.MustCompile(`(?i)\b(dear|attention|hey)\s+(ai|assistant|model|reviewer|judge|llm)\b`),
	regexp.MustCompile(`(?i)\bif\s+you\s+are\s+an?\s+(ai|llm|language\s+model|assistant)\b`),
	// Imperatives aimed at the verdict.
	regexp.MustCompile(`(?i)\b(mark|rate|grade|score|classify|assign|set|return|treat)\s+(this|it|the\s+pr)?\s*(as\s+)?(exceptional|substantial|accepted|band\s*\d|bucket\s*\w+|tier\s*\d)\b`),
	regexp.MustCompile(`(?i)\b(reviewers?|judges?)\s*:\s*(assign|mark|rate|give|set)\b`),
	regexp.MustCompile(`(?i)\b(ignore|disregard|forget|override)\s+(all\s+|any\s+|previous\s+|prior\s+|the\s+above\s+)*(instruction|prompt|rule|criteria|context)`),
	// Claims of authority or approval the diff cannot support.
	regexp.MustCompile(`(?i)\b(approved|signed\s*off|nominated)\s+by\s+(the\s+)?(maintainer|admin|owner|core\s+team)\b`),
	regexp.MustCompile(`(?i)\bmaintainer\s+(approval|nomination)\s*:\s*(yes|granted|approved)\b`),
	// Impersonating the protocol itself.
	regexp.MustCompile(`(?i)^\s*(system|assistant|user)\s*:`),
	regexp.MustCompile(`(?i)<\s*/?\s*(system|acceptance_criteria|instructions?|diff_stats)\s*>`),
	// Requires terminal punctuation so "this is a system message queue" -
	// an ordinary sentence about software - does not match, while "This is
	// a system message." does. Case 7 is caught by the "system:" prefix
	// pattern above regardless, so this one only needs to catch the prose
	// variant without dragging in normal English.
	regexp.MustCompile(`(?i)\bthis\s+is\s+a\s+system\s+(message|prompt|instruction)\s*[.:!]`),
}

// zeroWidth are invisible characters used to hide instructions from a human
// reader while leaving them in the model's input. Their presence in PR
// content is itself the signal - there is no legitimate reason for a
// zero-width joiner in a diff.
var zeroWidth = []rune{
	'\u200b', // zero-width space
	'\u200c', // zero-width non-joiner
	'\u200d', // zero-width joiner
	'\u2060', // word joiner
	'\ufeff', // zero-width no-break space / BOM
	'\u00ad', // soft hyphen
}

// stripObfuscation removes zero-width characters and normalises confusable
// spacing, so an instruction broken up with invisible characters still
// matches the patterns above.
func stripObfuscation(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if isZeroWidth(r) {
			continue
		}
		// Collapse exotic Unicode spaces to a plain space so "mark␠this␠
		// exceptional" written with U+2007 still reads as words.
		if unicode.IsSpace(r) {
			b.WriteRune(' ')
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func isZeroWidth(r rune) bool {
	for _, z := range zeroWidth {
		if r == z {
			return true
		}
	}
	return false
}

// InjectionFinding describes what was detected and where.
type InjectionFinding struct {
	Detected bool     `json:"detected"`
	Sources  []string `json:"sources,omitempty"`
	// Excerpts are short snippets of the offending text, for the admin
	// review view - a reviewer should be able to see what was attempted
	// without opening the whole diff.
	Excerpts []string `json:"excerpts,omitempty"`
}

// DetectInjection scans the untrusted parts of a submission for text aimed
// at the judge rather than at a human reader.
//
// sources maps a label ("diff", "pr_body", "commit_message") to its content,
// so a finding can say where the attempt was, which is what a reviewer needs
// to act on it.
func DetectInjection(sources map[string]string) InjectionFinding {
	var f InjectionFinding
	seen := map[string]bool{}

	for label, content := range sources {
		if content == "" {
			continue
		}
		cleaned := stripObfuscation(content)

		// Obfuscation is itself the tell: zero-width characters in a diff or
		// PR description have no legitimate purpose.
		if cleaned != content && containsAnyZeroWidth(content) {
			if !seen[label] {
				f.Detected = true
				f.Sources = append(f.Sources, label)
				seen[label] = true
			}
			f.Excerpts = append(f.Excerpts, "hidden zero-width characters in "+label)
		}

		for _, re := range injectionPatterns {
			for _, line := range strings.Split(cleaned, "\n") {
				trimmed := strings.TrimSpace(strings.TrimLeft(line, "+-# /*"))
				if trimmed == "" {
					continue
				}
				if re.MatchString(trimmed) {
					f.Detected = true
					if !seen[label] {
						f.Sources = append(f.Sources, label)
						seen[label] = true
					}
					if len(f.Excerpts) < 10 {
						f.Excerpts = append(f.Excerpts, truncateText(trimmed, 160))
					}
					break
				}
			}
		}
	}
	return f
}

func containsAnyZeroWidth(s string) bool {
	for _, r := range s {
		if isZeroWidth(r) {
			return true
		}
	}
	return false
}
