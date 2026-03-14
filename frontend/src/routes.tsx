import { Routes, Route } from 'react-router-dom';
import Home from './pages/Home';
import Health from './pages/Health';
import Items from './pages/Items';

const AppRoutes = () => {
  return (
    <Routes>
      <Route path="/" element={<Home />} />
      <Route path="/health" element={<Health />} />
      <Route path="/items" element={<Items />} />
    </Routes>
  );
};

export default AppRoutes;
