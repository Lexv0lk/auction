// Small pure helpers of the load scenarios.

import crypto from 'k6/crypto';

// uuidv4 builds a canonical hyphenated UUID from random bytes: the exact
// shape the bid service validates and the unique constraint groups by.
export function uuidv4() {
  const bytes = new Uint8Array(crypto.randomBytes(16));
  bytes[6] = (bytes[6] & 0x0f) | 0x40;
  bytes[8] = (bytes[8] & 0x3f) | 0x80;
  let hex = '';
  for (const b of bytes) {
    hex += b.toString(16).padStart(2, '0');
  }

  return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`;
}

// addDecimalString adds a small non-negative integer to a decimal string
// without ever touching float64: money keeps its integer precision no matter
// how the run pushes the amounts upward.
export function addDecimalString(dec, n) {
  if (!/^\d+$/.test(dec)) {
    throw new Error(`addDecimalString: not a decimal string: ${JSON.stringify(dec)}`);
  }
  if (!Number.isInteger(n) || n < 0) {
    throw new Error(`addDecimalString: n must be a non-negative integer, got ${n}`);
  }
  if (n === 0) {
    return dec;
  }

  const digits = dec.split('').map(Number);
  let carry = n;
  for (let i = digits.length - 1; i >= 0 && carry > 0; i -= 1) {
    const sum = digits[i] + (carry % 10);
    carry = Math.floor(carry / 10);
    if (sum >= 10) {
      digits[i] = sum - 10;
      carry += 1;
    } else {
      digits[i] = sum;
    }
  }
  while (carry > 0) {
    digits.unshift(carry % 10);
    carry = Math.floor(carry / 10);
  }

  return digits.join('');
}

// isoLocalUTC renders a Date as the datetime-local shape in UTC, with
// seconds: the exact format the lot form parses for ends_at (the timezone
// travels in its own field).
export function isoLocalUTC(date) {
  return date.toISOString().slice(0, 19);
}

// apiErrorCode reads the machine code of a JSON error body without trusting
// the answer to be JSON at all.
export function apiErrorCode(resp) {
  const body = safeJSON(resp);
  if (body !== null && body.error && typeof body.error.code === 'string') {
    return body.error.code;
  }

  return null;
}

// safeJSON parses a response body, answering null instead of throwing when
// the body is not JSON.
export function safeJSON(resp) {
  try {
    return resp.json();
  } catch (e) {
    return null;
  }
}

// escapeRegExp makes a plain string safe inside a regular expression.
export function escapeRegExp(text) {
  return text.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
}
