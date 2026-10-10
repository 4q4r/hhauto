// T116: spintax rendering of template cover letters. The owner writes
// {variant1|variant2} groups (nesting ≤ 2) straight into the letter
// template; renderSpintax picks one variant per group with the provided
// random source. Groups without a separator and unbalanced braces pass
// through verbatim so template mistakes stay visible, and deeper nesting
// is left literal instead of failing the letter.

package multiapply

import (
	"math/rand/v2"
	"strings"
)

// renderSpintax resolves {a|b} groups in text using rnd. At most two
// nesting levels resolve; a third-level group stays literal. Text without
// resolvable groups (placeholders included — {{.Field}} carries no
// separator) is returned unchanged.
func renderSpintax(text string, rnd *rand.Rand) string {
	return spinDepth(text, rnd, 1)
}

func spinDepth(text string, rnd *rand.Rand, depth int) string {
	if depth > 2 {
		return text
	}
	var b strings.Builder
	for i := 0; i < len(text); {
		if text[i] == '{' {
			if end, ok := spintaxGroup(text, i); ok {
				variants := spinVariants(text[i+1 : end])
				if len(variants) > 1 {
					b.WriteString(spinDepth(variants[rnd.IntN(len(variants))], rnd, depth+1))
					i = end + 1
					continue
				}
			}
		}
		b.WriteByte(text[i])
		i++
	}
	return b.String()
}

// spintaxGroup reports the index of the brace closing the one at start,
// counting nested braces; ok is false when the group never closes.
func spintaxGroup(text string, start int) (end int, ok bool) {
	depth := 0
	for i := start; i < len(text); i++ {
		switch text[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i, true
			}
		}
	}
	return 0, false
}

// spinVariants splits group content on top-level pipes (nested braces are
// skipped); content without a separator yields a single variant.
func spinVariants(content string) []string {
	var variants []string
	depth, start := 0, 0
	for i := 0; i < len(content); i++ {
		switch content[i] {
		case '{':
			depth++
		case '}':
			depth--
		case '|':
			if depth == 0 {
				variants = append(variants, content[start:i])
				start = i + 1
			}
		}
	}
	return append(variants, content[start:])
}

// spinLetter wraps a rendered template letter with renderSpintax when
// [features.multiapply.cover_letter].spin is on. Only the template and
// template-fallback returns of buildLetter pass through it — AI letters
// vary naturally and must not be rewritten.
func (f *Feature) spinLetter(letter string, err error) (string, error) {
	if err != nil || !f.opt.Config.Features.MultiApply.CoverLetter.Spin {
		return letter, err
	}
	rnd := rand.New(rand.NewPCG(uint64(f.opt.Now().UnixNano()), 1))
	return renderSpintax(letter, rnd), nil
}
