import { expect, it } from 'vitest';
import { workerInstanceURL } from './instance-url';

it('binds desktop HTTP and gateway configuration to the complete same-origin instance base', () => {
  expect(workerInstanceURL('https://chat.test/nested/', 'https://chat.test')).toBe('https://chat.test/nested');
  expect(workerInstanceURL(undefined, 'https://chat.test')).toBe('https://chat.test');
  expect(workerInstanceURL('https://chat.test/', 'https://chat.test')).toBe('https://chat.test');
  for (const input of ['https://evil.test/nested/', 'https://user:password@chat.test/', 'https://chat.test/?secret=x', 'https://chat.test/#x', 'https://chat.test/nested/%2fother', 'https://chat.test/nested\\other']) {
    expect(() => workerInstanceURL(input, 'https://chat.test')).toThrow();
  }
});
