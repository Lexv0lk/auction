// Load scenario: competing bids on one lot (realisation step 15).
//
// The run prepares one published lot whose deadline carries a margin over
// warmup + measured phase, then several participants read the current price
// and submit increases with unique request keys through the ordinary JSON
// API — sessions and CSRF included, no application shortcut. Every accepted
// intention is deliberately repeated the same amount of times with the same
// key, so the replay behavior is exercised continuously. HTTP 409 on a stale
// price is an expected business outcome of contention, not an error of the
// script; errors of login, CSRF and format would turn the run into an
// authorization failure test, so their thresholds fail the run instead.
//
// Run (see docs/load-testing.md):
//
//	make load-bids                                      # defaults: 3 VUs, 15s warmup, 60s phase
//	make load-bids LOAD_VUS=12 LOAD_PROFILE=heavy       # the more concurrent run
//
// After the run the worker finishes the lot; the printed verifier command
// proves the stored rows, the winner and the count reconciliation.

import { sleep } from 'k6';

import { loadConfig } from './lib/config.js';
import { createClient } from './lib/auth.js';
import { uuidv4, addDecimalString, isoLocalUTC, apiErrorCode, safeJSON } from './lib/k6utils.js';
import {
  authErrors,
  serverErrors,
  networkErrors,
  unexpectedStatus,
  bidsAccepted,
  bidsReplayed,
  bidsTooLow,
  auctionClosed,
  keyConflicts,
  commitUnknown,
  closedSeen,
  replayAnomalies,
  stateReads,
  bidSubmits,
  stateReadDuration,
  bidSubmitDuration,
  setupDuration,
  formatErrors,
} from './lib/metrics.js';
import {
  scrapeAppMetrics,
  setupReportBase,
  finishReport,
  metricCount,
  stdoutBlock,
} from './lib/report.js';

const cfg = loadConfig();

export const options = {
  scenarios: {
    bidding: {
      executor: 'ramping-vus',
      startVUs: 0,
      stages: [
        { duration: `${cfg.warmup}s`, target: cfg.vus },
        { duration: `${cfg.duration}s`, target: cfg.vus },
      ],
    },
  },
  // Business contention (bid_too_low, auction_closed, 5xx, timeouts) is
  // measured, never failed. A broken session, a rejected token or a
  // rejected body would measure the wrong thing; those fail the run.
  thresholds: {
    load_auth_errors: ['count==0'],
    load_format_errors: ['count==0'],
    load_key_conflicts: ['count==0'],
    load_replay_anomalies: ['count==0'],
  },
};

// The VU's own session client; module state persists across the iterations
// of one VU, so the login happens once per VU, inside the warmup ramp. The
// intent carries the last accepted bid {amount, key} waiting for its
// deliberate repeats, or a pending repeat of an unknown outcome.
const vu = createClient(cfg.baseURL);
const state = { loggedIn: false, loginFailed: false, intent: null, stopped: false };

export function setup() {
  const started = Date.now();

  const admin = createClient(cfg.baseURL);
  const loginOutcome = admin.login(cfg.adminLogin, cfg.adminPassword);
  if (!loginOutcome.ok) {
    throw new Error(`admin login failed: ${loginOutcome.reason}`);
  }

  const categoryName = `load-bidcat-${cfg.runID}`;
  createCategory(cfg.baseURL, admin, categoryName);
  const categoryID = scrapeCategoryID(cfg.baseURL, admin, categoryName);

  // The deadline: warmup + measured phase + margin, computed on the host
  // clock. The margin absorbs the skew between the host and the database
  // clock, so the measured phase ends while the bidding is still open.
  const totalSeconds = cfg.warmup + cfg.duration + cfg.bid.margin;
  const endsAt = isoLocalUTC(new Date(Date.now() + totalSeconds * 1000));
  const lotTitle = `load-bidlot-${cfg.runID}`;
  const lotID = createAndPublishLot(cfg.baseURL, admin, {
    title: lotTitle,
    description: `Competing-bids target of run ${cfg.runID}; one lot, several participants.`,
    categoryID: categoryID,
    startPrice: String(cfg.bid.startPrice),
    endsAt: endsAt,
  });

  const base = setupReportBase({
    scenario: 'bidding',
    cfg: cfg,
    artifacts: {
      category_name: categoryName,
      lot_title: lotTitle,
      lot_id: lotID,
      preparation_ms: Date.now() - started,
    },
    verify: {
      mode: 'bids',
      lot_title: lotTitle,
      wait_seconds: cfg.bid.margin + 60,
    },
    appMetricsBefore: scrapeAppMetrics(cfg.baseURL),
  });
  setupDuration.add(Date.now() - started);

  return { base: base, lotID: lotID };
}

