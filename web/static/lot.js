// Periodic state refresh of the lot page. The server-rendered page stays the
// baseline: this script only replaces text content, never builds money
// values through Number, and shows an explicit staleness note while the
// refresh is failing. Requests never overlap: a slow answer skips its tick.
(function () {
  'use strict';

  var script = document.currentScript;
  if (!script) {
    return;
  }
  var apiUrl = script.getAttribute('data-lot-api');
  if (!apiUrl) {
    return;
  }

  var REFRESH_INTERVAL_MS = 4000;
  var TICK_MS = 1000;

  var stateLabel = document.querySelector('[data-lot-state]');
  var priceElement = document.getElementById('lot-current-price');
  var minimumElement = document.getElementById('lot-minimum-next-bid');
  var countdownElement = document.getElementById('lot-countdown');
  var staleElement = document.getElementById('lot-data-stale');
  var bidPlace = document.getElementById('bid-form-place');

  var displayLabels = {
    active: 'торги идут',
    determining: 'торги завершены, определяется результат',
    finished: 'торги завершены'
  };

  var inFlight = false;
  var stale = false;
  var endsAtMs = null;
  var serverTimeMs = null;
  var fetchedAtMs = 0;

  function setText(element, text) {
    if (element) {
      element.textContent = text;
    }
  }

  function setStale(isStale) {
    if (stale === isStale) {
      return;
    }
    stale = isStale;
    if (staleElement) {
      staleElement.hidden = !stale;
    }
  }

  function countdownText() {
    if (endsAtMs === null || serverTimeMs === null) {
      return '';
    }
    // The countdown runs on the server time of the last answer plus the local
    // time since it was fetched; the browser clock decides nothing.
    var remainingMs = endsAtMs - (serverTimeMs + (Date.now() - fetchedAtMs));
    if (remainingMs <= 0) {
      return 'дедлайн наступил';
    }

    var totalSeconds = Math.floor(remainingMs / 1000);
    var days = Math.floor(totalSeconds / 86400);
    var hours = Math.floor((totalSeconds % 86400) / 3600);
    var minutes = Math.floor((totalSeconds % 3600) / 60);
    var seconds = totalSeconds % 60;

    var parts = ['осталось:'];
    if (days > 0) {
      parts.push(days + ' д');
    }
    if (days > 0 || hours > 0) {
      parts.push(hours + ' ч');
    }
    if (days > 0 || hours > 0 || minutes > 0) {
      parts.push(minutes + ' мин');
    }
    parts.push(seconds + ' с');

    return parts.join(' ');
  }

  function applyState(data) {
    if (stateLabel) {
      stateLabel.textContent = displayLabels[data.display_status] || data.display_status;
    }
    // Money values arrive as decimal strings and stay strings: textContent
    // never converts them through Number.
    setText(priceElement, data.current_price);
    setText(minimumElement, data.minimum_next_bid === null ? '—' : data.minimum_next_bid);
    if (bidPlace) {
      bidPlace.hidden = data.display_status !== 'active';
    }

    endsAtMs = Date.parse(data.ends_at);
    serverTimeMs = Date.parse(data.server_time);
    fetchedAtMs = Date.now();
    setText(countdownElement, countdownText());
  }

  function refresh() {
    if (inFlight) {
      return;
    }
    inFlight = true;
    fetch(apiUrl, { credentials: 'same-origin', headers: { Accept: 'application/json' } })
      .then(function (response) {
        if (!response.ok) {
          throw new Error('state refresh failed with status ' + response.status);
        }
        return response.json();
      })
      .then(function (data) {
        applyState(data);
        setStale(false);
      })
      .catch(function () {
        // A failed refresh must not look like fresh data: the note stays
        // until the next successful answer, and the loop keeps running.
        setStale(true);
      })
      .then(function () {
        inFlight = false;
      });
  }

  setInterval(refresh, REFRESH_INTERVAL_MS);
  setInterval(function () {
    setText(countdownElement, countdownText());
  }, TICK_MS);
  refresh();
})();
