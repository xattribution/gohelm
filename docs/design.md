# gohelm design

gohelm gives an assistant a browser and, optionally, the Windows desktop, as a set of tools a model can call. It is pure Go with no helper programs, no Node, no Python and no DLLs, so it can live inside a program like Quartermaster and be covered by that program's own approval.

It has two profiles:

- **Pane.** A browser window the host program owns (Microsoft Edge WebView2), driven only through WebView2's own DevTools API. No debugging port is opened and nothing outside the program can attach to it. This is the profile for a locked-down work computer.
- **Full.** Everything in Pane, plus an outside Edge or Chrome started by gohelm with its own profile, and control of other Windows apps through UI Automation, the mouse and the keyboard. This is for development builds and personal use.

## What it borrows, and from where

| Idea | Source | How gohelm uses it |
|---|---|---|
| Accessibility snapshot with refs | Playwright MCP, Vercel agent-browser | The page is described as a short tree of roles and names, and every element the model can act on gets a ref such as `e12`. |
| Interactive-only snapshot | agent-browser `snapshot -i` | The default snapshot lists only what can be clicked, typed or chosen, plus headings for context. |
| Stable refs | (new) | A ref stays the same while its element exists, so the model can keep using it after small changes. Refs reset when the page navigates. |
| Change lists after actions | (new, for token cost) | After a click or a typed value, the result lists what appeared or disappeared instead of the whole page again. |
| Actionability checks | Playwright | Before a click: the element is visible, enabled, in view, and nothing covers it. If something covers it, the result names it (for example a cookie banner). |
| Origin sets | Chrome agent security (Dec 2025) | The agent may only read and act on sites the policy or the user allowed. Frames from other sites are left out of what the model sees. |
| User Alignment Critic | Chrome agent security | An optional second check, supplied by the host, sees only the user's task and the proposed action (never page text) and can veto it. |
| Takeover mode, confirmations, watch mode | OpenAI Operator | Passwords, PINs and payment fields are never typed by the agent; it hands over to the user. Actions that send, submit, buy, delete or sign wait for the user's OK. |
| Hidden-text defenses | Brave's Comet research | Text styled to be invisible (transparent, tiny, off-screen, same color as its background) is left out of snapshots and reading, and counted. |
| Injection hints | Perplexity Comet's classifier layer | A cheap pattern check flags page text that talks to an AI assistant. The result says so. It's a hint, never a guarantee. |
| Accessibility tree for desktop apps | Windows-MCP, UFO2 | Desktop apps are read through UI Automation. Actions use UI Automation patterns (Invoke, Toggle, Value, Select, Expand) first and the mouse only as a fallback. |

## Packages

```
gohelm/            Session, tool profiles and tool definitions (the part a host wires up)
  cdp/             a small DevTools Protocol client over any Transport
  cdp/wsconn       WebSocket transport (RFC 6455 client, no dependencies)
  cdp/pipeconn     pipe transport (--remote-debugging-pipe), no port at all
  browser/         the page driver: snapshot, refs, actions, reading, waiting, dialogs
  policy/          site rules, consent, action classes, the Gate and Critic interfaces, audit
  launch/          finds and starts Edge or Chrome with its own profile (Full only)
  pane/            the WebView2 window and its DevTools transport (Windows only)
  desktop/         UI Automation, screenshots and input (Windows only, Full only)
```

Each package builds on every platform. Windows-only parts return `ErrUnsupported` elsewhere, so the host's cross-platform build and tests stay simple.

## The transport seam

Everything above `cdp` talks to a `cdp.Transport`:

```go
type Transport interface {
    Call(ctx context.Context, method string, params any) (json.RawMessage, error)
    Subscribe(event string, fn func(json.RawMessage)) (cancel func())
    Close() error
}
```

- **Pane:** `Call` posts to the window's thread and uses `ICoreWebView2::CallDevToolsProtocolMethod`. `Subscribe` registers a `DevToolsProtocolEventReceiver` for that one event name.
- **WebSocket and pipe:** a JSON message connection to a browser. A page session is attached with `Target.attachToTarget` (flatten), and its `Transport` stamps the session ID on every message.

So the driver, snapshot, policy and tools are the same code for the pane and the outside browser, and they are tested against a real headless Chrome in CI.

## Snapshots and refs

1. `Accessibility.getFullAXTree` gives the tree with each node's DOM `backendDOMNodeId`.
2. `DOMSnapshot.captureSnapshot` with a few computed styles (opacity, color, background color, font size, visibility) and layout boxes gives what each node looks like, in one call.
3. Text nodes that are invisible to a person are dropped (opacity 0 along the ancestor chain, alpha 0 color, font under 6 px, zero area, entirely off the page, or contrast against the nearest background under 1.2:1). The snapshot ends with how many were left out.
4. Frames from sites outside the allowed set are left out and named only by their site.
5. Nodes render one per line, Playwright style: `- button "Send" [ref=e7] [disabled]`. States shown: checked, selected, expanded, disabled, required, value of text boxes (masked for password fields), heading level, and the link's address when asked.
6. A ref maps to the backend node ID. The map lives until the page navigates; a ref whose node is gone gives "That element is gone; take a new snapshot".

