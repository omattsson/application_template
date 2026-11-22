import { Chip } from '@mui/material';
import SyncIcon from '@mui/icons-material/Sync';
import WifiIcon from '@mui/icons-material/Wifi';
import WifiOffIcon from '@mui/icons-material/WifiOff';
import type { ReactElement } from 'react';
import { useWebSocketContext } from '../../context/WebSocketContext';
import type { ConnectionStatus as Status } from '../../hooks/useWebSocket';

interface Presentation {
  label: string;
  /** The accessible name: it starts with the visible label. */
  description: string;
  icon: ReactElement;
}

const PRESENTATION: Record<Status, Presentation> = {
  open: { label: 'Live', description: 'Live: changes show up as they happen', icon: <WifiIcon /> },
  connecting: {
    label: 'Connecting',
    description: 'Connecting: waiting for live updates',
    icon: <SyncIcon />,
  },
  closing: {
    label: 'Offline',
    description: 'Offline: the data may be out of date',
    icon: <WifiOffIcon />,
  },
  closed: {
    label: 'Offline',
    description: 'Offline: the data may be out of date',
    icon: <WifiOffIcon />,
  },
};

/**
 * The state of the live-update socket, for the app bar. The state is in the
 * text and in the shape of the icon, never in the colour alone, and it is a
 * polite live region so a screen reader hears when the socket drops. Nothing
 * is shown when no socket is wanted (auth on and nobody signed in).
 */
const ConnectionStatus = () => {
  const { connectionStatus, active } = useWebSocketContext();
  if (!active) return null;
  const { label, description, icon } = PRESENTATION[connectionStatus];
  return (
    <Chip
      role="status"
      aria-label={description}
      icon={icon}
      label={label}
      size="small"
      variant="outlined"
      // The app bar's own colours: they already have the contrast.
      sx={{ color: 'inherit', borderColor: 'currentColor', ml: 1, '& .MuiChip-icon': { color: 'inherit' } }}
    />
  );
};

export default ConnectionStatus;
