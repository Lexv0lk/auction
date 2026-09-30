// Load scenario: catalog reading (realisation step 15).
//
// The run prepares its own isolated data through the ordinary administrative
// forms — categories and published lots named after the run ID — then reads
// the catalog with category filters and different pages. Preparation and
// logins are measured separately from the measured reading phase (k6 stages:
// warmup, then the phase). No application shortcut is used or exists: every
// request carries the session cookie, and the preparation walks the admin
// forms with CSRF tokens like a browser.
//
// Run (see docs/load-testing.md):
//
//	make load-catalog                                   # defaults: 3 VUs, 15s warmup, 60s phase
//	make load-catalog LOAD_VUS=15 LOAD_PROFILE=heavy    # the more concurrent run
//
// The scenario is strictly read-only: its lots must keep zero bids — checked
// by the verifier command printed at the end of the run.

import { sleep } from 'k6';

import { loadConfig } from './lib/config.js';
import { createClient } from './lib/auth.js';
import { isoLocalUTC } from './lib/k6utils.js';
import {
  authErrors,
  catalogReads,
  readDuration,
  setupDuration,
  classifyStatus,
} from './lib/metrics.js';
import {
  scrapeAppMetrics,
  setupReportBase,
  finishReport,
  stdoutBlock,
} from './lib/report.js';

// The fixed page size of the catalog list (step 08).
const CATALOG_PAGE_SIZE = 20;

const cfg = loadConfig();

export const options = {
  scenarios: {
    catalog: {
      executor: 'ramping-vus',
      startVUs: 0,
      stages: [
        { duration: `${cfg.warmup}s`, target: cfg.vus },
        { duration: `${cfg.duration}s`, target: cfg.vus },
      ],
    },
  },
  // Only correctness fails a run. Latency and RPS have no invented targets:
  // the measured numbers are the result, not the gate.
  thresholds: {
    load_auth_errors: ['count==0'],
    load_format_errors: ['count==0'],
    load_unexpected_status: ['count==0'],
  },
};

// The VU's own session client; module state persists across the iterations
// of one VU, so the login happens once per VU, inside the warmup ramp.
const vu = createClient(cfg.baseURL);
const state = { loggedIn: false, loginFailed: false, iteration: 0 };

export function setup() {
  const started = Date.now();

  const admin = createClient(cfg.baseURL);
  const loginOutcome = admin.login(cfg.adminLogin, cfg.adminPassword);
  if (!loginOutcome.ok) {
    throw new Error(`admin login failed: ${loginOutcome.reason}`);
  }

  const categoryNames = [];
  for (let i = 0; i < cfg.catalog.categories; i += 1) {
    categoryNames.push(`load-cat-${cfg.runID}-${i}`);
  }
  for (const name of categoryNames) {
    createCategory(cfg.baseURL, admin, name);
  }
  const categoryIDs = scrapeCategoryIDs(cfg.baseURL, admin, categoryNames);

  const lotsPerCategory = Math.ceil(cfg.catalog.lots / cfg.catalog.categories);
  const endsAt = isoLocalUTC(new Date(Date.now() + cfg.catalog.deadlineHours * 3600 * 1000));
  for (let i = 0; i < cfg.catalog.lots; i += 1) {
    const categoryID = categoryIDs[i % categoryIDs.length];
    createAndPublishLot(cfg.baseURL, admin, {
      title: `load-lot-${cfg.runID}-${i}`,
      description: `Read-only fixture of catalog run ${cfg.runID}; lot ${i}.`,
      categoryID: categoryID,
      startPrice: '500',
      endsAt: endsAt,
    });
  }

  const titlePrefix = `load-lot-${cfg.runID}`;
  const base = setupReportBase({
    scenario: 'catalog_read',
    cfg: cfg,
    artifacts: {
      categories_created: categoryNames.length,
      category_names_prefix: `load-cat-${cfg.runID}-`,
      lots_created: cfg.catalog.lots,
      lot_title_prefix: titlePrefix,
      preparation_ms: Date.now() - started,
    },
    verify: {
      mode: 'catalog',
      title_prefix: titlePrefix,
      expected_lots: cfg.catalog.lots,
    },
    appMetricsBefore: scrapeAppMetrics(cfg.baseURL),
  });
  setupDuration.add(Date.now() - started);

  return {
    base: base,
    categoryIDs: categoryIDs,
    pagesPerCategory: Math.max(1, Math.ceil(lotsPerCategory / CATALOG_PAGE_SIZE)),
  };
}

export default function (data) {
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

  state.iteration += 1;
  let url;
  let name;
  if (state.iteration % 5 === 0) {
    const page = (Math.floor(state.iteration / 5) % data.pagesPerCategory) + 1;
    url = `${cfg.baseURL}/lots?page=${page}`;
    name = 'catalog_unfiltered';
  } else {
    const categoryID = data.categoryIDs[state.iteration % data.categoryIDs.length];
    const page = (Math.floor(state.iteration / data.categoryIDs.length) % data.pagesPerCategory) + 1;
    url = `${cfg.baseURL}/lots?category=${categoryID}&page=${page}`;
    name = 'catalog_filtered';
  }

  const started = Date.now();
  const resp = vu.get(url, { tags: { name: name } });
  readDuration.add(Date.now() - started, { name: name });
  catalogReads.add(1);
  classifyStatus(resp);

  sleep(cfg.pause);
}

export function handleSummary(data) {
  const sd = data.setup_data.base;
  const verifyEnv = [
    ['VERIFY_MODE', sd.verify.mode],
    ['VERIFY_TITLE_PREFIX', sd.verify.title_prefix],
    ['VERIFY_EXPECTED_LOTS', String(sd.verify.expected_lots)],
  ];
  const report = finishReport(sd, data, {}, verifyEnv);

  return {
    [report.report_path]: JSON.stringify(report, null, 2),
    stdout: stdoutBlock(report),
  };
}

// createCategory walks the administrative form; anything but the 303
// redirect aborts the run before the measured phase starts.
function createCategory(baseURL, admin, name) {
  const resp = admin.postForm(`${baseURL}/admin/categories`, { name: name }, { tags: { name: 'prep_category_create' } });
  if (resp.status !== 303) {
    throw new Error(`category ${name} create answered ${resp.status}`);
  }
}

// scrapeCategoryIDs reads the created IDs back from the admin list page,
// so the lot forms reference real categories without any direct database
// access.
function scrapeCategoryIDs(baseURL, admin, names) {
  const resp = admin.get(`${baseURL}/admin/categories`, { tags: { name: 'prep_categories_page' } });
  if (resp.status !== 200) {
    throw new Error(`categories page answered ${resp.status}`);
  }
  const pattern = /<td>([^<]+)<\/td>\s*<td class="actions">\s*<a href="\/admin\/categories\/(\d+)\/edit"/g;
  const byName = {};
  let match = pattern.exec(resp.body);
  while (match !== null) {
    byName[match[1]] = match[2];
    match = pattern.exec(resp.body);
  }
  const ids = [];
  for (const name of names) {
    if (byName[name] === undefined) {
      throw new Error(`category ${name} not found on the admin page`);
    }
    ids.push(byName[name]);
  }

  return ids;
}

// createAndPublishLot creates one draft through the form and publishes it;
// the creation redirect names the new lot ID.
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
