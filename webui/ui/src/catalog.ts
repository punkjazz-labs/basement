// Family identity: official publisher logos, embedded so the console works
// offline. Every mark is the lab's own: the marks added for MiniMax and
// Thinking Machines are the avatars those labs publish on their own Hugging
// Face organization pages.
export const LOGOS: Record<string, string> = {
  'qwen36-35b-a3b-nvfp4-1s': '/logos/qwen.webp',
  'qwen36-27b-nvfp4-1s': '/logos/qwen.webp',
  'qwen35-122b-a10b-nvfp4-1s': '/logos/qwen.webp',
  'qwen38-27b-nvfp4-1s': '/logos/qwen.webp',
  'qwen38-27b-obliterated-q8-0-1s': '/logos/qwen.webp',
  'qwen38-flash-next-nvfp4-2s': '/logos/qwen.webp',
  'laguna-s-2-1-nvfp4-dflash-1s': '/logos/poolside.webp',
  'nemotron-omni-30b-a3b-nvfp4-1s': '/logos/nvidia.webp',
  'deepseek-v4-flash-0731-2s': '/logos/deepseek.webp',
  'deepseek-v4-flash-0731-ud-iq3-xxs-1s': '/logos/deepseek.webp',
  'minimax-h3-comfyui-1s': '/logos/minimax.webp',
  'inkling-small-nvfp4-2s': '/logos/thinkingmachines.webp',
}

// The mark for a model, or an empty string when this build ships no asset for
// the lab that made it. An empty answer is not a gap to fill with another
// lab's logo: the caller draws a quiet initial block instead (Mark, mark.tsx).
export const logoFor = (recipeIDs: string[]): string => LOGOS[recipeIDs[0]] ?? ''

// Which lab made the model, in the words the catalog groups on. The recipe
// already names it in model_by, so that field is the source and it is used as
// written. Only the Qwen team's two forms are mapped, because the obliterated
// build names its ablation author in the same field and still belongs on the
// same shelf as the rest of the family.
const LAB_LABELS: Record<string, string> = {
  'Qwen team, Alibaba': 'Qwen · Alibaba',
  'Qwen team, Alibaba; abliteration by OBLITERATUS': 'Qwen · Alibaba',
}

// A recipe that names no lab at all reads as this rather than under a lab
// that has not claimed it.
const LAB_FALLBACK = 'Community'

// model_by is the model's own maker. publisher answers for a recipe that
// declares no maker, because it is the nearest thing the recipe knows.
export const labFor = (recipe: { model_by?: string; publisher?: string }): string => {
  const named = recipe.model_by?.trim() || recipe.publisher?.trim() || ''
  return LAB_LABELS[named] ?? (named || LAB_FALLBACK)
}

// When each model first came out, as YYYY-MM. This is the model's own release
// date, not the date of the quantization that serves it and not the date the
// recipe was written. The catalog reads it to put the newest model of each lab
// at the top of that lab's group.
//
// Every line names the source it was read from. A model that is absent from
// this map sorts last inside its lab rather than being given a guessed date.
export const RELEASED: Record<string, string> = {
  'qwen38-flash-next-nvfp4-2s': '2026-08', // Qwen/Qwen3.8-Flash-Next, published 2026-08-24 (Hugging Face API)
  'qwen38-27b-nvfp4-1s': '2026-08', // recipe model_released: August 2026
  'qwen38-27b-obliterated-q8-0-1s': '2026-08', // recipe model_released: August 2026
  'qwen36-35b-a3b-nvfp4-1s': '2026-04', // recipe model_released: April 2026
  'qwen36-27b-nvfp4-1s': '2026-04', // recipe model_released: April 2026
  'qwen35-122b-a10b-nvfp4-1s': '2026-02', // the model card's own citation: month February, year 2026
  'laguna-s-2-1-nvfp4-dflash-1s': '2026-07', // recipe model_released: July 2026
  'deepseek-v4-flash-0731-2s': '2026-07', // recipe model_released: July 2026
  'deepseek-v4-flash-0731-ud-iq3-xxs-1s': '2026-07', // recipe model_released: July 2026
  'minimax-h3-comfyui-1s': '2026-07', // MiniMaxAI/MiniMax-H3, published 2026-07-28 (Hugging Face API)
  'nemotron-omni-30b-a3b-nvfp4-1s': '2026-04', // the card's release date: build.nvidia.com, 04/28/2026
  'inkling-small-nvfp4-2s': '2026-07', // thinkingmachines/Inkling-Small-NVFP4, published 2026-07-27 (Hugging Face API)
}

