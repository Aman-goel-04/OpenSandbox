#
# Copyright 2026 Alibaba Group Holding Ltd.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
#
"""Pure retry-decision and backoff helpers. No httpx dependency."""

from __future__ import annotations

import email.utils
import random
from dataclasses import dataclass
from datetime import datetime, timedelta, timezone

from opensandbox.transport.retry import (
    JitterMode,
    RetryCause,
    RetryPolicy,
    is_idempotent_method,
)


@dataclass(frozen=True)
class Outcome:
    """Classifier-friendly view of one request/response cycle."""

    is_transport_error: bool
    status_code: int | None = None
    is_pre_send: bool = False
    is_opaque_transport: bool = False
    # Read/write timeout and unexpected-EOF are useful for observability
    # but treated identically by ``should_retry``.
    cause: RetryCause = RetryCause.STATUS_OTHER


def should_retry(
    method: str,
    outcome: Outcome,
    retries_used: int,
    policy: RetryPolicy,
    elapsed: timedelta,
    cancelled: bool = False,
) -> bool:
    """
    Decide whether to retry.

    Checks budget/deadline/cancellation first, then the transport-vs-status
    branch. Pre-send transport failures retry on any method; post-send
    and opaque transport failures retry on idempotent methods only.
    """
    if retries_used >= policy.max_retries:
        return False
    if (
        policy.overall_deadline is not None
        and elapsed >= policy.overall_deadline
    ):
        return False
    if cancelled:
        return False

    idempotent = is_idempotent_method(method)

    if outcome.is_transport_error:
        if outcome.is_pre_send:
            return True
        return idempotent

    if outcome.status_code is None:
        return False
    return outcome.status_code in policy.retryable_statuses_for(method)


def compute_backoff(
    retry_index: int,
    policy: RetryPolicy,
    previous_sleep: timedelta,
    rng: random.Random,
) -> timedelta:
    """Backoff sleep for retry ``retry_index`` (0-based). ``previous_sleep`` is used only by decorrelated jitter."""
    max_s = policy.max_backoff.total_seconds()
    base_s = policy.initial_backoff.total_seconds()
    m = policy.backoff_multiplier

    exp_s = min(max_s, base_s * (m**retry_index)) if base_s > 0 else 0.0

    if policy.jitter is JitterMode.NONE:
        sleep_s = exp_s
    elif policy.jitter is JitterMode.FULL:
        sleep_s = rng.uniform(0.0, exp_s) if exp_s > 0 else 0.0
    else:  # DECORRELATED
        prev_s = previous_sleep.total_seconds()
        if retry_index == 0 or prev_s <= 0:
            # Clamp the first delay to max_backoff too so a low
            # max_backoff (or the fast-fail-oriented `0`) is respected
            # on the very first retry, matching the other jitter modes.
            sleep_s = min(base_s, max_s)
        else:
            upper = min(max_s, prev_s * m)
            lower = base_s
            if upper < lower:
                sleep_s = upper
            else:
                sleep_s = rng.uniform(lower, upper)

    return timedelta(seconds=max(0.0, sleep_s))


# Default operational ceiling for server-supplied ``Retry-After`` waits.
# Callers can override via ``RetryPolicy.retry_after_cap``.
_DEFAULT_RETRY_AFTER_CAP: timedelta = timedelta(seconds=60)

# Overflow guard for numeric ``Retry-After`` values, independent of the
# policy cap. ``timedelta`` cannot represent more than ~999999999 days,
# so a huge header would raise ``OverflowError`` during construction.
# We clamp the raw seconds to a safe upper bound here; the operational
# cap is applied later by ``apply_retry_after_cap``.
_MAX_RETRY_AFTER_SECONDS: int = 100 * 365 * 24 * 3600  # ~100 years


def parse_retry_after(
    header_value: str | None, *, now: datetime | None = None
) -> timedelta | None:
    """Parse a ``Retry-After`` header (delta-seconds or HTTP-date). Past dates and negative deltas normalize to zero; unparseable returns ``None``."""
    if header_value is None:
        return None
    raw = header_value.strip()
    if not raw:
        return None

    # delta-seconds (integer)
    try:
        seconds = int(raw)
    except ValueError:
        seconds = None
    if seconds is not None:
        # A pathologically large numeric value overflows ``timedelta``.
        # Clamp to a safe upper bound so parsing never raises; the
        # operational ceiling is applied later via
        # ``apply_retry_after_cap``.
        if seconds < 0:
            return timedelta(0)
        return timedelta(seconds=min(seconds, _MAX_RETRY_AFTER_SECONDS))

    # HTTP-date
    try:
        parsed = email.utils.parsedate_to_datetime(raw)
    except (TypeError, ValueError):
        return None
    if parsed is None:
        return None
    if parsed.tzinfo is None:
        parsed = parsed.replace(tzinfo=timezone.utc)
    reference = now or datetime.now(timezone.utc)
    delta = parsed - reference
    if delta.total_seconds() < 0:
        return timedelta(0)
    return delta


def apply_retry_after_cap(
    retry_after: timedelta | None,
    cap: timedelta = _DEFAULT_RETRY_AFTER_CAP,
) -> timedelta | None:
    """Cap ``retry_after`` at ``cap`` (default 60s)."""
    if retry_after is None:
        return None
    if retry_after > cap:
        return cap
    return retry_after
