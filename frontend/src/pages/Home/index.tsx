import { Typography, Box, Paper, List, ListItem, ListItemButton, ListItemText } from '@mui/material';
import { Link as RouterLink } from 'react-router-dom';
import { useUIConfig } from '../../context/UIConfigContext';
import { useNavItems } from '../../components/Layout/navItems';

const Home = () => {
  const { config, loading } = useUIConfig();
  // The Home page itself is not a place to start.
  const startItems = useNavItems().filter((item) => item.path !== '/');

  return (
    <Box>
      <Typography variant="h4" component="h1" gutterBottom>
        {/* No fallback name while the config loads (no flash). */}
        Welcome to {loading ? '' : config.title}
      </Typography>
      <Typography variant="body1">
        This is the starting point for your application: a Go API, a React web UI and a MySQL database that run together.
      </Typography>
      {startItems.length > 0 && (
        <Paper component="section" aria-labelledby="home-start" sx={{ p: 3, mt: 3 }}>
          <Typography variant="h5" component="h2" id="home-start">
            Where to start
          </Typography>
          <List>
            {startItems.map((item) => (
              <ListItem key={item.path} disablePadding>
                <ListItemButton component={RouterLink} to={item.path}>
                  <ListItemText primary={item.label} secondary={item.description} />
                </ListItemButton>
              </ListItem>
            ))}
          </List>
        </Paper>
      )}
      <Paper component="section" aria-labelledby="home-own" sx={{ p: 3, mt: 3 }}>
        <Typography variant="h5" component="h2" id="home-own" gutterBottom>
          Make this your own
        </Typography>
        <Typography component="ul" sx={{ pl: 2, m: 0 }}>
          <li>Copy the Items shape for your own resources: the steps are in EXTENDING.md.</li>
          <li>Set APP_TITLE to change the name shown in the app bar, the footer and the browser tab.</li>
          <li>Remove the pages and modules that you do not need.</li>
        </Typography>
      </Paper>
    </Box>
  );
};

export default Home;
