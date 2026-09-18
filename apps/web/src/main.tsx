import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';

import { App } from './app';
import './styles.css';

async function enableDevelopmentMock() {
  if (!import.meta.env.DEV) {
    return;
  }
  const { startBrowserMock } = await import('@jrp/devmock/browser');
  await startBrowserMock();
}

function renderApplication() {
  const rootElement = document.querySelector('#root');
  if (!rootElement) {
    throw new Error('未找到应用挂载节点');
  }

  createRoot(rootElement).render(
    <StrictMode>
      <App />
    </StrictMode>,
  );
}

enableDevelopmentMock().then(renderApplication, renderApplication);
