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

it('binds the mobile app (capacitor://localhost, https://localhost) to a cross-origin HTTPS instance, and nothing else crosses origins', () => {
  for (const app of ['capacitor://localhost', 'https://localhost']) {
    expect(workerInstanceURL('https://chat.example/', app)).toBe('https://chat.example');
    expect(workerInstanceURL('https://10.0.2.2:18443/', app)).toBe('https://10.0.2.2:18443');
    for (const input of ['http://chat.example/', 'wss://chat.example/', 'https://u:p@chat.example/', 'https://chat.example/?q', 'https://chat.example/assets/', 'capacitor://chat.example/']) {
      expect(() => workerInstanceURL(input, app)).toThrow();
    }
  }
  // Any other worker origin stays same-origin only: a browser or desktop page cannot point its core elsewhere.
  for (const origin of ['https://chat.test', 'http://localhost', 'https://localhost:8443', 'capacitor://evil.test']) {
    expect(() => workerInstanceURL('https://chat.example/', origin)).toThrow();
  }
});
