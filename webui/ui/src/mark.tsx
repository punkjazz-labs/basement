import { logoFor } from './catalog'

// The lab's own mark beside a model, at the size the caller draws it.
//
// basement ships an asset for every lab in the pack, so a model with no mark
// is one this build has never seen. It gets a quiet initial block, taken from
// the name the row already shows, and never another lab's logo: a wrong mark
// tells the owner something false about who made the model.
export function Mark({ recipeIDs, name, size }: {
  recipeIDs: string[]
  name: string
  size: number
}) {
  const logo = logoFor(recipeIDs)
  if (logo) return <img src={logo} alt="" width={size} height={size} />
  return (
    <span
      className="mark"
      style={{ width: size, height: size, fontSize: Math.round(size * 0.42) }}
      aria-hidden="true"
    >
      {name.trim().charAt(0)}
    </span>
  )
}
