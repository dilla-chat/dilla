//! What the parity world can seed beyond plain text history, one file per Wave 1 area (ruling R11): this file only lists
//! them, one per line in sorted order, and each area edits its own file.

use dilla_client::Client;
use dilla_client::testing::host::Person;
use dilla_client::testing::spec::ScreenFixture;

mod attachments;
mod calls;
mod diagnostics;
mod dms;
mod gifs;
mod invites;
mod members;
mod messages;
mod polls;
mod reactions;
mod roles;
mod threads;

/// Posts `m` when it is more than plain text: with its files (attachments), or as a reply or a thread reply (threads).
/// `None` for a plain message, which the history loop sends with `send_text`.
pub async fn post(
    m: &dilla_client::testing::spec::MessageSpec,
    client: &Client,
    channel: dilla_client::state::ChannelId,
    posted: &std::collections::HashMap<String, dilla_client::state::MessageId>,
) -> Option<Result<dilla_client::state::MessageId, dilla_client::ClientError>> {
    if let Some(sent) = attachments::send(client, channel, m).await {
        return Some(sent);
    }
    threads::post(m, client, channel, posted).await
}

/// What this world cannot seed yet, by facet; empty when it can seed the fixture.
pub fn unseedable(f: &ScreenFixture) -> Vec<String> {
    let mut out = Vec::new();
    attachments::unseedable(f, &mut out);
    calls::unseedable(f, &mut out);
    diagnostics::unseedable(f, &mut out);
    dms::unseedable(f, &mut out);
    gifs::unseedable(f, &mut out);
    invites::unseedable(f, &mut out);
    members::unseedable(f, &mut out);
    messages::unseedable(f, &mut out);
    polls::unseedable(f, &mut out);
    reactions::unseedable(f, &mut out);
    roles::unseedable(f, &mut out);
    threads::unseedable(f, &mut out);
    out
}

/// Seeds what the browser reads when it signs in (profiles, roles and the access they give), once the servers are made.
pub async fn before_sign_in(f: &ScreenFixture, people: &[(String, Person)]) {
    members::before_sign_in(f, people).await;
    roles::before_sign_in(f, people).await;
}

/// Seeds every facet once the history is posted.
pub async fn seed(f: &ScreenFixture, people: &[(String, Person)], clients: &[(&str, Client)]) {
    attachments::seed(f, people).await;
    calls::seed(f, people).await;
    diagnostics::seed(f, people).await;
    dms::seed(f, people).await;
    gifs::seed(f, people).await;
    invites::seed(f, people).await;
    members::seed(f, people).await;
    messages::seed(f, people).await;
    polls::seed(f, people).await;
    reactions::seed(f, people, clients).await;
    threads::seed(f, people).await;
}
