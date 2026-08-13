# Doc Redactor Locale Expansion Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Expand the doc redactor's deterministic pass and qualification corpus from IT/US to all major languages: new checksummed national-identifier detectors (FR, ES, PT, DE, NL, UK, BR, CN, JP), date-format coverage for those languages, IBAN length entries, and corpus documents in each language including model-category-only docs for RU and AR.

**Owner directive, 2026-08-13:** "expand to all major languages." The concrete scope below (which locales get detectors, which get corpus-only coverage) is the executor's reading of that directive, recorded here so the owner can trim or extend it. Spec 12 left "which locales ship first" open; this plan answers it.

**Architecture:** Every new detector follows the existing shape exactly (`Detector` interface, own file, category constant + prefix + ParseCategory entry, registry entry, checksum validation where one exists). Detectors with no checksum (UK NINO) use strict structural rules and say so honestly. The corpus grows by ~14 native-language documents whose gold lists are validated by the existing `TestCorpusInvariants` oracle. No engine, model-pass, or API changes.

**Tech Stack:** Go standard library only.

**Spec:** `docs/plans/12-doc-redactor.md` (detection section; "national identifiers for the locales the owner picks"). ADR 0021/0022 bind engine choices. The SSN precedent binds: a bare digit-run that could be anything is NOT matched when a distinctive form exists (hyphenated SSN; grouped MyNumber).

## Global Constraints

- Branch: `spec/12-locale-expansion` off `main`. Merge to main needs the owner's grant (already standing for this session's work stream: owner said "merge to main and push" for the model pass; for THIS branch, ask again at finish).
- Commit style: `manual: <plain lowercase sentence>`. Before every commit: `go build ./... && go vet ./... && go test ./...` green from repo root.
- Go stdlib only. Comments state constraints code cannot show; no process references. Error strings lowercase.
- **Every hardcoded "valid" identifier literal in tests and corpus MUST be verified against the detector's own published algorithm before commit** (write a throwaway checker in /private/tmp, never committed). Where a canonical public test value exists, prefer it (ES DNI `12345678Z`; NL BSN `111222333`; BR CPF `529.982.247-25` — verify each anyway). Invalid-case tests flip one digit/letter of a verified-valid literal.
- Corpus documents: unmistakably synthetic (example.com/.fr/.de/..., 555-style or clearly reserved phone shapes, invented names/companies), native-quality prose in each language, never real people or organizations.
- No modification of existing detectors' accepted/rejected sets except where a task explicitly says so (DOB formats, IBAN table). Existing tests stay green unmodified.
- Category constants extend the existing naming pattern (`it_codice_fiscale`, `us_ssn` →) exactly as specified per task; prefixes stay short and locale-identifying.

## Existing interfaces (read first)

- `internal/docredact/docredact.go`: `Category`, `Prefix()`, `ParseCategory`, `Locales`, `Registry()`, `Detector`, `Match`.
- `internal/docredact/it_codicefiscale.go` + `us_ssn.go`: the shape every new detector copies (regexp candidate scan + structural/checksum validation + word-boundary discipline).
- `internal/docredact/iban.go`: `ibanLength` map (country → exact length) — additive.
- `internal/docredact/dob.go`: numeric slash/dash, ISO, English month-name patterns; `plausibleBirthYear`; `isCalendarDate`.
- `internal/docredact/benchcorpus.go` + `testdata/corpus/` + `benchcorpus_test.go` (`TestCorpusInvariants` is the corpus oracle).
- `docs/research/redactor-model-qualification.md` states corpus counts (13 docs, 6 IT / 7 US) — Task 6 updates it.

---

### Task 1: IBAN length entries + romance-locale identifiers (FR NIR, ES DNI/NIE, PT NIF)

**Files:**
- Modify: `internal/docredact/iban.go` (ibanLength additions), `internal/docredact/docredact.go` (categories, prefixes, ParseCategory, Locales, Registry)
- Create: `internal/docredact/fr_nir.go`, `internal/docredact/es_dni.go`, `internal/docredact/pt_nif.go`
- Test: `internal/docredact/fr_nir_test.go`, `es_dni_test.go`, `pt_nif_test.go`, extend `iban_test.go`

**Interfaces produced:** `CategoryFRNIR Category = "fr_nir"` (prefix `NIR`), `CategoryESDNI Category = "es_dni"` (prefix `DNI`, covers both DNI and NIE forms), `CategoryPTNIF Category = "pt_nif"` (prefix `NIF`); detectors `FRNIRDetector`, `ESDNIDetector`, `PTNIFDetector`; `Locales` gains "FR", "ES", "PT"; `Registry()` gains the three detectors after `USSSNDetector`.

