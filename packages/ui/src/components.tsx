import { Card, ConfigProvider, Tag } from 'antd';
import type { PropsWithChildren, ReactNode } from 'react';

import { jrpTheme } from './theme';

export type SignalTone = 'active' | 'attention' | 'muted';

const toneColors: Record<SignalTone, string> = {
  active: 'success',
  attention: 'warning',
  muted: 'default',
};

export function JrpThemeProvider({ children }: PropsWithChildren) {
  return (
    <ConfigProvider theme={jrpTheme}>
      <div className="jrp-theme-root" data-ui="jrp-theme-provider">
        {children}
      </div>
    </ConfigProvider>
  );
}

interface SignalTagProps {
  children: ReactNode;
  tone?: SignalTone;
}

export function SignalTag({ children, tone = 'muted' }: SignalTagProps) {
  return (
    <Tag className="jrp-signal-tag" color={toneColors[tone]} data-ui="jrp-signal-tag">
      {children}
    </Tag>
  );
}

interface TelemetryPanelProps extends PropsWithChildren {
  eyebrow: string;
  title: string;
}

export function TelemetryPanel({ children, eyebrow, title }: TelemetryPanelProps) {
  return (
    <Card className="telemetry-panel" data-ui="telemetry-panel" variant="borderless">
      <p className="panel-eyebrow">{eyebrow}</p>
      <h2>{title}</h2>
      {children}
    </Card>
  );
}
