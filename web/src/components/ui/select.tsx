import { Children, CSSProperties, Fragment, isValidElement, KeyboardEvent as ReactKeyboardEvent, ReactNode, useCallback, useEffect, useRef, useState } from 'react'
import { createPortal } from 'react-dom'
import { calcMenuPlacement, MenuPlacement } from '../geometry'
import { getViewportBounds, lockBodyScroll, useMediaQuery } from './hooks'
import { Icon } from './icons'

function useDropdownDismiss({
  open,
  floating,
  triggerRef,
  listRef,
  onClose,
  onReposition,
  returnFocusOnEsc = true,
}: {
  open: boolean
  floating: boolean
  triggerRef: { current: HTMLElement | null }
  listRef: { current: HTMLElement | null }
  onClose: () => void
  onReposition?: () => void
  returnFocusOnEsc?: boolean
}) {
  useEffect(() => {
    if (!open) return
    const onDown = (e: PointerEvent) => {
      const t = e.target as Node
      if (!triggerRef.current?.contains(t) && !listRef.current?.contains(t)) onClose()
    }
    const onEsc = (e: KeyboardEvent) => {
      if (e.key !== 'Escape') return
      e.preventDefault()
      e.stopImmediatePropagation()
      onClose()
      if (returnFocusOnEsc) triggerRef.current?.focus()
    }
    const onMove = (e: Event) => {
      if (listRef.current && e.target instanceof Node && listRef.current.contains(e.target)) return
      onReposition?.()
    }
    document.addEventListener('pointerdown', onDown)
    window.addEventListener('keydown', onEsc, true)
    if (floating && onReposition) {
      window.addEventListener('resize', onMove)
      window.addEventListener('scroll', onMove, true)
      window.visualViewport?.addEventListener('resize', onMove)
    }
    return () => {
      document.removeEventListener('pointerdown', onDown)
      window.removeEventListener('keydown', onEsc, true)
      window.removeEventListener('resize', onMove)
      window.removeEventListener('scroll', onMove, true)
      window.visualViewport?.removeEventListener('resize', onMove)
    }
  }, [open, floating, triggerRef, listRef, onClose, onReposition, returnFocusOnEsc])
}

// ---------- combobox ----------
// Free-text input with a suggestion list: type anything, or open the list (chevron, ArrowDown)
// and pick. Typing filters the list; the menu shares Select's look and portal placement.

export type ComboOption = { value: string; label?: ReactNode; hint?: ReactNode; disabled?: boolean }

let comboSeq = 0

