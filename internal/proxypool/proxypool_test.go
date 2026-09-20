package proxypool

import "testing"

func TestValidate(t *testing.T) {
	if err := Validate([]Entry{{Code: "HK01", URL: "socks5://127.0.0.1:11001", Enabled: true}}); err != nil {
		t.Fatalf("valid entry rejected: %v", err)
	}
	if err := Validate([]Entry{{Code: "", URL: "x"}}); err == nil {
		t.Error("empty code should fail")
	}
	if err := Validate([]Entry{{Code: "a b", URL: "x"}}); err == nil {
		t.Error("code with space should fail")
	}
	if err := Validate([]Entry{{Code: "A_1-2", URL: "x"}}); err != nil {
		t.Errorf("allowed charset rejected: %v", err)
	}
	if err := Validate([]Entry{{Code: "HK01", URL: "x"}, {Code: "hk01", URL: "y"}}); err == nil {
		t.Error("case-insensitive duplicate code should fail")
	}
	if err := Validate([]Entry{{Code: "HK01", URL: "  "}}); err == nil {
		t.Error("empty url should fail")
	}
}

func TestLookup(t *testing.T) {
	p := New([]Entry{
		{Code: "HK01", URL: "socks5://127.0.0.1:11001", Enabled: true},
		{Code: "JP02", URL: "http://127.0.0.1:11002", Enabled: false},
	})
	if u, found, en := p.Lookup("hk01"); !found || !en || u != "socks5://127.0.0.1:11001" {
		t.Errorf("Lookup(hk01) = (%q,%v,%v), want (socks5://127.0.0.1:11001,true,true)", u, found, en)
	}
	if _, found, en := p.Lookup("JP02"); !found || en {
		t.Errorf("Lookup(JP02) found=%v enabled=%v, want true,false", found, en)
	}
	if _, found, _ := p.Lookup("nope"); found {
		t.Error("Lookup(nope) should not be found")
	}
	// Replace 后旧代号失效、新代号生效。
	p.Replace([]Entry{{Code: "SG03", URL: "socks5://127.0.0.1:11003", Enabled: true}})
	if _, found, _ := p.Lookup("HK01"); found {
		t.Error("old code should be gone after Replace")
	}
	if u, found, en := p.Lookup("sg03"); !found || !en || u != "socks5://127.0.0.1:11003" {
		t.Errorf("Lookup(sg03) after Replace = (%q,%v,%v)", u, found, en)
	}
}
