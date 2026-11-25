import {
  ChartLine, Check, ChevronDown, ChevronLeft, ChevronRight, Clock, Container, Copy, Database, Download, ExternalLink, File, Folder, GitBranch, Globe,
  Hammer, HardDrive, HardDriveDownload, KeyRound, Layers, LayoutDashboard, LayoutTemplate, Link, ListChecks, Lock, LogOut, Menu, Pencil, Play, Plug, Plus,
  Package, RefreshCw, Rocket, RotateCcw, ScrollText, Search, Settings, ShieldCheck, Square, SquareTerminal, Trash2, TriangleAlert, Upload, Users, X,
  type LucideIcon,
} from 'lucide-react'

// One icon set for the whole panel: Lucide, fixed 1.5px stroke at every size. Use <Icon name="..."> everywhere
// so nothing ships a one-off SVG with a different weight or style.

const icons: Record<string, LucideIcon> = {
  overview: LayoutDashboard,
  projects: Layers,
  image: Package,
  volume: HardDrive,
  build: Hammer,
  docker: Container,
  databases: Database,
  backups: HardDriveDownload,
  sites: Globe,
  certs: ShieldCheck,
  jobs: ListChecks,
  integrations: Plug,
  access: Users,
  settings: Settings,
  logout: LogOut,
  x: X,
  plus: Plus,
  menu: Menu,
  play: Play,
  stop: Square,
  restart: RotateCcw,
  rocket: Rocket,
  template: LayoutTemplate,
  terminal: SquareTerminal,
  logs: ScrollText,
  trash: Trash2,
  download: Download,
  refresh: RefreshCw,
  check: Check,
  lock: Lock,
  key: KeyRound,
  copy: Copy,
  edit: Pencil,
  search: Search,
  alert: TriangleAlert,
  git: GitBranch,
  folder: Folder,
  file: File,
  upload: Upload,
  chart: ChartLine,
  clock: Clock,
  link: Link,
  external: ExternalLink,
  'chevron-down': ChevronDown,
  'chevron-left': ChevronLeft,
  'chevron-right': ChevronRight,
}

export function Icon({ name, size = 18 }: { name: string; size?: number }) {
  const C = icons[name]
  if (!C) return null
  return <C size={size} strokeWidth={1.5} absoluteStrokeWidth aria-hidden="true" style={{ flex: 'none' }} />
}
