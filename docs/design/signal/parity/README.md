# Prototype-vs-story parity

`shoot.mjs` opens the Signal prototype (`../prototype/Dilla Signal Prototype.dc.html`, from `file://`) and a built
Storybook story at the same viewport, and writes three PNGs per pair: the prototype, the story, and the two side by side
(prototype left, story right, 1px grey divider).

## Run it

```
npm run build-storybook -w @dilla/ui
npx http-server packages/ui/storybook-static -p 6006 --silent &
node docs/design/signal/parity/shoot.mjs <spec.json> <outDir> [--storybook http://127.0.0.1:6006]
```

Output goes to `<outDir>/<name>.prototype.png`, `<name>.story.png` and `<name>.pair.png`. Keep `<outDir>` outside the
repo. One line is printed per pair (`ok` or `FAIL`); the exit code is non-zero if any side fails to load.
`examples/shell.json` is a working spec (desktop shell and phone shell).

The tool's own tests: `node --test docs/design/signal/parity/shoot.test.mjs` (pure helpers; not part of the root
`npm test`, there is no pattern for design-tooling tests there).

## Write a spec

A spec is a JSON array of pairs:

```json
{
  "name": "channel-row-active",
  "viewport": { "width": 1280, "height": 800 },
  "prototype": {
    "props": { "startAt": "app" },
    "steps": [{ "click": "text=general" }, { "hover": ".row" }, { "press": "Escape" }, { "type": ["input", "hi"] }, { "wait": 300 }],
    "clip": ".some-selector"
  },
  "story": { "id": "primitives-channelrow--active", "globals": "theme:signal;motion:reduced", "clip": ".d-chrow" }
}
```

- `viewport`: 1280x800 for desktop, 390x844 for phone.
- `prototype.props` are the prototype's component props (`startAt: 'signin' | 'app'`, `device: 'auto' | 'phone'`).
  The prototype reads them from the `data-props` attribute of its `script[data-dc-script]` (`parseDataProps` in
  `support.js`; each entry's `default` becomes `this.props`). The tool rewrites that attribute before the prototype's
  scripts run (a MutationObserver installed with `addInitScript`), so the file on disk is never modified.
- `prototype.steps` run in order after load: one of `click`, `rightClick`, `hover`, `waitFor` (selectors; `waitFor` waits up to 30s for a
  state the prototype reaches on its own clock, such as a speaker), `press` (a key), `type`
  (`[selector, text]`) or `wait` (ms) per step.
- `clip` (either side) crops to the first element matching the selector; without it the whole viewport is shot.
- `story.id` is the Storybook story id (`<title-slug>--<story-slug>`, see `storybook-static/index.json`).
  `story.globals` defaults to `theme:signal;motion:reduced`.
- Both sides run with `prefers-reduced-motion: reduce` at deviceScaleFactor 2, wait for `document.fonts.ready` and 600 ms.
  The prototype's Google Fonts requests are intercepted and answered with `@font-face` rules built from the repo's own
  `@fontsource-variable/jetbrains-mono` and `@fontsource/dm-serif-display` files, so no network is needed and both sides
  use the same font files.
- A `clip` crop gets 8px of padding on every side (both frames), so edges, rings and shadows are visible.

## The rule

A pair matches when **colour, size, spacing, radius, type and copy** agree. The contrast fixes in the plan's Global
Constraints (for example `--edge` instead of the prototype's `#2A303A` on controls whose border is their only boundary,
and `fg-3` text never on `surfaceHi`) are the only accepted deviations. Anything else that differs is a bug in the
story or component, not in the prototype.