**Algorithms (implement exactly):**
- IBAN lengths to add if absent: FR 27, DE 22, ES 24, PT 25, NL 18, GB 22, BR 29.
- **FR NIR**: 15 digits = 13-digit number + 2-digit key. Accepted written forms: contiguous 15 digits, or space-grouped `S YY MM DD CCC OOO KK` (1-2-2-2-3-3-2). Structure: first digit (sex) in {1,2,7,8}; month 01–12; department 01–95, 2A, 2B, or 97–98 (accept `2A`/`2B` letters in the department slot for the grouped and contiguous forms). Key check: take the first 13 as a number, with Corsica substitution first (`2A`→`19`, `2B`→`18` in the department position), then key must equal `97 - (number mod 97)`. Reject otherwise.
- **ES DNI/NIE**: DNI = 8 digits + control letter; NIE = leading X/Y/Z + 7 digits + control letter. Control letter = `"TRWAGMYFPDXBNJZSQVHLCKE"[n mod 23]` where n is the 8-digit number (for NIE, replace X/Y/Z with 0/1/2 before computing). Optional single space or hyphen before the control letter. Uppercase letters only (documents write them uppercase; lowercase matching would triple the false-positive surface).
- **PT NIF**: 9 digits; first digit in {1,2,3,5,6,8,9}; check digit: `s = sum(d[i]*(9-i)) for i=0..7`, `r = s mod 11`, check = `0` if `r < 2` else `11 - r`; last digit must equal check.

- [ ] **Step 1: Failing tests first.** Per detector: (a) a verified-valid literal in realistic sentence context is matched with the right category and exact span (build the valid literal IN the test via a tiny check-digit helper local to the test file, so the test is self-validating; additionally use the canonical values `12345678Z` for DNI where applicable); (b) flipping one digit/letter breaks the match; (c) word-boundary discipline: a longer digit run containing a valid substring is NOT matched; (d) format variants (NIR grouped/contiguous incl. a 2A department; NIE with X and Z; NIF plain). Extend `iban_test.go` with one valid IBAN per added country (compute check digits with a throwaway script; verify with the existing detector).
- [ ] **Step 2: Run to fail.** `go test ./internal/docredact/ -run 'NIR|DNI|NIF|IBAN'`
- [ ] **Step 3: Implement** the three detector files + docredact.go/iban.go additions.
- [ ] **Step 4: Full suite green** (existing tests untouched).
- [ ] **Step 5: Commit** — `manual: the redactor knows french spanish and portuguese identifiers`

### Task 2: Germanic/northern identifiers (DE Steuer-ID, NL BSN, UK NINO)

**Files:**
- Modify: `internal/docredact/docredact.go` (categories `CategoryDESteuerID = "de_steuer_id"` prefix `IDNR`, `CategoryNLBSN = "nl_bsn"` prefix `BSN`, `CategoryUKNINO = "uk_nino"` prefix `NINO`; ParseCategory; Locales gains "DE", "NL", "UK"; Registry gains three)
- Create: `internal/docredact/de_steuerid.go`, `nl_bsn.go`, `uk_nino.go` (+ matching `_test.go` files)

**Algorithms (implement exactly):**
- **DE Steuer-ID**: 11 digits, plain or space-grouped 2-3-3-3. First digit ≠ 0. Digit-structure rule: among the first 10 digits, exactly one digit value occurs two or three times and at least one digit value is absent (both the 2x and 3x variants are legal). Check digit (ISO 7064 mod 11,10) over the first 10: `product = 10; for each digit d: sum = (d + product) mod 10; if sum == 0 { sum = 10 }; product = (2 * sum) mod 11`; check = `11 - product`, and `11 → 0`; must equal digit 11.
- **NL BSN**: exactly 9 digits (leading zero allowed), word-bounded. Elfproef with negative last weight: `9*d1 + 8*d2 + ... + 2*d8 - 1*d9 ≡ 0 (mod 11)`, and the number is not all zeros. Canonical valid test value: `111222333` (verify).
- **UK NINO**: **structure only — no checksum exists**, and the file's doc comment must say so plainly (same honesty as the SSN hyphenated-form decision). Form: 2 letters + 6 digits + suffix letter A–D, plain or grouped `AA 12 34 56 C`. First letter not in {D,F,I,Q,U,V}; second letter not in {D,F,I,O,Q,U,V}; prefix pair not in {GB, BT, NK, KN, TN, NT, ZZ}. Uppercase only.

