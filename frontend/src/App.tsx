import { BrowserRouter } from 'react-router-dom';
import AppRoutes from './routes';
import Layout from './components/Layout';
import { ThemeModeProvider } from './context/ThemeContext';
import { WebSocketProvider } from './context/WebSocketContext';

function App() {
  return (
    <ThemeModeProvider>
      <BrowserRouter>
        <WebSocketProvider>
          <Layout>
            <AppRoutes />
          </Layout>
        </WebSocketProvider>
      </BrowserRouter>
    </ThemeModeProvider>
  );
}

export default App;
