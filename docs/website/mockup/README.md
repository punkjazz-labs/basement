# Website mockup sources

The approved full-page mockup as of 2026-08-03 (artifact "full-page-v17-seo").

- `full6-template.html` — the page source with `@@LETTERS@@ / @@ICONS@@ / @@B64@@ / @@CONSOLE64@@` placeholders
- `hero-letters.txt` / `hero-band.txt` — generated wordmark letter spans and icon-band rows
- `sparks-b64.txt` / `console-b64.txt` — base64 jpeg assets (NVIDIA marketing photo: rights check pending before launch)
- `basement-full6.html` — the fully-injected build (what the artifact serves)

Rebuild: replace the four placeholders in the template with the corresponding file contents.
OG image + its template live in `../assets/`. Direction and call notes: `../DIRECTION.md`, `../CALL-NOTES.md`.
This whole `docs/website/` tree is meant to move wholesale to the future site repo.
