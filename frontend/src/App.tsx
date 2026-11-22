import { BrowserRouter } from 'react-router-dom';
import AppRoutes from './routes';
import Layout from './components/Layout';
import { ThemeModeProvider } from './context/ThemeContext';
import { UIConfigProvider } from './context/UIConfigContext';
import { AuthProvider } from './context/AuthContext';
import { WebSocketProvider } from './context/WebSocketContext';
import { NotificationProvider } from './context/NotificationContext';

function App() {
  return (
    <ThemeModeProvider>
      <UIConfigProvider>
        <AuthProvider>
          <BrowserRouter>
            <NotificationProvider>
              <WebSocketProvider>
                <Layout>
                  <AppRoutes />
                </Layout>
              </WebSocketProvider>
            </NotificationProvider>
          </BrowserRouter>
        </AuthProvider>
      </UIConfigProvider>
    </ThemeModeProvider>
  );
}

export default App;
