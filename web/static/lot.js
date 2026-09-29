// Periodic state refresh of the lot page and the bid submission. The
// server-rendered page stays the baseline: the script replaces text content,
// never builds money values through Number, and shows an explicit staleness
// note while the refresh is failing. When the poller learns that the worker
// has finished the auction, it reloads the page once so the server renders
// the recorded result — the winner is never assembled in the browser.
// Requests never overlap: a slow answer skips its tick. The bid intention
// (amount + request key) lives in the tab storage — never a password or a
// session token — so a reload or a retry after a lost answer repeats the
// same request, which the server answers idempotently.
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
    // The recorded result exists only on the server: when the worker has
    // finished the auction and this page still predates the result block, a
    // single reload brings the server-rendered winner in. The page returned
    // by the reload carries #lot-result, so the condition fires at most once.
    if (data.display_status === 'finished' && !document.getElementById('lot-result')) {
      window.location.reload();
      return;
    }
    // Money values arrive as decimal strings and stay strings: textContent
    // never converts them through Number. The bid intention (the input value
    // and the request key) is deliberately untouched here: a state refresh
    // never rewrites the request being sent.
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

  // ---- Bid submission -----------------------------------------------------
  // The form works without JavaScript; the script only upgrades it: it sends
  // the same contract to the JSON API, keeps one request key per intention
  // and blocks a second click while one answer is pending.

  var form = document.getElementById('bid-form');
  if (!form) {
    return;
  }
  var amountInput = document.getElementById('bid-amount');
  var submitButton = form.querySelector('button[type="submit"]');
  var messageElement = document.getElementById('bid-message');
  var csrfField = form.querySelector('input[name="gorilla.csrf.Token"]');
  var keyField = form.querySelector('input[name="request_key"]');
  if (!amountInput || !submitButton || !messageElement || !csrfField || !keyField) {
    return;
  }

  var bidApiUrl = apiUrl + '/bids';
  var lotPageUrl = form.getAttribute('action').replace(/\/bids$/, '');
  var intentStorageKey = 'auction-bid-intent:' + lotPageUrl;
  var bidInFlight = false;

  var UNKNOWN_OUTCOME_MESSAGE =
    'Результат отправки неизвестен: повторите отправку той же суммы — повтор безопасен.';

  function loadIntent() {
    try {
      var intent = JSON.parse(sessionStorage.getItem(intentStorageKey));
      if (intent && typeof intent.amount === 'string' && typeof intent.request_key === 'string') {
        return intent;
      }
    } catch (error) {
      // A broken or absent entry simply means "no pending intention".
    }

    return null;
  }

  function saveIntent(intent) {
    try {
      sessionStorage.setItem(intentStorageKey, JSON.stringify(intent));
    } catch (error) {
      // Storage may be unavailable; the server-side idempotency still makes
      // every attempt safe.
    }
  }

  function clearIntent() {
    try {
      sessionStorage.removeItem(intentStorageKey);
    } catch (error) {
      // Nothing to do: the next confirmed answer rewrites the storage anyway.
    }
  }

  function newRequestKey() {
    if (window.crypto && typeof crypto.randomUUID === 'function') {
      return crypto.randomUUID();
    }
    // A fallback for the rare old browser without crypto.randomUUID: a
    // random version-4 UUID in the canonical shape.
    return 'xxxxxxxx-xxxx-4xxx-yxxx-xxxxxxxxxxxx'.replace(/[xy]/g, function (symbol) {
      var random = Math.floor(Math.random() * 16);
      var value = symbol === 'x' ? random : (random & 0x3) | 0x8;

      return value.toString(16);
    });
  }

  function showMessage(text) {
    messageElement.textContent = text;
    messageElement.hidden = false;
  }

  function hideMessage() {
    messageElement.hidden = true;
  }

  function finishConfirmed(status, body) {
    // A confirmed answer ends the intention: the next bid is a new one.
    clearIntent();
    var marker = status === 201 ? 'placed' : 'replayed';
    window.location.assign(lotPageUrl + '?' + marker + '=' + body.bid.id);
  }

  form.addEventListener('submit', function (event) {
    event.preventDefault();
    if (bidInFlight) {
      return;
    }

    var amount = amountInput.value;
    var intent = loadIntent();
    if (!intent || intent.amount !== amount) {
      // A new amount is a new intention: it gets its own key, so a stored
      // result of the old intention can never be mistaken for this one.
      intent = { amount: amount, request_key: newRequestKey() };
      saveIntent(intent);
    }

    bidInFlight = true;
    submitButton.disabled = true;
    hideMessage();

    fetch(bidApiUrl, {
      method: 'POST',
      credentials: 'same-origin',
      headers: {
        'Content-Type': 'application/json',
        Accept: 'application/json',
        'X-CSRF-Token': csrfField.value
      },
      body: JSON.stringify({ amount: intent.amount, request_key: intent.request_key })
    })
      .then(function (response) {
        if (response.status === 201 || response.status === 200) {
          return response.json().then(function (body) {
            finishConfirmed(response.status, body);
          });
        }

        return response
          .json()
          .catch(function () {
            return null;
          })
          .then(function (body) {
            bidInFlight = false;
            submitButton.disabled = false;

            var code = body && body.error ? body.error.code : '';
            var message = body && body.error && body.error.message ? body.error.message : UNKNOWN_OUTCOME_MESSAGE;

            if (response.status === 403 && code === 'csrf_invalid') {
              // The masked token of this page no longer matches the stored
              // CSRF cookie: the request never reached the auction logic, so
              // the plain form submission is the safe retry.
              form.submit();
              return;
            }
            if (response.status === 401) {
              // The session has expired: name the next step, keep the
              // intention and never send the money action automatically
              // after a re-login.
              showMessage('Сессия истекла: войдите заново, затем повторите отправку той же суммы.');
              return;
            }
            if (response.status >= 500 || response.status === 0) {
              // The outcome is unknown — the bid may have been stored. The
              // intention stays; the same amount and key repeat the request.
              showMessage(message || UNKNOWN_OUTCOME_MESSAGE);
              refresh();
              return;
            }
            if (response.status === 409) {
              // A confirmed refusal: refresh the price and the state so the
              // next attempt starts from fresh data.
              refresh();
            }
            showMessage(message);
          });
      })
      .catch(function () {
        // A network failure leaves the outcome unknown: the intention and
        // its key stay, the user repeats the same request.
        bidInFlight = false;
        submitButton.disabled = false;
        showMessage(UNKNOWN_OUTCOME_MESSAGE);
      });
  });
})();
