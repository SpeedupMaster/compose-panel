// CodeMirror setup for <CodeArea>. Loaded on demand (dynamic import) so pages
// without an editor don't pay for it.
import { autocompletion, closeBrackets, closeBracketsKeymap } from '@codemirror/autocomplete'
import { defaultKeymap, history, historyKeymap, indentWithTab } from '@codemirror/commands'
import { json } from '@codemirror/lang-json'
import { PostgreSQL, sql } from '@codemirror/lang-sql'
import { yaml } from '@codemirror/lang-yaml'
import { bracketMatching, foldGutter, foldKeymap, HighlightStyle, indentOnInput, indentUnit, StreamLanguage, syntaxHighlighting } from '@codemirror/language'
import { dockerFile } from '@codemirror/legacy-modes/mode/dockerfile'
import { nginx } from '@codemirror/legacy-modes/mode/nginx'
import { shell } from '@codemirror/legacy-modes/mode/shell'
import { toml } from '@codemirror/legacy-modes/mode/toml'
import { highlightSelectionMatches, searchKeymap } from '@codemirror/search'
import { Compartment, EditorState, Extension, Prec } from '@codemirror/state'
import {
  Decoration, drawSelection, EditorView, highlightActiveLine, highlightActiveLineGutter, highlightSpecialChars, keymap,
  lineNumbers, MatchDecorator, placeholder as placeholderExt, ViewPlugin, ViewUpdate,
} from '@codemirror/view'
import { tags as t } from '@lezer/highlight'

export type CodeLang = 'yaml' | 'env' | 'sql' | 'json' | 'nginx' | 'shell' | 'dockerfile' | 'toml' | 'text'

