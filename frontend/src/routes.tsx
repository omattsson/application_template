import { Routes, Route, useLocation } from 'react-router-dom';
import Home from './pages/Home';
import Health from './pages/Health';
import Items from './pages/Items';
import AdminUsers from './pages/Admin/Users';
import ApiKeys from './pages/Account/ApiKeys';
import AuditLogs from './pages/Admin/AuditLogs';
import Login from './pages/Login';
import AuthCallback from './pages/AuthCallback';
import ForgotPassword from './pages/ForgotPassword';
import ResetPassword from './pages/ResetPassword';
import NotFound from './pages/NotFound';
import ErrorBoundary from './components/ErrorBoundary';
import LoadingState from './components/LoadingState';
import ProtectedRoute from './components/ProtectedRoute';
import { useAuth } from './context/AuthContext';
import { useUIConfig } from './context/UIConfigContext';

/** The password reset pages (they exist only with PASSWORD_RESET_ENABLED and for a visitor who is not signed in). */
const PASSWORD_RESET_PATHS: readonly string[] = ['/forgot-password', '/reset-password'];

/** The pages that exist only with auth on: the account pages of a signed-in user and the admin audit log. */
const AUTH_ONLY_PATHS: readonly string[] = ['/account/api-keys', '/admin/audit-logs'];

const AppRoutes = () => {
  const { pathname } = useLocation();
  const { config } = useUIConfig();
  const { authEnabled, isLoading, isAuthenticated } = useAuth();
  const resetRoutes = config.password_reset_enabled && !isAuthenticated;
  const normalizedPath = pathname.replace(/\/+$/, '');
  // While the config or the session loads, a reset or account page shows the loading state
  // instead of the not-found page (its routes are not registered yet).
  const resetPending = isLoading && PASSWORD_RESET_PATHS.includes(normalizedPath);
  const authOnlyPending = isLoading && AUTH_ONLY_PATHS.includes(normalizedPath);
  return (
    // Keyed by path: a page that crashed is reset when the user navigates away.
    <ErrorBoundary key={pathname}>
      {resetPending || authOnlyPending ? (
        <LoadingState />
      ) : (
        <Routes>
          {/* Public: the sign-in page and the OIDC landing page. */}
          <Route path="/login" element={<Login />} />
          <Route path="/auth/callback" element={<AuthCallback />} />
          {/* Public, and only with the password reset on and nobody signed in. */}
          {resetRoutes && <Route path="/forgot-password" element={<ForgotPassword />} />}
          {resetRoutes && <Route path="/reset-password" element={<ResetPassword />} />}
          {/* ProtectedRoute renders the page directly when auth is disabled. */}
          <Route path="/" element={<ProtectedRoute><Home /></ProtectedRoute>} />
          <Route path="/health" element={<ProtectedRoute><Health /></ProtectedRoute>} />
          <Route path="/items" element={<ProtectedRoute><Items /></ProtectedRoute>} />
          <Route path="/admin/users" element={<ProtectedRoute roles={['admin']}><AdminUsers /></ProtectedRoute>} />
          {/* Only with auth on: with auth off the path is not found. */}
          {authEnabled && (
            <Route path="/account/api-keys" element={<ProtectedRoute><ApiKeys /></ProtectedRoute>} />
          )}
          {authEnabled && (
            <Route path="/admin/audit-logs" element={<ProtectedRoute roles={['admin']}><AuditLogs /></ProtectedRoute>} />
          )}
          <Route path="*" element={<NotFound />} />
        </Routes>
      )}
    </ErrorBoundary>
  );
};

export default AppRoutes;
