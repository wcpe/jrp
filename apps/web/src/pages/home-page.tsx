import { useQuery } from '@tanstack/react-query';
import { useNavigate } from '@tanstack/react-router';
import { useEffect } from 'react';

import { SignalTag, TelemetryPanel, type SignalTone } from '@jrp/ui';

import { fetchHealth, type HealthSnapshot } from '../api/health';
import { fetchSession } from '../api/session';
import { NotificationPanel } from './notification-panel';
import { SessionBar } from './session-bar';

const COMPATIBILITY_BASELINE = 'jrp@e1f1d6ab1b591ec9b596f4b1b6a81cd2960c6314';

function ConsoleHeader() {
  return (
    <header className="console-header">
      <div className="brand-lockup">
        <span className="brand-mark" aria-hidden="true">
          J
        </span>
        <div>
          <strong>JRP</strong>
          <span>网络信号台</span>
        </div>
      </div>
      <div className="header-channel">
        <span className="channel-light" aria-hidden="true" />
        控制通道 / 本机
      </div>
    </header>
  );
}

function HeroSection() {
  return (
    <section className="hero-section">
      <div className="hero-copy">
        <p className="section-code">控制界面 / 001</p>
        <h1>
          让控制面可见，
          <br />让 Core 保持独立。
        </h1>
        <p className="hero-summary">
          JRP 将管理 API、嵌入式 Web 与运行适配器留在 jrps 外壳，Core 只承载协议、传输与代理数据面。
        </p>
      </div>
      <div className="hero-index" aria-label="当前阶段">
        <span>当前阶段</span>
        <strong>P1 / 骨架联通</strong>
        <small>单机优先 · 固定兼容基线</small>
      </div>
    </section>
  );
}

function LinkTrack() {
  return (
    <div className="link-track" aria-label="稳定端口边界">
      <span className="track-node" />
      <span className="track-line" />
      <span className="track-label">不可变快照 / 运行事件</span>
      <span className="track-line" />
      <span className="track-node" />
    </div>
  );
}

function TopologySection() {
  return (
    <section className="topology-section" aria-labelledby="topology-title">
      <div className="section-heading">
        <div>
          <p className="section-code">边界图 / 实时</p>
          <h2 id="topology-title">控制面与 Core 链路</h2>
        </div>
        <SignalTag tone="active">边界清晰</SignalTag>
      </div>
      <div className="topology-grid">
        <TelemetryPanel eyebrow="控制面" title="jrps 控制外壳">
          <ul className="signal-list">
            <li>React Web / 管理 API</li>
            <li>SQLite 与配置协调</li>
            <li>通知、审计与运行适配</li>
          </ul>
        </TelemetryPanel>
        <LinkTrack />
        <TelemetryPanel eyebrow="数据面" title="独立 Core">
          <ul className="signal-list">
            <li>wire v1 / v2</li>
            <li>连接传输与控制会话</li>
            <li>代理、访客与 NAT</li>
          </ul>
        </TelemetryPanel>
      </div>
    </section>
  );
}

interface HealthPanelProps {
  data: HealthSnapshot | undefined;
  error: Error | null;
  isPending: boolean;
  onRetry: () => void;
}

function HealthPanel({ data, error, isPending, onRetry }: HealthPanelProps) {
  const headline = data ? 'jrps 在线' : error ? 'jrps 无法连接' : '正在建立链路';
  const tone: SignalTone = data ? 'active' : error ? 'attention' : 'muted';
  const detail = data
    ? `版本 ${data.version} · 协议 ${data.protocol}`
    : (error?.message ?? '等待 /healthz 响应');

  return (
    <TelemetryPanel eyebrow="JRPS 健康" title="服务健康状态">
      <div className="health-readout" aria-live="polite">
        <span className={`health-orb health-orb--${tone}`} aria-hidden="true" />
        <div>
          <strong>{headline}</strong>
          <p>{isPending ? '正在探测本机控制端点' : detail}</p>
        </div>
      </div>
      <div className="health-actions">
        <SignalTag tone={tone}>{data ? 'HTTP 200' : error ? '链路异常' : '探测中'}</SignalTag>
        {error ? (
          <button className="retry-button" type="button" onClick={onRetry}>
            重新探测
          </button>
        ) : null}
      </div>
    </TelemetryPanel>
  );
}

function BaselinePanel({ data }: { data: HealthSnapshot | undefined }) {
  const isAligned = data?.core_baseline === COMPATIBILITY_BASELINE;
  return (
    <TelemetryPanel eyebrow="兼容基线" title="固定兼容基线">
      <code className="baseline-code">{COMPATIBILITY_BASELINE}</code>
      <div className="baseline-status">
        <SignalTag tone={isAligned ? 'active' : 'muted'}>
          {isAligned ? '运行上报一致' : '等待运行上报'}
        </SignalTag>
        <span>只读参考，不进入依赖图</span>
      </div>
    </TelemetryPanel>
  );
}

function SkeletonPanel() {
  return (
    <TelemetryPanel eyebrow="骨架状态" title="当前骨架状态">
      <dl className="status-matrix">
        <div>
          <dt>/healthz</dt>
          <dd>已接通</dd>
        </div>
        <div>
          <dt>/readyz</dt>
          <dd>已定义</dd>
        </div>
        <div>
          <dt>Web SPA</dt>
          <dd>已装载</dd>
        </div>
        <div>
          <dt>管理能力</dt>
          <dd>按阶段交付</dd>
        </div>
      </dl>
    </TelemetryPanel>
  );
}

function StatusSection() {
  const health = useQuery({
    queryFn: fetchHealth,
    queryKey: ['healthz'],
    refetchInterval: (query) => (query.state.status === 'error' ? 5_000 : 30_000),
    staleTime: 15_000,
  });

  return (
    <section className="status-section" aria-labelledby="status-title">
      <div className="section-heading">
        <div>
          <p className="section-code">信号板 / 003</p>
          <h2 id="status-title">运行信号</h2>
        </div>
        <span className="utc-label">本机控制</span>
      </div>
      <div className="status-grid">
        <HealthPanel
          data={health.data}
          error={health.error}
          isPending={health.isPending}
          onRetry={() => void health.refetch()}
        />
        <BaselinePanel data={health.data} />
        <SkeletonPanel />
      </div>
    </section>
  );
}

export function HomePage() {
  const navigate = useNavigate();
  const session = useQuery({ queryFn: fetchSession, queryKey: ['session'] });

  // 未登录跳转登录页：查得空会话是"未登录"的确定信号。
  useEffect(() => {
    if (session.data === null) {
      void navigate({ to: '/login' });
    }
  }, [session.data, navigate]);

  if (session.isPending) {
    return (
      <div className="console-shell">
        <main>
          <p>正在校验会话…</p>
        </main>
      </div>
    );
  }
  if (!session.data) {
    // 查询失败与未登录分开呈现：失败时不该静默跳转，那会掩盖真实故障。
    return (
      <div className="console-shell">
        <main>
          <p className="form-error" role="alert">
            {session.isError ? session.error.message : '会话不可用，正在跳转登录页…'}
          </p>
        </main>
      </div>
    );
  }

  return (
    <div className="console-shell">
      <div className="signal-noise" aria-hidden="true" />
      <ConsoleHeader />
      <main>
        <SessionBar csrfToken={session.data.csrfToken} username={session.data.username} />
        <HeroSection />
        <TopologySection />
        <StatusSection />
        <NotificationPanel csrfToken={session.data.csrfToken} />
      </main>
      <footer>
        <span>JRP / 工业控制界面</span>
        <span>Core 边界：独立</span>
      </footer>
    </div>
  );
}