Default limits: interactive-only, about 6,000 characters, with `scope` (a ref) to look inside one part and `offset` to page.

## Actions

| Action | How |
|---|---|
| click | scroll into view, find its box (`DOM.getContentQuads`), check what's at the center (`DOM.getNodeForLocation`) is the element or inside it, then mouse moved, pressed, released |
| type | focus, select what's there, `Input.insertText`, optional Enter |
| select | set the option by its label or value, then fire `input` and `change` |
| check / uncheck | click only when the state differs |
| press | key events for named keys and combinations (Control+A, Shift+Tab) |
| scroll | mouse wheel at the element or the page center |
| hover | mouse moved |
| upload (Full) | `DOM.setFileInputFiles`, after the Gate |
| script (Full) | `Runtime.evaluate`, only when the policy allows it, after the Gate |

After each action gohelm waits for the page to settle: the navigation it started, `document.readyState`, and a short quiet period with no DOM changes (capped at 8 seconds). Then it reports the address, the title, any open dialog, and the change list.

## Safety model

**Sites.** The policy has three lists of origin patterns: `allow` (read and act), `read_only` (read, never act) and `block` (never). Anything else asks the user once through the Gate: allow once, always allow this site, or no. Navigation is checked before it happens: the agent's own, a click's, a redirect's and a frame's (`Fetch` interception of documents in the outside browser; `NavigationStarting` and `FrameNavigationStarting` in the pane). A site the user opens by hand in the pane counts as allowed for reading in that session.

**Action classes.** Each action gets a class before it runs:

- `read`: snapshots, reading, scrolling, waiting, hover.
- `input`: typing into a field, choosing an option, checking a box.
- `commit`: anything that sends or changes something outside the page: a click on a submit button, or on a button or link whose name says send, submit, post, pay, buy, order, delete, remove, sign, approve, confirm, transfer, publish, apply or book; Enter in a form field; accepting a confirm dialog; upload; download.
- `secret`: typing into a password, PIN, one-time code, card number or security code field. Never done by the agent. It hands over to the user instead.

With the default `confirm: "commit"`, `commit` actions go to the Gate with a plain sentence ("Click "Submit request" on travel.example.mil"). `confirm: "input"` asks for `input` too; `confirm: "never"` asks for none (Full, personal use only).

**The Gate** is the host's: `Decide(ctx, Ask) (Answer, error)`. It can block until the user answers, or return `ErrPending` after showing a card, so a host that stops a turn at a card and runs the call again after the click works too. Each `Ask` carries a stable key, so the retried call finds its answer.

**Sign-in with a card (CAC).** In the pane, WebView2's `ClientCertificateRequested` asks the Gate whether this site may ask for the user's certificate. If yes, WebView2 shows its own certificate picker and Windows shows its own PIN prompt. gohelm never chooses a certificate, never sees the PIN and never types it. In the desktop driver, the Windows Security, credential and smart card windows are off limits.

**The Critic** (optional) gets the user's task as the host states it and the proposed action's metadata (class, role, name, site), never page text. It can veto. The host decides which model runs it.

**Desktop limits (Full).** The host's own windows are off limits, so the agent can never click its own approval buttons. So are UAC, credential and smart card prompts, and any process on the policy's block list. Mouse and keyboard input are used only when the target window is in front, and stop if the user switches away.

**Audit.** Every action produces an `AuditEvent` (time, tool, action, class, site, target role and name, decision, outcome). Typed text is never logged, only its length.

**Data, not instructions.** Every result that carries page or window content is marked untrusted with its origin, so the host fences it (Quartermaster's `<data>` tags) and its memory rules apply.

## Tools

Pane profile (names as the model sees them):

`browser_open`, `browser_snapshot`, `browser_read`, `browser_find`, `browser_click`, `browser_type`, `browser_select`, `browser_check`, `browser_press`, `browser_scroll`, `browser_history`, `browser_wait`, `browser_dialog`, `browser_screenshot`, `browser_hand_over`, `browser_close`.

Full adds: `browser_tabs`, `browser_hover`, `browser_upload`, `browser_script`, `desktop_windows`, `desktop_snapshot`, `desktop_click`, `desktop_type`, `desktop_press`, `desktop_scroll`, `desktop_screenshot`.

Each tool is a plain struct (name, label, description, JSON Schema parameters, read-only flag, action class, run function), so a host maps it to its own tool type in a few lines. `integration/quartermaster/` shows that for Quartermaster.

## Not in this version

- Cross-site frames in the pane (WebView2's per-frame sessions need a newer interface). They're left out and named.
- Pop-up windows open in the same pane, so sign-ins that need `window.opener` may not finish in the pane.
- Vision-only clicking (coordinates from a screenshot) is in the desktop driver, Full only; the browser always acts by ref.
