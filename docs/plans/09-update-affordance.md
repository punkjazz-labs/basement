# Spec 09: update affordance at row level

Branch `spec/09-update-affordance`. One commit.

**Problem.** When an installed model's recipe has a newer version, the only
visible affordance is inside the expanded card (the `Recipe updated` line with the
ghost `Update` button, spec 04). the owner: the update must be visible at the row
level, next to the row's existing action buttons, without expanding anything.

**Change** (`webui/ui/src/views/Models.tsx`):
1. In the installed-model row's action cluster (where Open / Start / Switch to
   live), when `model.recipe_version < recipe.version`, add a ghost pill labeled
   `Update` that calls the existing `startInstall(recipe)`. Placement: immediately
   before the row's current primary action. It must respect the same `busy`
   disable as its neighbors. Never a second primary: the pill is `ghost`.
2. The expanded card keeps the `Recipe updated` fact line (it explains what the
   pill means and shows versions), but drop the duplicate button from it — one
   control per action, and it now lives on the row.
3. No other rows change. When no update is available, rendering is identical to
   today, byte for byte.

This is an incremental edit of existing components (existing pill family, existing
handler), not a new visual concept, so it is not mockup-gated.

**Acceptance.** Mock-harness screenshots: an installed row with an update
available (pill visible, correct placement, correct disabled state while a job
runs) and one without (unchanged). Typecheck and build green; no Go changes
expected.
