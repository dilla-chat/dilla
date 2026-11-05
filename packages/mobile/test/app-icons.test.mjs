import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import { existsSync, readFileSync, readdirSync } from 'node:fs';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';

const root = fileURLToPath(new URL('..', import.meta.url));
const read = path => readFileSync(join(root, path));
const md5 = bytes => createHash('md5').update(bytes).digest('hex');

/** Width, height, colour type and whether a tRNS chunk adds transparency. */
const png = path => {
  const bytes = read(path);
  assert.equal(bytes.toString('hex', 0, 8), '89504e470d0a1a0a', `${path} is a PNG`);
  let transparency = false;
  for (let at = 8; at < bytes.length; ) {
    const length = bytes.readUInt32BE(at);
    if (bytes.toString('ascii', at + 4, at + 8) === 'tRNS') transparency = true;
    at += 12 + length;
  }
  return { width: bytes.readUInt32BE(16), height: bytes.readUInt32BE(20), colourType: bytes[25], transparency, bytes };
};

// Capacitor 8.6 template placeholders.
const PLACEHOLDER_IOS_ICON = '0ac741c9e1701ee14dd05ea131f7cfd8';
const PLACEHOLDER_ANDROID_ICON = '9e029293ab1ae8e3a6a7b7d0b7177e46';
const PLACEHOLDER_IOS_SPLASH = '958532b95c3e7d4997327dc977b80600';

const ios = 'ios/App/App/Assets.xcassets';
const res = 'android/app/src/main/res';

test('iOS: the AppIcon is the dilla icon, 1024x1024 with no alpha channel', () => {
  const contents = JSON.parse(read(`${ios}/AppIcon.appiconset/Contents.json`));
  assert.equal(contents.images.length, 1);
  const [image] = contents.images;
  assert.equal(image.size, '1024x1024');
  const icon = png(`${ios}/AppIcon.appiconset/${image.filename}`);
  assert.equal(icon.width, 1024);
  assert.equal(icon.height, 1024);
  assert.equal(icon.colourType, 2, 'RGB without alpha');
  assert.equal(icon.transparency, false);
  for (const file of readdirSync(join(root, ios, 'AppIcon.appiconset')))
    if (file.endsWith('.png')) assert.notEqual(md5(read(`${ios}/AppIcon.appiconset/${file}`)), PLACEHOLDER_IOS_ICON, file);
});

test('iOS: the splash images are the dilla splash, not the Capacitor placeholder', () => {
  const contents = JSON.parse(read(`${ios}/Splash.imageset/Contents.json`));
  assert.equal(contents.images.length, 3);
  for (const { filename } of contents.images) {
    const splash = png(`${ios}/Splash.imageset/${filename}`);
    assert.deepEqual([splash.width, splash.height], [2732, 2732], filename);
    assert.notEqual(md5(splash.bytes), PLACEHOLDER_IOS_SPLASH, filename);
  }
});

test('Android: every mipmap density carries the dilla launcher icons at the right size', () => {
  const densities = { mdpi: 1, hdpi: 1.5, xhdpi: 2, xxhdpi: 3, xxxhdpi: 4 };
  const placeholders = new Set([PLACEHOLDER_ANDROID_ICON, PLACEHOLDER_IOS_ICON]);
  for (const [density, scale] of Object.entries(densities)) {
    const sizes = {
      ic_launcher: 48 * scale,
      ic_launcher_round: 48 * scale,
      ic_launcher_foreground: 108 * scale,
      ic_launcher_background: 108 * scale,
      ic_launcher_monochrome: 108 * scale,
    };
    for (const [name, size] of Object.entries(sizes)) {
      const path = `${res}/mipmap-${density}/${name}.png`;
      const icon = png(path);
      assert.deepEqual([icon.width, icon.height], [size, size], path);
      assert.ok(!placeholders.has(md5(icon.bytes)), `${path} is the Capacitor placeholder`);
    }
  }
});

test('Android: the adaptive icons use the dilla layers, with a monochrome layer for themed icons', () => {
  for (const name of ['ic_launcher', 'ic_launcher_round']) {
    const xml = read(`${res}/mipmap-anydpi-v26/${name}.xml`).toString('utf8');
    assert.match(xml, /<background android:drawable="@mipmap\/ic_launcher_background"\/>/);
    assert.match(xml, /<foreground android:drawable="@mipmap\/ic_launcher_foreground"\/>/);
    assert.match(xml, /<monochrome android:drawable="@mipmap\/ic_launcher_monochrome"\/>/);
  }
  // The template's placeholder vector layers are gone.
  assert.ok(!existsSync(join(root, res, 'drawable/ic_launcher_background.xml')));
  assert.ok(!existsSync(join(root, res, 'drawable-v24/ic_launcher_foreground.xml')));
});

test('Android: every splash drawable is replaced and opaque', () => {
  const dirs = readdirSync(join(root, res)).filter(dir => existsSync(join(root, res, dir, 'splash.png')));
  assert.equal(dirs.length, 11);
  const hashes = new Set();
  for (const dir of dirs) {
    const splash = png(`${res}/${dir}/splash.png`);
    assert.equal(splash.colourType, 2, `${dir}/splash.png is RGB`);
    if (dir.includes('port')) assert.ok(splash.height > splash.width, dir);
    if (dir.includes('land')) assert.ok(splash.width > splash.height, dir);
    hashes.add(md5(splash.bytes));
  }
  for (const old of ['acc976d4a36479233371a53021525c0c', 'b5b1b22b59abe686c1df0d74a423d034', 'b8c72969bc1f78aab71a76035360d4af'])
    assert.ok(!hashes.has(old), `a splash still matches the Capacitor placeholder ${old}`);
});
