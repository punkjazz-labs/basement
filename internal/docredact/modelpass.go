package docredact

import (
	"context"
	"errors"
	"strings"
)

// Completer is one round to a model: system and user prompt in, raw text
// reply out. This package never sees the transport underneath it.
type Completer interface {
	Complete(ctx context.Context, system, user string, structured bool) (string, error)
}

// ErrStructuredUnsupported is returned by a Completer whose backend rejected
// response_format structured output; ApplyModelPass retries the same chunk
// unstructured and stops asking for structured output on every later chunk,
// since a backend that rejects it once will reject it again.
var ErrStructuredUnsupported = errors.New("structured output unsupported")

// modelPassSystemPrompt asks the model to name text that could identify a
// person, and only that -- copied verbatim, never invented, so every
// literal it returns can be checked against the document the same way a
// manual selection is.
const modelPassSystemPrompt = `Find every piece of text in the document that could help identify a specific person: full names, organisations, street addresses, job titles, and amounts of money tied to a person.

Reply with ONLY a JSON array, nothing else -- no prose, no markdown fence. Each element is {"literal": "<exact substring copied verbatim from the document>", "category": "person|org|address|job_title|amount"}.

Copy each literal exactly as it appears in the document: same spelling, spacing, and punctuation. Do not invent or paraphrase text that is not in the document. If nothing qualifies, reply with an empty array: [].`

// modelRepairPrompt asks the model to fix a reply ApplyModelPass could not
// parse as a JSON array, folding the previous reply in so the model has
// something concrete to correct rather than starting over blind.
const modelRepairPrompt = "Your previous reply was not a JSON array. Reply with only the JSON array, nothing else.\n\nPrevious reply:\n"

// modelMatches finds every non-overlapping occurrence of a literal the
// model pass accepted.
func modelMatches(text, literal string, category Category) []Match {
	return literalMatches(text, literal, category, SourceModel)
}

// ModelPassResult tallies what one call to ApplyModelPass did with the
// model's answers. Every field is a count over the whole document, not a
// per-chunk figure: a literal returned by two chunks is one literal here.
type ModelPassResult struct {
	Accepted     int  `json:"accepted"`     // model literals that produced findings
	Duplicates   int  `json:"duplicates"`   // literals another pass already covers
	Hallucinated int  `json:"hallucinated"` // literals not present in the document
	ChunksTotal  int  `json:"chunks_total"`
	ChunksFailed int  `json:"chunks_failed"` // unusable after repair retry or transport error
	Degraded     bool `json:"degraded"`      // no chunk was usable: pattern-only stands
}

// literalKnown reports whether literal already has a finding, or is already
// queued as a manual or model literal, from an earlier pass. Checked before
// a model literal is accepted so re-running the model pass, or a model
// literal echoing what the owner already picked by hand, never double-counts.
func (d *Document) literalKnown(literal string) bool {
	if _, ok := d.byLiteral[literal]; ok {
		return true
	}
	for _, m := range d.manual {
		if m.literal == literal {
			return true
		}
	}
	for _, m := range d.model {
		if m.literal == literal {
			return true
		}
	}
	return false
}

// ApplyModelPass sends the document to c in chunks, asking it to name text
// that could identify a person, and folds every literal it can verify into
// the document's findings alongside the pattern and manual passes.
//
// The model's answer is treated as a claim, never a fact: a literal that
// does not appear in the document verbatim is dropped and counted
// Hallucinated rather than trusted, and a literal that survives is still
// subject to the same longest-literal-wins overlap resolution as every
// other source. The only error this returns is ctx cancellation -- a model
// that answers badly, or a chunk that fails outright, is data (ChunksFailed,
// Degraded), not a reason to fail the call.
func (d *Document) ApplyModelPass(ctx context.Context, c Completer) (ModelPassResult, error) {
	if err := ctx.Err(); err != nil {
		return ModelPassResult{}, err
	}

	chunks := ChunkText(d.Text, ModelChunkSize, ModelChunkOverlap)
	result := ModelPassResult{ChunksTotal: len(chunks)}

	structured := true
	seen := make(map[string]bool)
	var items []ModelItem

	for _, chunk := range chunks {
		if err := ctx.Err(); err != nil {
			return ModelPassResult{}, err
		}

		reply, err := c.Complete(ctx, modelPassSystemPrompt, chunk, structured)
		if errors.Is(err, ErrStructuredUnsupported) {
			// This backend will keep rejecting it, so every later chunk
			// skips straight to unstructured instead of paying for the
			// same doomed attempt again.
			structured = false
			reply, err = c.Complete(ctx, modelPassSystemPrompt, chunk, false)
		}
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ModelPassResult{}, ctxErr
			}
			result.ChunksFailed++
			continue
		}

		parsed, err := ExtractModelItems(reply)
		if err != nil {
			repaired, rerr := c.Complete(ctx, modelPassSystemPrompt, modelRepairPrompt+reply, false)
			if rerr != nil {
				if ctxErr := ctx.Err(); ctxErr != nil {
					return ModelPassResult{}, ctxErr
				}
				result.ChunksFailed++
				continue
			}
			parsed, err = ExtractModelItems(repaired)
			if err != nil {
				result.ChunksFailed++
				continue
			}
		}

		for _, item := range parsed {
			literal := strings.TrimSpace(item.Literal)
			if literal == "" || seen[literal] {
				continue
			}
			seen[literal] = true
			items = append(items, ModelItem{Literal: literal, Category: item.Category})
		}
	}

	var accepted []string
	for _, item := range items {
		switch {
		case !strings.Contains(d.Text, item.Literal):
			result.Hallucinated++
		case d.literalKnown(item.Literal):
			result.Duplicates++
		default:
			category, _ := ParseCategory(item.Category)
			d.model = append(d.model, manualLiteral{literal: item.Literal, category: category})
			accepted = append(accepted, item.Literal)
		}
	}

	d.recompute()

	for _, literal := range accepted {
		// A literal queued above can still lose the overlap contest to a
		// longer finding from another source; that is neither acceptance
		// nor a hallucination, so it counts as a duplicate instead.
		if f, ok := d.byLiteral[literal]; !ok || f.Source != SourceModel {
			result.Duplicates++
		}
	}
	for _, f := range d.Findings {
		if f.Source == SourceModel {
			result.Accepted++
		}
	}

	result.Degraded = result.ChunksFailed == result.ChunksTotal
	return result, nil
}
