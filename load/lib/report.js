// Report building for the load scenarios. handleSummary writes one machine-
// readable JSON file per run (k6 accepts file paths as summary keys) and a
// human block on stdout.
//
// In k6 v2 every lifecycle phase runs in its own module instance: nothing
// crosses phases through globals. The report therefore rides on the setup
// return value (k6 exposes it to handleSummary as data.setup_data): setup
// builds the base document via setupReportBase — configuration without
// secrets (the reportableConfig allowlist), the preparation artifacts, the
// verify basis and the /metrics snapshot taken before the measured phase —
// and handleSummary's finishReport adds the k6 aggregates, the verify
// command for the run's own counters and the /metrics snapshot after the
// run. Passwords and cookies never enter the report.

import http from 'k6/http';

import { reportableConfig } from './config.js';

// scrapeAppMetrics fetches the Prometheus exposition of the loaded process.
// The text is plain Prometheus format, contains no secrets and lands in the
// report verbatim so the pool statistics and the server-side histograms stay
// computable afterwards.
export function scrapeAppMetrics(baseURL) {
  const resp = http.get(`${baseURL}/metrics`, { redirects: 0, tags: { name: 'metrics_scrape' } });
  if (resp.status !== 200) {
    return { ok: false, status: resp.status, text: '' };
  }

  return { ok: true, status: resp.status, text: resp.body };
}

// setupReportBase assembles the part of the report only the setup phase
// knows: it becomes the setup return value and reaches handleSummary as
// data.setup_data.
export function setupReportBase(parts) {
  return {
    scenario: parts.scenario,
    run_id: parts.cfg.runID,
    profile: parts.cfg.profile,
    started_at: new Date().toISOString(),
    config: reportableConfig(parts.cfg),
    artifacts: parts.artifacts,
    verify: parts.verify,
    app_metrics_before: parts.appMetricsBefore || { ok: false, status: 0, text: '' },
  };
}

// k6MetricValues flattens data.metrics into plain value objects, thresholds
// included when present.
function flattenMetrics(data) {
  const metrics = {};
  for (const name of Object.keys(data.metrics)) {
    const entry = { values: data.metrics[name].values };
    if (data.metrics[name].thresholds) {
      entry.thresholds = data.metrics[name].thresholds;
    }
    metrics[name] = entry;
  }

  return metrics;
}

// metricCount answers the count of a custom counter inside handleSummary
// data; a metric that never fired is absent and counts as zero.
export function metricCount(data, name) {
  if (data.metrics[name] === undefined) {
    return 0;
  }

  return data.metrics[name].values.count || 0;
}

// verifyCommand renders the verification command the operator runs next; the
// arguments come from the run's own counters, so nothing is retyped by hand.
export function verifyCommand(parts) {
  const env = (parts.verify_env || []).map((pair) => `${pair[0]}=${pair[1]}`).join(' ');

  return `${env} go run ./load/verify`;
}

// finishReport completes the setup base with everything the end of the run
// knows: the k6 metric aggregates, the extra verify fields computed from the
// counters, and the /metrics snapshot after the run. The result carries its
// own report path and the ready-to-run verify command.
export function finishReport(base, data, extraVerify, verifyEnv) {
  const after = scrapeAppMetrics(base.config.base_url);
  const report = {
    scenario: base.scenario,
    run_id: base.run_id,
    profile: base.profile,
    started_at: base.started_at,
    finished_at: new Date().toISOString(),
    config: base.config,
    artifacts: base.artifacts,
    verify: Object.assign({}, base.verify, extraVerify || {}),
    verify_env: verifyEnv || [],
    k6: flattenMetrics(data),
    app_metrics: {
      before_ok: base.app_metrics_before.ok,
      after_ok: after.ok,
      before: base.app_metrics_before.text,
      after: after.text,
    },
  };
  report.report_path = `${base.config.results_dir}/${report.scenario}-${report.run_id}.json`;
  report.verify_command = verifyCommand(report);

  return report;
}

// stdoutBlock renders the human summary printed at the end of a run.
export function stdoutBlock(report) {
  const k = (name) => {
    const m = report.k6[name];

    return m ? m.values : {};
  };
  const lines = [];
  lines.push(`=== auction load: ${report.scenario} (run ${report.run_id}, profile ${report.profile}) ===`);
  const reads = k('load_catalog_reads');
  if (reads.count !== undefined) {
    const lat = k('load_read_duration');
    lines.push(`catalog reads: ${reads.count} (med ${fmtMs(lat.med)}, p95 ${fmtMs(lat['p(95)'])})`);
  }
  const submits = k('load_bid_submits');
  if (submits.count !== undefined) {
    const lat = k('load_bid_submit_duration');
    lines.push(`bid submits: ${submits.count} (med ${fmtMs(lat.med)}, p95 ${fmtMs(lat['p(95)'])})`);
  }
  const states = k('load_state_reads');
  if (states.count !== undefined) {
    const lat = k('load_state_read_duration');
    lines.push(`state reads: ${states.count} (med ${fmtMs(lat.med)}, p95 ${fmtMs(lat['p(95)'])})`);
  }
  const accepted = k('load_bids_accepted');
  if (accepted.count !== undefined) {
    lines.push(`bids: new ${accepted.count}, replayed ${k('load_bids_replayed').count || 0}, ` +
      `too_low ${k('load_bids_too_low').count || 0}, closed ${k('load_auction_closed').count || 0}, ` +
      `commit_unknown ${k('load_commit_unknown').count || 0}`);
  }
  const loginCount = k('load_logins');
  if (loginCount.count !== undefined) {
    const lat = k('load_login_duration');
    lines.push(`logins: ${loginCount.count} (med ${fmtMs(lat.med)}, p95 ${fmtMs(lat['p(95)'])})`);
  }
  lines.push(`errors: auth ${k('load_auth_errors').count || 0}, format ${k('load_format_errors').count || 0}, ` +
    `5xx ${k('load_server_errors').count || 0}, network ${k('load_network_errors').count || 0}, ` +
    `unexpected ${k('load_unexpected_status').count || 0}`);
  lines.push(`thresholds failed: ${countFailedThresholds(report.k6)}`);
  lines.push(`report: ${report.report_path}`);
  lines.push(`verify next: ${report.verify_command}`);

  return lines.join('\n') + '\n';
}

function countFailedThresholds(metrics) {
  let failed = 0;
  for (const name of Object.keys(metrics)) {
    const entry = metrics[name];
    if (!entry.thresholds) {
      continue;
    }
    // k6 serializes thresholds as an object keyed by the threshold
    // expression, each with an ok flag.
    for (const key of Object.keys(entry.thresholds)) {
      const t = entry.thresholds[key];
      if (t && t.ok === false) {
        failed += 1;
      }
    }
  }

  return failed;
}

function fmtMs(value) {
  if (value === undefined || value === null) {
    return '—';
  }

  return `${value.toFixed(1)}ms`;
}
