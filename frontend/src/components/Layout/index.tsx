import React, { useEffect, useState } from 'react';
import {
  Box,
  AppBar,
  Toolbar,
  Typography,
  Container,
  Button,
  IconButton,
  Link,
  Tooltip,
  Drawer,
  List,
  ListItem,
  ListItemButton,
  ListItemIcon,
  ListItemText,
  Divider,
  useMediaQuery,
  useTheme,
} from '@mui/material';
import DarkModeIcon from '@mui/icons-material/DarkMode';
import LightModeIcon from '@mui/icons-material/LightMode';
import LogoutIcon from '@mui/icons-material/Logout';
import MenuIcon from '@mui/icons-material/Menu';
import { Link as RouterLink, useNavigate } from 'react-router-dom';
import { useThemeMode } from '../../context/ThemeContext';
import { useAuth } from '../../context/AuthContext';
import { useUIConfig } from '../../context/UIConfigContext';
import ConnectionStatus from '../ConnectionStatus';
import { useNavItems } from './navItems';

interface LayoutProps {
  children: React.ReactNode;
}

const DRAWER_ID = 'navigation-drawer';

const Layout: React.FC<LayoutProps> = ({ children }) => {
  const { mode, toggleMode } = useThemeMode();
  const { authEnabled, isLoading, isAuthenticated, user, logout } = useAuth();
  const { config, loading: configLoading } = useUIConfig();
  const navigate = useNavigate();
  const theme = useTheme();
  // Below `sm` the links move into a drawer.
  const isNarrow = useMediaQuery(theme.breakpoints.down('sm'));
  const [drawerOpen, setDrawerOpen] = useState(false);
  const toggleLabel = mode === 'light' ? 'Switch to dark mode' : 'Switch to light mode';
  // Nothing while the UI config and the session load (no flicker). With auth
  // on the navigation is for signed-in users only (the pages would redirect to login).
  const navItems = useNavItems();
  const showNavigation = navItems.length > 0;
  const showAccount = !isLoading && authEnabled && isAuthenticated;
  // The account page (`/account`) exists with auth on and the password form only (see routes.tsx).
  const accountPageExists = showAccount && config.local_auth_enabled;
  const userName = user?.display_name ?? user?.username ?? 'Signed in';
  // No title while the config loads (no flash of the fallback name).
  const title = configLoading ? '' : config.title;

  useEffect(() => {
    if (!configLoading) document.title = config.title;
  }, [configLoading, config.title]);

  // A drawer that was open must not reopen when the window narrows again.
  useEffect(() => {
    if (!isNarrow) setDrawerOpen(false);
  }, [isNarrow]);

  const closeDrawer = () => setDrawerOpen(false);

  const handleLogout = async () => {
    await logout();
    navigate('/login', { replace: true });
  };

  const handleDrawerLogout = async () => {
    closeDrawer();
    await handleLogout();
  };

  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', minHeight: '100vh' }}>
      <AppBar position="static">
        <Toolbar>
          {isNarrow && showNavigation && (
            <IconButton
              color="inherit"
              edge="start"
              aria-label="Open navigation menu"
              aria-expanded={drawerOpen}
              aria-controls={DRAWER_ID}
              onClick={() => setDrawerOpen(true)}
              sx={{ mr: 1 }}
            >
              <MenuIcon />
            </IconButton>
          )}
          <Typography variant="h6" component="div" noWrap sx={{ flexGrow: 1, minWidth: 0 }}>
            {title}
          </Typography>
          {!isNarrow &&
            navItems.map((item) => (
              <Button key={item.path} color="inherit" component={RouterLink} to={item.path}>
                {item.label}
              </Button>
            ))}
          <ConnectionStatus />
          <Tooltip title={toggleLabel}>
            <IconButton color="inherit" onClick={toggleMode} aria-label={toggleLabel} sx={{ ml: 1 }}>
              {mode === 'light' ? <DarkModeIcon /> : <LightModeIcon />}
            </IconButton>
          </Tooltip>
          {!isNarrow && showAccount && (
            <>
              {accountPageExists ? (
                <Link
                  component={RouterLink}
                  to="/account"
                  color="inherit"
                  variant="body2"
                  underline="hover"
                  sx={{ ml: 2 }}
                  data-testid="current-user"
                >
                  {userName}
                </Link>
              ) : (
                <Typography variant="body2" component="span" sx={{ ml: 2 }} data-testid="current-user">
                  {userName}
                </Typography>
              )}
              <Button color="inherit" component={RouterLink} to="/account/api-keys" sx={{ ml: 1 }}>
                API keys
              </Button>
              <Button color="inherit" onClick={handleLogout} startIcon={<LogoutIcon />} sx={{ ml: 1 }}>
                Log out
              </Button>
            </>
          )}
        </Toolbar>
      </AppBar>
      {isNarrow && showNavigation && (
        <Drawer
          id={DRAWER_ID}
          open={drawerOpen}
          onClose={closeDrawer}
          slotProps={{ paper: { 'aria-label': 'Navigation menu' } }}
        >
          <Box component="nav" aria-label="Main navigation" sx={{ width: 260 }}>
            <List>
              {navItems.map((item) => (
                <ListItem key={item.path} disablePadding>
                  <ListItemButton component={RouterLink} to={item.path} onClick={closeDrawer}>
                    <ListItemText primary={item.label} />
                  </ListItemButton>
                </ListItem>
              ))}
            </List>
            {showAccount && (
              <>
                <Divider />
                <List>
                  <ListItem>
                    {accountPageExists ? (
                      <Link
                        component={RouterLink}
                        to="/account"
                        color="inherit"
                        underline="hover"
                        onClick={closeDrawer}
                        data-testid="current-user"
                      >
                        {userName}
                      </Link>
                    ) : (
                      <Typography component="span" data-testid="current-user">
                        {userName}
                      </Typography>
                    )}
                  </ListItem>
                  <ListItem disablePadding>
                    <ListItemButton component={RouterLink} to="/account/api-keys" onClick={closeDrawer}>
                      <ListItemText primary="API keys" />
                    </ListItemButton>
                  </ListItem>
                  <ListItem disablePadding>
                    <ListItemButton onClick={handleDrawerLogout}>
                      <ListItemIcon sx={{ minWidth: 40 }}>
                        <LogoutIcon />
                      </ListItemIcon>
                      <ListItemText primary="Log out" />
                    </ListItemButton>
                  </ListItem>
                </List>
              </>
            )}
          </Box>
        </Drawer>
      )}
      <Container component="main" sx={{ mt: 4, mb: 4, flex: '1 0 auto' }}>
        {children}
      </Container>
      <Box
        component="footer"
        sx={{ py: 3, px: 2, mt: 'auto', backgroundColor: 'background.paper', borderTop: 1, borderColor: 'divider' }}
      >
        <Container maxWidth="sm">
          <Typography variant="body2" color="text.secondary" align="center">
            © {new Date().getFullYear()} {title}
          </Typography>
        </Container>
      </Box>
    </Box>
  );
}

export default Layout;