export default function (data) {
  if (state.stopped) {
    sleep(cfg.pause);

    return;
  }
  if (!state.loggedIn) {
    if (state.loginFailed) {
      sleep(cfg.pause);

      return;
    }
    const user = cfg.participantLogins[(__VU - 1) % cfg.participantLogins.length];
    const res = vu.login(user, cfg.participantPassword);
    if (!res.ok) {
      state.loginFailed = true;
      authErrors.add(1);
      console.error(`VU ${__VU}: login failed for ${user}: ${res.reason}`);

      return;
    }
    state.loggedIn = true;
  }

  // The participant reads the current conditions before every attempt: the
  // price move of other participants decides the amount of this one.
  const started = Date.now();
  const stateResp = vu.get(`${cfg.baseURL}/api/lots/${data.lotID}`, { tags: { name: 'lot_state' } });
  stateReadDuration.add(Date.now() - started);
  stateReads.add(1);
  if (stateResp.status !== 200) {
    classifyFallback(stateResp);
    sleep(cfg.pause);

    return;
  }
  const lot = stateResp.json();
  if (!lot.can_bid) {
    closedSeen.add(1);
    state.stopped = true;
    sleep(cfg.pause);

    return;
  }

  // The attempt: either a deliberate repeat of the current intention (the
  // same amount and the same key — the lost-response case) or a new bid
  // above the price just read. Amounts stay decimal strings: the addition
  // never goes through float64.
  let amount;
  let key;
  let repeat = false;
  if (state.intent !== null) {
    amount = state.intent.amount;
    key = state.intent.key;
    repeat = true;
  } else {
    amount = addDecimalString(lot.current_price, cfg.bid.step);
    key = uuidv4();
  }

  const resp = submitBid(data.lotID, amount, key);
  handleBidResponse(resp, amount, key, repeat);

  sleep(cfg.pause);
}

function submitBid(lotID, amount, key) {
  const started = Date.now();
  bidSubmits.add(1);
  const resp = vu.postJSON(
    `${cfg.baseURL}/api/lots/${lotID}/bids`,
    { amount: amount, request_key: key },
    { tags: { name: 'bid_submit' } },
  );
  bidSubmitDuration.add(Date.now() - started);

  return resp;
}

// handleBidResponse routes one bid answer. The stored-intent state machine:
// after 201 the intent waits for its deliberate repeats (kind replay); after
// a 503 commit-unknown the same request repeats first (kind pending) — the
// safe client behavior the API contract prescribes.
function handleBidResponse(resp, amount, key, repeat) {
  if (resp.status === 0) {
    networkErrors.add(1);

    return;
  }
  const code = apiErrorCode(resp);

  switch (resp.status) {
    case 201:
      if (repeat && state.intent !== null && !state.intent.pending) {
        // A repeat of a stored key can never insert again.
        replayAnomalies.add(1);
        state.intent = null;

        return;
      }
      bidsAccepted.add(1);
      state.intent = { amount: amount, key: key, repeatsLeft: cfg.bid.repeats, pending: false };

      return;

    case 200: {
      const body = safeJSON(resp);
      if (body === null || body.replayed !== true) {
        replayAnomalies.add(1);

        return;
      }
      bidsReplayed.add(1);
      if (state.intent !== null && state.intent.key === key) {
        if (state.intent.pending) {
          // The uncertain outcome resolved: the row was stored after all.
          state.intent = null;
        } else {
          state.intent.repeatsLeft -= 1;
          if (state.intent.repeatsLeft <= 0) {
            state.intent = null;
          }
        }
      }

      return;
    }

    case 409:
      if (code === 'bid_too_low') {
        bidsTooLow.add(1);
        if (state.intent !== null && state.intent.key === key) {
          // A pending repeat refused by price was never stored.
          state.intent = null;
        }

        return;
      }
      if (code === 'auction_closed') {
        auctionClosed.add(1);
        state.stopped = true;

        return;
      }
      if (code === 'request_key_conflict') {
        keyConflicts.add(1);

        return;
      }
      unexpectedStatus.add(1);

      return;

    case 401:
    case 403:
      authErrors.add(1);

      return;

    case 422:
      formatErrors.add(1);

      return;

    case 503:
      if (code === 'commit_outcome_unknown') {
        commitUnknown.add(1);
        if (state.intent === null || state.intent.key !== key) {
          state.intent = { amount: amount, key: key, repeatsLeft: 0, pending: true };
        }

        return;
      }
      serverErrors.add(1);

      return;

    default:
      if (resp.status >= 500) {
        serverErrors.add(1);

        return;
      }
      unexpectedStatus.add(1);
  }
}