export function Combobox({ value, onChange, options, placeholder, className, mono, autoFocus, ...aria }: {
  value: string
  onChange: (v: string) => void
  options: ComboOption[]
  placeholder?: string
  className?: string
  mono?: boolean
  autoFocus?: boolean
  'aria-label'?: string
}) {
  const [open, setOpen] = useState(false)
  const [filter, setFilter] = useState(false) // filter by the typed text (off when opened from the chevron)
  const [active, setActive] = useState(-1)
  const [pos, setPos] = useState<MenuPlacement | null>(null)
  const boxRef = useRef<HTMLDivElement>(null)
  const inputRef = useRef<HTMLInputElement>(null)
  const listRef = useRef<HTMLUListElement>(null)
  const isMobile = useMediaQuery('(max-width: 640px)')
  const [uid] = useState(() => 'combo' + ++comboSeq)

  const q = value.trim().toLowerCase()
  const shown = filter && q
    ? options.filter((o) => o.value.toLowerCase().includes(q) || nodeText(o.label).toLowerCase().includes(q) || nodeText(o.hint).toLowerCase().includes(q))
    : options

  const place = useCallback(() => {
    const r = inputRef.current?.getBoundingClientRect() ?? boxRef.current?.getBoundingClientRect()
    if (!r) return
    setPos(calcMenuPlacement(r, getViewportBounds(), { maxPreferredWidth: 480, minPreferredWidth: 280 }))
  }, [])

  const show = (withFilter: boolean) => {
    place()
    setFilter(withFilter)
    setActive(-1)
    setOpen(true)
  }
  const close = useCallback(() => setOpen(false), [])
  const pick = (o: ComboOption | undefined) => {
    if (!o || o.disabled) return
    onChange(o.value)
    setOpen(false)
    inputRef.current?.focus()
  }
  const step = (from: number, dir: 1 | -1) => {
    for (let i = from + dir; i >= 0 && i < shown.length; i += dir) if (!shown[i].disabled) return i
    return from
  }

  useDropdownDismiss({
    open,
    floating: !isMobile,
    triggerRef: boxRef,
    listRef,
    onClose: close,
    onReposition: place,
    returnFocusOnEsc: false,
  })

  useEffect(() => {
    if (!open || !isMobile || shown.length === 0) return
    const id = requestAnimationFrame(() => listRef.current?.scrollIntoView({ block: 'nearest', behavior: 'smooth' }))
    return () => cancelAnimationFrame(id)
  }, [open, isMobile, shown.length])

  useEffect(() => {
    if (open && active >= 0) listRef.current?.children[active]?.scrollIntoView({ block: 'nearest' })
  }, [open, active])

  const onKey = (e: ReactKeyboardEvent<HTMLInputElement>) => {
    const k = e.key
    if (!open) {
      if (k === 'ArrowDown' || k === 'ArrowUp') { e.preventDefault(); show(false) }
      return
    }
    if (k === 'Escape') { e.preventDefault(); e.stopPropagation(); setOpen(false) }
    else if (k === 'Tab') setOpen(false)
    else if (k === 'ArrowDown') { e.preventDefault(); setActive((a) => step(a, 1)) }
    else if (k === 'ArrowUp') { e.preventDefault(); setActive((a) => step(a, -1)) }
    else if (k === 'Enter' && active >= 0) { e.preventDefault(); pick(shown[active]) }
  }

  const menuList = open && shown.length > 0 && (
    <ul
      ref={listRef}
      id={uid}
      role="listbox"
      className={'select-menu combo-menu' + (isMobile ? ' combo-inline' : '')}
      style={!isMobile && pos ? { left: pos.left, top: pos.top, bottom: pos.bottom, minWidth: pos.minWidth, maxWidth: pos.maxWidth, maxHeight: pos.maxHeight } : undefined}
      onClick={(e) => e.stopPropagation()}
    >
      {shown.map((o, i) => (
        <li key={o.value} id={`${uid}-${i}`} role="option" aria-selected={o.value === value} aria-disabled={o.disabled || undefined}
          className={(i === active ? 'active' : '') + (o.value === value ? ' selected' : '') + (o.disabled ? ' disabled' : '')}
          onMouseEnter={() => !o.disabled && setActive(i)}
          onMouseDown={(e) => e.preventDefault()}
          onClick={() => pick(o)}>
          <span className="combo-main">
            <span className="mono">{o.value}</span>
            {o.label && <span className="combo-label">{o.label}</span>}
          </span>
          {o.hint && <span className="combo-hint">{o.hint}</span>}
        </li>
      ))}
    </ul>
  )

  return (
    <div ref={boxRef} className={'combo' + (open ? ' open' : '') + (className ? ' ' + className : '')}>
      <div className="combo-box">
        <input
          ref={inputRef}
          className={'input' + (mono ? ' mono' : '')}
          value={value}
          placeholder={placeholder}
          autoFocus={autoFocus}
          autoComplete="off"
          spellCheck={false}
          role="combobox"
          aria-expanded={open}
          aria-controls={uid}
          aria-autocomplete="list"
          aria-activedescendant={open && active >= 0 ? `${uid}-${active}` : undefined}
          aria-label={aria['aria-label']}
          onChange={(e) => { onChange(e.target.value); if (!open) show(true); else { setFilter(true); setActive(-1) } }}
          onKeyDown={onKey}
        />
        <button type="button" className="combo-toggle" tabIndex={-1} aria-label="展开选项"
          onMouseDown={(e) => e.preventDefault()}
          onClick={(e) => { e.preventDefault(); if (open) setOpen(false); else { show(false); inputRef.current?.focus() } }}>
          <Icon name="chevron-down" size={16} />
        </button>
      </div>
      {isMobile ? menuList : (menuList && pos && createPortal(menuList, document.body))}
    </div>
  )
}

