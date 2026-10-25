# duod brand guidelines

## Essence
duod = two slots, one daemon. Two colors of every service (blue, green), one operator that flips traffic safely. The brand shows that duality: blue and green, always paired.

## Personality
Calm, precise, practical. Sysadmin-friendly. No hype.

## Logo
- **Mark**: two stacked containers (blue top, green bottom) inside a swap-arrow loop, on a navy rounded square.
- **Logo**: mark + lowercase wordmark `duod`. Files: `assets/duod-logo.svg` (light bg), `assets/duod-logo-dark.svg` (dark bg).
- Always lowercase `duod`. Never "Duod" or "DUOD", except at sentence start where lowercase still wins.
- Clear space: half the mark height on all sides. Minimum mark size 16 px (use `favicon.svg`), minimum logo width 96 px.
- Do not recolor, rotate, add shadows, stretch, or put on busy photos.

## Color
| Name | Hex | Use |
|---|---|---|
| Navy | `#0B2545` | Brand background, wordmark on light |
| Blue | `#1D8CF8` | "Blue" slot, links, primary accent |
| Green | `#20C38B` | "Green" slot, success, live state |
| Mist | `#B8C6DB` | Secondary text on navy |
| Slate | `#8FA3BF` | Tertiary text on navy |
| Ink | `#1B2433` | Body text on light |
| Paper | `#FFFFFF` | Light background |

Rule: blue and green appear together or in meaningful sequence (old → new). Never use green for errors. Docs site uses darker variants for light-mode contrast (`#1566c4`, `#0c8a5f`).

## Typography
- Headings and UI: system sans (Inter if available, else system-ui), bold, tight tracking (-0.02em).
- Code, commands, URLs: monospace (JetBrains Mono, else ui-monospace).
- Sentence case everywhere. No title case headlines.

## Imagery
Flat vector, rounded corners (radius ≈ 23% of size), soft blue/green glows on navy. Diagrams: two lanes, blue and green, one arrow showing the switch.

## Voice
| Do | Don't |
|---|---|
| "Roll out. Probe. Switch." | "Revolutionary deployment platform" |
| Say what it does: GitOps blue/green for Docker Compose | Buzzwords: seamless, next-gen, cloud-native magic |
| Compare honestly to Argo CD / Argo Rollouts | Disparage Kubernetes |
| Commands and numbers | Vague claims |

## Social preview checklist
- 1280x640 for GitHub, 1200x630 for Open Graph / Twitter, 1080x1080 square.
- Keep text inside a 84 px safe margin.
- Logo top-left, headline, one-line proof, URL bottom-right.
