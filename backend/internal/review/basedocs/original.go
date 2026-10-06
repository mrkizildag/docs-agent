package basedocs

import (
	"fmt"
	"strings"

	"github.com/mrkizildag/pollux-agent/backend/internal/docs"
	"github.com/mrkizildag/pollux-agent/backend/internal/review"
)

// FillOriginal sets p.Original and p.Lines from the section of doc that p
// replaces. It does nothing for a new-doc proposal (empty Section) and returns
// an error listing doc's headings when p.Section does not name exactly one of
// them. p.Section must already be review.NormalizeSection'd.
func FillOriginal(p *review.Proposal, doc docs.Doc) error {
	if p.Section == "" {
		return nil
	}
	text, start, end, ok := doc.SectionSpan(p.Section)
	if !ok {
		var headings []string
		for _, s := range doc.Sections {
			if s.Level > 0 {
				headings = append(headings, fmt.Sprintf("%q", s.Heading))
			}
		}
		return fmt.Errorf("section %q: not exactly one such heading in %s; headings are: %s", p.Section, doc.Path, strings.Join(headings, ", "))
	}
	p.Original, p.Lines = text, review.LineRange{Start: start, End: end}
	return nil
}
