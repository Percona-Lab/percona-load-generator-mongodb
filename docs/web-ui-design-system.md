# Web UI design system

The Web UI is one server-rendered page, `internal/webui/static/index.html`, embedded in the Go binary. It uses plain HTML, CSS, and JavaScript with no build step and no runtime CDN (fonts use the system stack; `chart.js` is bundled next to the page). This file lists the conventions the stylesheet follows so new screens stay consistent.

[Back to the Web UI guide](./web-ui.md)

## Why no component library

PLGM is not a React app, so shadcn/ui does not apply, and adding Bootstrap would put a second set of base styles on top of about 2,000 lines of existing CSS and the dashboard's own components. Instead, the page keeps one token-based stylesheet. Components use only the tokens below; page-specific rules should be the exception.

## Tokens (`:root`)

| Group | Tokens |
| :--- | :--- |
| Surfaces | `--bg`, `--surface`, `--surface-muted`, `--surface-sunken`, `--border`, `--border-strong` |
| Text | `--text-strong` (headings), `--text`, `--text-secondary` (help, captions), `--text-muted` |
| Interaction | `--accent`, `--accent-hover`, `--accent-soft`, `--accent-border`, `--focus`, `--focus-ring` |
| Status | `--ok*`, `--warn*`, `--danger*` (text, `-soft` background, `-border`) |
| Type | `--font-sans`, `--font-mono`, `--fs-xs` 11 · `--fs-sm` 12 · `--fs-base` 13 · `--fs-md` 14 · `--fs-lg` 16 · `--fs-xl` 20 · `--fs-2xl` 24 |
| Space and shape | `--space-1`…`--space-6` (4, 8, 12, 16, 24, 32 px), `--radius-sm/md/lg` (6, 8, 12), `--control-h` 34, `--control-h-sm` 28, `--elevation-1/2/3` |

Heading hierarchy: page title `--fs-2xl` (start page) or `--fs-xl` (settings, completed runs); card title `--fs-md` 600; subsection `--fs-base` 600; field label `--fs-base` 500.

## Field widths

Inputs are sized by the values they accept, never by the current value, so nothing moves while you type. Put the class on the control (or a wrapper):

| Class | Width | Use for |
| :--- | :--- | :--- |
| `f-xs` | 6.5rem | counts, short integers, percentages (Runs to keep, pool sizes, mix %) |
| `f-sm` | 9.5rem | durations, rates, thresholds, large counts, byte sizes |
| `f-md` | 16rem | names, usernames, labels, 64-bit seeds |
| `f-lg` | 26rem | descriptions, long selects, paths in compact toolbars |
| `f-xl` | 46rem | URIs, connection strings, server paths |
| `f-full` | 100% | text areas (queries, JSON, scripts) |

Inside a settings card, unclassed number inputs default to `f-sm`, text inputs to `f-md`, selects to their longest option, and text areas to full width. Every control keeps `max-width: 100%`, so nothing overflows on a narrow screen.

Settings fields flow in rows (`.grid-2`/`.grid-3` inside a `.settings-card`, or `.field-row`): each `.form-group` is as wide as its label or control, so related short fields sit side by side and a URI takes its own line. Use `.field-stack` for one field per line, and `.field-check` for a checkbox that sits beside labelled fields.

## Label and help

Write a field as a label followed by its control and, optionally, a `.settings-note` or `.settings-help`. On load, `installHelpTips()` collapses notes of 80 or more characters behind a **?** button and produces:

```html
<div class="label-help"><label for="x">Runs to keep</label><button class="help-tip-btn" aria-label="Help: Runs to keep">?</button></div>
```

The label is inline with end padding that the button overlaps, so the button always stays on the same line as the label's last word, including when the label wraps. The button is outside the `<label>`, so the control's accessible name is not affected. The note stays in place in the DOM (hidden) and moves into a floating panel while open. Hover, focus, click, or tap opens it; Escape or a click outside closes it.

