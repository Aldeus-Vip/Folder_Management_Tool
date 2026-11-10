package server

import "testing"

func TestWebURL(t *testing.T) {
	u, ok := webURL("https://example.sharepoint.com/sites/T/Shared Documents/部門/a b#1.xlsx")
	if !ok || u != "https://example.sharepoint.com/sites/T/Shared%20Documents/%E9%83%A8%E9%96%80/a%20b%231.xlsx" {
		t.Errorf("%s", u)
	}
	if _, ok := webURL(`\\srv\share\a.txt`); ok {
		t.Error("UNC")
	}
}
