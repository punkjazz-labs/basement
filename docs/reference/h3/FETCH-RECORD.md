# MiniMax H3 prompt guide fetch record

Fetched on 2026-08-05 in Europe/Paris.

## Directory listing

Source:
https://huggingface.co/api/models/MiniMaxAI/MiniMax-H3/tree/main/docs

HTTP status: 200

SHA-256 of fetched response:
6ef9e520bcf9d0ba79d68c372ad0f13cef4cefc6106b3b536e681c2567c7fec1

The response listed these files:

| Path | Hugging Face file id | Bytes |
| --- | --- | ---: |
| docs/QA-about-License.md | 08ad0d3b9e61d74a278fe1ac81d6251e198aee7d | 3,951 |
| docs/VIDEO_PROMPT_WRITING_GUIDE_base_en.md | 40cf586a634d677d6b7107b367cf0ec9621be728 | 15,773 |
| docs/VIDEO_PROMPT_WRITING_GUIDE_ref_en.md | 7ae1b2d07d743fd2392258a96449be9e9e322d35 | 23,553 |

Raw response:

    [{"type":"file","oid":"08ad0d3b9e61d74a278fe1ac81d6251e198aee7d","size":3951,"path":"docs/QA-about-License.md"},{"type":"file","oid":"40cf586a634d677d6b7107b367cf0ec9621be728","size":15773,"path":"docs/VIDEO_PROMPT_WRITING_GUIDE_base_en.md"},{"type":"file","oid":"7ae1b2d07d743fd2392258a96449be9e9e322d35","size":23553,"path":"docs/VIDEO_PROMPT_WRITING_GUIDE_ref_en.md"}]

## Successful guide fetches

The following URL returned HTTP 200 and 23,553 bytes:

https://huggingface.co/MiniMaxAI/MiniMax-H3/raw/main/docs/VIDEO_PROMPT_WRITING_GUIDE_ref_en.md

Its fetched-byte SHA-256 is
1e574f356716ad55612247ffb7bbccbcdb484ad96599d63c7dca1af186b1fab7.
The saved copy is VIDEO_PROMPT_WRITING_GUIDE_ref_en.md in this directory.

The directory listing identified the base guide. The following URL returned
HTTP 200 and 15,773 bytes:

https://huggingface.co/MiniMaxAI/MiniMax-H3/raw/main/docs/VIDEO_PROMPT_WRITING_GUIDE_base_en.md

Its fetched-byte SHA-256 is
2cfebc096a6e08370f288d468d90b60f7f9bcb938f94bf090816e910e48e75fc.
The saved copy is VIDEO_PROMPT_WRITING_GUIDE_base_en.md in this directory.
That copy records its four punctuation normalizations.

## Failed filename

The initially suggested URL below returned HTTP 404 and 15 bytes:

https://huggingface.co/MiniMaxAI/MiniMax-H3/raw/main/docs/VIDEO_PROMPT_WRITING_GUIDE_en.md

The exact response body was:

    Entry not found

Its fetched-byte SHA-256 is
f36668ddf22403a332f978057d527cf285b01468bc3431b04094a7bafa6aba59.

## Interpretation

The base guide explicitly covers I2VA, which is the single-image first-frame
mode used here. It defines the fixed first-frame instruction followed by
integrated_multimodal_description, overall_soundscape, and
non_diegetic_music.

The reference guide is titled Full-Reference Mode Rewrite Output Format Guide.
It defines six sections for richer reference-asset workflows and points back
to the base guide for the ordinary I2VA shot, camera, speaker, dialogue, and
sound rules. Its detailed_description field is not a substitute for the base
guide's integrated_multimodal_description field in the single-image I2VA
prompt.
