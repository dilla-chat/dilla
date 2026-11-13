# Behaviour gates: the e2e suites mapped to the native client

Every test title in `e2e/web`, `e2e/voice`, `e2e/media` and `packages/desktop/test/electron` has one row. The last cell says where the behaviour is held now:

- a Rust test, `crate::path::test_name` (`dilla-client`, `dilla-desktop`, `dilla-media`; `tests/<file>` is an integration test, `src/...` an inline test; every name is checked to exist);
- `n/a (browser/phone only)` with the reason: the behaviour belongs to the browser or phone client, or to the Electron shell the native app replaces;
- `next plan (native)`: native integrations (tray, deep links, updater, OS notifications and permission prompts, capture, accessibility) planned after the screens wave 1 (see the "Next plans" list of `docs/superpowers/plans/2026-11-13-gpui-desktop-screens-wave1.md`);
- `TODO: crate::path::test_name`: in scope of wave 1 but with no Rust test yet; the proposed name is the work item of the Task 28 test lane.

`scripts/behaviour-gates.test.mjs` (CI test `behaviour_gate_table_complete`) parses the `test(` titles of those directories and fails on a title missing from this table, on a row whose title no e2e source has any more, and on a Rust reference that is not a real test function. Titles are the literal text of the e2e source (`test.describe` groups are not rows). When an e2e test is added, renamed or removed, change its row here.

Totals: 233 e2e tests; 141 with a Rust test, 42 n/a (browser/phone only), 21 next plan (native), 29 TODO only (32 TODO tests proposed in all).

## e2e/web

