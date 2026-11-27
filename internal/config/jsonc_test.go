package config

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestJSONCAcceptsCommentsAndTrailingCommas(t *testing.T) {
	src := "\xef\xbb\xbf// header\n{\n  /* block\n     comment */ \"a\": 1, // one\n  \"b\": [1, 2, 3,],\n  \"c\": {\"d\": \"x // not a comment\", \"e\": \"/* nor this */\",},\n  \"f\": -1.5e3, \"g\": true, \"h\": null, \"i\": \"\\u00e9\\n\",\n}\n// trailer"
	n, extended, err := parseJSONC([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if !extended {
		t.Fatal("extended syntax not reported")
	}
	var got map[string]any
	if err := json.Unmarshal(n.JSON(), &got); err != nil {
		t.Fatalf("JSON() is not JSON: %v\n%s", err, n.JSON())
	}
	want := `{"a":1,"b":[1,2,3],"c":{"d":"x // not a comment","e":"/* nor this */"},"f":-1.5e3,"g":true,"h":null,"i":"\u00e9\n"}`
	if string(n.JSON()) != want {
		t.Fatalf("JSON()\n got %s\nwant %s", n.JSON(), want)
	}
	if got["i"] != "é\n" {
		t.Fatalf("string decoding: %q", got["i"])
	}
	if a := n.Get("a"); a == nil || a.Pos.Line != 4 || a.Pos.Col != 22 {
		t.Fatalf("position of a: %+v", a)
	}
	if c := n.Get("c").Get("e"); c.Str != "/* nor this */" || c.Pos.Line != 6 {
		t.Fatalf("c.e: %+v", c)
	}
}

func TestJSONCPlainJSONIsNotExtended(t *testing.T) {
	_, extended, err := parseJSONC([]byte(`{"a": [1, {"b": []}], "c": {}}`))
	if err != nil || extended {
		t.Fatalf("err=%v extended=%v", err, extended)
	}
}

func TestJSONCSyntaxErrorsHavePositions(t *testing.T) {
	cases := []struct {
		src       string
		line, col int
		msg       string
	}{
		{"", 1, 1, "empty"},
		{"{\n  \"a\": 1\n  \"b\": 2\n}", 3, 3, "comma missing"},
		{"{\n  \"a\": 'x'\n}", 2, 8, "double quotes"},
		{"{\"a\": tru}", 1, 7, "unknown word"},
		{"{\"a\": 01}", 1, 7, "leading zeros"},
		{"{\"a\": 1.}", 1, 7, "decimal point"},
		{"{\"a\": \"x\n\"}", 1, 7, "end of the line"},
		{"{\"a\": \"\\q\"}", 1, 8, "invalid escape"},
		{"{\"a\": 1 /* open", 1, 9, "not closed"},
		{"{\"a\": 1", 1, 1, "not closed with }"},
		{"[1, 2", 1, 1, "not closed with ]"},
		{"{a: 1}", 1, 2, "quoted member name"},
		{"{\"a\" 1}", 1, 6, "':' was expected"},
		{"{} {}", 1, 4, "after the end"},
		{"{\"é\": 1, x}", 1, 10, "quoted member name"},
		{"{\"a\": [1,,2]}", 1, 10, "value was expected"},
		{"{\"a\": 1} / x", 1, 10, "unexpected '/'"},
	}
	for _, c := range cases {
		_, err := ParseJSONC([]byte(c.src))
		var se *SyntaxError
		if !errors.As(err, &se) {
			t.Errorf("%q: want SyntaxError, got %v", c.src, err)
			continue
		}
		if se.Pos.Line != c.line || se.Pos.Col != c.col || !strings.Contains(se.Msg, c.msg) {
			t.Errorf("%q: got %d:%d %q, want %d:%d containing %q", c.src, se.Pos.Line, se.Pos.Col, se.Msg, c.line, c.col, c.msg)
		}
	}
}

func TestJSONCDepthLimit(t *testing.T) {
	deep := strings.Repeat("[", 1000) + strings.Repeat("]", 1000)
	if _, err := ParseJSONC([]byte(deep)); err == nil || !strings.Contains(err.Error(), "nesting") {
		t.Fatalf("deep nesting: %v", err)
	}
}

// Strict JSON accepted by encoding/json must parse identically.
func TestJSONCAgreesWithEncodingJSON(t *testing.T) {
	for _, s := range []string{
		`{}`, `[]`, `0`, `-0`, `1e10`, `"\ud83d\ude00"`, `{"a":{"b":[null,false,true,"x"]}}`,
		`  {"k" : 1.25E-3 }  `, `"\/"`,
	} {
		n, err := ParseJSONC([]byte(s))
		if err != nil {
			t.Errorf("%s: %v", s, err)
			continue
		}
		var a, b any
		if err := json.Unmarshal([]byte(s), &a); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(n.JSON(), &b); err != nil {
			t.Errorf("%s: JSON() invalid: %v", s, err)
			continue
		}
		ja, _ := json.Marshal(a)
		jb, _ := json.Marshal(b)
		if string(ja) != string(jb) {
			t.Errorf("%s: %s != %s", s, ja, jb)
		}
	}
}

func FuzzParseJSONC(f *testing.F) {
	for _, s := range []string{`{"a":1,}`, "// c\n[1,2]", `{"a":"\u00e9"}`, `/* x */ null`, `{"a":[{"b":-1.5e+3}]}`} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		n, err := ParseJSONC(data)
		if err != nil {
			return
		}
		if !json.Valid(n.JSON()) {
			t.Fatalf("JSON() of accepted input is invalid:\n%q -> %q", data, n.JSON())
		}
	})
}
