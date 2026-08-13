// Package docredact is the deterministic core of the document redactor
// (docs/plans/12-doc-redactor.md): plain Go pattern detectors plus
// checksums where a checksum exists, grouped into findings by literal
// rather than by occurrence, with stable pseudonym replacement and a
// pseudonym-to-literal mapping. It never talks to a model -- that is the
// spec's step 3, not built yet -- and it never writes to disk: callers in
// internal/httpapi turn its output into browser downloads instead.
//
// Note for anyone searching this repository: internal/redact is a
// different, unrelated package that scrubs tokens out of job receipts and
// log lines. This package does not use it and does not extend it.
package docredact

import "strings"

// Source identifies which pass produced a match. The detectors here only
// ever produce "pattern"; text the owner selected in the console is added
// through AddManual and carries SourceManual; a later model pass (spec step
// 3) would tag its own matches "model".
const Source = "pattern"

// SourceManual marks a literal the owner picked out by hand rather than one
// any detector found. It is a fact about where the finding came from, so it
// travels all the way into the exported mapping.
const SourceManual = "manual"

// SourceModel marks a literal the model pass (spec step 3) found rather than
// a pattern detector or the owner's own selection.
const SourceModel = "model"

// Category is a short, stable identifier for a kind of sensitive literal.
// It doubles as the pseudonym prefix (uppercased) used in replacement
// tokens like [EMAIL_1].
type Category string

const (
	CategoryEmail        Category = "email"
	CategoryPhone        Category = "phone"
	CategoryIBAN         Category = "iban"
	CategoryCard         Category = "card"
	CategoryIPv4         Category = "ipv4"
	CategoryIPv6         Category = "ipv6"
	CategoryDOB          Category = "dob"
	CategoryITCF         Category = "it_codice_fiscale"
	CategoryUSSSN        Category = "us_ssn"
	CategoryFRNIR        Category = "fr_nir"
	CategoryESDNI        Category = "es_dni"
	CategoryPTNIF        Category = "pt_nif"
	CategoryDESteuerID   Category = "de_steuer_id"
	CategoryNLBSN        Category = "nl_bsn"
	CategoryUKNINO       Category = "uk_nino"
	CategoryBRCPF        Category = "br_cpf"
	CategoryCNResidentID Category = "cn_resident_id"
	CategoryJPMyNumber   Category = "jp_my_number"

	// CategoryPhrase is the category of a literal no detector claims: a
	// phrase the owner selected in the preview. It has no pattern and no
	// checksum, so it is deliberately not a Detector category.
	CategoryPhrase Category = "phrase"

	// The categories below are produced by the model pass (spec step 3) or
	// picked by the owner -- no regex or checksum defines a person's name,
	// an organization, a street address, a job title, or a free-form amount,
	// so none of them has a Detector and Registry does not list them.
	CategoryPerson   Category = "person"
	CategoryOrg      Category = "org"
	CategoryAddress  Category = "address"
	CategoryJobTitle Category = "job_title"
	CategoryAmount   Category = "amount"
)

// Prefix returns the uppercase token used in a pseudonym, e.g. "EMAIL" for
// [EMAIL_1]. Kept short and specific per category rather than a generic
// "NATIONAL_ID" so the owner can see which locale's rule fired.
func (c Category) Prefix() string {
	switch c {
	case CategoryEmail:
		return "EMAIL"
	case CategoryPhone:
		return "PHONE"
	case CategoryIBAN:
		return "IBAN"
	case CategoryCard:
		return "CARD"
	case CategoryIPv4:
		return "IPV4"
	case CategoryIPv6:
		return "IPV6"
	case CategoryDOB:
		return "DOB"
	case CategoryITCF:
		return "ITCF"
	case CategoryUSSSN:
		return "SSN"
	case CategoryFRNIR:
		return "NIR"
	case CategoryESDNI:
		return "DNI"
	case CategoryPTNIF:
		return "NIF"
	case CategoryDESteuerID:
		return "IDNR"
	case CategoryNLBSN:
		return "BSN"
	case CategoryUKNINO:
		return "NINO"
	case CategoryBRCPF:
		return "CPF"
	case CategoryCNResidentID:
		return "CNID"
	case CategoryJPMyNumber:
		return "MYNUM"
	case CategoryPhrase:
		return "PHRASE"
	case CategoryPerson:
		return "PERSON"
	case CategoryOrg:
		return "ORG"
	case CategoryAddress:
		return "ADDRESS"
	case CategoryJobTitle:
		return "TITLE"
	case CategoryAmount:
		return "AMOUNT"
	default:
		return "MATCH"
	}
}

