import { Routes, Route } from 'react-router-dom';
import Home from './pages/Home';
import Health from './pages/Health';

const AppRoutes = () => {
  return (
    <Routes>
      <Route path="/" element={<Home />} />
      <Route path="/health" element={<Health />} />
    </Routes>
  );
};

export default AppRoutes;
