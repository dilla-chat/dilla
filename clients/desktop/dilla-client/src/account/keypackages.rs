//! Key-package refill: a port of `packages/client-core/src/account/keypackages.ts` (`refillKeyPackages`) and of the
//! controller's `topUpKeyPackages` (worker/controller.ts), which retries a failed publish with a backoff.
//!
//! A device that publishes no KeyPackages cannot be added to a group by anyone (a channel group someone else registers,
//! a DM): the instance proposes an `Add` only for a device holding one. A sign-up sends none (they are refused while the
//! account owes a second factor), so the first ready publishes the full set, and every ready after it tops the
//! directory up once the server reports fewer than the instance's refill threshold.

use crate::gateway::frames::Value;
use crate::http::routes::{self as r, NoBody, Raw};
use crate::sync::api::{Api, ApiError, CoreHandle, try_core};
use crate::sync::value::{self, at_least, u53};

/// core `MAX_KEY_PACKAGES`: never more than this many in one publish (keypackages.ts `Math.min(.., 32)`).
pub const MAX_PER_PUBLISH: u32 = 32;
/// controller.ts `KEYPACKAGE_RETRY_MIN_MS`: a failed publish is tried again after 5 s ...
pub const RETRY_MIN_MS: u64 = 5_000;
/// ... doubling to 5 min (`KEYPACKAGE_RETRY_MAX_MS`).
pub const RETRY_MAX_MS: u64 = 300_000;

/// routes.ts `Limits`, the two values the refill reads: `GET /v1/instance/limits` elements 4 and 5.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct Limits {
    /// `keypackagesPerDevice`: the directory's target.
    pub per_device: u32,
    /// `keypackageRefillThreshold`: below this the device tops up.
    pub threshold: u32,
}

/// keypackages.ts `refillKeyPackages`' decision: how many to publish, and whether a last-resort package goes with
/// them (only when the directory is empty). `None` at or above the threshold.
pub fn plan(remaining: u32, limits: Limits) -> Option<(u32, bool)> {
    if remaining >= limits.threshold {
        return None;
    }
    let count = limits
        .per_device
        .saturating_sub(remaining)
        .min(MAX_PER_PUBLISH);
    (count > 0).then_some((count, remaining == 0))
}

/// controller.ts' backoff: 5 s after the first failure, doubling, capped at 5 min.
pub fn next_delay_ms(previous: u64) -> u64 {
    previous.saturating_mul(2).clamp(RETRY_MIN_MS, RETRY_MAX_MS)
}

/// routes.ts `getLimits`.
pub(crate) async fn get_limits(api: &Api) -> Result<Limits, ApiError> {
    let Raw(raw) = api.http_call::<r::GetLimits>(&[], NoBody, true).await?;
    let decode = |e: value::DecodeError| ApiError::local(0, "E_DECODE", e.0);
    let v = value::decode(&raw).map_err(decode)?;
    let a = at_least(&v, 9).map_err(decode)?;
    let small = |v: &Value| -> Result<u32, ApiError> {
        u32::try_from(u53(v).map_err(decode)?)
            .map_err(|_| ApiError::local(0, "E_DECODE", "a limit"))
    };
    Ok(Limits {
        per_device: small(&a[4])?,
        threshold: small(&a[5])?,
    })
}

/// routes.ts `getKeyPackageCount`: the ordinary count of this device's directory, or `None` on an older server without
/// the route (404, 405).
pub(crate) async fn get_count(api: &Api) -> Result<Option<u32>, ApiError> {
    let reply = api
        .http_call::<r::GetKeyPackageCount>(&[], NoBody, true)
        .await?;
    if reply.status != 200 {
        return Ok(None);
    }
    let decode = |e: value::DecodeError| ApiError::local(0, "E_DECODE", e.0);
    let v = value::decode(&reply.body).map_err(decode)?;
    let a = at_least(&v, 2).map_err(decode)?;
    let ordinary = u32::try_from(u53(&a[0]).map_err(decode)?)
        .map_err(|_| ApiError::local(0, "E_DECODE", "a count"))?;
    Ok(Some(ordinary))
}

/// keypackages.ts `refillKeyPackages`: publishes what [`plan`] says; answers how many were published.
pub(crate) async fn refill(
    core: &CoreHandle,
    api: &Api,
    remaining: u32,
    limits: Limits,
) -> Result<u32, ApiError> {
    let Some((count, last_resort)) = plan(remaining, limits) else {
        return Ok(0);
    };
    let body = try_core(core, move |c| c.key_packages(count, last_resort))
        .await
        .map_err(|_| ApiError::local(0, "E_CORE_POISONED", ""))?
        .map_err(|e| ApiError::local(0, e.code, ""))?;
    // Idempotent per kp_ref on the server, so a transport retry cannot double-publish.
    api.http_call::<r::PostKeyPackages>(&[], Raw(body), true)
        .await?;
    Ok(count)
}

#[cfg(test)]
mod tests {
    use super::*;

    const LIMITS: Limits = Limits {
        per_device: 32,
        threshold: 8,
    };

    // keypackages.test.ts, case for case.
    #[test]
    fn does_nothing_at_or_above_the_threshold() {
        assert_eq!(plan(8, LIMITS), None);
        assert_eq!(plan(31, LIMITS), None);
    }

    #[test]
    fn tops_up_to_the_per_device_count_without_a_last_resort_package() {
        assert_eq!(plan(7, LIMITS), Some((25, false)));
        assert_eq!(plan(1, LIMITS), Some((31, false)));
    }

    #[test]
    fn publishes_the_full_set_and_a_last_resort_package_from_zero() {
        assert_eq!(plan(0, LIMITS), Some((32, true)));
    }

    #[test]
    fn never_asks_the_core_for_more_than_32() {
        let wide = Limits {
            per_device: 40,
            threshold: 8,
        };
        assert_eq!(plan(0, wide), Some((32, true)));
    }

    // worker/keypackages.test.ts: 5 s, 10 s, 20 s ... capped at 5 min; a success resets to 5 s (the caller sets 0).
    #[test]
    fn a_failed_publish_backs_off_from_five_seconds_to_five_minutes() {
        let mut delay = 0;
        let mut waits = Vec::new();
        for _ in 0..9 {
            delay = next_delay_ms(delay);
            waits.push(delay / 1000);
        }
        assert_eq!(waits, [5, 10, 20, 40, 80, 160, 300, 300, 300]);
        assert_eq!(next_delay_ms(0), RETRY_MIN_MS);
    }
}
