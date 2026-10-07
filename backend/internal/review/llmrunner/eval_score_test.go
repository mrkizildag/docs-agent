//go:build eval

package llmrunner_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/mrkizildag/pollux-agent/backend/internal/llm"
	"github.com/mrkizildag/pollux-agent/backend/internal/review"
)

type runScore struct {
	VerdictOK      bool     `json:"verdict_ok"`
	Recall         float64  `json:"recall"`
	Precision      float64  `json:"precision"`
	SectionsOK     bool     `json:"sections_ok"`
	FactCoverage   float64  `json:"fact_coverage"`
	Contradictions []string `json:"contradictions,omitempty"`
	Pass           bool     `json:"pass"`
}

type factJudgement struct {
	Facts []struct {
		Fact   string `json:"fact"`
		Stated bool   `json:"stated"`
	} `json:"facts"`
	Contradictions []string `json:"contradictions"`
}

// docJudgement is the judge's verdict on one matched expected doc.
type docJudgement struct {
	Doc    string         `json:"doc"`
	Output *factJudgement `json:"output,omitempty"`
	Error  string         `json:"error,omitempty"`
}

func globMatch(pattern, name string) bool {
	ok, err := path.Match(pattern, name)
	return err == nil && ok
}

func verdictKind(v review.Verdict) string {
	switch v.(type) {
	case review.Proposals:
		return verdictProposals
	case review.NoImpact:
		return verdictNoImpact
	default:
		return ""
	}
}

func matchesDoc(d evalDoc, p review.Proposal) bool {
	return globMatch(d.Path, p.DocPath) && d.New == (p.Section == "")
}

type structureScore struct {
	recall     float64
	precision  float64
	sectionsOK bool
	// matches[i] holds the proposals that match expect.Docs[i].
	matches [][]review.Proposal
}

func scoreProposals(expect evalExpect, props review.Proposals) structureScore {
	s := structureScore{recall: 1, precision: 1, sectionsOK: true, matches: make([][]review.Proposal, len(expect.Docs))}

	matchedDocs := 0
	for i, d := range expect.Docs {
		for _, p := range props {
			if matchesDoc(d, p) {
				s.matches[i] = append(s.matches[i], p)
			}
		}
		if len(s.matches[i]) == 0 {
			continue
		}
		matchedDocs++
		if !d.New && len(d.Sections) > 0 && !anySectionListed(s.matches[i], d.Sections) {
			s.sectionsOK = false
		}
	}
	if len(expect.Docs) > 0 {
		s.recall = float64(matchedDocs) / float64(len(expect.Docs))
	}

	if len(props) > 0 {
		expected := 0
		for _, p := range props {
			isExpected := false
			for _, d := range expect.Docs {
				isExpected = isExpected || matchesDoc(d, p)
			}
			for _, g := range expect.Allow {
				isExpected = isExpected || globMatch(g, p.DocPath)
			}
			if isExpected {
				expected++
			}
		}
		s.precision = float64(expected) / float64(len(props))
	}
	return s
}

func anySectionListed(props []review.Proposal, sections []string) bool {
	for _, p := range props {
		got := strings.TrimSpace(strings.TrimLeft(p.Section, "# "))
		for _, want := range sections {
			if strings.EqualFold(got, want) {
				return true
			}
		}
	}
	return false
}

const judgeSystem = `You check whether proposed documentation text states given facts.
For each fact, decide whether the proposed text states it, in any wording, without contradicting it.
Also list every claim in the proposed text that contradicts one of the facts.
Reply with only JSON: {"facts":[{"fact":"<fact verbatim>","stated":true}],"contradictions":["<claim>"]}
The facts array has one entry per fact, in the order given.`

func judgePrompt(facts []string, props []review.Proposal) string {
	var b strings.Builder
	b.WriteString("Facts:\n")
	for i, f := range facts {
		fmt.Fprintf(&b, "%d. %s\n", i+1, f)
	}
	b.WriteString("\nProposed text:\n")
	for _, p := range props {
		fmt.Fprintf(&b, "\n--- proposal for %s (section %q)\nReason: %s\n%s\n", p.DocPath, p.Section, p.Reason, p.Content)
	}
	return b.String()
}

