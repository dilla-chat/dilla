export function createTestCertificate(directory: string, nssdb: string): Promise<{ ca: string; cert: string; key: string }>;
