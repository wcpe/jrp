export interface HealthSnapshot {
  core_baseline: string;
  protocol: string;
  status: string;
  version: string;
  wire_versions: string[];
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null;
}

function isStringArray(value: unknown): value is string[] {
  return Array.isArray(value) && value.every((item) => typeof item === 'string');
}

function isHealthSnapshot(value: unknown): value is HealthSnapshot {
  return (
    isRecord(value) &&
    typeof value.status === 'string' &&
    typeof value.version === 'string' &&
    typeof value.protocol === 'string' &&
    typeof value.core_baseline === 'string' &&
    isStringArray(value.wire_versions)
  );
}

export async function fetchHealth({ signal }: { signal: AbortSignal }) {
  const response = await fetch('/healthz', { signal });
  if (!response.ok) {
    throw new Error(`健康检查返回 HTTP ${response.status}`);
  }

  const payload: unknown = await response.json();
  if (!isHealthSnapshot(payload)) {
    throw new Error('健康检查返回了无法识别的数据');
  }
  return payload;
}