// ---------- select ----------
// Styled replacement for <select>. Takes the same <option> children, so call sites read the
// same; onChange receives the chosen value as a string. The menu is portalled to <body> so it
// is never clipped by modals or scrolling tables, and flips upward near the bottom edge.

type Opt = { value: string; label: ReactNode; text: string; disabled?: boolean }

function nodeText(n: ReactNode): string {
  if (n == null || typeof n === 'boolean') return ''
  if (typeof n === 'string' || typeof n === 'number') return String(n)
  if (Array.isArray(n)) return n.map(nodeText).join('')
  if (isValidElement(n)) return nodeText((n.props as { children?: ReactNode }).children)
  return ''
}

function readOptions(children: ReactNode): Opt[] {
  const out: Opt[] = []
  Children.forEach(children, (c) => {
    if (!isValidElement(c)) return
    if (c.type === Fragment) return void out.push(...readOptions((c.props as { children?: ReactNode }).children))
    const p = c.props as { value?: string | number; children?: ReactNode; disabled?: boolean }
    const text = nodeText(p.children)
    out.push({ value: p.value != null ? String(p.value) : text, label: p.children, text, disabled: p.disabled })
  })
  return out
}

let selectSeq = 0

export function Select({ value, onChange, children, disabled, style, className, placeholder, ...aria }: {
  value: string | number | undefined
  onChange: (v: string) => void
  children: ReactNode
  disabled?: boolean
  style?: CSSProperties
  className?: string
  placeholder?: string
  'aria-label'?: string
  id?: string
}) {
  const opts = readOptions(children)
  const cur = value == null ? '' : String(value)
  const selIdx = opts.findIndex((o) => o.value === cur)
  const sel = selIdx >= 0 ? opts[selIdx] : undefined
  const [open, setOpen] = useState(false)
  const [active, setActive] = useState(-1)
  const [pos, setPos] = useState<MenuPlacement | null>(null)
  const [sheetTitle, setSheetTitle] = useState(aria['aria-label'] || placeholder || '请选择')
  const isMobile = useMediaQuery('(max-width: 640px)')
  const btnRef = useRef<HTMLButtonElement>(null)
  const listRef = useRef<HTMLUListElement>(null)
  const typed = useRef({ s: '', t: 0 })
  const [uid] = useState(() => 'sel' + ++selectSeq)

  const place = useCallback(() => {
    const r = btnRef.current?.getBoundingClientRect()
    if (!r) return
    setPos(calcMenuPlacement(r, getViewportBounds(), {
      preferHeight: Math.min(320, opts.length * 34 + 10),
    }))
  }, [opts.length])

  const close = useCallback(() => setOpen(false), [])
  const show = () => {
    if (disabled) return
    place()
    const fieldLabel = btnRef.current?.closest('.field')?.querySelector(':scope > span')?.textContent?.trim()
    setSheetTitle(aria['aria-label'] || fieldLabel || placeholder || '请选择')
    setActive(selIdx >= 0 ? selIdx : opts.findIndex((o) => !o.disabled))
    setOpen(true)
  }
  const pick = (i: number) => {
    const o = opts[i]
    if (!o || o.disabled) return
    setOpen(false)
    btnRef.current?.focus()
    if (o.value !== cur) onChange(o.value)
  }
  const step = (from: number, dir: 1 | -1) => {
    for (let i = from + dir; i >= 0 && i < opts.length; i += dir) if (!opts[i].disabled) return i
    return from
  }

  useDropdownDismiss({
    open,
    floating: !isMobile,
    triggerRef: btnRef,
    listRef,
    onClose: close,
    onReposition: place,
  })

  useEffect(() => {
    if (open && isMobile) return lockBodyScroll()
  }, [open, isMobile])

  useEffect(() => {
    if (open && active >= 0) listRef.current?.children[active]?.scrollIntoView({ block: 'nearest' })
  }, [open, active])

  const onKey = (e: ReactKeyboardEvent) => {
    if (disabled) return
    const k = e.key
    if (!open) {
      if (k === 'ArrowDown' || k === 'ArrowUp' || k === 'Enter' || k === ' ') { e.preventDefault(); show() }
      return
    }
    if (k === 'Escape' || k === 'Tab') {
      if (k === 'Escape') { e.preventDefault(); e.stopPropagation() }
      setOpen(false)
    } else if (k === 'ArrowDown') { e.preventDefault(); setActive((a) => step(a, 1)) }
    else if (k === 'ArrowUp') { e.preventDefault(); setActive((a) => step(a, -1)) }
    else if (k === 'Home') { e.preventDefault(); setActive(step(-1, 1)) }
    else if (k === 'End') { e.preventDefault(); setActive(step(opts.length, -1)) }
    else if (k === 'Enter' || k === ' ') { e.preventDefault(); pick(active) }
    else if (k.length === 1) {
      const now = Date.now()
      typed.current = { s: (now - typed.current.t < 600 ? typed.current.s : '') + k.toLowerCase(), t: now }
      const i = opts.findIndex((o) => !o.disabled && o.text.toLowerCase().startsWith(typed.current.s))
      if (i >= 0) setActive(i)
    }
  }

  const optionNodes = (
    <>
      {opts.map((o, i) => (
        <li
          key={o.value + ':' + i}
          id={`${uid}-${i}`}
          role="option"
          aria-selected={i === selIdx}
          aria-disabled={o.disabled || undefined}
          className={(i === active ? 'active' : '') + (i === selIdx ? ' selected' : '') + (o.disabled ? ' disabled' : '')}
          onMouseEnter={() => !o.disabled && setActive(i)}
          onClick={() => pick(i)}
        >
          <span className="grow">{o.label}</span>
          {i === selIdx && <Icon name="check" size={16} />}
        </li>
      ))}
      {opts.length === 0 && <li className="disabled">无可选项</li>}
    </>
  )

  return (
    <>
      <button
        ref={btnRef}
        type="button"
        id={aria.id}
        className={'select' + (open ? ' open' : '') + (className ? ' ' + className : '')}
        style={style}
        disabled={disabled}
        role="combobox"
        aria-label={aria['aria-label']}
        aria-haspopup="listbox"
        aria-expanded={open}
        aria-controls={open ? uid : undefined}
        aria-activedescendant={open && active >= 0 ? `${uid}-${active}` : undefined}
        onClick={() => (open ? setOpen(false) : show())}
        onKeyDown={onKey}
      >
        <span className={'select-value' + (sel ? '' : ' placeholder')}>{sel ? sel.label : placeholder ?? '请选择'}</span>
        <Icon name="chevron-down" size={16} />
      </button>
      {open && isMobile && createPortal(
        <div
          className="select-sheet-backdrop"
          onMouseDown={(e) => { if (e.target === e.currentTarget) { e.stopPropagation(); setOpen(false) } }}
          onClick={(e) => e.stopPropagation()}
        >
          <div className="select-sheet" role="dialog" aria-modal="true" aria-label={sheetTitle}>
            <div className="select-sheet-grip" aria-hidden="true" />
            <div className="select-sheet-head">
              <span className="select-sheet-title">{sheetTitle}</span>
              <button
                type="button"
                className="btn btn-sm btn-ghost btn-icon"
                aria-label="关闭"
                onClick={() => { setOpen(false); btnRef.current?.focus() }}
              >
                <Icon name="x" size={16} />
              </button>
            </div>
            <ul ref={listRef} id={uid} role="listbox" className="select-sheet-list">
              {optionNodes}
            </ul>
          </div>
        </div>,
        document.body,
      )}
      {open && !isMobile && pos && createPortal(
        <ul
          ref={listRef}
          id={uid}
          role="listbox"
          className="select-menu"
          style={{ left: pos.left, top: pos.top, bottom: pos.bottom, minWidth: pos.minWidth, maxWidth: pos.maxWidth, maxHeight: pos.maxHeight }}
          onMouseDown={(e) => e.preventDefault()}
        >
          {optionNodes}
        </ul>,
        document.body,
      )}
    </>
  )
}
