# basement: site direction

Decisions so far (2026-08-02): the project's public identity is **basement**, first
hosted at **basement.punkjazz.ai**. Direction **B (one ink)** is chosen: red #d92500
on paper #f2ede3, Didot/Bodoni display cropping off-canvas, typewriter caps furniture,
typography leads everything. Directions A and C are dropped as site skins; the flyer
survives only as a possible printable campaign asset inside B's world. Current
explorations within B: B1 refined original, B2 Lichtenstein Ben-Day (comic burst
"2 Sparks. Free local intelligence!", never prices), B3 Eigen-Labs-flavored minimal
manifesto (numbered conviction lines, huge whitespace). Tagline in use:
"Stop renting intelligence."

Status: exploration, not a spec. Nothing here is buildable until the owner picks a
direction from static mockups. The site will likely live in its own repo; this doc
sits here until that repo exists.

## Job of the site

Not a product landing page. It is the manifesto and reference point for a movement:
local AI on your own hardware, dual-Spark as the flagship experience. Success looks
like: someone who is *considering* a Spark lands here to do their thinking (what can I
run, what does it cost, can I trust it), and someone who has Sparks lands here to feel
represented. The software download is almost a footnote of the page, the way the
Bitcoin client was the footnote of a much bigger idea.

Audience order: 1) people about to buy or just bought GB10 hardware, 2) local-AI
curious pro users, 3) the small firm (the lawyer test) that fears cloud AI, 4) press
and X onlookers who screenshot things.

## Structure (settled)

Borrowed from Anthropic's "Keep thinking." page, which nails statement-first editorial:

1. **Statement hero.** One declaration in enormous type. Working line:
   `Local AI is finally competitive.` Alternatives to test in mockups:
   `Your intelligence. Your hardware.` / `Stop renting intelligence.`
   One short paragraph. One action.
2. **Explorable proof canvas.** A loose, wanderable field of real artifacts, clustered
   around questions (`What can I run?`, `What does it cost?`, `Can I trust it?`,
   `Who is doing this?`). Every tile is verifiable substance, never decoration:
   - the Pareto chart: capability vs cost, local models against dead cloud pricing
   - the cost calculator (hardware amortization + electricity vs API/subscription)
   - the memory-fit calculator (shared math with the console, spec 05)
   - each curated model as a card with measured tok/s from real benchmark receipts
   - community deployments and posts; the dual-Spark benchmark table
   - the trust page: pinning, receipts, verification, read-only access patterns
   - buy links for the hardware (affiliate later)
   Credibility through density of real work. This is also the only marketing style
   compatible with the no-invented-facts rule: every number on the site traces to a
   receipt or a source.
3. **The software**, presented late and simply: one screenshot of the console, one
   install command, one download button. Hermes-agent's install block (native button +
   copyable one-liner, OS tabs) is the reference for this fragment.

## Visual direction: three candidates to mockup

The console keeps its own identity (dark, dense, NVIDIA green). The site is the
movement, not the appliance, and should NOT reuse the console's skin. NVIDIA green is
dropped as the site accent: it reads as vendor property, and the movement must feel
from-the-people. Green may appear only where it factually denotes the hardware.

### A. The offer flyer (giga's flag)

The language of the supermarket promo flyer, weaponized: harsh red/yellow/white,
jagged starburst "splash" shapes, giant prices, halftone product shots of the two
Sparks, tabloid grid, urgency furniture. `LOCAL AI. FINALLY COMPETITIVE.` /
starburst: `€0/token FOREVER` / `NO SUBSCRIPTION. NO LANDLORD.` / small print that is
actually honest (real wattage, real tok/s, real amortization months).
Why it works: instantly iconic, screenshots well, inherently anti-corporate, and the
flyer is *literally printable*: a PDF people pin above their desks becomes the growth
asset, and "Add one Spark" campaigns reuse the same visual language.
Risk: kitsch collapse. Needs a rigid grid and one disciplined type family underneath
the chaos, and every number real, or it becomes the slop it mocks.

### B. One-ink world (the Hermes structural lesson)

What hermes-agent.nousresearch.com actually teaches: commit to ONE ink on paper and
let engravings, halftones, and type all live in that single color; oversized fashion
wordmark bleeding off the canvas; typewriter caps for labels; numbered features. It
reads as a zine/print object, not a website, which is exactly the "impresentabile ma
iconico" quality. Constraint: blue is owned by Nous. Candidate inks on off-white:
signal red, safety orange, or black with yellow paper. Duotone the Spark photography
and vintage engineering engravings into the ink.
Risk: elegant-underground rather than punk-populist; may undersell the "for everyone,
even your lawyer" promise.

### C. Flyer artifact inside a quiet zine (hybrid, likely winner)

The site itself is direction B's restrained one-ink zine; the HERO ARTIFACT is
direction A's flyer, rendered as a physical printed object (slightly rotated, shadow,
staple) that you can zoom, download as PDF, and print. The canvas tiles inherit the
zine style; the flyer stays the loud centerpiece. Bold where it counts, disciplined
everywhere else, and the meme-able artifact still exists as a first-class thing.

## Copy register

From-the-people, declarative, zero corporate vocabulary, zero hedging, numbers with
receipts. Words never used: revolutionize, empower, seamless, unleash, democratize
(the friend's explicit veto), decentralized. The site says what a thing costs and what
it does; screenshots of real terminal output beat adjectives.

## Open questions

- Whether Spark product photography can be used and under what terms; halftone
  treatment likely transforms it enough, verify before launch.
- Pareto chart data sourcing: must be our own benchmarks + published prices, dated.
- Where the site repo lives and how the console's benchmark receipts feed it
  (`spec 04`'s index is the natural pipe).

## Next step

Claude produces static mockups of directions A, B, C (same content, three skins:
hero + a slice of the canvas + the install fragment). the owner picks; the winner gets a
real spec with a token system. Per the design loop, no production code before that.
