import { ReactNode } from 'react'
import { Navigate, Route, Routes, useLocation } from 'react-router-dom'
import { AuthProvider, useAuth } from './auth'
import Layout from './components/Layout'
import { JobLogProvider, Page, ToastProvider } from './components/ui'
import Login from './pages/Login'
import Overview from './pages/Overview'
import Projects from './pages/Projects'
import ProjectNew from './pages/ProjectNew'
import ProjectDetail from './pages/ProjectDetail'
import DeploymentDetail from './pages/DeploymentDetail'
import ContainerPage from './pages/Container'
import DockerResources from './pages/DockerResources'
import Databases from './pages/Databases'
import DatabaseDetail from './pages/DatabaseDetail'
import Backups from './pages/Backups'
import Sites from './pages/Sites'
import Certificates from './pages/Certificates'
import Jobs from './pages/Jobs'
import Integrations from './pages/Integrations'
import Access from './pages/Access'
import Settings from './pages/Settings'

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
        </JobLogProvider>
      </AuthProvider>
    </ToastProvider>
  )
}
