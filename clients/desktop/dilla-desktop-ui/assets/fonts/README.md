# Fonts

GPUI's text system loads TrueType/OpenType, not WOFF2, so these are the upstream TTFs of the same releases the web client's
`@fontsource*` packages (5.3.0) carry. Both fonts are licensed under the SIL Open Font License 1.1 (the texts sit beside them).

| File | Family | Source | SHA-256 |
| --- | --- | --- | --- |
| `JetBrainsMono-Regular.ttf` | JetBrains Mono 400 | instance of the variable font below | `35326f87dac7ef89eae27ffae487af3244b8b8fa79847e395d6d808c6084d3dd` |
| `JetBrainsMono-SemiBold.ttf` | JetBrains Mono 600 | instance of the variable font below | `d377e4d9c3e8ec5e9fb2d148de92024b37039e8426f81b375cc2c75d9cb28b9a` |
| `JetBrainsMono-Bold.ttf` | JetBrains Mono 700 | instance of the variable font below | `a97560ba1fe8e05e250360fca01872150118242516194b09ce0d8729f156f426` |
| `DMSerifDisplay-Italic.ttf` | DM Serif Display Italic | https://raw.githubusercontent.com/google/fonts/main/ofl/dmserifdisplay/DMSerifDisplay-Italic.ttf | `df74c0ac387baeaeb0fe4f2324e1668e6a3ed8c09cd9796fe162c71753e19e45` |

The three JetBrains Mono weights are static instances of the variable font
https://github.com/JetBrains/JetBrainsMono/raw/v2.304/fonts/variable/JetBrainsMono%5Bwght%5D.ttf
(SHA-256 `662a196d58f1183bf2d77428b6d5283fe3f45161ab021bea4036bc98e5cac016`), cut with fontTools 4.68.0:

```
fonttools varLib.instancer 'JetBrainsMono[wght].ttf' wght=400 --update-name-table -o JetBrainsMono-Regular.ttf
fonttools varLib.instancer 'JetBrainsMono[wght].ttf' wght=600 --update-name-table -o JetBrainsMono-SemiBold.ttf
fonttools varLib.instancer 'JetBrainsMono[wght].ttf' wght=700 --update-name-table -o JetBrainsMono-Bold.ttf
```

Why static: GPUI 0.3.12 does not select a weight on a variable font; every weight of the variable file drew at its default
(400), so the bold author names and channel titles of the Signal design came out regular (Task 12, measured against the web
reference). An instance is the same outlines at a fixed axis position, so the glyphs match the web's variable font.

Licences: `OFL-JetBrainsMono.txt` and `OFL-DMSerifDisplay.txt` (copied from the fontsource packages' `LICENSE`).

The DM Serif Display URL tracks `main` of google/fonts; the hash above is what was fetched on 2026-11-12.
