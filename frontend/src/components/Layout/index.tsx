import React from 'react';
import { Box, AppBar, Toolbar, Typography, Container, Button, IconButton, Tooltip } from '@mui/material';
import DarkModeIcon from '@mui/icons-material/DarkMode';
import LightModeIcon from '@mui/icons-material/LightMode';
import LogoutIcon from '@mui/icons-material/Logout';
import { Link as RouterLink, useNavigate } from 'react-router-dom';
import { useThemeMode } from '../../context/ThemeContext';
import { useAuth } from '../../context/AuthContext';

interface LayoutProps {
  children: React.ReactNode;
}

const Layout: React.FC<LayoutProps> = ({ children }) => {
  const { mode, toggleMode } = useThemeMode();
  const { authEnabled, isLoading, isAuthenticated, user, logout } = useAuth();
  const navigate = useNavigate();
  const toggleLabel = mode === 'light' ? 'Switch to dark mode' : 'Switch to light mode';
  // Nothing while the UI config and the session load (no flicker). With auth
  // on the navigation is for signed-in users only (the pages would redirect to login).
  const showNavigation = !isLoading && (!authEnabled || isAuthenticated);
  const showAccount = !isLoading && authEnabled && isAuthenticated;

  const handleLogout = async () => {
    await logout();
    navigate('/login', { replace: true });
  };

  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', minHeight: '100vh' }}>
      <AppBar position="static">
        <Toolbar>
          <Typography variant="h6" component="div" sx={{ flexGrow: 1 }}>
            Full Stack App
          </Typography>
          {showNavigation && (
            <>
              <Button color="inherit" component={RouterLink} to="/">
                Home
              </Button>
              <Button color="inherit" component={RouterLink} to="/health">
                Health
              </Button>
              <Button color="inherit" component={RouterLink} to="/items">
                Items
              </Button>
            </>
          )}
          <Tooltip title={toggleLabel}>
            <IconButton color="inherit" onClick={toggleMode} aria-label={toggleLabel} sx={{ ml: 1 }}>
              {mode === 'light' ? <DarkModeIcon /> : <LightModeIcon />}
            </IconButton>
          </Tooltip>
          {showAccount && (
            <>
              <Typography variant="body2" component="span" sx={{ ml: 2 }} data-testid="current-user">
                {user?.display_name ?? user?.username ?? 'Signed in'}
              </Typography>
              <Button color="inherit" onClick={handleLogout} startIcon={<LogoutIcon />} sx={{ ml: 1 }}>
                Log out
              </Button>
            </>
          )}
        </Toolbar>
      </AppBar>
      <Container component="main" sx={{ mt: 4, mb: 4, flex: '1 0 auto' }}>
        {children}
      </Container>
      <Box
        component="footer"
        sx={{ py: 3, px: 2, mt: 'auto', backgroundColor: 'background.paper', borderTop: 1, borderColor: 'divider' }}
      >
        <Container maxWidth="sm">
          <Typography variant="body2" color="text.secondary" align="center">
            © {new Date().getFullYear()} Full Stack Application
          </Typography>
        </Container>
      </Box>
    </Box>
  );
}

export default Layout;
