# Pinned workflow graphs

Every file beside this one is a ComfyUI API-format workflow, shipped inside
basement and embedded with the recipes. A recipe names a graph under
`service.comfyui.graphs`; it can never supply one, edit one, or read one back,
and the graph JSON never appears in any API response.

A graph is parameterised only by the things the user controls, written as
whole JSON string values. The generation driver replaces each token with a
typed value, so a token has to be the entire string it appears as:
`"seed": "{{SEED}}"` becomes `"seed": 12345`, and `"seed": "s{{SEED}}"` is
left alone and fails validation instead.

| Token | Replaced with | Required in |
| --- | --- | --- |
| `{{PROMPT}}` | the user's prompt, as a string | every graph |
| `{{SEED}}` | the seed, as a number | every graph |
| `{{FRAMES}}` | the frame count, as a number | every graph |
| `{{WIDTH}}` | the width in pixels, as a number | every graph |
| `{{HEIGHT}}` | the height in pixels, as a number | every graph |
| `{{IMAGE}}` | the staged source image file name, as a string | `image_to_video` only |

The validator checks at load time that every graph a recipe names exists,
parses as JSON, and carries exactly the token set its mode requires. A graph
missing `{{PROMPT}}` is rejected rather than shipped, so a graph can never
silently ignore what the user asked for; a graph carrying `{{IMAGE}}` in a
text-to-video mode is rejected too, because nothing would ever substitute it.

No graph ships yet. The MiniMax H3 graphs arrive with the recipe that names
them, from the official ComfyUI templates for that model.
