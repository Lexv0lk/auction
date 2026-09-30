// Authentication and request helpers shared by the load scenarios. The
// scenarios walk the ordinary login path — the /login form with its CSRF
// field — and carry the session cookie and the matching CSRF cookie with
// every following request, explicitly. No application shortcut exists or is
// used: without a valid session and token the request is answered like any
// guest's, the scenario counts the error and the run fails its thresholds.
//
// Cookies travel explicitly (k6 resets its per-iteration jar, and the VU jar
// of older versions is not assumed): login() extracts both cookie values and
// the CSRF token of the page, createClient() attaches them to every request.
import http from 'k6/http';
import { Trend } from 'k6/metrics';

import { logins } from './metrics.js';

// The gorilla field the server renders into every form (name and token in
// one input element).
const csrfFieldPattern = /name="gorilla\.csrf\.Token" value="([^"]+)"/;

export const loginDuration = new Trend('load_login_duration', true);

export function extractCSRFToken(body) {
  const match = csrfFieldPattern.exec(body);
  if (match === null) {
    return null;
  }

  return match[1];
}

// createClient builds a small authenticated HTTP wrapper for one session:
// an administrator in setup, a participant in a VU. Every request disables
// redirect following (statuses stay observable) and carries the session and
// CSRF cookies, replaced on collision with the per-iteration jar.
export function createClient(baseURL) {
  const s = { session: null, csrfCookie: null, csrfToken: null };

  function params(extra) {
    const cookies = {};
    if (s.session !== null) {
      cookies.auction_session = { value: s.session, replace: true };
    }
    if (s.csrfCookie !== null) {
      cookies._gorilla_csrf = { value: s.csrfCookie, replace: true };
    }

    return Object.assign({ redirects: 0, cookies: cookies }, extra || {});
  }

  return {
    // login performs the browser-like flow: GET /login issues the CSRF
    // cookie, POST /login exchanges the credentials for the session cookie.
    // A 303 is the success signal; the returned token matches the CSRF
    // cookie kept for the later requests.
    login(user, password) {
      const started = Date.now();
      logins.add(1);
      const page = http.get(`${baseURL}/login`, params({ tags: { name: 'login_page' } }));
      const token = page.status === 200 ? extractCSRFToken(page.body) : null;
      const csrfCookie = page.cookies._gorilla_csrf ? page.cookies._gorilla_csrf[0].value : null;
      if (page.status !== 200 || token === null || csrfCookie === null) {
        loginDuration.add(Date.now() - started, { outcome: 'page_error' });

        return { ok: false, reason: `login page answered ${page.status} without a usable CSRF field` };
      }

      const form = { login: user, password: password, 'gorilla.csrf.Token': token };
      const submitted = http.post(`${baseURL}/login`, form, params({ tags: { name: 'login_submit' } }));
      loginDuration.add(Date.now() - started, { outcome: submitted.status === 303 ? 'ok' : 'refused' });
      if (submitted.status !== 303) {
        return { ok: false, reason: `login answered ${submitted.status}` };
      }
      const session = submitted.cookies.auction_session;
      if (!session || session.length === 0 || !session[0].value) {
        return { ok: false, reason: 'login answered without a session cookie' };
      }

      s.session = session[0].value;
      s.csrfCookie = csrfCookie;
      s.csrfToken = token;

      return { ok: true };
    },

    get(url, extra) {
      return http.get(url, params(extra));
    },

    postForm(url, form, extra) {
      const body = Object.assign({}, form);
      body['gorilla.csrf.Token'] = s.csrfToken;

      return http.post(url, body, params(extra));
    },

    postJSON(url, body, extra) {
      const merged = Object.assign({}, extra);
      merged.headers = Object.assign(
        { 'Content-Type': 'application/json', 'X-CSRF-Token': s.csrfToken },
        (extra && extra.headers) || {},
      );

      return http.post(url, JSON.stringify(body), params(merged));
    },
  };
}
