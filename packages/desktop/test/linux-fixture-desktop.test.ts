import {test} from 'node:test';
import assert from 'node:assert/strict';
import {createRequire} from 'node:module';
// @ts-expect-error fixture helper is JavaScript
import { fixtureDesktopMetadata, fixtureLinuxDesktopOptions } from './support/linux-fixture-desktop.mjs';
const require=createRequire(import.meta.url);
test('fixture desktop settings select the production filename through electron-builder',()=>{
 const {LinuxTargetHelper}=require('app-builder-lib/out/targets/LinuxTargetHelper');
 const packager={executableName:'dilla-desktop',platformSpecificBuildOptions:fixtureLinuxDesktopOptions(),info:{metadata:fixtureDesktopMetadata()}};
 assert.equal(new LinuxTargetHelper(packager).getDesktopFileName()+'.desktop','dev.dilla.desktop.desktop');
 assert.equal(fixtureDesktopMetadata().desktopName,require('../package.json').desktopName);
 assert.equal(new LinuxTargetHelper({...packager,platformSpecificBuildOptions:{}}).getDesktopFileName()+'.desktop','dilla-desktop.desktop');
});
