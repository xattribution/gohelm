# gohelm design

gohelm gives an assistant a browser and, in its Full profile, other Windows programs, as tools a model can call. It is pure Go, so it can live inside a program like Quartermaster and be covered by that program's own approval.

## Profiles

- **Pane.** A browser window the host owns (Microsoft Edge WebView2), driven only through WebView2's own DevTools API (`ICoreWebView2::CallDevToolsProtocolMethod`). No debugging port opens and nothing outside the program can attach to it. For a locked-down work computer, where the CIS benchmark (and likely the STIG) turns Edge's remote debugging off.
- **Full.** Everything in Pane, plus an outside Edge or Chrome started by gohelm with its own profile over a pipe, and other Windows programs through UI Automation. For development builds and personal use.

## What it borrows, and from where

| Idea | Source | How gohelm uses it |
|---|---|---|
| Accessibility snapshot with refs | Playwright MCP, Vercel agent-browser | The page is a short tree of roles and names. Every element the model can act on gets a ref such as `e12`. |
| Interactive-only snapshot | agent-browser `snapshot -i` | The default snapshot lists only what can be clicked, typed or chosen, plus headings and dialogs. |
| Clickable elements without roles | browser-use (`isClickable`) | Elements with click handlers and no role show as `clickable`, from DOMSnapshot's `isClickable`. |
| Stable refs | (new) | A ref stays the same while its element exists, so it survives small changes. Refs reset when the page navigates. |
| Change lists after actions | (new, for token cost) | After a click or fill, the result lists what appeared, changed or disappeared, instead of the whole page again. |
| Actionability checks | Playwright | Before a click: visible, enabled, scrolled into view, and nothing covering it, tested at several points. A covering element is named with its ref. |
| Origin sets | Chrome agent security (Dec 2025) | The agent only reads and acts on sites the policy or the user allowed. Frames from other sites are left out of what the model sees. |
| User Alignment Critic | Chrome agent security | An optional host check of commit actions that sees the user's task and the action's role and short name, never page text. |
| Takeover mode, confirmations | OpenAI Operator | Secrets are never typed: the agent hands over to the user. Actions that send or change things wait for the user's OK. |
| Hidden-text defenses | Brave's research on Comet | Text styled to be invisible (opacity, transparent color, tiny font, clipped, off the page, never drawn) is left out and counted. |
| Injection hints | Perplexity Comet's classifier layer | A cheap pattern check flags page text that gives instructions to an assistant. It's a hint, never a guarantee. |
| Accessibility tree for desktop apps | Windows-MCP, UFO2 | Programs are read through UI Automation. Actions use its patterns first and the mouse only as an opt-in fallback. |

## Packages

```
gohelm/            Session, profiles and tool definitions (what a host wires up)
  cdp/             a small DevTools Protocol client over any Transport
  cdp/pipeconn     the pipe transport (--remote-debugging-pipe): no port
  browser/         the page driver: snapshot, refs, actions, reading, settling, dialogs, request checks
  policy/          site rules, action classes, the Gate, Critic, Consent and Auditor interfaces
  launch/          finds and starts Edge or Chrome with its own profile (Full)
  pane/            the WebView2 window and its DevTools transport (Windows)
  desktop/         UI Automation, screenshots and opt-in input (Windows, Full)
  internal/        the hostile test site and the Windows checks
```

Every package builds on every platform. Windows-only parts return `ErrUnsupported` elsewhere.

## The transport seam

Everything above `cdp` talks to a `cdp.Transport` (Call, Subscribe, Close).

- **Pane:** `Call` posts to the window's thread and calls `CallDevToolsProtocolMethod`. `Subscribe` registers a `DevToolsProtocolEventReceiver` for that event name. Completion handlers are Go COM objects with one shared function table, so the number of syscall callbacks stays fixed, and they're freed when WebView2 releases them. Nothing waits on the window's thread.
- **Pipe:** a JSON message connection to a browser gohelm started. A tab is attached with `Target.attachToTarget` (flatten), and its Transport stamps the session ID on each message. On Windows the pipes are passed as two inherited handles named by `--remote-debugging-io-pipes`, and inheritance is limited to those two.

So the driver, snapshots, policy and tools are the same code for the pane and the outside browser, and they're tested against a real headless Chrome or Edge.

## Snapshots

1. `Page.getFrameTree` lists the frames. Frames from sites the agent may not see are left out and named. Each allowed frame's tree comes from `Accessibility.getFullAXTree` and is joined under its iframe element (`DOM.getFrameOwner`).
2. `DOMSnapshot.captureSnapshot` with opacity, color, font size, clip-path and clip gives what each node looks like, in one call. Opacity multiplies down the tree; being drawn goes up it. Color and font size hide text only (a container with font-size 0 still shows what's in it).
3. Lines render Playwright style, with states (checked, expanded, disabled, required, focused, selected) and field values. Values of secret fields are masked. A date or time field is one line.
4. Limits: about 6,000 characters, with `query`, `scope` (a ref) and `offset`.

## Actions

| Action | How |
|---|---|
| click | re-read the element's role and name, scroll it into view, find a point on its boxes that reaches it (`DOM.getNodeForLocation`, checked in gohelm's isolated world), then mouse moved, pressed, released |
| fill | text: focus, select all, `Input.insertText` (newlines become spaces in single-line fields); lists: set the option by label or value and fire `input` and `change`; checkboxes: click only when the state differs; dates and times: set the value with the element's own setter |
| press | an allowed list of keys; Enter or Space on a submit button counts as a commit |
| dialogs | `Page.handleJavaScriptDialog`; accepting a confirm counts as a commit |