- [ ] **Step 1: Failing tests** (same pattern as Task 1: self-validating valid literal, one-digit flip, boundary discipline, format variants; NINO additionally: forbidden prefix `QQ 12 34 56 A` NOT matched, forbidden pair `GB...` NOT matched).
- [ ] **Step 2: Run to fail.**
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Full suite green.**
- [ ] **Step 5: Commit** — `manual: the redactor knows german dutch and british identifiers`

### Task 3: Non-European majors (BR CPF, CN resident ID, JP My Number)

**Files:**
- Modify: `internal/docredact/docredact.go` (categories `CategoryBRCPF = "br_cpf"` prefix `CPF`, `CategoryCNResidentID = "cn_resident_id"` prefix `CNID`, `CategoryJPMyNumber = "jp_my_number"` prefix `MYNUM`; ParseCategory; Locales gains "BR", "CN", "JP"; Registry gains three)
- Create: `internal/docredact/br_cpf.go`, `cn_residentid.go`, `jp_mynumber.go` (+ `_test.go` files)

**Algorithms (implement exactly):**
- **BR CPF**: 11 digits, formatted `000.000.000-00` or bare 11-digit run. Reject all-eleven-same-digit. First check digit: `s = sum(d[i] * (10 - i)) for i=0..8`, `r = (s * 10) mod 11`, `r == 10 → 0`, must equal d10. Second: `s = sum(d[i] * (11 - i)) for i=0..9`, same reduction, must equal d11. Canonical valid test value: `529.982.247-25` (verify before use).
- **CN resident ID**: 18 chars: 6-digit area code (first digit 1–9), 8-digit birth date `YYYYMMDD` (real calendar date, year 1900..current), 3-digit order, 1 check char. Check (ISO 7064 mod 11-2): weights `w[i] = 2^(17-i) mod 11` over the first 17 digits, `r = sum mod 11`, check char = `"10X98765432"[r]`; accept lowercase `x` on input but match the literal exactly as written.
- **JP My Number**: **grouped form only** — `NNNN NNNN NNNN` with single spaces or hyphens (a bare 12-digit run is indistinguishable from any other 12-digit number: same reasoning as the bare-SSN refusal, and the file comment says so). Check digit over the first 11 digits: reading those 11 right-to-left as n = 1..11, weight `q = n + 1` if `n <= 6` else `n - 5`; `r = sum(d_n * q_n) mod 11`; check = `0` if `r <= 1` else `11 - r`; must equal the 12th digit.

- [ ] **Step 1: Failing tests** (pattern as before; CPF: formatted and bare, all-same-digit rejected; CN: invalid calendar date rejected, X check char accepted; JP: bare 12 digits NOT matched, grouped valid matched).
- [ ] **Step 2: Run to fail.**
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Full suite green.**
- [ ] **Step 5: Commit** — `manual: the redactor knows brazilian chinese and japanese identifiers`

### Task 4: Date formats for the new languages

**Files:**
- Modify: `internal/docredact/dob.go`
- Test: extend `internal/docredact/dob_test.go` (additive only — existing cases untouched)

**Scope (implement exactly, nothing more):**
- Dotted numeric dates `D.M.YYYY` / `DD.MM.YYYY` (German/European): same both-readings + `plausibleBirthYear` logic as the slash form. Word-bounded so `1.2.2020.3` noise does not match.
- Month names for it, fr, de, es, pt, nl, ru: extend the written-date patterns to recognize these month names (case-insensitive; for ru include both nominative and genitive forms, e.g. "март"/"марта"). Keep one merged name→month table; the day-month-year and month-day-year pattern pair stays as is structurally.
- CJK dates: `YYYY年M月D日` (used in both zh and ja documents), real calendar date + plausible birth year.
- No other formats. Two-digit years stay unmatched (ambiguous decade guessing would invent facts).

- [ ] **Step 1: Failing tests**: one positive per language family (`12.03.1990`, `12 marzo 1990`, `12 mars 1990`, `12. März 1990`, `12 de marzo de 1990`, `12 de março de 1990`, `12 maart 1990`, `12 марта 1990`, `1990年3月12日`) each in sentence context with exact-span assertions, plus negatives: `1.2.3` version string, dotted date outside the birth window, `2026年1月1日` (in window? current-year birth is valid age 0 — pick `1875年...` out of window instead).
- [ ] **Step 2: Run to fail.**
- [ ] **Step 3: Implement.** Note: `\b` does not work adjacent to CJK; anchor the CJK pattern by its own digit/character shape instead.
- [ ] **Step 4: Full suite green** — including the corpus invariants (existing corpus dates must not change category).
- [ ] **Step 5: Commit** — `manual: birth dates are found in nine more languages`

