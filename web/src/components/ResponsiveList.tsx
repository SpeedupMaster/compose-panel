import { CSSProperties, Fragment, ReactNode } from 'react'
import { useMediaQuery } from './ui/hooks'

export type CardMeta = {
  label?: ReactNode
  value: ReactNode
  mono?: boolean
  full?: boolean
}

/** Single-branch desktop table / mobile card list switcher driven by matchMedia('(max-width: 860px)'). */
export function ResponsiveTable({ table, cards, cardsStyle }: { table: ReactNode; cards: ReactNode; cardsStyle?: CSSProperties }) {
  const isMobile = useMediaQuery('(max-width: 860px)')
  if (isMobile) {
    return <div className="m-cards" style={cardsStyle}>{cards}</div>
  }
  return <>{table}</>
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
    <article
      className={'m-card' + (tight ? ' m-card-tight' : '') + (onClick ? ' clickable' : '')}
      role={onClick ? 'button' : undefined}
      tabIndex={onClick ? 0 : undefined}
      onClick={onClick}
      onKeyDown={
        onClick
          ? (e) => {
              if (e.target !== e.currentTarget) return
              if (e.key === 'Enter' || e.key === ' ') {
                e.preventDefault()
                onClick()
              }
            }
          : undefined
      }
    >
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
