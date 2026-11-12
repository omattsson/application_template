import { Routes, Route, useLocation } from 'react-router-dom';
import Home from './pages/Home';
import Health from './pages/Health';
import Items from './pages/Items';
import Login from './pages/Login';
import AuthCallback from './pages/AuthCallback';
import NotFound from './pages/NotFound';
import ErrorBoundary from './components/ErrorBoundary';
import ProtectedRoute from './components/ProtectedRoute';

const AppRoutes = () => {
  const { pathname } = useLocation();
  return (
    // Keyed by path: a page that crashed is reset when the user navigates away.
    <ErrorBoundary key={pathname}>
      <Routes>
        {/* Public: the sign-in page and the OIDC landing page. */}
        <Route path="/login" element={<Login />} />
        <Route path="/auth/callback" element={<AuthCallback />} />
        {/* ProtectedRoute renders the page directly when auth is disabled. */}
        <Route path="/" element={<ProtectedRoute><Home /></ProtectedRoute>} />
        <Route path="/health" element={<ProtectedRoute><Health /></ProtectedRoute>} />
        <Route path="/items" element={<ProtectedRoute><Items /></ProtectedRoute>} />
        <Route path="*" element={<NotFound />} />
      </Routes>
    </ErrorBoundary>
  );
};

export default AppRoutes;
