// Storybook only: the CI ui job never builds packages/core-wasm/pkg, and no story runs the real core. Whole-screen stories reach
// client-core's worker runtime (`await import('@dilla/core-wasm')`); if one ever initialises it, it says so.
export default function init(): never {
  throw new Error('core-wasm is not available in Storybook');
}
