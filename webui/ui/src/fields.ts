// Password managers decide what is a login form by heuristics: a masked
// field, a text field next to it, a form that appears or disappears. The
// console has several secret-shaped fields that are not logins for this
// site, so every one of them opts out explicitly rather than hoping the
// heuristic guesses right.
//
// The three data attributes are the documented opt-outs of 1Password,
// LastPass and Bitwarden. They are additive: a manager that does not know
// one simply ignores it.
export const IGNORED_BY_MANAGERS = {
  autoComplete: 'off',
  'data-1p-ignore': '',
  'data-lpignore': 'true',
  'data-bwignore': '',
} as const

// Forms carrying those fields say the same thing at the form level, which is
// what Chrome and Safari read before they look at individual inputs.
export const FORM_IGNORED_BY_MANAGERS = {
  autoComplete: 'off',
  'data-1p-ignore': '',
  'data-lpignore': 'true',
} as const
