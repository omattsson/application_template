import { Typography, Box, Paper } from '@mui/material';

const Home = () => {
  return (
    <Box>
      <Typography variant="h4" component="h1" gutterBottom>
        Welcome to the Full Stack Application
      </Typography>
      <Paper sx={{ p: 3, mt: 3 }}>
        <Typography variant="body1" paragraph>
          This is a modern full-stack application built with:
        </Typography>
        <Typography component="ul" sx={{ pl: 2 }}>
          <li>Go backend with Gin framework</li>
          <li>React frontend with Material-UI</li>
          <li>RESTful API architecture</li>
          <li>Health monitoring</li>
          <li>Swagger documentation</li>
        </Typography>
      </Paper>
    </Box>
  );
};

export default Home;
