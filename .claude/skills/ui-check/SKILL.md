---
name: ui-check
description: Screenshots an lt report page (or the trend page) in light and dark mode at desktop and phone width so the pages can be inspected visually, with a checklist of what to look for. Use after any change to internal/web/assets or to data the pages show.
allowed-tools: Bash(bash .claude/skills/ui-check/scripts/shots.sh*)
---

1. Get a report: the `report.html` printed by `/e2e`, or `bin/lt report <result.json> -o <file>`.
2. Run `bash .claude/skills/ui-check/scripts/shots.sh <report.html> [outdir]`. It renders four PNGs (light/dark ×
   1440px/390px wide, full page) plus a crop of the top and prints their paths. Add `#slowest` to the HTML path
   to open the slowest traced transaction in the modal (`shots.sh "file:///…/report.html#slowest"`).
3. Look at every image with the Read tool. Check:
   - The headline states the outcome; KPI tiles readable; nothing overlaps; no header wraps onto two lines.
   - Hitmap draws dots, error dots red, markers labelled without overlap; selection works conceptually (list area).
   - Every chart has units on the y-axis and a legend for 2+ series; empty states say why they are empty.
   - Hidden sections stay hidden (no empty bars or blank cards).
   - Dark mode: no light boxes, text contrast fine, chart colors are the dark tokens.
   - Phone width: single column, no horizontal page scroll, tables scroll inside their box.
4. Report what you saw with the image paths; fix issues (or hand them to the ui-designer agent) and re-run.
