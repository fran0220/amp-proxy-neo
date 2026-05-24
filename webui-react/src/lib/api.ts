// REST + WS wiring for the CF Worker coordinator.

interface DeployConfig {
  coordinatorURL?: string;
}

let DEPLOY_CONFIG: DeployConfig = {};
let configPromise: Promise<DeployConfig> | null = null;

export function loadDeployConfig(): Promise<DeployConfig> {
  if (!configPromise) {
    configPromise = fetch("/config.json", { cache: "no-store" })
      .then((r) => (r.ok ? r.json() : {}))
      .then((c) => (DEPLOY_CONFIG = c))
      .catch(() => ({}));
  }
  return configPromise;
}

export async function getCoordinatorURL(): Promise<string> {
  const cfg = await loadDeployConfig();
  let url = cfg.coordinatorURL || localStorage.getItem("amp.coordinatorURL");
  if (!url) {
    url = prompt("Coordinator URL (wss://amp-coord.example.com)") || "";
    if (url) localStorage.setItem("amp.coordinatorURL", url);
  }
  // Keep localStorage in sync with deploy config so settings drawer reflects truth.
  if (cfg.coordinatorURL && localStorage.getItem("amp.coordinatorURL") !== cfg.coordinatorURL) {
    localStorage.setItem("amp.coordinatorURL", cfg.coordinatorURL);
  }
  return url || "";
}

export function getClientSecret(): string {
  return localStorage.getItem("amp.clientSecret") || "";
}

export function setClientSecret(s: string) {
  localStorage.setItem("amp.clientSecret", s);
}

export async function apiFetch(path: string, init: RequestInit = {}): Promise<any> {
  const url = await getCoordinatorURL();
  const httpURL = url.replace(/^ws/, "http");
  const headers: Record<string, string> = {
    "Content-Type": "application/json",
    ...((init.headers as Record<string, string>) || {}),
  };
  const secret = getClientSecret();
  if (secret) headers["Authorization"] = `Bearer ${secret}`;
  const res = await fetch(httpURL + path, { ...init, headers, credentials: "include" });
  if (!res.ok) throw new Error(`${path} → ${res.status}`);
  return res.json();
}
