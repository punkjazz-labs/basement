// Family identity: official publisher logos, embedded so the console works offline.
export const LOGOS: Record<string, string> = {
  'qwen36-35b-a3b-nvfp4-1s': '/logos/qwen.webp',
  'qwen36-27b-nvfp4-1s': '/logos/qwen.webp',
  'qwen35-122b-a10b-nvfp4-1s': '/logos/qwen.webp',
  'laguna-s-2-1-nvfp4-dflash-1s': '/logos/poolside.webp',
  'nemotron-omni-30b-a3b-nvfp4-1s': '/logos/nvidia.webp',
  'deepseek-v4-flash-0731-2s': '/logos/deepseek.webp',
}

export const logoFor = (recipeIDs: string[]): string =>
  LOGOS[recipeIDs[0]] ?? '/logos/nvidia.webp'

// CURATED is the shelf basement puts its name to, in the order it means. It
// holds only the recipes this console has written copy for (the USE lines and
// reference speeds in Models.tsx), and its first entry is the recommendation
// the hero makes.
//
// Every recipe outside it is real and installable, it simply has not been
// written up, so it sorts after the curated ones rather than before them. It
// used to sort before them by accident: indexOf returns -1 for an unlisted
// id, and -1 sorts ahead of 0.
export const CURATED = [
  'qwen36-35b-a3b-nvfp4-1s',
  'qwen36-27b-nvfp4-1s',
  'laguna-s-2-1-nvfp4-dflash-1s',
] as const

export const RECOMMENDED_ID: string = CURATED[0]

// The tail is ordered from the recipe's own data rather than from another
// list that could fall out of date: models that run on one Spark before
// models that need two, then alphabetically. Every shipped recipe today
// declares the same trust and verification, so neither can order anything.
interface Sortable {
  id: string
  display_name: string
  topology: { spark_count: number }
}

export function sortCatalog<T extends Sortable>(recipes: readonly T[]): T[] {
  const rank = (id: string) => {
    const index = CURATED.indexOf(id as (typeof CURATED)[number])
    return index === -1 ? CURATED.length : index
  }
  return [...recipes].sort((a, b) =>
    rank(a.id) - rank(b.id) ||
    a.topology.spark_count - b.topology.spark_count ||
    a.display_name.localeCompare(b.display_name))
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
