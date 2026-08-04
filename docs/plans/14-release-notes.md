# Spec 14: release notes in the console

Branch `spec/14-release-notes`. Three commits: release chain, API, console.

## Problem

The update affordance exists and says nothing. `App.tsx` renders one pill in the sidebar
footer, `Update {latest_version} available`, linking to the GitHub release page in a new
tab. The owner has to leave the console, land on GitHub, and read whatever is there to
find out whether the update is worth taking.

Whatever is there is currently machine-generated. `.github/workflows/release.yml`
publishes with `gh release create ... --generate-notes`, so the body is a list of commit
subjects in this repo's internal voice (`manual: 09: update affordance at row level`).
That is a build log, not product copy, and putting it in the console would break the
copy rules on the first release. There is no `CHANGELOG.md` in the repo.

The manager also never reads the body. `updateCheck` in `internal/httpapi/server.go`
decodes only `tag_name` and `html_url` from the GitHub response into an anonymous
struct, and returns an untyped `map[string]any`.

## User-visible outcome

Clicking the update pill opens a dialog that says what changed in the new version, in
the console's voice, with the version and publication date, and one primary action that
opens the release page. When the check failed or the release carries no notes, the
dialog says exactly that instead of showing an empty panel.

## A. The notes get written at tag time, or the release does not publish

The release chain is a tag push into CI, followed by a manual signing pass on the owner's
laptop (`packaging/sign-macos-release.sh`, which re-uploads the darwin assets). Notes
therefore have to exist before the tag, in the repository, because there is no later
step where a human is guaranteed to be present with the right permissions.

1. New `CHANGELOG.md` at the repository root. One `## v0.9.4` section per released tag,
   newest first, written in the console's voice: sentence case, plain verbs, no emoji,
   no em dashes, no claims the code does not back. A short paragraph or a few bullets
   about what the owner will notice, not a commit list.
2. `.github/workflows/release.yml`: before the build, extract the section matching
   `${GITHUB_REF_NAME}` from `CHANGELOG.md` into `dist/RELEASE_NOTES.md`. If the section
   is missing or empty, **fail the workflow** with a message naming the tag. A release
   without notes is a release the console cannot explain, and failing here costs a
   retag, which is cheap; discovering it in the console costs a lie.
3. Replace `--generate-notes` with `--notes-file dist/RELEASE_NOTES.md`. Do not upload
   `RELEASE_NOTES.md` as a release asset; the body is the artifact.
4. Add the changelog section as a step in whatever release runbook exists (start from
   `packaging/sign-macos-release.sh`'s header) so the manual half of the chain names it.

The extraction is a small shell function; keep it in the workflow rather than adding a
script, and make sure `shellcheck packaging/*.sh` in `ci.yml` still passes untouched.

**Acceptance for A.** A tag pushed on a branch with no matching changelog section fails
the workflow at the extract step. Do not create a real release to prove this; test the
extractor as a shell function against fixtures and state in the report that the
end-to-end path was not exercised.

## B. The manager carries the notes

`internal/httpapi/server.go`, `updateCheck` (line 1770) and the fields
`updateMu`/`updateResult`/`updateFetched`.

1. Replace the `map[string]any` with a named struct so the response shape is reviewable
   in one place. Keep every field name exactly as it is today (`current_version`,
   `latest_version`, `update_available`, `checked`, `release_url`, `note`,
   `checked_at`); the console and its type in `webui/ui/src/api.ts` already depend on
   them. Add:
   - `release_notes string` (the release body, markdown, verbatim from GitHub)
   - `release_notes_truncated bool`
   - `published_at string` (RFC3339, from the release's `published_at`)
2. Decode `body` and `published_at` alongside `tag_name` and `html_url`. The existing
   `io.LimitReader(resp.Body, 1<<20)` stays. Cap the body the manager keeps at 32 KB
   after decoding; when it is longer, cut at the last complete line inside the cap and
   set `release_notes_truncated`. Never cut mid-line: a half-sentence about what changed
   is worse than a short one.
3. The 1-hour cache is unchanged and now also covers the notes. The whole handler still
   holds `updateMu` across the network call; leave that alone, it is not this spec's
   problem.
4. `update_available` stays the string comparison it is today. Do not introduce semver
   ordering here; see the open question.

**Tests** (`internal/httpapi/server_test.go` already fakes HTTP): a release with a body
returns it verbatim; a body over the cap is truncated on a line boundary with the flag
set; a release with an empty body returns an empty string and no flag; a 404 keeps
today's `note` and adds nothing; a transport failure still returns 200 with
`checked: false` and empty notes; the second call inside the hour does not hit the fake
server and returns the same notes.

## C. The console reads them

Mockup-gated: this is a new dialog, not an edit to an existing one. Produce the static
mockup, get the owner's approval, then build.

1. `webui/ui/src/api.ts`: extend `UpdateInfo` with the three new fields, all optional,
   and add the `checked_at` field that the server already sends and the type has always
   omitted.
2. `webui/ui/src/App.tsx`: the sidebar pill becomes a button that opens a dialog instead
   of an anchor to GitHub. Dialog contents:
   - Title: `Update available` with `v{latest_version}` and, when `published_at` is
     present, the date in the format the rest of the console uses. Check what `Activity`
     does with timestamps and match it; do not invent a second date format.
   - Body: `release_notes` rendered with the existing pattern,
     `DOMPurify.sanitize(marked.parse(text, { async: false }) as string)`, exactly as
     `webui/ui/src/views/Playground.tsx:36` does it. No new dependency.
   - When truncated, one quiet line under the notes: `The rest of the notes are on
     GitHub.`
   - When `release_notes` is empty: `This release did not come with notes.` Do not
     render an empty panel and do not apologise.
   - When `checked` is false: the pill does not appear at all (today's behaviour, since
     `update_available` is false), so nothing to do. State that in the report rather
     than adding a state nobody can reach.
   - Actions: one primary pill `Open release page` (the existing `release_url`, new
     tab), one ghost `Close`. One primary per dialog, per the design system.
3. Copy honesty: the dialog says what the update contains. It must not say or imply that
   the console can apply it. Applying is spec 16 and does not exist yet, so the dialog
   ends at the link, and the copy stays `Open release page`, not `Update now`.

**Acceptance for C.** Playwright mock harness per the conventions, with fixtures for:
notes present, notes truncated, notes empty. Screenshots of each. `npx tsc --noEmit` and
`npm run build` green, committed assets regenerated (CI compares them).

## Open questions (owner)

- **Spanning several releases.** An owner two versions behind sees only the newest
  release's notes. Showing everything in between needs the releases list endpoint and a
  real version ordering, which the manager deliberately does not have today (the check
  is a string inequality). Is "what is in the newest release" enough for v1, or should
  the manager learn semver ordering as part of this work?
- **Changelog authorship.** The changelog is written by whoever tags. Should it be
  assembled from the spec pack (each merged spec appends its own user-visible line), or
  written fresh per release? The first keeps it accurate, the second keeps it readable.
- **Private repository.** `updateRepository` is `punkjazz-labs/basement` and the GitHub
  API call is unauthenticated. While the repository is private, the check gets a 404 and
  the console shows nothing, so none of this is visible until the repository is public
  or the release feed moves. Confirm which of those is the plan before B ships.