// CURATED is the shelf basement puts its name to, in the order it means, and
// its first entry is the recommendation the hero makes.
//
// It no longer decides which row comes first inside a lab. It decides which
// lab the catalog reads first, and it breaks the ties the release dates leave.
// Every recipe outside it is real and installable, so it sorts after the
// curated ones rather than before them. It used to sort before them by
// accident: indexOf returns -1 for an unlisted id, and -1 sorts ahead of 0.
export const CURATED = [
  'qwen36-35b-a3b-nvfp4-1s',
  'qwen36-27b-nvfp4-1s',
  'laguna-s-2-1-nvfp4-dflash-1s',
] as const

export const RECOMMENDED_ID: string = CURATED[0]

// The least a recipe has to say for the catalog to place it. The tail is
// ordered from the recipe's own data rather than from another list that could
// fall out of date: models that run on one Spark before models that need two,
// then alphabetically. Every shipped recipe today declares the same trust and
// verification, so neither can order anything.
interface Sortable {
  id: string
  display_name: string
  model_by?: string
  publisher?: string
  topology: { spark_count: number }
}

// The catalog in the order it reads on screen: one lab after another, and the
// newest model of each lab at the top of its group.
//
// The shelf order below does two jobs. A lab takes the place of its first
// model on that shelf, which keeps the curated models in front and puts Qwen
// before poolside. Inside a lab it breaks every tie, so two models released in
// the same month keep the order they already had, and a model with no recorded
// release date sorts last instead of first.
export function sortCatalog<T extends Sortable>(recipes: readonly T[]): T[] {
  const rank = (id: string) => {
    const index = CURATED.indexOf(id as (typeof CURATED)[number])
    return index === -1 ? CURATED.length : index
  }
  const shelf = [...recipes].sort((a, b) =>
    rank(a.id) - rank(b.id) ||
    a.topology.spark_count - b.topology.spark_count ||
    a.display_name.localeCompare(b.display_name))
  const labRank = new Map<string, number>()
  for (const recipe of shelf) {
    const lab = labFor(recipe)
    if (!labRank.has(lab)) labRank.set(lab, labRank.size)
  }
  // Every lab in the map came from the shelf, so the fallback below is only
  // there to keep the comparison a number.
  const labIndex = (recipe: T) => labRank.get(labFor(recipe)) ?? labRank.size
  const released = (id: string) => RELEASED[id] ?? ''
  return shelf
    .map((recipe, place) => ({ recipe, place }))
    .sort((a, b) =>
      labIndex(a.recipe) - labIndex(b.recipe) ||
      released(b.recipe.id).localeCompare(released(a.recipe.id)) ||
      a.place - b.place)
    .map(entry => entry.recipe)
}

const QUANTS = new Set(['NVFP4', 'FP8', 'FP4', 'INT8', 'INT4', 'BF16', 'FP16', 'AWQ', 'GPTQ', 'GGUF'])
const OWNERS: Record<string, string> = { nvidia: 'NVIDIA', poolside: 'poolside', unsloth: 'Unsloth', qwen: 'Qwen' }

export const ownerName = (repository: string): string => {
  const owner = repository.split('/')[0] ?? ''
  return OWNERS[owner.toLowerCase()] ?? owner
}

// "poolside/Laguna-S-2.1-NVFP4" reads as "Laguna S 2.1" with NVFP4 called out
// as the quantization, so rows speak the model's name, not its repo path.
export function readableWeights(repository: string): { name: string; quant?: string } {
  const basename = repository.split('/').pop() ?? repository
  let quant: string | undefined
  const words = basename.split(/[-_]/).filter(word => {
    if (QUANTS.has(word.toUpperCase())) {
      quant = word.toUpperCase()
      return false
    }
    return true
  })
  return { name: words.join(' ').replace(/^([A-Za-z]+?)(\d)/, '$1 $2'), quant }
}
