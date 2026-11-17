# gohelm in Quartermaster

`quartermaster.patch` wires gohelm into Quartermaster's tools layer. It's for the Quartermaster session to review, apply or send back with fixes. Nothing here has been merged.

- Made against `main` at `90aa787`.
- Also applies to `dev` at `cc36e61` with `git apply -C1`, because `AGENTS.md` moved there.

## What it changes

| File | Change |
|---|---|
| `browsertools.go` (new) | Makes the gohelm session and maps its tools to `toolDef`. Adds the card gate (`qmBrowserGate`, modeled on the zip card), the card (`browserAsk`) and the resume (`resumeBrowser`). |
| `browsertools_test.go` (new) | Checks four things: the tools are offered only under Superuser, they're wired into notes, sets and outside tools, the gate asks once and an answer is used once, and the card reads only its own buttons. |
| `tools.go` | Appends `browserTools()` to the catalog. `toolsFor` checks `needs: "browser"`. |
| `toolnotes.go`, `toolsets.go`, `agent.go` (`outsideTools`) | One line each per tool. There are two new sets, `browser` and `desktop`. |
| `agent.go`, `placegate.go`, `zipgate.go`, `audit.go` | The browser card next to the place and zip cards: the marker, the rerun of waiting calls, `resumeGate`, `gateLater` and `outcomeOf`. |
| `access.go`, `ask.go` | The gate keeps the question and the answers. `Ask.Browser` carries the question to the page. |
| `banner.go` | The deployment file's `browser` rules. |
| `features.go` | The `browser` feature, so an administrator can turn on the pane in the main build. |
| `webview_other.go` | `appWindowHandle` off Windows. |
| `AGENTS.md`, `docs/status.md` | The browser's rule, the line about what the program talks to, and the checks still needed at work. |

## How it turns on

- **Superuser (dev):** the Full profile. That's the pane, the outside Edge or Chrome, and other programs. With no `browser` key in the deployment file, it asks about any site on a card.
- **Main build:** off until the deployment file turns on the feature and lists sites. Then it's the pane only, on those sites only:

  ```json
  {
    "features": ["browser"],
    "browser": {
      "allow": ["travel.example.mil", "pay.example.mil"],
      "read_only": ["example.mil"],
      "block": ["social.example.com"]
    }
  }
  ```

- **Other programs (Superuser only):** add `"desktop": {"apps": ["excel.exe"]}` inside `browser`. None are allowed by default.

The pane's profile is `Local\browser`, beside the app window's own `Local\webview`. The outside browser's profile is `Local\browser-outside`. Both stay on the computer and never go to OneDrive.

## Applying

1. On `dev`, run `git apply -C1 quartermaster.patch`. On `main`, run `git apply quartermaster.patch`.
2. The module is public, so `go mod tidy` fetches `v0.1.0` through the Go proxy like any other module.
3. Run `go mod tidy` to add its lines to `go.sum`.

## What was tested

- **Quartermaster `main`:** the full suite with the patch passes (`go test ./...` in 205 s). So do `go vet` and `GOOS=windows go vet`, each also with `-tags personal`, and `TestPersonal`.
- **Quartermaster `dev`:** the full suite with the patch passes too (209 s).
- **gohelm on Linux:** all tests pass with headless Chrome, `-race` included.
- **gohelm on Windows (kdesktop):** all tests pass with Edge started over Windows pipes. `internal/wincheck` passes all 29 checks:
  - the WebView2 pane on public sites;
  - a form post that waits for the card, then posts after the OK;
  - a certificate request reaching the check;
  - the session opening its own pane;
  - the desktop driver on a test window (Value, Toggle and Invoke patterns, the card before Send, a screenshot, and never listing its own windows).

## Open for the Quartermaster session

1. **The card.** The browser card uses the generic question card (`askCard`): title, details, then buttons. A card of its own, showing the site, what's sent and where it goes, would read better and follow DESIGN.md.
2. **Pictures.** `Vision` is off, so there are no screenshot tools. To turn it on, the model request needs to carry an image from a tool result.
3. **Where the pane lives.** It's a separate window owned by Quartermaster's window, so it stays in front of Quartermaster and minimizes with it. It could be docked as a second WebView2 controller inside the app window later.
4. **Settings.** Sites and programs come from the deployment file only. A Superuser page listing them, and the "always" answers (`policy.Consent`), would be the next step.
5. **Sends that ask too often.** Sites that read with POST (GraphQL, some SharePoint calls) ask after clicks until the user picks "Allow for this site". If that's too much, a deployment list of sites whose sends aren't checked is a small addition in gohelm.
6. **Ledger.** The browser isn't tied to a provider, so no `docs/v2/ledger.md` row was added.
