// Custom metrics of the load scenarios. The names form a fixed set: nothing
// here grows with lots, requests or users, so the label cardinality stays
// bounded (the same rule the application's own metrics follow).

import { Counter, Trend } from 'k6/metrics';

// Technical error classes. The thresholds of each scenario decide which of
// these fail a run: business load (409/5xx/timeouts) is reported, broken
// authentication or broken formatting is not load at all and fails the run.
export const authErrors = new Counter('load_auth_errors');
export const formatErrors = new Counter('load_format_errors');
export const serverErrors = new Counter('load_server_errors');
export const networkErrors = new Counter('load_network_errors');
export const unexpectedStatus = new Counter('load_unexpected_status');

// Read-scenario counters. These count attempts (requests sent), while the
// trend metrics carry the latency distributions; k6 v2 trend values have no
// count field, so the report reads the volume from the counters.
export const catalogReads = new Counter('load_catalog_reads');
export const stateReads = new Counter('load_state_reads');
export const bidSubmits = new Counter('load_bid_submits');
export const logins = new Counter('load_logins');

// Bid-scenario counters: outcomes of POST /api/lots/{id}/bids and the
// state reads that showed a closed bidding.
export const bidsAccepted = new Counter('load_bids_accepted');
export const bidsReplayed = new Counter('load_bids_replayed');
export const bidsTooLow = new Counter('load_bids_too_low');
export const auctionClosed = new Counter('load_auction_closed');
export const keyConflicts = new Counter('load_key_conflicts');
export const commitUnknown = new Counter('load_commit_unknown');
export const closedSeen = new Counter('load_closed_seen');
export const replayAnomalies = new Counter('load_replay_anomalies');

// Durations, in milliseconds. The setup trend covers data preparation and
// the administrative login; the login trend covers every session opening,
// so the measured phases stay separable from the preparation in the report.
export const setupDuration = new Trend('load_setup_duration', true);
export const loginDuration = new Trend('load_login_duration', true);
export const readDuration = new Trend('load_read_duration', true);
export const stateReadDuration = new Trend('load_state_read_duration', true);
export const bidSubmitDuration = new Trend('load_bid_submit_duration', true);

// classifyStatus routes one answered request into the error counters by its
// HTTP status alone. Business outcomes (409, and the 503 commit-unknown of
// the bid flow) are counted by their callers before falling back here; a
// bare 303 on a page GET means the session was lost and counts as an auth
// error, not as traffic.
export function classifyStatus(resp) {
  if (resp.status === 0) {
    networkErrors.add(1);

    return 'network';
  }
  const s = resp.status;
  if (s >= 200 && s < 300) {
    return 'ok';
  }
  if (s === 303 || s === 401 || s === 403) {
    authErrors.add(1);

    return 'auth';
  }
  if (s === 422) {
    formatErrors.add(1);

    return 'format';
  }
  if (s >= 500) {
    serverErrors.add(1);

    return 'server';
  }
  unexpectedStatus.add(1);

  return 'unexpected';
}
