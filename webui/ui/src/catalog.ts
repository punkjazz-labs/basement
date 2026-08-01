// Family identity: official publisher logos, embedded so the console works offline.
export const LOGOS: Record<string, string> = {
  'qwen36-35b-a3b-nvfp4-1s': '/logos/qwen.webp',
  'qwen36-27b-nvfp4-1s': '/logos/qwen.webp',
  'laguna-s-2-1-nvfp4-dflash-1s': '/logos/poolside.webp',
}

export const logoFor = (recipeIDs: string[]): string =>
  LOGOS[recipeIDs[0]] ?? '/logos/nvidia.webp'
