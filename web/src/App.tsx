import { lazy, ReactNode, Suspense } from 'react'
import { Navigate, Route, Routes, useLocation } from 'react-router-dom'
import { AuthProvider, useAuth } from './auth'
import Layout from './components/Layout'
import { JobLogProvider, Loading, Page, ToastProvider } from './components/ui'

const Login = lazy(() => import('./pages/Login'))
const Overview = lazy(() => import('./pages/Overview'))
const Projects = lazy(() => import('./pages/Projects'))
const ProjectNew = lazy(() => import('./pages/ProjectNew'))
const ProjectDetail = lazy(() => import('./pages/ProjectDetail'))
const DeploymentDetail = lazy(() => import('./pages/DeploymentDetail'))
const ContainerPage = lazy(() => import('./pages/Container'))
const DockerResources = lazy(() => import('./pages/DockerResources'))
const Databases = lazy(() => import('./pages/Databases'))
const DatabaseDetail = lazy(() => import('./pages/DatabaseDetail'))
const Backups = lazy(() => import('./pages/Backups'))
const Sites = lazy(() => import('./pages/Sites'))
const Certificates = lazy(() => import('./pages/Certificates'))
const Jobs = lazy(() => import('./pages/Jobs'))
const Integrations = lazy(() => import('./pages/Integrations'))
const Access = lazy(() => import('./pages/Access'))
const Settings = lazy(() => import('./pages/Settings'))

function RequireAuth({ children }: { children: ReactNode }) {
  const { me } = useAuth()
  const loc = useLocation()
  if (!me) return <Navigate to={'/login?next=' + encodeURIComponent(loc.pathname + loc.search)} replace />
  return <>{children}</>
}

export default function App() {
  return (
    <ToastProvider>
      <AuthProvider>
        <JobLogProvider>
          <Suspense fallback={<Loading />}>
            <Routes>
              <Route path="/login" element={<Login />} />
              <Route
                element={
                  <RequireAuth>
                    <Layout />
                  </RequireAuth>
                }
              >
                <Route index element={<Overview />} />
                <Route path="projects" element={<Projects />} />
                <Route path="projects/new" element={<ProjectNew />} />
                <Route path="projects/:id/*" element={<ProjectDetail />} />
                <Route path="deployments/:id" element={<DeploymentDetail />} />
                <Route path="containers/:id" element={<ContainerPage />} />
                <Route path="docker" element={<DockerResources />} />
                <Route path="databases" element={<Databases />} />
                <Route path="databases/:id" element={<DatabaseDetail />} />
                <Route path="backups" element={<Backups />} />
                <Route path="sites" element={<Sites />} />
                <Route path="certificates" element={<Certificates />} />
                <Route path="jobs" element={<Jobs />} />
                <Route path="integrations" element={<Integrations />} />
                <Route path="access" element={<Access />} />
                <Route path="settings" element={<Settings />} />
                <Route path="*" element={<Page title="页面不存在"><div className="empty">没有找到这个页面。</div></Page>} />
              </Route>
            </Routes>
          </Suspense>
        </JobLogProvider>
      </AuthProvider>
    </ToastProvider>
  )
}