// ParseCategory maps a caller-supplied category name to a known Category. An
// unknown or empty name is not an error: text the owner selected by hand is a
// phrase until something says otherwise, so it falls back to CategoryPhrase
// and reports that it did.
func ParseCategory(name string) (Category, bool) {
	candidate := Category(strings.ToLower(strings.TrimSpace(name)))
	switch candidate {
	case CategoryEmail, CategoryPhone, CategoryIBAN, CategoryCard, CategoryIPv4,
		CategoryIPv6, CategoryDOB, CategoryITCF, CategoryUSSSN, CategoryFRNIR,
		CategoryESDNI, CategoryPTNIF, CategoryDESteuerID, CategoryNLBSN, CategoryUKNINO,
		CategoryBRCPF, CategoryCNResidentID, CategoryJPMyNumber,
		CategoryPhrase,
		CategoryPerson, CategoryOrg, CategoryAddress, CategoryJobTitle, CategoryAmount:
		return candidate, true
	default:
		return CategoryPhrase, false
	}
}

// Locales lists the national-identifier locales this build registers. The
// owner expanded this set from the IT/US default to its current form on
// 2026-08-13 (docs/decisions/0022-doc-redactor-model-pass.md, amendment of
// that date); it is a settled choice, not a placeholder. Add an entry here
// and a matching Detector to extend it.
var Locales = []string{"IT", "US", "FR", "ES", "PT", "DE", "NL", "UK", "BR", "CN", "JP"}

// Match is one exact occurrence of a literal found by a detector. Start and
// End are byte offsets into the text the detector scanned, and Text is the
// literal exactly as written in the source -- never normalized, so it can be
// found again with a plain string search.
type Match struct {
	Start    int
	End      int
	Text     string
	Category Category
	Source   string
}

// Detector finds every occurrence of one category of literal in text.
// Detectors do not deduplicate, group, or resolve overlaps with other
// detectors -- that happens once, centrally, in Analyze, after every
// detector has run over the whole document.
type Detector interface {
	Name() string
	Detect(text string) []Match
}

// Registry returns every detector this build runs, in a fixed order. The
// order only affects tie-breaking when two detectors match the exact same
// span (see ResolveOverlaps); it has no effect on which literals are
// eventually found.
func Registry() []Detector {
	return []Detector{
		EmailDetector{},
		PhoneDetector{},
		IBANDetector{},
		CardDetector{},
		IPv4Detector{},
		IPv6Detector{},
		DOBDetector{},
		ITCodiceFiscaleDetector{}, // locale: IT
		USSSNDetector{},           // locale: US
		FRNIRDetector{},           // locale: FR
		ESDNIDetector{},           // locale: ES
		PTNIFDetector{},           // locale: PT
		DESteuerIDDetector{},      // locale: DE
		NLBSNDetector{},           // locale: NL
		UKNINODetector{},          // locale: UK
		BRCPFDetector{},           // locale: BR
		CNResidentIDDetector{},    // locale: CN
		JPMyNumberDetector{},      // locale: JP
	}
}

// DetectAll runs every registered detector over text and returns every
// match found, unsorted and unresolved. Callers that need a clean,
// non-overlapping set should pass this through ResolveOverlaps (Analyze
// does this already).
func DetectAll(text string) []Match {
	var all []Match
	for _, d := range Registry() {
		all = append(all, d.Detect(text)...)
	}
	return all
}
