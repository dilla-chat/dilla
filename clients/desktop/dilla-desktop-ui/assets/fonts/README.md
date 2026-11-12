# Fonts

GPUI's text system loads TrueType/OpenType, not WOFF2, so these are the upstream TTFs of the same releases the web client's
`@fontsource*` packages (5.3.0) carry. Both fonts are licensed under the SIL Open Font License 1.1 (the texts sit beside them).

| File | Family | Source | SHA-256 |
| --- | --- | --- | --- |
| `JetBrainsMono[wght].ttf` | JetBrains Mono (variable, weight axis) | https://github.com/JetBrains/JetBrainsMono/raw/v2.304/fonts/variable/JetBrainsMono%5Bwght%5D.ttf | `662a196d58f1183bf2d77428b6d5283fe3f45161ab021bea4036bc98e5cac016` |
| `DMSerifDisplay-Italic.ttf` | DM Serif Display Italic | https://raw.githubusercontent.com/google/fonts/main/ofl/dmserifdisplay/DMSerifDisplay-Italic.ttf | `df74c0ac387baeaeb0fe4f2324e1668e6a3ed8c09cd9796fe162c71753e19e45` |

Licences: `OFL-JetBrainsMono.txt` and `OFL-DMSerifDisplay.txt` (copied from the fontsource packages' `LICENSE`).

The DM Serif Display URL tracks `main` of google/fonts; the hash above is what was fetched on 2026-11-12.