function classifyFallback(resp) {
  if (resp.status === 0) {
    networkErrors.add(1);

    return;
  }
  if (resp.status >= 500) {
    serverErrors.add(1);

    return;
  }
  if (resp.status === 303 || resp.status === 401 || resp.status === 403) {
    authErrors.add(1);

    return;
  }
  unexpectedStatus.add(1);
}

export function handleSummary(data) {
  const sd = data.setup_data.base;
  const accepted = metricCount(data, 'load_bids_accepted');
  const uncertain = metricCount(data, 'load_commit_unknown');
  const verifyEnv = [
    ['VERIFY_MODE', sd.verify.mode],
    ['VERIFY_LOT_TITLE', sd.verify.lot_title],
    ['VERIFY_EXPECTED_NEW', String(accepted)],
    ['VERIFY_UNCERTAIN', String(uncertain)],
    ['VERIFY_WAIT', `${sd.verify.wait_seconds}s`],
  ];
  const report = finishReport(sd, data, { expected_new: accepted, uncertain_outcomes: uncertain }, verifyEnv);

  return {
    [report.report_path]: JSON.stringify(report, null, 2),
    stdout: stdoutBlock(report),
  };
}

function createCategory(baseURL, admin, name) {
  const resp = admin.postForm(`${baseURL}/admin/categories`, { name: name }, { tags: { name: 'prep_category_create' } });
  if (resp.status !== 303) {
    throw new Error(`category ${name} create answered ${resp.status}`);
  }
}

function scrapeCategoryID(baseURL, admin, name) {
  const resp = admin.get(`${baseURL}/admin/categories`, { tags: { name: 'prep_categories_page' } });
  if (resp.status !== 200) {
    throw new Error(`categories page answered ${resp.status}`);
  }
  const pattern = new RegExp(`<td>${name}</td>\\s*<td class="actions">\\s*<a href="/admin/categories/(\\d+)/edit"`);
  const match = pattern.exec(resp.body);
  if (match === null) {
    throw new Error(`category ${name} not found on the admin page`);
  }

  return match[1];
}

function createAndPublishLot(baseURL, admin, lot) {
  const created = admin.postForm(
    `${baseURL}/admin/lots`,
    {
      title: lot.title,
      description: lot.description,
      category_id: String(lot.categoryID),
      start_price: lot.startPrice,
      ends_at: lot.endsAt,
      ends_at_tz: 'UTC',
    },
    { tags: { name: 'prep_lot_create' } },
  );
  if (created.status !== 303) {
    throw new Error(`lot ${lot.title} create answered ${created.status}`);
  }
  const match = /\/admin\/lots\/(\d+)\/edit/.exec(created.headers.Location || '');
  if (match === null) {
    throw new Error(`lot ${lot.title} create redirected to ${created.headers.Location}`);
  }
  const lotID = match[1];

  const published = admin.postForm(
    `${baseURL}/admin/lots/${lotID}/publish`,
    {},
    { tags: { name: 'prep_lot_publish' } },
  );
  if (published.status !== 303) {
    throw new Error(`lot ${lot.title} publish answered ${published.status}`);
  }

  return lotID;
}