| File | Test title | Held by |
| --- | --- | --- |
| `e2e/web/admin-keypackages.spec.ts` | an instance admin enrols a second device by recovery key and finds the Bot commands there | dilla-client::tests/onboarding::enrol_by_password_and_recovery_key; dilla-client::tests/keypackages::fresh_device_can_be_added_by_welcome |
| `e2e/web/admin.spec.ts` | a server run from an empty instance: members, rename, channel edit, kick, ban, unban, leave, hand over and delete | dilla-client::tests/server_settings::server_rename_channel_edit_leave_hand_over_delete; dilla-client::tests/members::members_kick_ban_unban |
| `e2e/web/afk.spec.ts` | 1. a new server has one AFK voice channel, listed last, for the owner and for a member | dilla-desktop::tests/shell::afk_channel_listed_last_for_owner_and_member |
| `e2e/web/afk.spec.ts` | 2. Ada renames AFK "Idle" and moves it into the new category "Lounge"; Björn sees both without a reload | dilla-client::tests/server_settings::afk_rename_and_move_into_category_seen_live |
| `e2e/web/afk.spec.ts` | 3. no delete, no permissions and no reorder for AFK in server settings, the instance refuses its delete, and it stays last when a sibling moves | dilla-client::tests/server_settings::afk_has_no_delete_permissions_or_reorder |
| `e2e/web/afk.spec.ts` | 4. one timeout row: an idle desktop in general is cut by itself, told it was moved, joins no call, and is listed away under Idle; the peer in general hears no removal | dilla-desktop::tests/ring::an_afk_move_is_said_once_with_its_reason; dilla-desktop::tests/ring::the_afk_page_says_who_is_away_and_the_servers_timeout |
| `e2e/web/afk.spec.ts` | 5. Björn goes back to general with his own mute state and leaves the roster; Ada clicks Idle, leaves her call and is listed away until she sends a message | dilla-desktop::tests/ring::the_afk_channel_shows_the_afk_page_and_come_back_returns |
| `e2e/web/afk.spec.ts` | 6. a phone in general is never cut, its desktop twin cut beside it is not listed away, and the phone going AFK cuts the twin calmly | dilla-client::tests/ring::an_away_change_patches_the_roster_and_a_stale_or_empty_one_changes_nothing |
| `e2e/web/afk.spec.ts` | 7. with view_channel left out of his roles community-wide, Björn still sees Idle, and a click lists him away there with no call | dilla-client::tests/members::a_base_without_view_stays_0_and_the_afk_channel_grants_view_and_connect |
| `e2e/web/afk.spec.ts` | 8. Q2 (revised, AW2 final): his last connection closing keeps Björn away through the 30 s grace, then takes him off the roster; back, nothing joins | TODO: dilla-client::tests/ring::the_last_connection_closing_keeps_a_member_away_through_the_grace_then_lists_him_away |
| `e2e/web/afk.spec.ts` | 9. a web3.8 pick: after the upgrade the server has a new AFK, and lounge and Idle are ordinary | dilla-client::tests/ring::the_builtin_afk_channel_is_found_by_id_else_by_the_reservation |
| `e2e/web/afk.spec.ts` | 10. accessible: the channel list, the AFK row with its away roster and server settings | next plan (native): axe and CSP checks of the browser DOM, so native screen readers (AccessKit) come after the switch |
| `e2e/web/badges.spec.ts` | unopened channels badge their unread messages and mentions, opening reads them, and a mute hides the count but not the mention | dilla-client::tests/notify::unopened_channels_badge_open_reads_mute_hides_count |
| `e2e/web/badges.spec.ts` | the DM list head’s menu sets the device-wide notify default, and it is kept | dilla-client::tests/notify::dm_list_menu_sets_device_notify_default |
| `e2e/web/badges.spec.ts` | a direct message opened from the DMs tab reaches the peer, and the reply badges it until it is read | dilla-client::tests/notify::dm_from_tab_reaches_peer_reply_badges_until_read |
| `e2e/web/badges.spec.ts` | desktop notifications follow the default and the channel on screen | dilla-client::tests/notify::notifications_follow_default_and_channel_on_screen |
| `e2e/web/badges.spec.ts` | a direct message the peer opens appears, badges and notifies without a reload | dilla-client::tests/notify::dm_opened_by_peer_appears_badges_notifies_live |
| `e2e/web/bot-channels.spec.ts` | mark a channel, enable welcome, a new member is welcomed there | dilla-desktop::src/screens/bot_admin/bot_page::a_first_enable_offers_polls_and_reminders_on_and_an_installation_keeps_its_mask; dilla-client::src/features/bot_admin::welcome_settings_are_app_operations_and_the_save_sends_its_revision_first |
| `e2e/web/bot-channels.spec.ts` | a webhook POST appears in the channel | dilla-desktop::src/screens/timeline::a_webhook_post_of_the_pinned_bot_reads_via_webhook; dilla-client::tests/bot_admin::webhook_lifecycle_keeps_no_address |
| `e2e/web/bot-commands.spec.ts` | picker shows Bot commands with no network after sync | dilla-desktop::src/screens/commands/picker::bot_commands_are_greyed_with_the_reason_and_stay_listed |
| `e2e/web/bot-commands.spec.ts` | /remind round trip | dilla-client::tests/bot::remind_round_trip_through_sealed_commands |
| `e2e/web/bot-commands.spec.ts` | Enter to first answer is within RTT + 100 ms at p95 | dilla-client::tests/bot::enter_to_first_answer_is_within_rtt_plus_100ms_at_p95 |
| `e2e/web/bot-runtime.spec.ts` | the Firefox web guard exempts only its browser-chrome favicon refusal | n/a (browser/phone only): the Firefox browser-chrome console guard of the web e2e |
| `e2e/web/bot-runtime.spec.ts` | a bot enabled through server settings reaches ready through the native runtime | dilla-client::tests/bot_admin::a_bot_enabled_in_server_settings_reaches_ready |
| `e2e/web/bot-runtime.spec.ts` | literal music command text never executes through the native runtime | dilla-client::tests/bot::literal_music_command_text_never_executes |
| `e2e/web/bot-runtime.spec.ts` | the moderation log reads through sealed Bot commands on each device, across a Bot restart | dilla-desktop::src/screens/members/modlog::the_log_reads_pages_as_the_web_does |
| `e2e/web/bot-runtime.spec.ts` | a Roles-enabled bot applies the configured join role on join and rejoin | dilla-client::tests/bot_admin::a_roles_bot_applies_the_join_role_on_join_and_rejoin |
| `e2e/web/bot-runtime.spec.ts` | a human publishes a bot-authored self-service role panel and its reaction grant keeps its own source | TODO: dilla-client::tests/bot::a_bot_authored_role_panel_reaction_grant_keeps_its_own_source |
| `e2e/web/channel-list.spec.ts` | voice channels stand in order in the one channel list, with their people nested under them | dilla-desktop::src/screens/voice/mod::people_group_devices_by_user_and_a_mute_counts_when_every_device_is_muted; dilla-desktop::src/screens/channel_list::afk_is_listed_last_whatever_its_position |
| `e2e/web/conversation.spec.ts` | edit, reply, react, pin and delete between the browser and the peer | TODO: dilla-desktop::tests/integration::edit_reply_react_pin_delete_one_flow; dilla-client::tests/messages::edit_pin_delete_between_two_devices |
| `e2e/web/conversation.spec.ts` | an edit or a delete by someone else changes nothing | dilla-client::tests/messages::foreign_edit_or_delete_changes_nothing |
| `e2e/web/conversation.spec.ts` | mentions | dilla-client::tests/messages::mention_highlights_and_badges |
| `e2e/web/conversation.spec.ts` | images and files go both ways | dilla-client::tests/attachments::images_and_files_go_both_ways |
| `e2e/web/conversation.spec.ts` | a 25 MB file goes through and one byte more stays as an oversized card with a toast | dilla-client::tests/attachments::file_cap_exact_passes_one_more_refused |
| `e2e/web/conversation.spec.ts` | B sees what A makes | dilla-client::tests/messages::peer_sees_what_author_makes |
| `e2e/web/conversation.spec.ts` | a second browser of the same account edits and deletes the first browser’s message | dilla-client::tests/reactions::own_second_device_sees_reaction; dilla-client::tests/messages::edit_pin_delete_between_two_devices |
| `e2e/web/core-worker.spec.ts` | the core worker signs up, joins, reads and sends, survives a gateway restart and a reload, and keeps a second tab out | dilla-client::tests/sync::resume_after_sleep_no_gap_no_dup; dilla-client::tests/sync::two_devices_exchange_text; dilla-client::tests/store::first_open_creates_key_and_reopens |
| `e2e/web/core-worker.spec.ts` | a second browser joins the account by password and recovery key, badges and marks read through the worker, and forgets itself | dilla-client::tests/onboarding::enrol_by_password_and_recovery_key; dilla-client::tests/notify::unopened_channels_badge_open_reads_mute_hides_count |
| `e2e/web/core-worker.spec.ts` | two devices of one account fold edits, reactions, replies, pins and deletes, and files go through the worker | TODO: dilla-desktop::tests/integration::own_devices_fold_everything |
| `e2e/web/core-worker.spec.ts` | a pause failure retains the old Web Lock and leaves the follower outside | n/a (browser/phone only): the Web Lock and worker pause of the browser core worker |
| `e2e/web/core-worker.spec.ts` | a real core joins a DM call group and maps the SFU-less 501 to failed after its self-Remove | dilla-client::tests/calls::a_failed_start_of_a_call_this_device_registered_is_ended_alone |
| `e2e/web/core-worker.spec.ts` | a real core registers a DM call group and maps the SFU-less 501 to failed after its self-Remove | dilla-client::tests/calls::the_first_join_registers_the_call_group_then_starts_the_call |
| `e2e/web/create.spec.ts` | from an empty instance: start a server, invite a friend through a link, reply, DM and add a channel | dilla-client::tests/onboarding::empty_instance_start_server_invite_reply_dm_add_channel |
| `e2e/web/csp-axe.spec.ts` | the host serves the built client with the headers, types, cache rules and policy of the embed | n/a (browser/phone only): the headers, types and CSP of the web client the host serves |
| `e2e/web/csp-axe.spec.ts` | every state of the slice passes axe with target-size, in the Signal theme, without a CSP violation | next plan (native): axe and CSP checks of the browser DOM, so native screen readers (AccessKit) come after the switch |
| `e2e/web/enrol.spec.ts` | a second browser joins the account by password and recovery key, and the first can remove it | dilla-client::tests/onboarding::enrol_by_password_and_recovery_key |
| `e2e/web/enrol.spec.ts` | a wrong recovery key is refused in place, and the right one typed in lower case with hyphens works after it | dilla-client::tests/onboarding::wrong_recovery_key_refused_lowercase_hyphens_ok |
| `e2e/web/enrol.spec.ts` | sign out and remove takes this browser out of the account and out of the group | dilla-client::tests/user_settings::sign_out_and_remove_leaves_account_and_group |
| `e2e/web/enrol.spec.ts` | the peer’s enrolled device enters and leaves the group while the browser keeps talking | dilla-client::tests/onboarding::peer_enrolled_device_enters_and_leaves_group |
| `e2e/web/guard-window.spec.ts` | the cancelled-fetch line within 3 s of a navigation is dropped | n/a (browser/phone only): the browser console abort-line guard of the web e2e |
| `e2e/web/guard-window.spec.ts` | the same line 5 s after it is counted, and so is one with no navigation seen | n/a (browser/phone only): the browser console abort-line guard of the web e2e |
| `e2e/web/guard-window.spec.ts` | the same line on chromium or firefox is counted | n/a (browser/phone only): the browser console abort-line guard of the web e2e |
| `e2e/web/guard-window.spec.ts` | a slightly different line inside the window is counted | n/a (browser/phone only): the browser console abort-line guard of the web e2e |
| `e2e/web/installed-upgrade-account.spec.ts` | installed upgrade preserves the actual device keystore, encrypted history, draft guard and recovery enrollment | TODO: dilla-desktop::tests/integration::restart_keeps_keystore_history_draft_guard_recovery; dilla-client::tests/store::first_open_creates_key_and_reopens |
| `e2e/web/invite-landing.spec.ts` | the invite page names the community, keeps its limits and is shot in both schemes | n/a (browser/phone only): the invite landing page the instance serves to a browser |
| `e2e/web/keyboard.spec.ts` | the messaging flow runs on the keyboard alone, in the fixed focus order | dilla-desktop::tests/messages::keyboard_messages_focus_order; dilla-desktop::tests/shell::keyboard_shell_focus_order |
| `e2e/web/layout.spec.ts` | the layout grammar holds at desktop, tablet, phone, narrow and keyboard-height viewports | dilla-desktop::tests/shell::layout_grammar_holds_at_1280_and_1440 |
| `e2e/web/layout.spec.ts` | the AFK row and the AFK timeout setting keep the grammar | dilla-desktop::tests/ring::the_afk_page_says_who_is_away_and_the_servers_timeout |
| `e2e/web/layout.spec.ts` | FA1: phone controls outside the shell keep the prototype sizes, and the invites section keeps the settings grammar | n/a (browser/phone only): phone layout, the desktop client has no phone shell |
| `e2e/web/layout.spec.ts` | the DM and the voice layout give each element its room | dilla-desktop::tests/voice::twelve_people_with_long_names_draw |
| `e2e/web/layout.spec.ts` | FA4: search rows, phone settings index, channel reorder and privacy Compare | dilla-desktop::tests/search::parity_search_results |
| `e2e/web/layout.spec.ts` | FB1, FB2: the desktop Home title is not the server name; a file card stacks under the images at 360 and keeps its name | dilla-desktop::tests/shell::desktop_home_title_is_not_server_name; dilla-desktop::src/screens/attachments/mod::cards_wrap_like_a_flex_row_with_the_row_gap |
| `e2e/web/layout.spec.ts` | FC: the save button of a file card beside an image is not clipped at 1280 and at 1024 with the members open | dilla-desktop::src/screens/attachments/mod::cards_wrap_like_a_flex_row_with_the_row_gap |
| `e2e/web/layout.spec.ts` | a grouped row from an earlier day shows only its time of day in the gutter, which stays left of the content | dilla-desktop::src/screens/timeline::a_new_day_never_folds_into_the_run_above_and_starts_a_divider |
| `e2e/web/membership.spec.ts` | a kicked browser is told and loses the server, and after re-admission messages flow again | dilla-client::tests/members::kicked_client_told_loses_server_readmitted_flows |
| `e2e/web/messaging.spec.ts` | text flows both ways with the MLS-authenticated sender, and a send after the peer’s Update arrives once | dilla-client::tests/messages::text_both_ways_mls_sender_and_once_after_update |
| `e2e/web/moderation-evidence.spec.ts` | a warning made in the web client can be retrieved from Bot settings | dilla-client::tests/members::warn_lands_with_a_receipt_and_its_evidence_is_staged |
| `e2e/web/moderation-removal-recovery.spec.ts` | signed moderator removal reaches a live client and recovers as a tombstone after an offline reload | dilla-client::tests/removals::removal_reaches_live_client_as_tombstone; dilla-client::tests/removals::removal_while_offline_is_a_tombstone_after_restart |
| `e2e/web/native-interop.spec.ts` | native client and web client exchange messages | dilla-client::tests/messages::peer_sees_what_author_makes |
| `e2e/web/panes.spec.ts` | the sidebar splitter drags and takes keys, the width survives a reload, and the aside splitter exists only as a column | dilla-desktop::tests/shell::sidebar_splitter_drags_takes_keys_and_persists; dilla-desktop::tests/shell::aside_splitter_only_with_aside |
| `e2e/web/passkey.spec.ts` | passkeys: sign up with one, sign in with a copy of it, add the app, remove the passkey | dilla-client::tests/factors::browser_ceremony_against_dillad |
| `e2e/web/phone.spec.ts` | the phone shell: sign-up unzoomed, the tab bar, a pushed channel, the tabs, the link, tap targets and focus | n/a (browser/phone only): phone layout, the desktop client has no phone shell |
| `e2e/web/phone.spec.ts` | the sign-in fields stay at 16 px and unzoomed on a phone | n/a (browser/phone only): phone layout, the desktop client has no phone shell |
| `e2e/web/phone.spec.ts` | a landscape phone (844 x 390) gets the phone layout | n/a (browser/phone only): phone layout, the desktop client has no phone shell |
| `e2e/web/private-window.spec.ts` | an ephemeral WebKit context shows the unsupported screen, not E_INTERNAL | n/a (browser/phone only): the WebKit private-window storage refusal of the browser client |
| `e2e/web/private.spec.ts` | private channels run from an empty instance: create, gain, lose, make private and public, the lockout and a muted server | dilla-client::tests/server_settings::private_channels_client_gain_lose_toggle_lockout |
| `e2e/web/realtime-transport.spec.ts` | wire 6 (Bot v2) stays free of periodic version/activity/stats HTTP for fifteen idle minutes and reconciles reconnects | dilla-desktop::tests/diagnostics::idle_socket_no_periodic_http |
| `e2e/web/reload-mid-sync.spec.ts` | messages that reach a second device mid-sync are readable after it reloads, in every group it joined | TODO: dilla-desktop::tests/integration::restart_mid_sync_reads_every_group |
| `e2e/web/required-factor.spec.ts` | a sign-up left at the factor choice meets the forced screen on reopening | dilla-client::tests/onboarding::the_pending_join_survives_a_restart_at_the_factor |
| `e2e/web/required-factor.spec.ts` | a sign-up left at the factor choice joins the invited server after the forced set-up | dilla-client::tests/factors::forced_setup_adds_an_app_then_goes_ready |
| `e2e/web/required-factor.spec.ts` | an operator reset reaches a live page with no reload | TODO: dilla-client::tests/factors::an_operator_reset_reaches_a_live_client_with_no_reload |
| `e2e/web/required-factor.spec.ts` | the forced screen’s Forget this browser clears this browser and leaves the account’s device listed | dilla-client::tests/factors::forget_signs_the_device_out_on_the_instance |
| `e2e/web/required-factor.spec.ts` | a second tab takes dilla over from a tab on the forced screen | dilla-desktop::tests/instances::an_open_instance_is_held_until_released |
| `e2e/web/required-factor.spec.ts` | after a reset, a fresh browser takes the password and then the set-up, not a code | TODO: dilla-client::tests/factors::after_a_reset_a_fresh_device_takes_the_password_then_the_setup |
| `e2e/web/roles.spec.ts` | roles run from an empty instance: make, assign, pin by right, mention, demote and delete | dilla-client::tests/members::roles_make_assign_demote_delete_and_refusal |
| `e2e/web/search-settings.spec.ts` | native edit is searched by prefix and deleted text disappears after a positive result | dilla-client::tests/search::edit_found_by_prefix_deleted_text_gone |
| `e2e/web/search-settings.spec.ts` | Privacy, Keyboard and Diagnostics open on a phone with the nav as chips and close with Escape | n/a (browser/phone only): phone layout, the desktop client has no phone shell |
| `e2e/web/search-settings.spec.ts` | the search palette finds a setting and opens its section | dilla-desktop::tests/search::palette_finds_setting_opens_section |
| `e2e/web/search-settings.spec.ts` | Appearance controls fit desktop rows and the phone section header keeps its title readable | dilla-desktop::src/screens/user_settings/appearance::the_setting_steps_the_shared_components_density |
| `e2e/web/search-settings.spec.ts` | a dirty Profile draft survives Keep editing and is discarded only after confirmation | dilla-desktop::tests/settings_host::a_dirty_draft_blocks_the_switch_until_discard; dilla-desktop::src/screens/user_settings/draft::dirty_profile_draft_keep_editing_then_discard |
| `e2e/web/search-settings.spec.ts` | scopes and keyboard selection stay on the textbox until a target is chosen | dilla-desktop::tests/search::scopes_and_selection_stay_on_textbox |
| `e2e/web/search-settings.spec.ts` | typing is audience-scoped, expires by server time and is not replayed | dilla-client::tests/messages::typing_is_sent_and_received; dilla-desktop::src/screens/messages/typing::an_expired_row_and_an_empty_list_say_nothing |
| `e2e/web/search-settings.spec.ts` | switching channel with an unsent draft shows no error banner | dilla-desktop::src/screens/messages/composing::a_draft_belongs_to_one_channel_under_the_drafts_prefix |
| `e2e/web/search-settings.spec.ts` | desktop, phone and reflow keep controls accessible in Signal | next plan (native): axe and CSP checks of the browser DOM, so native screen readers (AccessKit) come after the switch |
| `e2e/web/search-settings.spec.ts` | settings rows align their controls, and the sign-out button holds its text | dilla-desktop::tests/voice_settings::parity_voice_settings_section; dilla-desktop::tests/components::every_component_renders_in_every_state |
| `e2e/web/second-factor.spec.ts` | second factor: sign-up step 5, a code and a backup code on new browsers, a wrong code, Settings → Sign-in | dilla-client::tests/factors::step_up_backup_codes_remove_and_password; dilla-desktop::tests/factors::password_then_factor_then_recovery_key_backup_code_waits |
| `e2e/web/second-tab.spec.ts` | a second tab waits while the first holds the device and takes over when it closes | dilla-desktop::tests/instances::an_open_instance_is_held_until_released; dilla-desktop::tests/instances::a_released_hold_lasts_until_the_client_stopped |
| `e2e/web/sidebar-actions.spec.ts` | the owner adds a channel with the "+"; a member makes a link under server settings → Invites; a phone has a gear and a search icon, no menu | dilla-client::tests/onboarding::owner_adds_channel_with_plus; dilla-client::tests/server_settings::member_makes_invite_link_under_invites |
| `e2e/web/signin-order.spec.ts` | sign-in order: the password, then the factor, then the recovery key; a backup code waits for the key | dilla-desktop::tests/factors::password_then_factor_then_recovery_key_backup_code_waits |
| `e2e/web/signup.spec.ts` | a server invite, a name and a written-down key make an account that lands in that server | dilla-client::tests/onboarding::invite_name_written_key_lands_in_server |
| `e2e/web/slash-commands.spec.ts` | composer keeps private results private, publishes only explicit text, and retains separate channel/thread drafts | dilla-client::tests/bot::private_results_stay_private_and_auto_dismiss; dilla-desktop::tests/bot::a_private_result_follows_the_last_message |
| `e2e/web/slash-commands.spec.ts` | polls publish in channels and threads, accept real votes, reject forged markers, and close by the creator | dilla-desktop::tests/bot::poll_card_draws_under_the_bots_post; dilla-client::src/features/utilities/polls::selection_rules |
| `e2e/web/slash-commands.spec.ts` | moderation requires confirmation and applies to a distinct joined member | TODO: dilla-desktop::tests/bot::a_moderation_command_asks_for_confirmation_then_applies_to_a_joined_member |
| `e2e/web/slash-commands.spec.ts` | reminders sync across devices and remain due after Bot restart and reload | dilla-client::tests/bot::reminders_sync_across_devices_and_stay_due_after_a_bot_restart |
| `e2e/web/slash-commands.spec.ts` | a reminder set from the picker chips appears under Upcoming | dilla-desktop::tests/bot::a_reminder_set_from_the_chips_appears_under_upcoming |
| `e2e/web/slowmode-announce.spec.ts` | slow mode and an announcement channel from an empty instance: the wait, the queue, the read-only notice, a refused post and a lying class | dilla-desktop::tests/messages::slow_mode_wait_queue_and_announcement_read_only |
| `e2e/web/socket-only.spec.ts` | no app request uses HTTP while the socket is up | dilla-desktop::tests/diagnostics::no_app_http_while_socket_up |
| `e2e/web/socket-only.spec.ts` | joining and leaving voice goes over the socket | dilla-client::tests/calls::call_signalling_uses_socket_when_up |
| `e2e/web/spike-joinall.spec.ts` | SP-W201: join-all and the load-free core at 200 groups | n/a (browser/phone only): the join-all load spike of the wasm core in a browser worker |
| `e2e/web/support.spec.ts` | the test host serves the client root same-origin, with its headers, to a persistent profile | n/a (browser/phone only): the test host serving the web client with its headers |
| `e2e/web/support.spec.ts` | axe evaluates under the served CSP without a policy violation | next plan (native): axe and CSP checks of the browser DOM, so native screen readers (AccessKit) come after the switch |
| `e2e/web/support.spec.ts` | the native peer enrols and builds its community through the public routes | dilla-client::tests/onboarding::empty_instance_start_server_invite_reply_dm_add_channel |
| `e2e/web/support.spec.ts` | the harness build is served by its manifest: the wasm as application/wasm, immutable, and unknown paths fall back to index.html | n/a (browser/phone only): the web e2e harness serving the browser build |
| `e2e/web/support.spec.ts` | totp() reproduces the RFC 6238 SHA-1 vectors | n/a (browser/phone only): the TypeScript totp() helper of the e2e harness |
| `e2e/web/threads.spec.ts` | a reply in a thread shows its count on both sides, the pane sits in the aside, and a reload keeps it | dilla-client::tests/threads::thread_reply_count_both_sides_aside_and_kept_after_restart; dilla-desktop::tests/threads::thread_pane_opens_in_the_aside_with_its_composer_unfocused |
| `e2e/web/threads.spec.ts` | a mention in a thread badges the channel, and the reply stays out of the channel | dilla-client::tests/threads::thread_mention_badges_channel_reply_stays_out |
| `e2e/web/threads.spec.ts` | Ada edits and deletes her own reply in the thread | dilla-client::tests/threads::edit_and_delete_own_thread_reply |
| `e2e/web/threads.spec.ts` | a thread opens from a search hit | TODO: dilla-desktop::tests/integration::thread_opens_from_search_hit |
| `e2e/web/threads.spec.ts` | a thread opens from the pin of a reply | TODO: dilla-desktop::tests/integration::thread_opens_from_pin_of_reply |
| `e2e/web/threads.spec.ts` | on a phone the thread is a full-screen sheet, the system back closes it and the channel remains | n/a (browser/phone only): phone layout, the desktop client has no phone shell |
| `e2e/web/threads.spec.ts` | two people thread in a DM | TODO: dilla-client::tests/threads::two_people_thread_in_a_dm |
| `e2e/web/timeout.spec.ts` | manual text timeout blocks community writes and recovers while read, voice, DMs and own deletion continue | dilla-client::tests/members::timeout_blocks_community_writes_and_recovers |
| `e2e/web/trust-polish.spec.ts` | rotation keeps already visible text; an independent trusted group still works | dilla-client::tests/dms::rotation_keeps_visible_text_independent_group_works |
| `e2e/web/trust-polish.spec.ts` | Copy alone does not approve; explicit exact-key comparison admits new traffic | dilla-client::tests/dms::copy_alone_does_not_approve_exact_compare_admits |
| `e2e/web/trust-polish.spec.ts` | after a reload Privacy has names and Compare; a changed contact can be compared and verified | dilla-client::tests/dms::privacy_names_and_compare_after_restart |
| `e2e/web/trust-polish.spec.ts` | a validated Remove of the changed device restores sending here only; the contact stays changed | dilla-client::tests/dms::remove_changed_device_restores_sending_here_only |
| `e2e/web/version-reload.spec.ts` | a tab older than the host is refused two ways: banner, no reconnect, and Reload keeps the draft | dilla-desktop::tests/diagnostics::older_client_refused_banner_no_reconnect_keeps_draft |
| `e2e/web/z/reload.spec.ts` | a relaunched profile comes back from its stored device key, even after its session expired | dilla-client::tests/sync::expired_session_reauthenticates; dilla-client::tests/store::first_open_creates_key_and_reopens |
| `e2e/web/z/reload.spec.ts` | a profile whose device key wrapper is gone says so and can start over after a confirmation | dilla-client::tests/store::database_without_key_is_refused_and_left_alone; dilla-desktop::tests/factors::forget_after_the_shell_closes_and_wipes_the_store |

