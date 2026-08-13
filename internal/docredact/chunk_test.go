package docredact

import (
	"strconv"
	"strings"
	"testing"
)

func TestChunkTextShortTextIsOneChunk(t *testing.T) {
	got := ChunkText("hello", 100, 10)
	if len(got) != 1 || got[0] != "hello" {
		t.Fatalf("got %#v", got)
	}
}

func TestChunkTextCoversEveryByteAndOverlaps(t *testing.T) {
	// A strictly repeated filler (strings.Repeat("lorem ipsum...", n)) is
	// exactly periodic with a period far shorter than the chunk size, so
	// strings.Index below would always find a chunk's content aliased onto
	// an earlier copy near the start of the text rather than its real
	// position -- a property of periodic text vs. leftmost-match search,
	// not something any chunker could satisfy. Numbering each repeat keeps
	// the text non-periodic so a found match is the real one.
	var sb strings.Builder
	for i := 0; i < 600; i++ {
		sb.WriteString("lorem ipsum dolor sit amet ")
		sb.WriteString(strconv.Itoa(i))
		sb.WriteString(" ")
	}
	text := sb.String() // ~16k
	chunks := ChunkText(text, 6000, 400)
	if len(chunks) < 3 {
		t.Fatalf("expected several chunks, got %d", len(chunks))
	}
	// every chunk is a substring at increasing positions, and the whole text is covered
	pos := 0
	covered := 0
	for _, c := range chunks {
		i := strings.Index(text[pos:], c)
		if i < 0 {
			t.Fatal("chunk is not a substring in order")
		}
		start := pos + i
		if start > covered {
			t.Fatalf("gap: chunk starts at %d but only %d covered", start, covered)
		}
		if start+len(c) > covered {
			covered = start + len(c)
		}
		pos = start
	}
	if covered != len(text) {
		t.Fatalf("covered %d of %d bytes", covered, len(text))
	}
}

func TestChunkTextStraddlingLiteralSurvives(t *testing.T) {
	pad := strings.Repeat("x ", 2995)
	literal := "mario.rossi@example.com"
	text := pad + literal + strings.Repeat(" y", 2000)
	found := false
	for _, c := range ChunkText(text, 6000, 400) {
		if strings.Contains(c, literal) {
			found = true
		}
	}
	if !found {
		t.Fatal("literal split across every chunk")
	}
}