// .env / KEY=value files: comments, keys, quoted or bare values, ${VAR} references.
const dotenv = StreamLanguage.define<{ value: boolean }>({
  name: 'dotenv',
  startState: () => ({ value: false }),
  token(stream, state) {
    if (stream.sol()) state.value = false
    if (stream.sol() && stream.match(/^\s*#.*/)) return 'comment'
    if (stream.eatSpace()) return null
    if (!state.value) {
      if (stream.match(/^export\b/)) return 'keyword'
      if (stream.match(/^[^=\s#]+/)) return 'propertyName'
      if (stream.eat('=')) { state.value = true; return 'operator' }
      stream.next()
      return null
    }
    if (stream.match(/^\$\{[^}]*\}/) || stream.match(/^\$[A-Za-z_]\w*/)) return 'variableName.special'
    if (stream.match(/^"(?:[^"\\]|\\.)*"?/) || stream.match(/^'[^']*'?/)) return 'string'
    if (stream.match(/^\s+#.*/)) return 'comment'
    stream.match(/^[^$\s"']+/) || stream.next()
    return 'string'
  },
  languageData: { commentTokens: { line: '#' } },
})

function language(lang: CodeLang): Extension {
  switch (lang) {
    case 'yaml': return yaml()
    case 'env': return dotenv
    case 'sql': return sql({ dialect: PostgreSQL })
    case 'json': return json()
    case 'nginx': return StreamLanguage.define(nginx)
    case 'shell': return StreamLanguage.define(shell)
    case 'dockerfile': return StreamLanguage.define(dockerFile)
    case 'toml': return StreamLanguage.define(toml)
    default: return []
  }
}

// Palette follows the panel: brown keys, green strings, blue numbers, amber ${VARS}.
const highlight = HighlightStyle.define([
  { tag: [t.propertyName, t.definition(t.propertyName), t.attributeName], color: '#93370D' },
  { tag: [t.string, t.special(t.string), t.inserted], color: '#17663E' },
  { tag: [t.number, t.integer, t.float], color: '#1D44B8' },
  { tag: [t.bool, t.null, t.atom, t.constant(t.name)], color: '#7A3E9D' },
  { tag: [t.keyword, t.operatorKeyword, t.controlKeyword, t.definitionKeyword, t.moduleKeyword], color: '#7A3E9D', fontWeight: '500' },
  { tag: [t.comment, t.lineComment, t.blockComment], color: '#8A8D93', fontStyle: 'italic' },
  { tag: [t.meta, t.labelName, t.annotation, t.processingInstruction], color: '#7A4F00' },
  { tag: [t.special(t.variableName), t.variableName], color: '#B54708' },
  { tag: [t.typeName, t.className, t.tagName], color: '#1D44B8' },
  { tag: [t.operator, t.separator, t.punctuation, t.squareBracket, t.brace, t.paren], color: '#5F6268' },
  { tag: t.invalid, color: '#A41E14' },
])

// ${VAR} / ${VAR:-default} inside compose files and env values.
const interpolation = ViewPlugin.fromClass(class {
  decorations
  matcher = new MatchDecorator({ regexp: /\$\{[A-Za-z_][\w]*(?:[:?+-][^}]*)?\}/g, decoration: Decoration.mark({ class: 'cm-interp' }) })
  constructor(view: EditorView) { this.decorations = this.matcher.createDeco(view) }
  update(u: ViewUpdate) { this.decorations = this.matcher.updateDeco(u, this.decorations) }
}, { decorations: (v) => v.decorations })

const theme = EditorView.theme({
  '&': { fontSize: '13px', height: '100%', backgroundColor: '#fff' },
  '&.cm-focused': { outline: 'none' },
  '.cm-scroller': { fontFamily: 'var(--mono)', lineHeight: '1.6' },
  '.cm-content': { padding: '8px 0', caretColor: '#18191B' },
  '.cm-line': { padding: '0 12px 0 6px' },
  '.cm-gutters': { backgroundColor: '#FAFAF8', borderRight: '1px solid #EEEEEB', color: '#A3A5AA' },
  '.cm-lineNumbers .cm-gutterElement': { padding: '0 8px 0 12px', minWidth: '36px' },
  '.cm-foldGutter .cm-gutterElement': { padding: '0 4px', cursor: 'pointer' },
  '.cm-activeLine': { backgroundColor: '#FAF7F2' },
  '.cm-activeLineGutter': { backgroundColor: '#F4F1EB', color: '#5F6268' },
  '.cm-selectionBackground, &.cm-focused .cm-selectionBackground, ::selection': { backgroundColor: '#F6DCC4 !important' },
  '.cm-selectionMatch': { backgroundColor: '#FBEBDC' },
  '&.cm-focused .cm-matchingBracket': { backgroundColor: '#E8F5EE', outline: '1px solid #A9DCC0' },
  '.cm-placeholder': { color: '#A3A5AA' },
  '.cm-interp': { color: '#B54708', backgroundColor: '#FDF1E7', borderRadius: '3px' },
  '.cm-panels': { backgroundColor: '#FAFAF8', borderColor: '#E2E2DE' },
  '.cm-panel.cm-search': { fontFamily: 'var(--font)', fontSize: '13px', padding: '6px 10px' },
  '.cm-panel.cm-search input, .cm-panel.cm-search button': { fontFamily: 'inherit', fontSize: '12px', borderRadius: '4px' },
  '.cm-textfield': { border: '1px solid #CFCFCA', padding: '2px 6px' },
  '.cm-button': { backgroundImage: 'none', backgroundColor: '#fff', border: '1px solid #D6D6D1' },
  '.cm-tooltip': { border: '1px solid #E2E2DE', borderRadius: '6px', backgroundColor: '#fff' },
})

export type EditorHandle = { setValue: (v: string) => void; setReadOnly: (ro: boolean) => void; destroy: () => void }

export function createEditor(parent: HTMLElement, opts: {
  value: string
  lang: CodeLang
  readOnly: boolean
  placeholder?: string
  label?: string
  onChange: (v: string) => void
  onSubmit?: () => void // Ctrl/Cmd+Enter
}): EditorHandle {
  const ro = new Compartment()
  const roExt = (r: boolean) => [EditorState.readOnly.of(r), EditorView.editable.of(!r)]
  const view = new EditorView({
    parent,
    state: EditorState.create({
      doc: opts.value,
      extensions: [
        lineNumbers(), highlightActiveLineGutter(), foldGutter(), highlightSpecialChars(), history(), drawSelection(),
        indentOnInput(), bracketMatching(), closeBrackets(), autocompletion({ activateOnTyping: opts.lang === 'sql' }),
        highlightActiveLine(), highlightSelectionMatches(), indentUnit.of('  '), EditorState.tabSize.of(2),
        keymap.of([...closeBracketsKeymap, ...defaultKeymap, ...searchKeymap, ...historyKeymap, ...foldKeymap, indentWithTab]),
        language(opts.lang), syntaxHighlighting(highlight), interpolation, theme,
        opts.placeholder ? placeholderExt(opts.placeholder) : [],
        opts.onSubmit ? Prec.highest(keymap.of([{ key: 'Mod-Enter', run: () => { opts.onSubmit!(); return true } }])) : [],
        EditorView.contentAttributes.of({ 'aria-label': opts.label || '代码', spellcheck: 'false', autocorrect: 'off', autocapitalize: 'off' }),
        ro.of(roExt(opts.readOnly)),
        EditorView.updateListener.of((u) => { if (u.docChanged) opts.onChange(u.state.doc.toString()) }),
      ],
    }),
  })
  return {
    setValue(v) {
      const cur = view.state.doc.toString()
      if (v !== cur) view.dispatch({ changes: { from: 0, to: cur.length, insert: v } })
    },
    setReadOnly(r) { view.dispatch({ effects: ro.reconfigure(roExt(r)) }) },
    destroy() { view.destroy() },
  }
}