## e2e/voice

| File | Test title | Held by |
| --- | --- | --- |
| `e2e/voice/autoplay.spec.ts` | call audio the browser holds back comes back with one press of turn on audio | n/a (browser/phone only): the browser autoplay policy, the native client plays through the device |
| `e2e/voice/blocked.spec.ts` | (a) a changed contact in the call group refuses the join before any call write is sent; the room never sees the browser | dilla-client::tests/ring::blocked_contact_refuses_join_before_any_call_write |
| `e2e/voice/blocked.spec.ts` | (b) a device that fails the signed-list check joins mid-call: alice leaves, her dock goes and so does her leaf | dilla-client::src/features/voice::a_refused_epoch_pauses_on_a_changed_identity_and_leaves_on_any_other_trust_block |
| `e2e/voice/blocked.spec.ts` | (c) a contact whose identity changes mid-call pauses the call for each browser until verified or gone (RV-3) | dilla-client::src/features/voice::a_refused_epoch_pauses_on_a_changed_identity_and_leaves_on_any_other_trust_block |
| `e2e/voice/blocked.spec.ts` | DM: a blocked DM call leaves the DM composer enabled | dilla-desktop::tests/ring::a_held_dm_block_hides_the_call_button_and_says_why |
| `e2e/voice/bot-music.spec.ts` | play from Enter to first audio frame meets G3 | TODO: dilla-media::tests/call::bot_play_from_enter_to_first_audio_frame_meets_g3 |
| `e2e/voice/bot-music.spec.ts` | next queued track gap is under 150 ms | TODO: dilla-media::tests/call::bot_next_queued_track_gap_is_under_150_ms |
| `e2e/voice/bot-music.spec.ts` | a repeated track starts within 500 ms at p95 | TODO: dilla-media::tests/call::bot_repeated_track_starts_within_500_ms_at_p95 |
| `e2e/voice/bot-music.spec.ts` | a Bot restart mid-queue resumes | TODO: dilla-desktop::tests/bot_admin::the_player_resumes_after_a_bot_restart_mid_queue |
| `e2e/voice/bot-music.spec.ts` | a device that joins while the Bot plays hears it | TODO: dilla-media::tests/call::a_device_joining_while_the_bot_plays_hears_it |
| `e2e/voice/bot-music.spec.ts` | a new device uses /play within its first session | TODO: dilla-desktop::tests/bot::a_new_device_uses_play_within_its_first_session |
| `e2e/voice/controls.spec.ts` | Control+Shift+M and Control+Shift+D show alice muted and deafened to bob; push to talk sends only while held | dilla-desktop::tests/voice::push_to_talk_opens_the_mic_outside_text_fields; dilla-desktop::tests/voice::push_to_talk_is_off_in_a_text_field; TODO: dilla-desktop::tests/voice::control_shift_m_and_d_show_muted_and_deafened_to_the_peer |
| `e2e/voice/csp-axe.spec.ts` | the ring, the call, a player and Settings → Voice pass axe in three themes and at 360×740, with no CSP violation | next plan (native): axe and CSP checks of the browser DOM, so native screen readers (AccessKit) come after the switch |
| `e2e/voice/enhanced-fallback.spec.ts` | a missing enhanced model visibly falls back; Off avoids all enhancement assets | dilla-desktop::tests/voice_settings::missing_model_falls_back_visibly_off_loads_nothing; dilla-media::tests/audio::missing_model_falls_back_visibly |
| `e2e/voice/enhanced-latency.spec.ts` | the enhanced microphone reaches the encrypted sender within the 80 ms budget under load | dilla-media::tests/audio::enhanced_within_80ms_budget |
| `e2e/voice/enhanced-noise.spec.ts` | Automatic enhances the local test and encrypted calls, enforces PTT, and remains stable under load | dilla-desktop::tests/voice_settings::automatic_enhances_test_and_call_enforces_ptt; dilla-media::tests/audio::enhanced_lowers_noise_10db_on_device_path |
| `e2e/voice/join-muted.spec.ts` | mute pressed in the user panel before joining: alice joins muted, on her tile, her panel and every audio sender | dilla-client::tests/voice::mute_before_join_mutes_every_sender |
| `e2e/voice/join-muted.spec.ts` | mute pressed while the join is pending (noise ${noise}): the call starts muted and no audio sender is ever enabled | dilla-client::tests/voice::mute_before_join_mutes_every_sender |
| `e2e/voice/join-muted.spec.ts` | a join that starts muted shows bob the muted glyph, and the voice_state carries self_mute | dilla-client::tests/voice::muted_join_shows_glyph_and_self_mute |
| `e2e/voice/join-talk.spec.ts` | two browsers join one voice channel, see each other ok and hear each other; no key in a ret or a slice | dilla-client::tests/voice::two_clients_join_see_and_hear_no_key_in_state; dilla-media::tests/call::desktop_and_web_in_one_call |
| `e2e/voice/leave-reload.spec.ts` | a reload mid-call is a leave bob sees, the rejoin is a fresh leaf, and leave returns focus to the voice row | dilla-client::tests/calls::a_leave_ends_the_call_when_alone_then_proposes_its_remove_and_forgets; dilla-desktop::tests/voice::closing_the_window_leaves_its_call |
| `e2e/voice/leave-reload.spec.ts` | a rejoin whose Remove nobody has committed yet waits, says so, and joins the same call once it is committed | dilla-client::tests/calls::a_rejoin_refused_while_its_remove_stands_waits_then_joins_the_same_group_once_it_moves |
| `e2e/voice/media-attachments.spec.ts` | bob plays alice's webm and WAV inline; a truncated file shows the error and save; every blob: URL is revoked after | dilla-desktop::src/screens/attachments/media::truncated_and_garbage_audio_never_panics; dilla-desktop::tests/attachments::audio_attachment_plays |
| `e2e/voice/microphone-test.spec.ts` | Let’s check plays you back live, shows the level and releases the microphone when it stops | dilla-desktop::tests/voice_settings::lets_check_plays_back_shows_level_releases_mic |
| `e2e/voice/noise-beta.spec.ts` | beta requires opt-in, enhances preview and encrypted calls, and can be left during a call | dilla-desktop::tests/voice_settings::enhanced_opt_in_preview_and_call_leave_during_call |
| `e2e/voice/phone-call.spec.ts` | on a phone the call bar opens the stage, which holds the one mute button, and back closes it | n/a (browser/phone only): phone layout, the desktop client has no phone shell |
| `e2e/voice/phone-call.spec.ts` | on a phone without getDisplayMedia the stage offers no share button | n/a (browser/phone only): phone layout, the desktop client has no phone shell |
| `e2e/voice/realtime-transport.spec.ts` | a live call sends coarse reports over the existing socket and defers an update until the call ends | TODO: dilla-client::tests/calls::coarse_reports_go_over_the_socket_and_an_update_is_deferred_until_the_call_ends |
| `e2e/voice/reload.spec.ts` | a reload mid-call leaves one tile for alice, and a reload without a rejoin leaves none within the sweep bound | dilla-client::tests/voice::voice_roster_follows_join_and_leave; dilla-desktop::tests/voice::closing_the_window_leaves_its_call |
| `e2e/voice/ring.spec.ts` | answer: bob rings (banner, sound, one notification while hidden), answers, every ring artefact goes and both are ok | dilla-client::tests/ring::answer; dilla-desktop::tests/ring::enter_answers_the_ring_with_focus; next plan (native): the ring sound and the OS notification (Notifier trait) |
| `e2e/voice/ring.spec.ts` | decline: alice sees "bob declined." and bob's banner goes | dilla-client::tests/ring::decline; dilla-desktop::tests/ring::enter_on_decline_declines |
| `e2e/voice/ring.spec.ts` | busy: a ring while bob is in voice shows the busy text, with no ring sound and no notification | dilla-client::tests/ring::busy |
| `e2e/voice/ring.spec.ts` | blocked DM: a ring of a DM whose contact's identity changed never shows; a trusted typing and ring after it do | dilla-client::tests/ring::blocked_dm |
| `e2e/voice/ring.spec.ts` | timeout: the ring ends when the instance's 30 s pass (op 53 state 0), waited for, never timed | dilla-client::tests/ring::timeout |
| `e2e/voice/sensitivity.spec.ts` | a voice under the threshold is not sent; the threshold and the levels persist | dilla-desktop::tests/voice_settings::under_threshold_not_sent_threshold_persists |
| `e2e/voice/share.spec.ts` | a screen share and a camera reach the other browser verified, and stop | dilla-client::tests/voice::share_and_camera_reach_peer_verified_and_stop |
| `e2e/voice/share.spec.ts` | a share without sound says so on both sides | TODO: dilla-desktop::tests/voice::a_share_without_sound_says_so_on_both_sides |
| `e2e/voice/share.spec.ts` | cancelling native screen selection publishes no video or sound and leaves the voice call usable | next plan (native): the screen-capture picker window (Capture) |
| `e2e/voice/share.spec.ts` | a shared tab’s sound is heard by the other browser | n/a (browser/phone only): a browser tab's audio through getDisplayMedia |
| `e2e/voice/speaking.spec.ts` | alice speaking lights her tile and bob's tile for her within a beat, and a mute puts it out | dilla-desktop::src/screens/voice/session::speaking_lights_after_its_onset_and_holds_for_the_hangover |
| `e2e/voice/stage.spec.ts` | the stage shows a tile per camera and screen, focuses a shared screen, and keeps every stream running | dilla-desktop::tests/voice::stage_tile_per_stream_focuses_share; dilla-desktop::src/screens/voice/mod::the_stage_has_a_tile_per_device_and_one_per_screen_share |
| `e2e/voice/stage.spec.ts` | a DM call box keeps compact tiles when the other person shares their screen | TODO: dilla-desktop::tests/voice::a_dm_call_box_keeps_compact_tiles_when_the_peer_shares |

