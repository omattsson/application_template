import { lazy, Suspense } from 'react';
import { Routes, Route, useLocation } from 'react-router-dom';
// Every page loads on demand, so the first load carries the shell and the page that was asked for.
// NotFound stays in the entry chunk: it is tiny and the fallback for any path.
const Home = lazy(() => import('./pages/Home'));
const Health = lazy(() => import('./pages/Health'));
const Items = lazy(() => import('./pages/Items'));
const AdminUsers = lazy(() => import('./pages/Admin/Users'));
const ApiKeys = lazy(() => import('./pages/Account/ApiKeys'));
const AuditLogs = lazy(() => import('./pages/Admin/AuditLogs'));
const Profile = lazy(() => import('./pages/Account/Profile'));
const Login = lazy(() => import('./pages/Login'));
const AuthCallback = lazy(() => import('./pages/AuthCallback'));
const ForgotPassword = lazy(() => import('./pages/ForgotPassword'));
const ResetPassword = lazy(() => import('./pages/ResetPassword'));
import NotFound from './pages/NotFound';
import ErrorBoundary from './components/ErrorBoundary';
import LoadingState from './components/LoadingState';
import ProtectedRoute from './components/ProtectedRoute';
import { useAuth } from './context/AuthContext';
import { useUIConfig } from './context/UIConfigContext';

/** The password reset pages (they exist only with PASSWORD_RESET_ENABLED and for a visitor who is not signed in). */
const PASSWORD_RESET_PATHS: readonly string[] = ['/forgot-password', '/reset-password'];

/** The pages that exist only with auth on: the account pages of a signed-in user and the admin audit log. */
const AUTH_ONLY_PATHS: readonly string[] = ['/account', '/account/api-keys', '/admin/audit-logs'];

const AppRoutes = () => {
  const { pathname } = useLocation();
  const { config } = useUIConfig();
  const { authEnabled, isLoading, isAuthenticated } = useAuth();
  const resetRoutes = config.password_reset_enabled && !isAuthenticated;
  // The account page changes the local password: it needs auth on and the password form.
  const profileRoute = authEnabled && config.local_auth_enabled;
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
        <Suspense fallback={<LoadingState />}>
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
            {profileRoute && <Route path="/account" element={<ProtectedRoute><Profile /></ProtectedRoute>} />}
            {authEnabled && (
              <Route path="/account/api-keys" element={<ProtectedRoute><ApiKeys /></ProtectedRoute>} />
            )}
            {authEnabled && (
              <Route path="/admin/audit-logs" element={<ProtectedRoute roles={['admin']}><AuditLogs /></ProtectedRoute>} />
            )}
            <Route path="*" element={<NotFound />} />
          </Routes>
        </Suspense>
      )}
    </ErrorBoundary>
  );
};

export default AppRoutes;
