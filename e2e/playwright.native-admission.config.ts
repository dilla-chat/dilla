import {defineConfig} from '@playwright/test';
import voice from './playwright.voice.config.ts';
import {resolve} from 'node:path';
// Dedicated CI invocation: actual desktop client plus existing SFU/test host.
export default defineConfig({...voice,testDir:resolve(import.meta.dirname,'web/support'),testMatch:'native-admission-account.e2e.ts',workers:1,retries:0,use:{...voice.use,trace:'off',screenshot:'off',video:'off'},projects:[{name:'native-admission',use:voice.projects![0].use}],outputDir:'native-admission-results',reporter:[['list']]});