// parseJudgeReply extracts the JSON object from a reply that may be wrapped in
// a code fence or prose, and checks it answers every fact.
func parseJudgeReply(text string, wantFacts int) (factJudgement, error) {
	start, end := strings.Index(text, "{"), strings.LastIndex(text, "}")
	if start < 0 || end < start {
		return factJudgement{}, errors.New("judge reply has no JSON object")
	}
	var j factJudgement
	if err := json.Unmarshal([]byte(text[start:end+1]), &j); err != nil {
		return factJudgement{}, fmt.Errorf("decode judge reply: %w", err)
	}
	if len(j.Facts) != wantFacts {
		return factJudgement{}, fmt.Errorf("judge reply has %d facts, want %d", len(j.Facts), wantFacts)
	}
	return j, nil
}

// judgeFunc sends one judge prompt and returns the reply text.
type judgeFunc func(ctx context.Context, prompt string) (string, error)

// modelJudge judges with an llm.Model, as the server runner does.
func modelJudge(m llm.Model, model string) judgeFunc {
	return func(ctx context.Context, prompt string) (string, error) {
		resp, err := m.Complete(ctx, llm.Request{
			Model:     model,
			System:    judgeSystem,
			Messages:  []llm.Message{{Role: llm.RoleUser, Text: prompt}},
			MaxTokens: 2000,
		})
		if err != nil {
			return "", fmt.Errorf("complete judge request: %w", err)
		}
		return resp.Text, nil
	}
}

// judgeDoc asks the judge about doc's facts, retrying once when the call or
// its reply is unusable.
func judgeDoc(ctx context.Context, judge judgeFunc, doc evalDoc, props []review.Proposal) (factJudgement, error) {
	prompt := judgePrompt(doc.MustSay, props)
	var lastErr error
	for range 2 {
		reply, err := judge(ctx, prompt)
		if err != nil {
			lastErr = fmt.Errorf("judge %s: %w", doc.Path, err)
			continue
		}
		j, err := parseJudgeReply(reply, len(doc.MustSay))
		if err != nil {
			lastErr = fmt.Errorf("judge %s: %w", doc.Path, err)
			continue
		}
		return j, nil
	}
	return factJudgement{}, lastErr
}

// scoreRun scores one finished run against c. The judge is called once per
// matched expected doc that lists facts.
func scoreRun(ctx context.Context, judge judgeFunc, c evalCase, res review.Result) (runScore, []docJudgement) {
	kind := verdictKind(res.Verdict)
	s := runScore{VerdictOK: kind == c.Expect.Verdict, Recall: 1, Precision: 1, SectionsOK: true, FactCoverage: 1}
	if c.Expect.Verdict == verdictNoImpact {
		s.Pass = s.VerdictOK
		return s, nil
	}

	props, _ := res.Verdict.(review.Proposals)
	st := scoreProposals(c.Expect, props)
	s.Recall, s.Precision, s.SectionsOK = st.recall, st.precision, st.sectionsOK

	var judgements []docJudgement
	totalFacts, stated := 0, 0
	for i, d := range c.Expect.Docs {
		totalFacts += len(d.MustSay)
		if len(d.MustSay) == 0 || len(st.matches[i]) == 0 {
			continue
		}
		j, err := judgeDoc(ctx, judge, d, st.matches[i])
		if err != nil {
			judgements = append(judgements, docJudgement{Doc: d.Path, Error: err.Error()})
			continue
		}
		judgements = append(judgements, docJudgement{Doc: d.Path, Output: &j})
		for _, f := range j.Facts {
			if f.Stated {
				stated++
			}
		}
		s.Contradictions = append(s.Contradictions, j.Contradictions...)
	}
	if totalFacts > 0 {
		s.FactCoverage = float64(stated) / float64(totalFacts)
	}

	s.Pass = s.VerdictOK && s.Recall == 1 && s.SectionsOK && s.Precision == 1 && s.FactCoverage == 1 && len(s.Contradictions) == 0
	return s, judgements
}

