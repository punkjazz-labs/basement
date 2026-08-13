package docredact

// ModelChunkSize and ModelChunkOverlap are the default window and overlap,
// in bytes, ChunkText uses to split a document for the model pass (spec
// step 3). The overlap is a few hundred characters -- enough that a literal
// split across a naive boundary still lands whole inside the overlap region
// of at least one chunk, without inflating the number of model calls a long
// document needs.
const (
	ModelChunkSize    = 6000
	ModelChunkOverlap = 400
)

// snapWindow bounds how far back from a chunk's natural end ChunkText will
// look for a newline or space to break on. It is tidiness, not correctness:
// the overlap below is what actually guarantees no literal is lost, so this
// only has to be big enough to usually find a word boundary, not exact.
const snapWindow = 200

// ChunkText splits text into windows of at most size bytes, each consecutive
// pair sharing overlap bytes, so a caller sending each chunk to a model
// separately never loses a literal that happened to fall on a chunk
// boundary -- it appears whole in the shared overlap of at least one chunk.
// A chunk boundary snaps back to the nearest newline or space within the
// last snapWindow bytes when one exists, so literals aren't split mid-word
// any more than the overlap already makes unnecessary. Text no longer than
// size is returned unchanged as the only chunk. size must be greater than
// overlap, or there is no window left to advance by; a caller that violates
// this is a programmer error, so ChunkText panics rather than loop forever.
func ChunkText(text string, size, overlap int) []string {
	if size <= overlap {
		panic("docredact: ChunkText size must be greater than overlap")
	}
	if len(text) <= size {
		return []string{text}
	}

	var chunks []string
	start := 0
	for {
		end := start + size
		if end >= len(text) {
			chunks = append(chunks, text[start:])
			break
		}

		snapFrom := end - snapWindow
		if snapFrom < start {
			snapFrom = start
		}
		if i := lastBreak(text[snapFrom:end]); i >= 0 {
			end = snapFrom + i + 1
		}

		chunks = append(chunks, text[start:end])

		next := end - overlap
		if next <= start {
			// Snapping ate the whole window this iteration; fall back to
			// the unsnapped boundary so start always advances.
			next = end
		}
		start = next
	}
	return chunks
}

// lastBreak returns the index of the last newline or space in s, or -1 if
// there is none.
func lastBreak(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '\n' || s[i] == ' ' {
			return i
		}
	}
	return -1
}
