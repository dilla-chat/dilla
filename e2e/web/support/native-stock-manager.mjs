// Only attach to the stock manager in this browser's sole owned context.
export function stockManagerPage(browser) {
 const contexts = browser.contexts();
 if (contexts.length !== 1) throw new Error('Expected one owned browser context');
 const context = contexts[0];
 const pages = context.pages().filter(page => page.url() === 'dilla-internal://setup/');
 if (pages.length > 1) throw new Error('Ambiguous stock manager');
 return pages[0];
}
