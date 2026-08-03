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