## e2e/media

| File | Test title | Held by |
| --- | --- | --- |
| `e2e/media/audio.spec.ts` | setProcessor and a mic restart keep the receiver decrypting (SP-10) | n/a (browser/phone only): the browser RTCRtpScriptTransform and setProcessor path, the Rust glue seals per epoch (dilla-media::tests/call::epoch_change_mid_stream) |
| `e2e/media/audio.spec.ts` | the worklet compiles without atob and reports constructor timing (SP-35) | n/a (browser/phone only): the browser AudioWorklet, native audio runs in dilla-media |
| `e2e/media/audio.spec.ts` | the worklet's wasm compile under "${policy}" ${compiles ? 'succeeds' : 'fails'} (SP-35) | n/a (browser/phone only): the browser AudioWorklet and CSP wasm policy |
| `e2e/media/canary.spec.ts` | a canary connected straight to the SFU is decoded by nobody (MD-13, SP-13, DEV-63) | TODO: dilla-media::tests/live::a_canary_connected_straight_to_the_sfu_is_decoded_by_nobody |
| `e2e/media/cross-sdk.spec.ts` | the Go peer and Chromium decrypt each other (SP-15 cross-SDK) | dilla-media::tests/call::rust_to_web_audio; dilla-media::tests/call::web_to_rust_audio |
| `e2e/media/e2ee-call.spec.ts` | three contexts decrypt per KID; a join and a leave move every receiver to the new KID | dilla-media::tests/live::desktop_to_desktop_through_the_sfu_with_client_core_keys_and_an_epoch_change; dilla-media::tests/call::epoch_change_mid_stream |
| `e2e/media/fail-closed.spec.ts` | control: a dilla sender and receiver carry video, and the path is the expected one | dilla-media::tests/live::desktop_to_desktop_through_the_sfu_with_client_core_keys_and_an_epoch_change |
| `e2e/media/fail-closed.spec.ts` | a sender labelled with a server-forced codec (AV1) still attaches and encrypts what it sends | next plan (native): video send path and codec policy arrive with capture |
| `e2e/media/fail-closed.spec.ts` | a sender whose frames have no prefix rule (AV1) sends 0 media bytes | next plan (native): video send path and codec policy arrive with capture |
| `e2e/media/fail-closed.spec.ts` | a sender with no slot (unknown source) is blocked: 0 media bytes, reported and unpublished | dilla-media::tests/glue::the_microphone_sender_is_bound_as_it_attaches_and_an_unbound_sender_sends_nothing |
| `e2e/media/fail-closed.spec.ts` | control: a receiver without any transform renders a plaintext sender | n/a (browser/phone only): control case of the browser fail-closed harness |
| `e2e/media/fail-closed.spec.ts` | a receiver whose transform cannot be attached at all renders 0 frames of that plaintext sender | dilla-media::tests/hook_enforced::the_glue_refuses_a_peer_connection_without_a_hook_and_any_content_bearing_pass |
| `e2e/media/fail-closed.spec.ts` | a sender whose worker fails mid-call sends 0 media bytes afterwards, and nothing renders | dilla-media::src/room::an_audio_failure_after_sealing_is_not_retryable |
| `e2e/media/fail-closed.spec.ts` | a receiver whose worker fails mid-call decodes and renders 0 frames of a plaintext sender afterwards | dilla-media::tests/glue::dropped_keys_seal_and_open_nothing_until_the_next_epoch |
| `e2e/media/fail-closed.spec.ts` | a receiver whose dilla options are refused gets a blocking transform and renders 0 frames | dilla-media::tests/glue::a_receiver_is_bound_by_its_handle_never_by_its_track_id_and_only_once |
| `e2e/media/firefox-smoke.spec.ts` | Firefox joins a Chromium call with voice, camera and a canvas screen share (DEV-63) | n/a (browser/phone only): the Firefox browser client |
| `e2e/media/frame-accounting.spec.ts` | frame accounting rejects unauthenticated video and decoded audio while counting refused RTP packets | dilla-media::tests/glue::a_frame_ending_in_the_sif_trailer_is_dropped_before_any_header_is_parsed; dilla-media::tests/glue::a_dropped_video_frame_asks_for_key_frames_until_a_decrypted_key_frame_passes |
| `e2e/media/harness.spec.ts` | three isolated Chromium contexts decode each other through the in-process SFU | n/a (browser/phone only): the Chromium harness that proves the browser path |
| `e2e/media/provider-opus.spec.ts` | mediabot timeout sends TERM, escalates to KILL, and reaps the process | n/a (browser/phone only): the Go mediabot process supervisor, a test harness of the browser suites |
| `e2e/media/provider-opus.spec.ts` | mediabot output overflow is capped, terminated, and reaped | n/a (browser/phone only): the Go mediabot process supervisor, a test harness of the browser suites |
| `e2e/media/provider-opus.spec.ts` | named Ogg Opus input is encrypted and authenticated in Chromium and Firefox | n/a (browser/phone only): Chromium and Firefox decoding of Ogg Opus input |
| `e2e/media/real-sfu-guards.spec.ts` | audio stops at a failed worker and a dead sender refuses replaceTrack through the real SFU | dilla-media::src/room::an_audio_failure_after_sealing_is_not_retryable |
| `e2e/media/real-sfu-guards.spec.ts` | a recorded pre-connect microphone is refused through the real SFU | dilla-media::tests/glue::the_microphone_sender_is_bound_as_it_attaches_and_an_unbound_sender_sends_nothing |
| `e2e/media/sender-binding.spec.ts` | valid ciphertext under another member’s track identity is never rendered | dilla-media::tests/glue::a_receiver_is_bound_by_its_handle_never_by_its_track_id_and_only_once |
| `e2e/media/unsupported-sfu-codec.spec.ts` | the real SFU offers AV1 but the client negotiates and sends VP8 | next plan (native): video codec negotiation arrives with the video send path |