### Task 5: Corpus expansion to all major languages

**Files:**
- Create: `internal/docredact/testdata/corpus/14-fr-employment-contract.json`, `15-fr-medical-letter.json`, `16-de-invoice.json`, `17-de-hr-letter.json`, `18-es-rental-agreement.json`, `19-pt-bank-letter.json`, `20-nl-cover-letter.json`, `21-uk-hr-complaint.json`, `22-br-invoice.json`, `23-cn-support-thread.json`, `24-jp-business-letter.json`, `25-ru-meeting-minutes.json`, `26-ar-formal-letter.json`, `27-de-negative-changelog.json`
- Modify: `internal/docredact/benchcorpus_test.go` ONLY to raise the minimum-doc-count assertion (13 → 27) and, if present, locale-set expectations.

**Content rules (all binding):**
- Native-quality prose in each document's language, realistic document shapes, unmistakably synthetic PII (example.tld domains, reserved/implausible phone shapes, invented names/companies).
- Every doc with a new-locale identifier carries at least one gold literal for its new pattern category (fr_nir, es_dni, pt_nif, de_steuer_id, nl_bsn, uk_nino, br_cpf, cn_resident_id, jp_my_number) — all literals verified valid per the Global Constraints rule. Include IBANs for FR/DE/ES/PT/NL/GB/BR docs (valid mod-97, computed).
- RU and AR docs are model-category-heavy (person/org/address/job_title/amount in native script) plus universal pattern categories only (email, phone, dates) — no national-ID detectors exist for them in this plan, and their gold must not pretend otherwise.
- `27-de-negative-changelog.json`: version strings (`2.4.11`), order numbers, ISO timestamps, IPs with out-of-range octets — empty or near-empty gold; must produce no spurious findings (verified via the invariant flow below).
- Locale field: use the locale codes added to `Locales` ("FR", "DE", "ES", "PT", "NL", "UK", "BR", "CN", "JP") and "RU"/"AR" for the corpus-only languages (corpus locale strings are data, not detector claims).
- Every gold literal appears verbatim in its text; every pattern-category gold literal must be detected by `Analyze` (the existing `TestCorpusInvariants` enforces this — it is the oracle; do not weaken it).
- Additionally add ONE new test in `benchcorpus_test.go`: for the negative docs (11, 12, 27), assert `Analyze` produces zero findings not in gold (this closes the deferred T7 finding from the model-pass branch).

- [ ] **Step 1: Write the new no-spurious-findings test first** (RED against doc 27 not existing / current corpus count).
- [ ] **Step 2: Author the 14 documents**, verifying every checksummed literal with a throwaway script.
- [ ] **Step 3: Full suite green** (invariants + new negative assertion).
- [ ] **Step 4: Run `go run ./cmd/docredact-bench`** (pattern-only) and paste the table in the report: pattern categories must show 0 leaks across the grown corpus.
- [ ] **Step 5: Commit** — `manual: the corpus speaks all the major languages`

### Task 6: Documentation truth-up

**Files:**
- Modify: `docs/research/redactor-model-qualification.md` (corpus counts, locale list, per-language note that model-category recall is now measurable per language)
- Modify: `docs/decisions/0022-doc-redactor-model-pass.md` (one dated amendment line: locales expanded per owner directive 2026-08-13, pointing at this plan's detector list)
- Modify: `internal/docredact/docredact.go` `Locales` comment (no longer "pending default" — the owner picked)

**Rules:** every number restated must be re-counted from the committed corpus; no invented facts; no em dashes; absolute dates.

- [ ] **Step 1: Update the three files.**
- [ ] **Step 2: Full suite green (docs-only, run anyway).**
- [ ] **Step 3: Commit** — `manual: the docs state the redactor's true locale coverage`

---

## Self-review notes

- SSN precedent honored for JP (grouped-only) and consciously NOT extended to DE Steuer-ID/NL BSN/PT NIF (checksums make bare digit runs meaningfully validatable; the checksum IS the disambiguator the bare SSN lacks).
- UK NINO is the one structure-only detector; its comment must carry the honesty note.
- Registry/Locales are touched by Tasks 1-3 sequentially on one branch — no parallel implementers.
- Task 5 depends on 1-4 (detectors must exist for gold to validate). Task 6 depends on 5.
