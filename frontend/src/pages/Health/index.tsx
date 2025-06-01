import { useEffect, useState } from 'react';
import { Typography, Box, Paper, CircularProgress, Alert } from '@mui/material';
import { healthService } from '../../api/client';

interface HealthStatus {
  live: boolean;
  ready: boolean;
  error?: string;
}

const Health = () => {
  const [status, setStatus] = useState<HealthStatus>({ live: false, ready: false });
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    const checkHealth = async () => {
      try {
        const [liveness, readiness] = await Promise.all([
          healthService.checkLiveness(),
          healthService.checkReadiness()
        ]);
        
        setStatus({
          live: liveness?.status === 'UP',
          ready: readiness?.status === 'UP'
        });
      } catch (error) {
        setStatus({
          live: false,
          ready: false,
          error: 'Failed to fetch health status'
        });
      } finally {
        setLoading(false);
      }
    };

    checkHealth();
    const interval = setInterval(checkHealth, 30000); // Check every 30 seconds
    return () => clearInterval(interval);
  }, []);

  if (loading) {
    return (
      <Box display="flex" justifyContent="center" alignItems="center" minHeight="200px">
        <CircularProgress />
      </Box>
    );
  }

  return (
    <Box>
      <Typography variant="h4" component="h1" gutterBottom>
        System Health
      </Typography>
      
      {status.error ? (
        <Alert severity="error" sx={{ mt: 2 }}>
          {status.error}
        </Alert>
      ) : (
        <Box sx={{ mt: 3 }}>
          <Paper sx={{ p: 3, mb: 2 }}>
            <Typography variant="h6" gutterBottom>
              Liveness Check
            </Typography>
            <Alert severity={status.live ? "success" : "error"}>
              {status.live ? "System is live" : "System is not responding"}
            </Alert>
          </Paper>

          <Paper sx={{ p: 3 }}>
            <Typography variant="h6" gutterBottom>
              Readiness Check
            </Typography>
            <Alert severity={status.ready ? "success" : "error"}>
              {status.ready ? "System is ready to handle requests" : "System is not ready"}
            </Alert>
          </Paper>
        </Box>
      )}
    </Box>
  );
};

export default Health;