func TestScoreProposals(t *testing.T) {
	t.Parallel()

	expect := evalExpect{
		Verdict: verdictProposals,
		Docs: []evalDoc{
			{Path: "docs/features/*.md", Sections: []string{"Retention"}},
			{Path: "docs/new.md", New: true},
		},
		Allow: []string{"docs/architecture.md"},
	}
	prop := func(doc, section string) review.Proposal { return review.Proposal{DocPath: doc, Section: section} }

	tests := []struct {
		name  string
		props review.Proposals
		want  structureScore
	}{
		{
			name:  "all matched",
			props: review.Proposals{prop("docs/features/q.md", "## retention"), prop("docs/new.md", "")},
			want:  structureScore{recall: 1, precision: 1, sectionsOK: true},
		},
		{
			name:  "new flag must agree",
			props: review.Proposals{prop("docs/features/q.md", "Retention"), prop("docs/new.md", "Intro")},
			want:  structureScore{recall: 0.5, precision: 0.5, sectionsOK: true},
		},
		{
			name:  "wrong section and allowed extra",
			props: review.Proposals{prop("docs/features/q.md", "Other"), prop("docs/architecture.md", "Overview")},
			want:  structureScore{recall: 0.5, precision: 1, sectionsOK: false},
		},
		{
			name:  "unexpected extra lowers precision",
			props: review.Proposals{prop("docs/features/q.md", "Retention"), prop("docs/other.md", "X")},
			want:  structureScore{recall: 0.5, precision: 0.5, sectionsOK: true},
		},
		{
			name: "no proposals",
			want: structureScore{recall: 0, precision: 1, sectionsOK: true},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := scoreProposals(expect, tc.props)
			got.matches = nil
			if d := cmp.Diff(tc.want, got, cmp.AllowUnexported(structureScore{})); d != "" {
				t.Errorf("scoreProposals() mismatch (-want +got):\n%s", d)
			}
		})
	}
}

func TestScoreParseJudgeReply(t *testing.T) {
	t.Parallel()

	const good = `{"facts":[{"fact":"a","stated":true}],"contradictions":[]}`
	tests := []struct {
		name    string
		text    string
		wantErr bool
	}{
		{name: "plain", text: good},
		{name: "fenced", text: "```json\n" + good + "\n```"},
		{name: "not json", text: "looks fine", wantErr: true},
		{name: "wrong fact count", text: `{"facts":[],"contradictions":[]}`, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := parseJudgeReply(tc.text, 1)
			if (err != nil) != tc.wantErr {
				t.Errorf("parseJudgeReply() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestScoreRun(t *testing.T) {
	t.Parallel()

	c := evalCase{Expect: evalExpect{
		Verdict: verdictProposals,
		Docs:    []evalDoc{{Path: "docs/a.md", Sections: []string{"A"}, MustSay: []string{"one", "two"}}},
	}}
	res := review.Result{Verdict: review.Proposals{{DocPath: "docs/a.md", Section: "A", Content: "one and two"}}}
	stated := `{"facts":[{"fact":"one","stated":true},{"fact":"two","stated":true}],"contradictions":[]}`
	partial := `{"facts":[{"fact":"one","stated":true},{"fact":"two","stated":false}],"contradictions":[]}`
	contradicted := `{"facts":[{"fact":"one","stated":true},{"fact":"two","stated":true}],"contradictions":["says three"]}`

	tests := []struct {
		name       string
		replies    []string
		wantPass   bool
		wantCover  float64
		wantJudged bool
	}{
		{name: "all stated", replies: []string{stated}, wantPass: true, wantCover: 1},
		{name: "partial", replies: []string{partial}, wantCover: 0.5},
		{name: "contradiction", replies: []string{contradicted}, wantCover: 1},
		{name: "retry after bad reply", replies: []string{"nope", stated}, wantPass: true, wantCover: 1},
		{name: "judge keeps failing", replies: []string{"nope", "still nope"}, wantCover: 0, wantJudged: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fm := &fakeModel{}
			for _, r := range tc.replies {
				fm.script = append(fm.script, textResponse(r))
			}
			got, judgements := scoreRun(t.Context(), modelJudge(fm, "judge"), c, res)
			if got.Pass != tc.wantPass || got.FactCoverage != tc.wantCover {
				t.Errorf("scoreRun() pass = %v, coverage = %v, want %v, %v", got.Pass, got.FactCoverage, tc.wantPass, tc.wantCover)
			}
			if len(judgements) != 1 || (judgements[0].Error != "") != tc.wantJudged {
				t.Errorf("scoreRun() judgements = %+v, want one with error = %v", judgements, tc.wantJudged)
			}
		})
	}

	t.Run("no impact expected", func(t *testing.T) {
		t.Parallel()
		got, _ := scoreRun(t.Context(), modelJudge(&fakeModel{}, "judge"), evalCase{Expect: evalExpect{Verdict: verdictNoImpact}}, review.Result{Verdict: review.NoImpact{Reason: "x"}})
		if !got.Pass {
			t.Errorf("scoreRun() pass = false, want true")
		}
		got, _ = scoreRun(t.Context(), modelJudge(&fakeModel{}, "judge"), evalCase{Expect: evalExpect{Verdict: verdictNoImpact}}, res)
		if got.Pass {
			t.Errorf("scoreRun() with proposals pass = true, want false")
		}
	})
}
