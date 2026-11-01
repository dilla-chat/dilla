# Task 2 — fullscreen UI primitives

Implemented the settings workspace primitives in `packages/ui/src/SettingsFrame`, `SettingsNav`, and their stories/tests.

- `SettingsFrame` now always uses `showModal()`, keeps nested dialogs in control of Escape, ignores backdrop clicks, restores opener focus, and focuses a visible main landmark if the opener is gone.
- Added identity/search, phone index/section, section title and Back, content ref, wide content measure, and activity control props. The existing phone layout provider supplies the shared narrow/landscape breakpoint; the frame uses its `data-layout="phone"` marker and visual viewport/safe area values.
- Desktop has a 15rem sidebar, independent scrolling, an 80rem maximum frame, 48rem normal and 64rem wide section measures, 2rem/1rem body spacing, and a persistent Close control. Phone index/section views show grouped navigation or Back/section content respectively.
- `SettingsNav` supports group headings, scrolls the active item into view, and preserves roving focus through grouped items. Existing `SettingsRow` sizing already met the 44px touch target requirement, so it needed no code change.
- Updated the frame/nav stories for grouped navigation and the phone index.

## Verification

- TDD red phase: the new behavior tests failed against the original modeless/backdrop-dismissable implementation.
- Focused tests: 54 passed.
- `npm test` in `packages/ui`: 111 files and 1,303 tests passed; the story shooting utility tests passed.
- `npm run typecheck` in `packages/ui`: passed.
- `npm run build-storybook` in `packages/ui`: passed. Vite emitted its existing large-chunk advisory.
- `git diff --check`: passed.

The full suite prints four jsdom “Not implemented: navigation” messages from existing hyperlink tests; all tests still pass.