Input that opens a dialog returns as soon as the dialog opens (a click that opens a confirm doesn't hang). Scripts gohelm runs in the page run in its own isolated world, where the page can't see or change them.

**Settling.** After an action, gohelm waits for the navigation it started (up to 10 s), then for 300 ms with no DOM changes, counted by a MutationObserver in gohelm's isolated world (up to 2.5 s). A page that never goes quiet is reported as still loading.

**User activity.** gohelm's world notes the time of real input. If the user used the page since the agent's last step, the next action stops and says so.

## Safety model

**Sites.** `allow` (read and act), `read_only` (read and follow links, never type or submit) and `block` (never). Patterns follow Edge's URL policies: `example.mil` covers its subdomains, `.example.mil` the host only. Sites on no list are refused, or asked about once when `AskOutside` is on. Always refused: this computer's own addresses, any scheme but https and http (`javascript:`, `data:`, `file:`, `blob:`, `edge:`, external protocols), addresses with user names, and plain http, private addresses and single-label hosts unless listed.

Checks happen at three levels:
1. Before the agent navigates.
2. On every document request (`Fetch` interception), so a click's, a redirect's and a frame's navigation are checked too.
3. After a navigation lands (`Page.frameNavigated`), which leaves a page that ended up somewhere refused.

The pane also checks at WebView2's own `NavigationStarting` and `FrameNavigationStarting`, whoever navigates. A site the user opens by hand counts as allowed for reading in that session.

**Action classes.**
- `read`: snapshots, reading, scrolling, waiting.
- `input`: typing, choosing, checking.
- `commit`: a submit button, a button or link whose name says send, submit, post, pay, buy, order, delete, remove, sign, approve, confirm, transfer, publish, apply, book, accept or agree; Enter or Space on one; accepting a confirm dialog.
- `secret`: typing into a password, PIN, one-time code, card or ID number field, found by type, autocomplete, name, id, label and placeholder. Never done.

With the default `confirm: "commit"`, commits wait for the user. `confirm: "input"` asks for input too. `card_sites_confirm` sets the level on sites that asked for the user's certificate.

**Sends after actions.** The name list is a first line only. While an action runs and for 1.5 s after it, any request other than GET, HEAD or OPTIONS (form posts, fetch and XHR) is held and asked about, unless the user approved that action as a commit or allowed sends on that site. Beacons sent during an action are dropped without asking. The card names where it goes and what the agent filled in. An approval covers the one action it was given for.

**The Gate** is the host's `Decide(ctx, Ask) (Answer, error)`. It can block until the user answers, or return `Pending` after showing a card, and the call runs again after the click. Each Ask carries a stable key built from what it's about (site, element, form destination, filled values), so the rerun finds its answer, and one answer is used once. Answers: allow, allow for this site, deny, done (hand-over), pending, cannot ask (scheduled runs, helpers).

**Certificates (CAC).** In the pane, WebView2's `ClientCertificateRequested` asks whether the site may ask. Only the page's own site may. If yes, WebView2 shows its own picker and Windows its own PIN prompt, which is the user's own yes.

**Desktop limits (Full).**
- Only programs on `desktop.apps`. The host's own process, and processes it names, are off limits, so the agent can never click its own approval buttons.
- Windows Security, User Account Control, credential and smart card windows are off limits by process and title.
- The first use of each program asks the user.
- Nothing is done while the host says a card is open (`Busy`).
- Mouse and keyboard only with `raw_input`, only when the program is in front, and typing stops if the user switches windows.
- Window titles of programs not on the list are never listed.

**Audit.** Every action calls the Auditor with an intent event before and a result event after. Typed text is never in an event.

**Data, not instructions.** Every result that carries page or window content is marked `Outside`, so the host fences it as data.

## Review

Fable reviewed the first design. These changes came from that review:
- Sends are gated by effect, not only by button names.
- The pane has its own profile with Windows sign-in off, and this computer's addresses always refuse.
- Keys are an allowed list.
- Cards show values and destinations, and their keys bind to them.
- Settling uses an isolated world and short caps.
- The tool list was cut to 9 for the pane.
- The WebSocket transport, upload and page scripts were cut.

## Not in this version

- Cross-site frames in the pane: WebView2's per-frame sessions need a newer interface, so they're left out and named.
- Pop-up windows open in the same pane, so sign-ins that need `window.opener` may not finish there.
- Downloads and uploads.
- Vision-only clicking at coordinates.
