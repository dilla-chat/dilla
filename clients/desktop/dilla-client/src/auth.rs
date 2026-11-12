//! The device session's bearer token, one type for the HTTP transport and the gateway (re-exported from
//! [`crate::http`] and [`crate::gateway`]). Every clone shares the one slot, so a session the sync engine renews reaches
//! the next HTTP call and the next gateway connect. The token is zeroized when it is replaced, cleared or the last clone
//! drops, and it is never printed.

use std::sync::{Arc, RwLock};

use zeroize::Zeroizing;

#[derive(Clone, Default)]
pub struct AuthToken(Arc<RwLock<Option<Zeroizing<String>>>>);

impl AuthToken {
    /// A token as the instance issued it.
    pub fn new(token: impl Into<String>) -> Self {
        let this = Self::default();
        this.set(token);
        this
    }

    /// No session yet: only the routes that take no bearer are answered.
    pub fn anonymous() -> Self {
        Self::default()
    }

    /// Replaces the token (a new session). The old one is zeroized as it drops.
    pub fn set(&self, token: impl Into<String>) {
        *self
            .0
            .write()
            .unwrap_or_else(std::sync::PoisonError::into_inner) =
            Some(Zeroizing::new(token.into()));
    }

    /// Forgets the token (sign-out).
    pub fn clear(&self) {
        *self
            .0
            .write()
            .unwrap_or_else(std::sync::PoisonError::into_inner) = None;
    }

    /// Whether a token is held.
    pub fn is_set(&self) -> bool {
        self.0
            .read()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
            .is_some()
    }

    /// Runs `f` on the token without copying it out of its zeroizing buffer.
    pub(crate) fn with<R>(&self, f: impl FnOnce(Option<&str>) -> R) -> R {
        let guard = self
            .0
            .read()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        f(guard.as_ref().map(|t| t.as_str()))
    }
}

impl std::fmt::Debug for AuthToken {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str("AuthToken(<redacted>)")
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn clones_share_one_slot_and_never_print() {
        let a = AuthToken::new("secret");
        let b = a.clone();
        b.set("renewed");
        assert_eq!(a.with(|t| t.map(str::to_owned)), Some("renewed".into()));
        assert_eq!(format!("{a:?}"), "AuthToken(<redacted>)");
        a.clear();
        assert!(!b.is_set());
    }
}