A card or subsection description (a `.settings-help` right after its heading) always goes behind the heading's **?**, whatever its length, so every heading looks the same. A radio option can carry its own **?**: wrap the option's `<label>` in `.label-help` and put its note right after that wrapper (see **Run on**).

Other notes shorter than 80 characters, `role="alert"`/`role="status"` messages, validation errors, and warnings that change what the run does stay visible inline. Labels written without `for=` are tied to the following control by `associateLabels()`.

## Settings card layout

Every settings card has the same shape: the title (with its level tag and **?**), then the fields 12px below. There's no visible lead text under the title. A 1px divider (`.settings-subhead`, `.det-servers`) only opens a subsection inside a card, and never sits right under the card title or above a button row. Section tags such as "Any run" are hidden once a task is chosen.

## Components

| Component | Classes | Notes |
| :--- | :--- | :--- |
| Buttons | `.btn` + `.btn-primary` / `.btn-secondary` / `.btn-flat` / `.btn-danger`, sizes `.btn-small`, `.btn-lg` | One primary action per area. `aria-busy="true"` shows a spinner and disables. |
| Small toolbar buttons | `.insights-explore-btn`, `.insights-drawer-nav`, `.btn-live` | Same look as `.btn-secondary.btn-small`. |
| Filter chips | `.toggle-chip` (`.active`) | Accent-soft when on. |
| Cards | `.settings-card`, `.mat-card`, `.insights-section-card` | `--radius-lg`, 1px border, `--elevation-1`. |
| Task cards | `.mode-card` in `.mode-grid` (`role="radiogroup"`), `.start-history` | Six equal cells (3x2, 2x3, 1x6). Compact switcher once a mode is chosen. |
| Settings level | `.tier-bar`, `.tier-option` | Basic is always on; Advanced and Expert toggle (`aria-expanded`). Advanced and expert cards carry a level marker. |
| Settings categories | `.settings-body.has-nav` > `.settings-nav` (`role="tablist"`, vertical) + `#settingsPanels` (`role="tabpanel"`) | Shown once Advanced or Expert adds settings. Cards carry `data-group`; Basic cards share `basic`. `.settings-nav-error` dot marks a category with a validation error (never a count). Wraps into a row under 900px. |
| Field errors | `.field-error`, `aria-invalid`, `aria-describedby` | Placed under the control by `validateConfigForm()`; removed when the control changes. `revealSetting(el)` opens the level and category holding a field (also used by `.goto-link` dependency links). |
| Launch bar | `.launch-bar` > `.footer` | Sticky at the bottom of the settings; holds the run summary, inline errors, and **Launch Workload**. |
| Tables | `.run-history-table`, `.eff-table`, `.latency-table` | Muted header row, uppercase 11px column names, hover row. Stack into labelled rows under 700px where needed. |
| Status | `.status-badge` (header), `.status-chip` (lists), `.insight-badge` (findings) | Status is never shown by color alone: each has text. |
| Notices | `.settings-inline-error`, `.history-banner`, `.history-gap`, `.mode-lock` | Danger, accent, and warning surfaces. |

## States

- Focus: every interactive element shows a 2px `--focus` outline on keyboard focus; inputs also show `--focus-ring`.
- Hover: borders move one step darker; cards and buttons tint toward `--accent-soft` or `--surface-muted`.
- Disabled: 55% opacity and `not-allowed` cursor; disabled inputs use `--surface-sunken`.
- Invalid: `:user-invalid`, `aria-invalid="true"`, or `.is-invalid` draw a danger border and ring.
- Hidden: the `hidden` attribute always wins (`[hidden] { display: none !important }`).

## Checks

`make test-ui` covers the opening page, settings categories and cross-category validation (`settings_categories.test.mjs`), help placement and keyboard use, settings levels, the absence of changed-count indicators, and field-width classes (`internal/webui/uitest/setup_modes.test.mjs`).
