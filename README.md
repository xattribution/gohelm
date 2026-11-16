# gohelm

gohelm gives an assistant a browser, and optionally other Windows programs, as tools a model can call. It is pure Go: no Node, no Python, no helper programs and no DLLs of its own.

## Start here

1. Add the module: `go get github.com/xattribution/gohelm`
2. Make a session and take its tools:

   ```go
   s, _ := gohelm.New(gohelm.Config{
       Profile:   gohelm.Pane,
       PaneDir:   `C:\Users\me\AppData\Local\MyApp\browser`,
       PaneTitle: "MyApp browser",
       AgentName: "MyApp",
       Rules:     policy.Rules{Allow: []string{"example.mil"}},
   })
   for _, t := range s.Tools() {
       // map t.Name, t.Description, t.Params, t.Required and t.Run to your tool type
   }
   ```

3. Call a tool with a `policy.Env` that holds your Gate (how you ask the user) and your Auditor:

   ```go
   res, err := tool.Run(ctx, policy.Env{Gate: myGate, Audit: myAudit, Owner: chatID}, gohelm.Args(args))
   if res.Pending != nil {
       // show res.Pending as a card; run the same call again after the user answers
   }
   ```

## Profiles

**Pane.** A browser window your program owns (Microsoft Edge WebView2), driven only through WebView2's own DevTools API. No debugging port opens, so an Edge policy that turns off remote debugging doesn't block it. The window has its own profile folder with Windows-account sign-in, password saving and autofill off.

Pane tools: `browser_go`, `browser_snapshot`, `browser_read`, `browser_click`, `browser_fill`, `browser_press`, `browser_dialog`, `browser_hand_over`, and `browser_screenshot` when `Vision` is on.

**Full.** The pane, plus:
- An outside Edge or Chrome that gohelm starts with its own profile, over a pipe (no port).
- Other Windows programs through UI Automation.

Full adds `browser_tabs`, `desktop_windows`, `desktop_snapshot`, `desktop_act`, and `desktop_screenshot` when `Vision` is on.

## What the agent sees

A snapshot lists what can be acted on, one line each, with a ref:

```
Page: Travel request
URL: https://travel.example.mil/new
- heading "Travel request" [ref=e1] [level=1]
- textbox "Full name" [ref=e2]
- combobox "Branch" [ref=e3] [collapsed]: "Choose"
- textbox "Password" [ref=e6]: "(the user types this)"
- button "Continue" [ref=e12]
Left out: 5 pieces of text styled to be invisible.
```

A ref stays the same while its element is on the page. After an action, the result lists what changed instead of the whole page again. `browser_read` gives the page's text as headings, paragraphs, lists and tables.

## Safety

- **Sites.** `policy.Rules` lists the sites the agent may use (`allow`), only read (`read_only`) or never open (`block`). Sites on no list are refused, or asked about when `AskOutside` is on. This computer's own addresses never open, and private addresses and plain http open only when listed.
- **Sends.** Clicks on buttons that send, submit, buy, delete, sign or agree wait for the user's OK. So does anything the page tries to send (any request other than GET) right after an action the user didn't approve. The card shows what was filled in and where it goes.
- **Secrets.** Passwords, PINs, one-time codes, card numbers and ID numbers are never typed. The agent hands those to the user with `browser_hand_over`.
- **Certificates (CAC).** When the page's own site asks for the user's certificate, the host's check decides whether it may ask. If yes, WebView2 shows its own picker and Windows its own PIN prompt. gohelm never chooses a certificate and never sees a PIN.
- **Hidden text.** Text styled to be invisible and frames from other sites are left out of what the model sees. Text that seems to give instructions to an assistant is flagged as data. Both are hints, never a guarantee.
- **Refused outright.** Downloads, file uploads, permission prompts and new windows (pop-ups open in the same window after the same checks).
- **Desktop.** Only programs on the rules' list. The host's own windows, Windows Security, User Account Control, credential and smart card prompts are always off limits. Actions use UI Automation patterns, and fall back to mouse and keyboard only when the rules allow it and the program is in front.
- **Audit.** Every action calls your Auditor before and after. Typed text is never in an audit event.
- **One user at a time.** A session follows one owner (a chat) at a time.

## Testing

- `go test ./...` runs everything that works on any platform. The browser tests drive a headless Chrome or Edge against a local test site of hostile pages, and skip when neither is installed.
- On Windows, `go build -o testapp.exe ./internal/wincheck/testapp` then `go run ./internal/wincheck -testapp testapp.exe` checks the WebView2 pane on public test sites and the desktop driver on a small test window. It opens windows on the screen for about a minute.

## More

- `docs/design.md`: how it's built and why.
- `integration/quartermaster/`: how it plugs into Quartermaster, with a ready patch.
