import { useEffect, useState } from "react";
import { X } from "lucide-react";
import { apiFetch } from "../../lib/api";
import styles from "./SettingsDrawer.module.css";

interface Props {
  open: boolean;
  onClose: () => void;
}

interface Profile {
  id: string;
  label?: string;
  active?: boolean;
  valid?: boolean;
  expires_in?: string;
}

interface Stats {
  input: number;
  output: number;
  cache_read: number;
  total: number;
}

interface LLMProbeResult {
  available_models?: string[];
  supports_tools?: boolean;
  supports_streaming?: boolean;
  latency_ms?: number;
  endpoint_kind?: string;
  error?: string;
}

export function SettingsDrawer({ open, onClose }: Props) {
  const [profiles, setProfiles] = useState<Profile[]>([]);
  const [stats, setStats] = useState<Stats | null>(null);
  const [coordURL, setCoordURL] = useState("");
  const [clientSecret, setClientSecret] = useState("");
  const [defaultMode, setDefaultMode] = useState("smart");
  const [probeURL, setProbeURL] = useState(localStorage.getItem("amp.localLLMProbeURL") || "http://localhost:11434/v1");
  const [probe, setProbe] = useState<LLMProbeResult | null>(null);
  const [probing, setProbing] = useState(false);

  useEffect(() => {
    if (!open) return;
    setCoordURL(localStorage.getItem("amp.coordinatorURL") || "");
    setClientSecret(localStorage.getItem("amp.clientSecret") || "");
    setDefaultMode(localStorage.getItem("amp.defaultMode") || "smart");
    apiFetch("/api/claude/profiles")
      .then((d) => setProfiles(d.profiles || []))
      .catch(() => {});
    apiFetch("/api/stats/tokens")
      .then(setStats)
      .catch(() => {});
  }, [open]);

  const switchProfile = async (id: string) => {
    await apiFetch("/api/claude/profiles/active", {
      method: "POST",
      body: JSON.stringify({ id }),
    });
    setProfiles((prev) => prev.map((p) => ({ ...p, active: p.id === id })));
  };

  const save = () => {
    localStorage.setItem("amp.coordinatorURL", coordURL);
    if (clientSecret) localStorage.setItem("amp.clientSecret", clientSecret);
    localStorage.setItem("amp.defaultMode", defaultMode);
    location.reload();
  };

  const runProbe = async () => {
    setProbing(true);
    localStorage.setItem("amp.localLLMProbeURL", probeURL);
    try {
      setProbe(await apiFetch("/api/llm-probe", {
        method: "POST",
        body: JSON.stringify({ base_url: probeURL }),
      }));
    } catch (e) {
      setProbe({ error: e instanceof Error ? e.message : String(e) });
    } finally {
      setProbing(false);
    }
  };

  return (
    <>
      {open && <div className={styles.overlay} onClick={onClose} />}
      <aside className={`${styles.drawer} ${open ? styles.open : ""}`}>
        <header className={styles.header}>
          <h2>Settings</h2>
          <button onClick={onClose} className={styles.closeBtn}>
            <X size={16} />
          </button>
        </header>

        <div className={styles.body}>
          <section>
            <h3>Claude profile</h3>
            <div className={styles.profiles}>
              {profiles.map((p) => (
                <label key={p.id} className={styles.profileRow}>
                  <input
                    type="radio"
                    name="profile"
                    checked={!!p.active}
                    onChange={() => switchProfile(p.id)}
                  />
                  <div className={styles.profileInfo}>
                    <div className={styles.profileLabel}>
                      {p.label || p.id}
                      {!p.valid && <span className={styles.invalidBadge}>expired</span>}
                    </div>
                    {p.expires_in && (
                      <div className={styles.profileMeta}>expires in {p.expires_in}</div>
                    )}
                  </div>
                </label>
              ))}
            </div>
          </section>

          <section>
            <h3>Usage (today)</h3>
            {stats ? (
              <div className={styles.stats}>
                <div className={styles.statRow}>
                  <span>Input</span>
                  <strong>{stats.input.toLocaleString()}</strong>
                </div>
                <div className={styles.statRow}>
                  <span>Output</span>
                  <strong>{stats.output.toLocaleString()}</strong>
                </div>
                <div className={styles.statRow}>
                  <span>Cache read</span>
                  <strong>{stats.cache_read.toLocaleString()}</strong>
                </div>
                <div className={styles.statRow}>
                  <span>Total</span>
                  <strong>{stats.total.toLocaleString()}</strong>
                </div>
              </div>
            ) : (
              <div className={styles.placeholder}>loading…</div>
            )}
          </section>

          <section>
            <h3>Default agent mode</h3>
            <select
              value={defaultMode}
              onChange={(e) => setDefaultMode(e.target.value)}
              className={styles.select}
            >
              <option value="smart">smart</option>
              <option value="large">large</option>
              <option value="rush">rush</option>
              <option value="deep">deep</option>
              <option value="frontier">frontier</option>
            </select>
          </section>

          <section>
            <h3>Local LLM probe</h3>
            <input
              type="text"
              value={probeURL}
              onChange={(e) => setProbeURL(e.target.value)}
              placeholder="http://localhost:11434/v1"
              className={styles.input}
            />
            <button onClick={runProbe} className={styles.saveBtn} disabled={probing}>
              {probing ? "Probing…" : "Probe endpoint"}
            </button>
            {probe && (
              <div className={styles.probeResult}>
                <div>Streaming: {probe.supports_streaming ? "✅" : "⚠️"}</div>
                <div>Tools: {probe.supports_tools ? "✅ native" : "⚠ fallback to JSON-mode prompt"}</div>
                {probe.latency_ms !== undefined && <div>Latency: {probe.latency_ms}ms</div>}
                {probe.endpoint_kind && <div>Kind: {probe.endpoint_kind}</div>}
                {probe.error && <div className={styles.probeError}>{probe.error}</div>}
                {!!probe.available_models?.length && (
                  <select className={styles.select} defaultValue={probe.available_models[0]}>
                    {probe.available_models.map((m) => <option key={m} value={m}>{m}</option>)}
                  </select>
                )}
              </div>
            )}
          </section>

          <section>
            <h3>Coordinator</h3>
            <input
              type="text"
              value={coordURL}
              onChange={(e) => setCoordURL(e.target.value)}
              placeholder="wss://amp-coord.example.com"
              className={styles.input}
            />
            <input
              type="password"
              value={clientSecret}
              onChange={(e) => setClientSecret(e.target.value)}
              placeholder="client secret"
              className={styles.input}
            />
            <button onClick={save} className={styles.saveBtn}>
              Save & reload
            </button>
          </section>
        </div>
      </aside>
    </>
  );
}
