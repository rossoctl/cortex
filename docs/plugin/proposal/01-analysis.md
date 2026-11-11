# Analysis: the cg band as it is today

This document describes the band that the `cg` Claude Code plugin (version 0.6.1)
draws above the prompt. It is based on reading the plugin source
(`hooks/register.tsx`, `hooks/model.ts`) and on one live session. See
[the as-is diagram](assets/01-band-as-is.svg).

The plugin is a Claude Code "mod". It lives in the plugin cache of the user's
machine. It is **not** part of this repository. This proposal is about what that
band shows, not about the Cortex `context-guru` pipeline plugin in
[`../../context-guru-plugin.md`](../../context-guru-plugin.md).

## 1. Structure of the band

The band has up to four rows. Row 1 always shows. The other rows show only when
the band has height for them.

| Row | Content | Example |
|---|---|---|
| 1 | Next-message price (warm and cold), Ping button, hide button, dashboard link | `◆ next $0.04 · $0.92 cold [Ping] [hide] dash ↗` |
| 2 | Status row: session saving, cache clock, context fill | `saved $3.65 of $15.52 │ cache ● 11:51 ↻ │ ctx 37%` |
| 3 | One chip per named sub-agent cache | `└ agents ● general-purpose 7m ↻` |
| 4 | Rotating command tip (one command per 12 s), or a weekly tip | `try /cg:insights — see where your money goes` |

The text "From the cg plugin" above the band is added by Claude Code. The mod does not
draw it.

In narrow mode (under 80 columns) the labels are short (`next`, `cold`, `dash`)
and the bars on `cache` and `ctx` are not drawn.

## 2. Data flow

1. The mod never uses the network. It calls `scripts/cg.py`.
2. `cg.py cachestate --json` reads the status line's on-disk cache. Rows 1 to 3 use it.
3. `view()` in `model.ts` turns that document into the values the band draws.
4. The band polls every 15 s. A one-second tick redraws only the clock row.
5. `cg.py ping` is the one write. It runs only when the user presses **Ping**.

## 3. Colors in the cache element

| Condition | Glyph | Color |
|---|---|---|
| A ping was sent, or a keep-alive plan exists (`↻`) | ● | cyan (takes priority) |
| Last 15 s, no ping or plan | ● | red, with `!` after the time |
| Expiring: `left ≤ max(60 s, TTL ÷ 5)` | ◐ | amber |
| Warm | ● | green |
| Cold | ○ | dim; the time counts up |

`ctx` is green up to 50%, amber up to 70%, and red above that. From 85% the wide
layout adds `⚠ rewrite near`.

## 4. What the live data showed

A `cachestate` document read during the session contained these values:

| Field | Value | Meaning |
|---|---|---|
| `ttl_s` | 300 | The cache entry lives 5 minutes after its last use. |
| entry expiry | about 73 s ahead | `max(last_ts, end_ts) + ttl_s − now_ms` |
| `keepalive` | `False` | No ping had been sent. |
| `pings_left` | 2 | Two pings were planned. |
| `next_ping_ms` | 40 s ahead | The first ping was due in 40 s. |
| `held_until_ms` | 610 s ahead (10:10) | The plan expected the cache to stay warm until then. |
| `manual_pings.count` | 0 | The Ping button was not pressed. |
| `window` | 1,000,000 | The model's measured context window. |

A later read in the same session showed `held_until_ms` 813 s ahead while the entry
had 275 s left.

## 5. Why the clock can show more than 5 minutes

This is how the user's screenshot showed `11:51` with a 5-minute TTL.

- `clockOf()` in `model.ts` returns the plan's horizon (`held_until_ms`) whenever the
  entry is still warm and `held_until_ms` is later than the entry's own expiry.
- In the first read above: 40 s to ping 1, then the rest of the plan. The data gives
  only the total after ping 1 (610 − 40 = 570 s). A split of 270 s to ping 2 plus
  300 s of hold fits that total, but it is an inference, not a field.

So the clock is a **forecast** that assumes every planned ping succeeds. It is not
the time the cache is guaranteed to stay warm.

## 6. Problems in the current band

| # | Problem | Evidence |
|---|---|---|
| P1 | The clock is a forecast, but the label and the number look like a deadline. | Section 5. If a ping fails, the cache goes cold at the entry expiry (73 s in the read above). |
| P2 | `↻` and the cyan color appear when a plan **exists**, before any ping is sent. | `pinged = remS > 0 && (isKeepalive \|\| isHeld)` in `cacheClock()`; `keepalive` was `False` in the read above. |
| P3 | The user cannot tell a planned extension (free, potential) from a sent one (paid). | Only a count (`pings_left`) exists in the data; the band shows none of it. |
| P4 | The user cannot see how many extensions ContextGuru offers (X), or how many are used. | Same as P3. |
| P5 | The automatic keep-alive and the manual **Ping** button are shown together without a clear difference. | Row 1 button next to a clock that already counts pings. |
| P6 | The band has no help for context decisions: compact, use a sub-agent, start a new session. | Only `ctx 37%` exists. |
| P7 | Text carries meaning that a shape could carry. | `next message`, `if cold`, state words, tips. |
| P8 | In the cold state the Ping button disappears with no explanation, and both prices become one. | `register.tsx` draws the cold price with the warning color, but a screenshot of the running band shows it white. The two disagree and the cause was not found. |

## 7. Facts that were checked and facts that were not

**Checked in the source or in live data:** the structure in section 1, the data flow,
the color rules, the fields in section 4, and the clock rule in section 5.

**Not checked:**
- Whether the VS Code surface passes the pointer to the band, so that a hover card works.
  The mod API has hover-reveal for boxes. It has no tooltip element.
- What a manual ping adds to the plan, and whether it counts against X.
- Where X is configured, and whether the cache document carries the plan's total.
- Whether the data has what a compact, sub-agent or new-session verdict needs.
- The cost of one ping. The proposal shows `$0.01`, which is the warm price.
