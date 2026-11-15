import { CSSProperties, Fragment, ReactNode } from 'react'

export type CardMeta = {
  label?: ReactNode
  value: ReactNode
  mono?: boolean
  full?: boolean
}

/** Desktop table + mobile card list switcher driven by CSS at max-width 860px. */
export function ResponsiveTable({ table, cards, cardsStyle }: { table: ReactNode; cards: ReactNode; cardsStyle?: CSSProperties }) {
  return (
    <>
      <div className="desktop-only">{table}</div>
      <div className="mobile-only m-cards" style={cardsStyle}>{cards}</div>
    </>
  )
}

/** Stacked mobile list item matching panel card and hairline typography. */
export function MobileCard({
  title,
  sub,
  badges,
  meta,
  actions,
  extra,
  onClick,
  tight,
}: {
  title: ReactNode
  sub?: ReactNode
  badges?: ReactNode
  meta?: (CardMeta | false | null | undefined)[]
  actions?: ReactNode
  extra?: ReactNode
  onClick?: () => void
  tight?: boolean
}) {
  const fields = (meta || []).filter((m): m is CardMeta => Boolean(m))
  return (
    <article className={'m-card' + (tight ? ' m-card-tight' : '') + (onClick ? ' clickable' : '')} onClick={onClick}>
      <div className="m-card-head">
        <div className="m-card-main">
          <div className="m-card-title">{title}</div>
          {sub && <div className="m-card-sub">{sub}</div>}
        </div>
        {badges && <div className="m-card-badges">{badges}</div>}
      </div>
      {fields.length > 0 && (
        <div className="m-card-meta">
          {fields.map((f, i) => (
            <div key={i} className={'m-card-kv' + (f.full ? ' full' : '')}>
              {f.label && <span className="m-card-k">{f.label}</span>}
              <div className={'m-card-v' + (f.mono ? ' mono' : '')}>{f.value}</div>
            </div>
          ))}
        </div>
      )}
      {extra}
      {actions && (
        <div className="m-card-actions" onClick={(e) => e.stopPropagation()}>
          {actions}
        </div>
      )}
    </article>
  )
}

/** Renders comma-free inline middot-separated summary fragments without trailing separators. */
export function joinInline(parts: (ReactNode | false | null | undefined)[], sep = ' · ') {
  const list = parts.filter((x) => x !== false && x !== null && x !== undefined && x !== '')
  return list.map((x, i) => <Fragment key={i}>{i > 0 ? sep : null}{x}</Fragment>)
}
