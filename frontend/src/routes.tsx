import { Routes, Route, useLocation } from 'react-router-dom';
import Home from './pages/Home';
import Health from './pages/Health';
import Items from './pages/Items';
import NotFound from './pages/NotFound';
import ErrorBoundary from './components/ErrorBoundary';

const AppRoutes = () => {
  const { pathname } = useLocation();
  return (
    // Keyed by path: a page that crashed is reset when the user navigates away.
    <ErrorBoundary key={pathname}>
      <Routes>
        <Route path="/" element={<Home />} />
        <Route path="/health" element={<Health />} />
        <Route path="/items" element={<Items />} />
        <Route path="*" element={<NotFound />} />
      </Routes>
    </ErrorBoundary>
  );
};

export default AppRoutes;
