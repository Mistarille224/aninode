# Web UI maintenance

The UI is plain HTML, CSS, and JavaScript embedded by the Go server. There is no frontend build step.

| File | Purpose |
|---|---|
| `index.html` | Page entry point |
| `app.js` | Views, forms, API calls, and application state |
| `controls.js` | Shared control helpers |
| `app.css` | Layout, colors, typography, and responsive styles |
| `app.test.mjs` | Frontend tests |

## Layout and wording

Use rows for managed works so titles, seasons, and status can be scanned together. Discovery candidates and migration groups use separate tiles because each has its own action. Settings use labeled form sections.

Show the result and any required action first. Put paths, release names, hashes, and task details behind a disclosure. Error messages should say what failed and what the user can do. Keep implementation terminology out of ordinary labels and help text.

The interface uses Chinese labels and spells the product name `aninode`. Reuse the same filter editor in discovery, migration, and work settings. Observed release traits are available choices, not automatically selected preferences.

## Styling and controls

Use the variables in `app.css` rather than duplicating color values here. Page accents are blue for works, gold for discovery, oxide red for migration, and teal for settings. State and error messages need text labels as well as color.

Reuse existing buttons, form fields, checkboxes, and dialog layouts. Preserve native input semantics, visible focus, and accessible labels. Reserve monospace text for paths and other technical details. Avoid decorative counters, badges, and animation.

On narrow screens, navigation moves above the content and grids reduce their column count. Keep titles and status visible when rows stack or secondary columns disappear. Pages should not require horizontal scrolling.

## Verification

From the repository root:

```sh
npm --prefix internal/server/web test
npm --prefix internal/server/web run check
```

For visual changes, also inspect the affected page and dialogs at desktop and narrow widths. Check keyboard navigation, empty results, loading states, and API errors where relevant. Rebuild the Go binary or container to serve changed embedded assets.
