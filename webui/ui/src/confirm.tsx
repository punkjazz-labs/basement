import { useEffect, useRef, useState } from 'react'

// Branded replacements for window.confirm / window.alert so every dialog in
// the console carries the product's own styling. App mounts one ConfirmHost;
// confirmBox()/noticeBox() can then be awaited from anywhere.

export interface ConfirmOptions {
  title: string
  body?: string
  confirmLabel?: string
  cancelLabel?: string
  danger?: boolean
  checkbox?: { label: string; note?: string }
}

export interface ConfirmResult {
  ok: boolean
  checked: boolean
}

let present: (options: ConfirmOptions) => Promise<ConfirmResult> = () =>
  Promise.resolve({ ok: false, checked: false })

export const confirmBox = (options: ConfirmOptions) => present(options)

export const noticeBox = (title: string, body?: string) =>
  present({ title, body, confirmLabel: 'OK', cancelLabel: '' }).then(() => undefined)

export function ConfirmHost() {
  const ref = useRef<HTMLDialogElement>(null)
  const [options, setOptions] = useState<ConfirmOptions | null>(null)
  const [checked, setChecked] = useState(false)
  const resolver = useRef<(result: ConfirmResult) => void>(() => {})

  useEffect(() => {
    present = requested =>
      new Promise(resolve => {
        resolver.current = resolve
        setChecked(false)
        setOptions(requested)
      })
    return () => {
      present = () => Promise.resolve({ ok: false, checked: false })
    }
  }, [])

  useEffect(() => {
    const dialog = ref.current
    if (!dialog) return
    if (options && !dialog.open) dialog.showModal()
    if (!options && dialog.open) dialog.close()
  }, [options])

  const finish = (ok: boolean) => {
    if (!options) return
    resolver.current({ ok, checked })
    setOptions(null)
  }

  return (
    <dialog ref={ref} className="confirm-box" onClose={() => finish(false)}>
      {options && (
        <div className="dialog-pad">
          <h2>{options.title}</h2>
          {options.body && <p className="confirm-body">{options.body}</p>}
          {options.checkbox && (
            <label className="confirm-check">
              <input
                type="checkbox"
                checked={checked}
                onChange={event => setChecked(event.target.checked)}
              />
              <span>
                {options.checkbox.label}
                {options.checkbox.note && <small>{options.checkbox.note}</small>}
              </span>
            </label>
          )}
          <div className="dialog-foot">
            {options.cancelLabel !== '' && (
              <button className="ghost" onClick={() => finish(false)}>
                {options.cancelLabel ?? 'Cancel'}
              </button>
            )}
            <button
              className={options.danger ? 'danger' : 'primary'}
              autoFocus
              onClick={() => finish(true)}
            >
              {options.confirmLabel ?? 'Confirm'}
            </button>
          </div>
        </div>
      )}
    </dialog>
  )
}
