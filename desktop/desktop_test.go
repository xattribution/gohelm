package desktop

import "testing"

func TestBlockedWindows(t *testing.T) {
	for _, c := range []struct{ app, title string }{
		{"consent.exe", "User Account Control"}, {"CredentialUIBroker.exe", "x"}, {"chrome.exe", "Windows Security"},
		{"acsagent.exe", "ActivClient"}, {"notepad.exe", "Enter your PIN"}, {"x.exe", "Smart Card PIN required"},
	} {
		if !blocked(c.app, c.title) {
			t.Errorf("%s %q should be blocked", c.app, c.title)
		}
	}
	if blocked("excel.exe", "Budget.xlsx - Excel") || blocked("notepad.exe", "Spinning notes") {
		t.Error("ordinary windows blocked")
	}
}

func TestWebRole(t *testing.T) {
	if webRole("edit") != "textbox" || webRole("checkbox") != "checkbox" || webRole("splitbutton") != "button" || webRole("hyperlink") != "link" {
		t.Error("roles")
	}
}
