import { labFor, logoFor, type LabNamed } from './catalog'

// The lab's own mark beside a model, at the size the caller draws it.
//
// basement ships an asset for every lab in the pack, so a model with no mark
// is one this build has never seen. It gets a quiet initial block and never
// another lab's logo: a wrong mark tells the owner something false about who
// made the model. The letter is the lab's own first letter where the caller
// knows the recipe, and the model's where it does not.
//
// Every field is optional on purpose. A model can serve on this Spark whose
// recipe is not in the catalog at all, which is a state this console meets
// elsewhere and always guards (App.tsx, Fleet.tsx and Storage.tsx all read
// "the recipe, or else the id"). Here the name can arrive undefined, so the
// mark asks for nothing it cannot do without: with no recipe, no id and no
// name it draws an empty block rather than taking the console down.
export function Mark({ recipe, recipeIDs = [], name, size }: {
  recipe?: LabNamed
  recipeIDs?: string[]
  name?: string
  size: number
}) {
  const logo = logoFor(recipeIDs, recipe)
  if (logo) return <img src={logo} alt="" width={size} height={size} />
  const letter = recipe ? labFor(recipe) : name ?? ''
  return <span className="mark" aria-hidden="true">{letter.trim().charAt(0)}</span>
}
