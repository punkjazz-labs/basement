// The rail: which screens the console has, what they are called on it, and
// the order they are read in. It is words and order only, so it is pure and
// App.tsx keeps every behaviour it already had.
//
// The names in TABS are code identifiers. Nothing renames them: they are the
// keys the view switch, the deep links from dialogs and the stored state all
// use, so a rename there would be a refactor rather than a label pass.

export const TABS = ['Models', 'Roles', 'Playground', 'Generate', 'Redactor', 'Connect', 'Monitor', 'Fleet', 'Storage', 'Activity'] as const
export type Tab = (typeof TABS)[number]

// What each screen is called on the rail. Only three tabs are called anything
// other than their own name: the chat is a chat and not a playground, a group
// of Sparks is Sparks and not a fleet, and the developer door holds API keys
// and endpoints, so it says API.
const LABELS: Partial<Record<Tab, string>> = {
  Playground: 'Chat',
  Fleet: 'Sparks',
  Connect: 'API',
}

export const tabLabel = (tab: Tab): string => LABELS[tab] ?? tab

export interface RailGroup {
  // The quiet uppercase label over the group. Empty for the group that leads
  // the rail, which needs no label: it is the job of the product.
  label: string
  tabs: Tab[]
}

// The rail in the order it is drawn. Models leads alone, then the apps that
// use the models, then the machinery, with the one developer item last.
//
// Generate stands beside the chat because it takes the chat's place: a Spark
// serving a video model has no chat to offer, and App decides which of the
// two is on the rail. Minutes has no row here: its console surface is a task
// of its own, and a rail must not name a screen that does not exist.
export const RAIL: RailGroup[] = [
  { label: '', tabs: ['Models'] },
  { label: 'Apps', tabs: ['Playground', 'Generate', 'Redactor'] },
  { label: 'System', tabs: ['Roles', 'Fleet', 'Monitor', 'Storage', 'Activity', 'Connect'] },
]

// The rail as it is drawn now: the same groups with every tab that is not on
// offer taken out, and a group left with nothing dropped rather than drawn as
// a label over an empty space.
export function railGroups(visible: readonly Tab[]): RailGroup[] {
  const shown = new Set(visible)
  return RAIL
    .map(group => ({ label: group.label, tabs: group.tabs.filter(tab => shown.has(tab)) }))
    .filter(group => group.tabs.length > 0)
}
