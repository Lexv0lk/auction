// Shared configuration of the load scenarios. Every knob comes from the
// environment (LOAD_* names), so runs repeat with the same parameters without
// editing the scripts; the demo credentials fall back to the SEED_* names the
// repository already uses. Passwords stay out of the reports: the config is
// serialized into them only through the allowlist in report.js.
import crypto from 'k6/crypto';

export function envStr(name, fallback) {
  const value = __ENV[name];
  if (value === undefined || value === '') {
    return fallback;
  }

  return value;
}

export function envInt(name, fallback) {
  const raw = __ENV[name];
  if (raw === undefined || raw === '') {
    return fallback;
  }
  const value = Number(raw);
  if (!Number.isInteger(value) || value <= 0) {
    throw new Error(`${name} must be a positive integer, got ${JSON.stringify(raw)}`);
  }

  return value;
}

export function envFloat(name, fallback) {
  const raw = __ENV[name];
  if (raw === undefined || raw === '') {
    return fallback;
  }
  const value = Number(raw);
  if (Number.isNaN(value) || value < 0) {
    throw new Error(`${name} must be a non-negative number, got ${JSON.stringify(raw)}`);
  }

  return value;
}

// envDurationSeconds accepts '15s', '1m30s' or a bare '90' and answers whole
// seconds: k6 stage durations keep the raw string, deadline math needs numbers.
export function envDurationSeconds(name, fallback) {
  const raw = envStr(name, '');
  if (raw === '') {
    return fallback;
  }
  const match = /^(\d+(?:\.\d+)?)(ms|s|m|h)?$/.exec(raw.trim());
  if (match === null) {
    throw new Error(`${name} must be a duration like 15s, 2m or 90, got ${JSON.stringify(raw)}`);
  }
  const value = Number(match[1]);
  const unit = match[2] || 's';
  const factor = { ms: 0.001, s: 1, m: 60, h: 3600 }[unit];

  return Math.round(value * factor);
}

export function randomHex(bytes) {
  const raw = new Uint8Array(crypto.randomBytes(bytes));

  return raw.reduce((acc, b) => acc + b.toString(16).padStart(2, '0'), '');
}

export function defaultRunID() {
  const t = new Date();
  const pad = (n) => String(n).padStart(2, '0');
  const stamp = `${t.getUTCFullYear()}${pad(t.getUTCMonth() + 1)}${pad(t.getUTCDate())}` +
    `T${pad(t.getUTCHours())}${pad(t.getUTCMinutes())}${pad(t.getUTCSeconds())}Z`;

  return `${stamp}-${randomHex(4)}`;
}

// loadConfig gathers every scenario knob. Called once per runtime (setup and
// each VU copy): a missing password aborts the script before any request.
export function loadConfig() {
  const cfg = {
    baseURL: envStr('LOAD_BASE_URL', 'http://127.0.0.1:8080').replace(/\/+$/, ''),
    adminLogin: envStr('LOAD_ADMIN_LOGIN', 'demo-admin'),
    adminPassword: envStr('LOAD_ADMIN_PASSWORD', envStr('SEED_ADMIN_PASSWORD', '')),
    participantLogins: envStr('LOAD_PARTICIPANT_LOGINS', 'demo-participant-1,demo-participant-2,demo-participant-3')
      .split(',')
      .map((s) => s.trim())
      .filter(Boolean),
    participantPassword: envStr('LOAD_PARTICIPANT_PASSWORD', envStr('SEED_PARTICIPANT_PASSWORD', '')),
    vus: envInt('LOAD_VUS', 3),
    warmup: envDurationSeconds('LOAD_WARMUP', 15),
    duration: envDurationSeconds('LOAD_DURATION', 60),
    pause: envFloat('LOAD_PAUSE', 0.5),
    resultsDir: envStr('LOAD_RESULTS_DIR', 'load/results'),
    profile: envStr('LOAD_PROFILE', 'custom'),
    runID: envStr('LOAD_RUN_ID', defaultRunID()),
    appVersion: envStr('LOAD_APP_VERSION', ''),
    dbVersion: envStr('LOAD_DB_VERSION', ''),
    k6Version: envStr('LOAD_K6_VERSION', ''),
    hostOS: envStr('LOAD_HOST_OS', ''),
    hostCPU: envStr('LOAD_HOST_CPU', ''),
    hostRAM: envStr('LOAD_HOST_RAM', ''),
    catalog: {
      categories: envInt('LOAD_CATEGORIES', 3),
      lots: envInt('LOAD_LOTS', 90),
      // The read scenario keeps its lots active well beyond the run: hours,
      // not seconds, so the worker has nothing to finish during the phase.
      deadlineHours: envFloat('LOAD_CATALOG_DEADLINE_HOURS', 24),
    },
    bid: {
      step: envInt('LOAD_BID_STEP', 10),
      startPrice: envInt('LOAD_BID_START_PRICE', 1000),
      repeats: envInt('LOAD_REPEATS', 1),
      // The deadline carries a margin over warmup+duration: the measured
      // phase must end while the bidding is still open by the server clock.
      margin: envDurationSeconds('LOAD_BID_MARGIN', 120),
    },
  };

  if (cfg.adminPassword === '') {
    throw new Error('set LOAD_ADMIN_PASSWORD or SEED_ADMIN_PASSWORD (the demo administrator password)');
  }
  if (cfg.participantPassword === '') {
    throw new Error('set LOAD_PARTICIPANT_PASSWORD or SEED_PARTICIPANT_PASSWORD (the demo participants password)');
  }
  if (cfg.participantLogins.length === 0) {
    throw new Error('LOAD_PARTICIPANT_LOGINS must name at least one participant login');
  }

  return cfg;
}

// reportableConfig strips the secrets: the report carries every non-secret
// knob but never a password. Add fields here, not by copying the whole config.
export function reportableConfig(cfg) {
  return {
    base_url: cfg.baseURL,
    admin_login: cfg.adminLogin,
    participant_logins: cfg.participantLogins,
    vus: cfg.vus,
    warmup_seconds: cfg.warmup,
    duration_seconds: cfg.duration,
    pause_seconds: cfg.pause,
    results_dir: cfg.resultsDir,
    profile: cfg.profile,
    run_id: cfg.runID,
    versions: {
      app: cfg.appVersion,
      db: cfg.dbVersion,
      k6: cfg.k6Version,
    },
    host: {
      os: cfg.hostOS,
      cpu: cfg.hostCPU,
      ram: cfg.hostRAM,
    },
    catalog: cfg.catalog,
    bid: cfg.bid,
  };
}