## packages/desktop/test/electron (Electron shell)

| File | Test title | Held by |
| --- | --- | --- |
| `packages/desktop/test/electron/00-mac-notifications.spec.ts` | macOS accepts a real native notification from the shipping sandboxed app | next plan (native): OS notifications behind the Notifier trait |
| `packages/desktop/test/electron/application-audio.spec.ts` | selected application reaches an encrypted listener while other applications and Dilla call audio stay excluded | next plan (native): per-application audio capture |
| `packages/desktop/test/electron/application-audio.spec.ts` | native application audio without an authenticated source slot cannot fall back to plaintext | next plan (native): per-application audio capture |
| `packages/desktop/test/electron/forwarding.spec.ts` | actual forwarding keeps worker bearer/body/response and omits the private session cookie | dilla-client::tests/http::the_request_is_what_the_typescript_client_sends; dilla-client::tests/http::credential_bodies_are_zeroizing_and_never_printed |
| `packages/desktop/test/electron/forwarding.spec.ts` | actual forwarding preserves the worker redirect-error policy | dilla-client::tests/http::a_redirect_is_not_followed |
| `packages/desktop/test/electron/media.spec.ts` | the real media worker encrypts and decrypts video in Electron | dilla-media::tests/live::desktop_to_desktop_through_the_sfu_with_client_core_keys_and_an_epoch_change |
| `packages/desktop/test/electron/media.spec.ts` | an unbound sender cannot fall back to plaintext | dilla-media::tests/glue::the_microphone_sender_is_bound_as_it_attaches_and_an_unbound_sender_sends_nothing |
| `packages/desktop/test/electron/media.spec.ts` | a failed media worker stops sending frames | dilla-media::src/room::an_audio_failure_after_sealing_is_not_retryable |
| `packages/desktop/test/electron/permissions.spec.ts` | the shipping permission boundary refuses ${kind} capture after native Deny | next plan (native): OS permission prompts and capture |
| `packages/desktop/test/electron/permissions.spec.ts` | a grant is reusable by its owner but cannot authorize another complete instance URL | next plan (native): OS permission prompts and capture |
| `packages/desktop/test/electron/permissions.spec.ts` | another sandboxed webContents cannot borrow an owning window permission grant | n/a (browser/phone only): Electron webContents permission grants, the native app has no web renderer |
| `packages/desktop/test/electron/permissions.spec.ts` | screen capture without a real user gesture is refused without opening a picker | next plan (native): OS permission prompts and capture |
| `packages/desktop/test/electron/permissions.spec.ts` | legacy desktop getUserMedia cannot capture a valid screen source without a user gesture or picker consent | n/a (browser/phone only): Electron legacy getUserMedia desktop sources |
| `packages/desktop/test/electron/permissions.spec.ts` | a trusted direct getDisplayMedia call cannot bypass the native broker picker | n/a (browser/phone only): Electron getDisplayMedia broker, native capture is the Capture plan |
| `packages/desktop/test/electron/permissions.spec.ts` | legacy desktop video without a source ID and system audio remain denied in the account renderer | n/a (browser/phone only): Electron legacy getUserMedia desktop sources |
| `packages/desktop/test/electron/permissions.spec.ts` | cancelling the typed native screen picker returns null without creating a broker | next plan (native): the screen-capture picker window (Capture) |
| `packages/desktop/test/electron/permissions.spec.ts` | an unfamiliar native invite is refused before connecting or persisting its host | dilla-desktop::tests/onboarding::an_invite_link_is_confirmed_before_anything_is_stored_or_sent |
| `packages/desktop/test/electron/picker.spec.ts` | Connect closes the picker: exactly one window, the instance | dilla-desktop::tests/instances::manager_lists_and_connects_by_url_and_invite |
| `packages/desktop/test/electron/picker.spec.ts` | cancelling the confirmation keeps the picker | dilla-desktop::tests/instances::remove_needs_a_confirmation |
| `packages/desktop/test/electron/screen-capture.spec.ts` | a selected native application window stays video-only and encrypted through the real media worker | next plan (native): screen and window capture |
| `packages/desktop/test/electron/screen-capture.spec.ts` | an unbound native screen source is blocked while its real input remains live | next plan (native): screen and window capture |
| `packages/desktop/test/electron/screen-capture.spec.ts` | a foreign instance cannot stop or reconfigure the capture and stopping its track destroys the broker | next plan (native): screen and window capture |
| `packages/desktop/test/electron/screen-capture.spec.ts` | owner navigation releases its native broker | next plan (native): screen and window capture |
| `packages/desktop/test/electron/screen-capture.spec.ts` | closing the selected source ends its consumer track and destroys the broker | next plan (native): screen and window capture |
| `packages/desktop/test/electron/shell.spec.ts` | runs the installed React client in an isolated sandbox without executing server HTML | n/a (browser/phone only): the Electron renderer sandbox, the native app has no web renderer |
| `packages/desktop/test/electron/shell.spec.ts` | focused copy writes text to the system clipboard while clipboard reads and hidden writes stay denied | TODO: dilla-desktop::tests/messages::copy_writes_to_the_system_clipboard_only_from_a_focused_action |
| `packages/desktop/test/electron/shell.spec.ts` | protected reload needs an explicit native confirmation and cancelling retains the document | n/a (browser/phone only): the web client reload, the native app has no document to reload |
| `packages/desktop/test/electron/shell.spec.ts` | an explicit focused video action can enter and leave HTML fullscreen | n/a (browser/phone only): the HTML fullscreen API, native video opens in the system player |
| `packages/desktop/test/electron/shell.spec.ts` | rejects malicious IPC and remote document navigation and keeps storage across reload | n/a (browser/phone only): Electron IPC and renderer navigation, the native app has neither |
| `packages/desktop/test/electron/shell.spec.ts` | closing a window keeps the client alive for background work | dilla-desktop::tests/instances::closing_a_window_keeps_client_alive; next plan (native): close-to-tray |
| `packages/desktop/test/electron/shell.spec.ts` | forwards real API requests and POST bodies to the selected base path | dilla-client::tests/http::the_request_is_what_the_typescript_client_sends; dilla-client::src/features/signup::a_base_path_invite_names_its_instance |
| `packages/desktop/test/electron/shell.spec.ts` | isolates storage for two complete URLs on the same host | dilla-desktop::tests/instances::two_instances_same_host_isolated; dilla-client::tests/instances::two_instances_same_host_isolated |
| `packages/desktop/test/electron/shell.spec.ts` | short startup content keeps an opaque theme-matched background across window resizes | n/a (browser/phone only): the Electron startup flash, the GPUI window paints its own background |
| `packages/desktop/test/electron/shell.spec.ts` | downloads a decrypted local attachment while refusing blob document navigation | TODO: dilla-desktop::tests/attachments::save_writes_the_decrypted_attachment_to_disk |
| `packages/desktop/test/electron/shell.spec.ts` | a desktop resize fills the real native content area and cannot shrink into a mobile window | dilla-desktop::tests/shell::resize_cannot_shrink_below_desktop_minimum |
| `packages/desktop/test/electron/shell.spec.ts` | startup and sign-in use one compact window which expands only for the workspace | dilla-desktop::tests/shell::startup_and_sign_in_compact_window_expands_for_workspace |
| `packages/desktop/test/electron/trace-retention.spec.ts` | retains both real peer renderer traces when body finally closes before failure status | n/a (browser/phone only): the Electron e2e trace harness |
