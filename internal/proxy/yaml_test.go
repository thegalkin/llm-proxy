package proxy

import (
	"strings"
	"testing"
)

// --- ParseYAML ---

func TestParseYAML_HappyPath(t *testing.T) {
	src := `
listen_addr: 127.0.0.1:8443
default_to: minimax sub/minimax*
rules:
  - from: GO/*
    to: opencode-go/*
`
	m, err := ParseYAML(src)
	if err != nil {
		t.Fatalf("ParseYAML: %v", err)
	}
	if m["listen_addr"] != "127.0.0.1:8443" {
		t.Errorf("listen_addr = %v", m["listen_addr"])
	}
	if m["default_to"] != "minimax sub/minimax*" {
		t.Errorf("default_to = %v", m["default_to"])
	}
}

func TestParseYAML_Empty(t *testing.T) {
	m, err := ParseYAML("")
	if err != nil {
		t.Fatalf("ParseYAML: %v", err)
	}
	if len(m) != 0 {
		t.Fatalf("expected empty root, got %v", m)
	}
}

func TestParseYAML_Indentation(t *testing.T) {
	src := "outer:\n  inner: value\n  more: 42\n"
	m, err := ParseYAML(src)
	if err != nil {
		t.Fatalf("ParseYAML: %v", err)
	}
	outer, ok := m["outer"].(map[string]any)
	if !ok {
		t.Fatalf("outer not a map: %T", m["outer"])
	}
	if outer["inner"] != "value" {
		t.Errorf("inner = %v", outer["inner"])
	}
	if v, ok := outer["more"].(int64); !ok || v != 42 {
		t.Errorf("more = %v (%T)", outer["more"], outer["more"])
	}
}

func TestParseYAML_Comments(t *testing.T) {
	src := "# leading\nkey: value  # trailing\n"
	m, err := ParseYAML(src)
	if err != nil {
		t.Fatalf("ParseYAML: %v", err)
	}
	if m["key"] != "value" {
		t.Errorf("key = %v", m["key"])
	}
}

func TestParseYAML_UnexpectedIndent(t *testing.T) {
	// A sibling key with deeper indent than its predecessor produces the
	// "unexpected indent" branch in parseYAMLLevel.
	src := "outer:\n  inner: 1\n  bad: 2\n   sibling_too_deep: x\n"
	if _, err := ParseYAML(src); err == nil {
		t.Fatalf("expected error on bad indent")
	}
}

func TestParseYAML_MissingColon(t *testing.T) {
	src := "no_colon_here\n"
	if _, err := ParseYAML(src); err == nil {
		t.Fatalf("expected error on missing colon")
	}
}

func TestParseYAML_BlockScalarRejected(t *testing.T) {
	src := "key: |\n  block\n"
	if _, err := ParseYAML(src); err == nil {
		t.Fatalf("expected error on block scalar")
	}
}

func TestParseYAML_SequenceRoot(t *testing.T) {
	src := "- one\n- two\n"
	if _, err := ParseYAML(src); err == nil {
		t.Fatalf("expected error for sequence root")
	}
}

// --- prepYAMLLines ---

func TestPrepYAMLLines(t *testing.T) {
	lines := prepYAMLLines("a\nb   \nc")
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want 3", len(lines))
	}
	if lines[1] != "b" {
		t.Fatalf("trailing whitespace not trimmed: %q", lines[1])
	}
}

func TestPrepYAMLLines_NoNewlines(t *testing.T) {
	lines := prepYAMLLines("just-one-line")
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1", len(lines))
	}
	if !strings.HasSuffix(lines[0], "just-one-line") {
		t.Errorf("lines = %v", lines)
	}
}

// --- stripYAMLComment ---

func TestStripYAMLComment_Outside(t *testing.T) {
	got := stripYAMLComment("key: value # comment")
	if got != "key: value " {
		t.Fatalf("got %q", got)
	}
}

func TestStripYAMLComment_InSingleQuotes(t *testing.T) {
	got := stripYAMLComment(`key: 'a # b' # real comment`)
	if got != `key: 'a # b' ` {
		t.Fatalf("got %q", got)
	}
}

func TestStripYAMLComment_InDoubleQuotes(t *testing.T) {
	got := stripYAMLComment(`key: "a # b" # real comment`)
	if got != `key: "a # b" ` {
		t.Fatalf("got %q", got)
	}
}

func TestStripYAMLComment_NoSpace(t *testing.T) {
	got := stripYAMLComment("key:value#nope")
	if got != "key:value#nope" {
		t.Fatalf("got %q", got)
	}
}

func TestStripYAMLComment_HashAtStart(t *testing.T) {
	got := stripYAMLComment("# whole line comment")
	if got != "" {
		t.Fatalf("got %q", got)
	}
}

func TestStripYAMLComment_HashInsideDoubleQuotesNotComment(t *testing.T) {
	got := stripYAMLComment(`"abc#def"`)
	if got != `"abc#def"` {
		t.Fatalf("got %q", got)
	}
}

func TestStripYAMLComment_HashInsideSingleQuotesNotComment(t *testing.T) {
	got := stripYAMLComment(`'abc#def'`)
	if got != `'abc#def'` {
		t.Fatalf("got %q", got)
	}
}

// --- indentOf ---

func TestIndentOf_Empty(t *testing.T) {
	if got := indentOf(""); got != 0 {
		t.Fatalf("got %d", got)
	}
}

func TestIndentOf_OnlySpaces(t *testing.T) {
	if got := indentOf("    "); got != 4 {
		t.Fatalf("got %d", got)
	}
}

func TestIndentOf_Mixed(t *testing.T) {
	if got := indentOf("  hello"); got != 2 {
		t.Fatalf("got %d", got)
	}
	if got := indentOf("\tworld"); got != 1 {
		t.Fatalf("got %d (tab counts as 1)", got)
	}
}

// --- parseYAMLLevel (indirectly, via ParseYAML) ---

func TestParseYAMLLevel_NestedMap(t *testing.T) {
	src := "a:\n  b: 1\n  c:\n    d: deep\n"
	m, err := ParseYAML(src)
	if err != nil {
		t.Fatalf("ParseYAML: %v", err)
	}
	a := m["a"].(map[string]any)
	if a["b"] != int64(1) {
		t.Errorf("a.b = %v", a["b"])
	}
	c := a["c"].(map[string]any)
	if c["d"] != "deep" {
		t.Errorf("a.c.d = %v", c["d"])
	}
}

func TestParseYAMLLevel_SequenceUnderKey(t *testing.T) {
	src := "items:\n  - one\n  - two\n  - three\n"
	m, err := ParseYAML(src)
	if err != nil {
		t.Fatalf("ParseYAML: %v", err)
	}
	items, ok := m["items"].([]any)
	if !ok {
		t.Fatalf("items not []any: %T", m["items"])
	}
	if len(items) != 3 {
		t.Fatalf("len = %d", len(items))
	}
	if items[0] != "one" || items[1] != "two" || items[2] != "three" {
		t.Errorf("items = %v", items)
	}
}

func TestParseYAMLLevel_SequenceOfMaps(t *testing.T) {
	src := "rules:\n  - from: a\n    to: b\n  - from: c\n    to: d\n"
	m, err := ParseYAML(src)
	if err != nil {
		t.Fatalf("ParseYAML: %v", err)
	}
	rules, ok := m["rules"].([]any)
	if !ok {
		t.Fatalf("rules not []any: %T", m["rules"])
	}
	if len(rules) != 2 {
		t.Fatalf("len = %d", len(rules))
	}
	r0 := rules[0].(map[string]any)
	if r0["from"] != "a" || r0["to"] != "b" {
		t.Errorf("rules[0] = %v", r0)
	}
}

func TestParseYAMLLevel_MixedTypes(t *testing.T) {
	src := "a: 1\nb: 1.5\nc: true\nd: false\ne: null\nf: ~\ng: hello\nh: \"with # hash\"\ni: 'single quoted'\n"
	m, err := ParseYAML(src)
	if err != nil {
		t.Fatalf("ParseYAML: %v", err)
	}
	if m["a"] != int64(1) {
		t.Errorf("a = %v", m["a"])
	}
	if m["b"] != 1.5 {
		t.Errorf("b = %v", m["b"])
	}
	if m["c"] != true {
		t.Errorf("c = %v", m["c"])
	}
	if m["d"] != false {
		t.Errorf("d = %v", m["d"])
	}
	if m["e"] != nil {
		t.Errorf("e = %v", m["e"])
	}
	if m["f"] != nil {
		t.Errorf("f = %v", m["f"])
	}
	if m["g"] != "hello" {
		t.Errorf("g = %v", m["g"])
	}
	if m["h"] != "with # hash" {
		t.Errorf("h = %v", m["h"])
	}
	if m["i"] != "single quoted" {
		t.Errorf("i = %v", m["i"])
	}
}

func TestParseYAMLLevel_EmptyValueNoChildren(t *testing.T) {
	src := "empty:\nother: 1\n"
	m, err := ParseYAML(src)
	if err != nil {
		t.Fatalf("ParseYAML: %v", err)
	}
	emp, ok := m["empty"].(map[string]any)
	if !ok {
		t.Fatalf("empty not map: %T", m["empty"])
	}
	if len(emp) != 0 {
		t.Errorf("expected empty map, got %v", emp)
	}
	if m["other"] != int64(1) {
		t.Errorf("other = %v", m["other"])
	}
}

func TestParseYAMLLevel_ErrorOnBadSequencePosition(t *testing.T) {
	src := "- one\n"
	if _, err := ParseYAML(src); err == nil {
		t.Fatalf("expected sequence-root error")
	}
}

func TestParseYAMLLevel_BlankLines(t *testing.T) {
	src := "a: 1\n\nb: 2\n"
	m, err := ParseYAML(src)
	if err != nil {
		t.Fatalf("ParseYAML: %v", err)
	}
	if m["a"] != int64(1) || m["b"] != int64(2) {
		t.Errorf("got %v", m)
	}
}

func TestParseYAMLLevel_SiblingSmallerIndent(t *testing.T) {
	src := "outer:\n  inner: 1\nsibling: 2\n"
	m, err := ParseYAML(src)
	if err != nil {
		t.Fatalf("ParseYAML: %v", err)
	}
	outer := m["outer"].(map[string]any)
	if outer["inner"] != int64(1) {
		t.Errorf("outer.inner = %v", outer["inner"])
	}
	if m["sibling"] != int64(2) {
		t.Errorf("sibling = %v", m["sibling"])
	}
}

func TestParseYAMLLevel_EmptyValueEOF(t *testing.T) {
	// A key with empty value at end of input → empty map (no children).
	src := "lonely:"
	m, err := ParseYAML(src)
	if err != nil {
		t.Fatalf("ParseYAML: %v", err)
	}
	emp, ok := m["lonely"].(map[string]any)
	if !ok {
		t.Fatalf("lonely not map: %T", m["lonely"])
	}
	if len(emp) != 0 {
		t.Errorf("expected empty map, got %v", emp)
	}
}

// --- parseYAMLScalar ---

func TestParseYAMLScalar_AllTypes(t *testing.T) {
	cases := []struct {
		in   string
		want any
	}{
		{"", ""},
		{"true", true},
		{"false", false},
		{"null", nil},
		{"~", nil},
		{"42", int64(42)},
		{"-7", int64(-7)},
		{"1.5", 1.5},
		{"hello", "hello"},
		{`"quoted"`, "quoted"},
		{`'single'`, "single"},
	}
	for _, tc := range cases {
		got := parseYAMLScalar(tc.in)
		if got != tc.want {
			t.Errorf("parseYAMLScalar(%q) = %v (%T), want %v (%T)", tc.in, got, got, tc.want, tc.want)
		}
	}
}

// --- parseYAMLSeq (indirectly via ParseYAML) ---

func TestParseYAMLSeq_SimpleSeq(t *testing.T) {
	src := "xs:\n  - a\n  - b\n"
	m, err := ParseYAML(src)
	if err != nil {
		t.Fatalf("ParseYAML: %v", err)
	}
	xs, ok := m["xs"].([]any)
	if !ok || len(xs) != 2 || xs[0] != "a" || xs[1] != "b" {
		t.Fatalf("xs = %v", m["xs"])
	}
}

func TestParseYAMLSeq_NestedSeq(t *testing.T) {
	src := "matrix:\n  - 1\n  - 2\n  - 3\n"
	m, err := ParseYAML(src)
	if err != nil {
		t.Fatalf("ParseYAML: %v", err)
	}
	matrix, ok := m["matrix"].([]any)
	if !ok || len(matrix) != 3 {
		t.Fatalf("matrix = %v", m["matrix"])
	}
}

func TestParseYAMLSeq_ScalarInference(t *testing.T) {
	src := "xs:\n  - 1\n  - 2.5\n  - true\n  - null\n  - foo\n"
	m, err := ParseYAML(src)
	if err != nil {
		t.Fatalf("ParseYAML: %v", err)
	}
	xs := m["xs"].([]any)
	if xs[0] != int64(1) {
		t.Errorf("xs[0] = %v", xs[0])
	}
	if xs[1] != 2.5 {
		t.Errorf("xs[1] = %v", xs[1])
	}
	if xs[2] != true {
		t.Errorf("xs[2] = %v", xs[2])
	}
	if xs[3] != nil {
		t.Errorf("xs[3] = %v", xs[3])
	}
	if xs[4] != "foo" {
		t.Errorf("xs[4] = %v", xs[4])
	}
}

func TestParseYAMLSeq_DashOnlyItem(t *testing.T) {
	src := "items:\n  -\n  - foo\n"
	m, err := ParseYAML(src)
	if err != nil {
		t.Fatalf("ParseYAML: %v", err)
	}
	items := m["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("items len = %d", len(items))
	}
	if _, ok := items[0].(map[string]any); !ok {
		t.Errorf("items[0] = %v (%T)", items[0], items[0])
	}
	if items[1] != "foo" {
		t.Errorf("items[1] = %v", items[1])
	}
}

func TestParseYAMLSeq_DashOnlyItemWithMapChild(t *testing.T) {
	src := "items:\n  -\n    name: foo\n    value: 1\n  - next\n"
	m, err := ParseYAML(src)
	if err != nil {
		t.Fatalf("ParseYAML: %v", err)
	}
	items := m["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("len = %d", len(items))
	}
	r0, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("items[0] not map: %T", items[0])
	}
	if r0["name"] != "foo" {
		t.Errorf("items[0].name = %v", r0["name"])
	}
	if items[1] != "next" {
		t.Errorf("items[1] = %v", items[1])
	}
}

func TestParseYAMLSeq_MapItemWithExtraKeys(t *testing.T) {
	src := "items:\n  - name: foo\n    value: 1\n    extra: x\n"
	m, err := ParseYAML(src)
	if err != nil {
		t.Fatalf("ParseYAML: %v", err)
	}
	items := m["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("len = %d", len(items))
	}
	r0 := items[0].(map[string]any)
	if r0["name"] != "foo" || r0["value"] != int64(1) || r0["extra"] != "x" {
		t.Errorf("items[0] = %v", r0)
	}
}

func TestParseYAMLSeq_MapItemBrokenBySiblingList(t *testing.T) {
	// After the first map item's continuation lines, a sibling "- " row
	// breaks the map item early.
	src := "items:\n  - a: 1\n    b: 2\n  - c: 3\n"
	m, err := ParseYAML(src)
	if err != nil {
		t.Fatalf("ParseYAML: %v", err)
	}
	items := m["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("len = %d", len(items))
	}
}

func TestParseYAMLSeq_SeqFollowedBySibling(t *testing.T) {
	// A sequence followed by a sibling key — the sibling key has smaller
	// indent than the seq base, terminating parseYAMLSeq.
	src := "outer:\n  items:\n    - a\n    - b\n  sibling: 1\n"
	m, err := ParseYAML(src)
	if err != nil {
		t.Fatalf("ParseYAML: %v", err)
	}
	outer := m["outer"].(map[string]any)
	items, ok := outer["items"].([]any)
	if !ok || len(items) != 2 {
		t.Fatalf("items = %v", outer["items"])
	}
	if outer["sibling"] != int64(1) {
		t.Errorf("sibling = %v", outer["sibling"])
	}

}

func TestParseYAMLSeq_MapItemMissingColon(t *testing.T) {
	// After a "- " line that starts a map item, a continuation line with no
	// colon breaks the loop (idx2 < 0). The sequence parser is tolerant
	// and treats this as an end-of-map signal.
	src := "items:\n  - name: foo\n  - no_colon\n"
	m, err := ParseYAML(src)
	if err != nil {
		t.Fatalf("ParseYAML: %v", err)
	}
	items := m["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("len = %d", len(items))
	}
}

func TestParseYAMLLevel_SeqAfterBlankLines(t *testing.T) {
	// Blank lines between the key and its sequence exercise the
	// "j < len(lines) && TrimSpace(lines[j]) == ''" skip loop.
	src := "items:\n\n  - a\n  - b\n"
	m, err := ParseYAML(src)
	if err != nil {
		t.Fatalf("ParseYAML: %v", err)
	}
	items, ok := m["items"].([]any)
	if !ok || len(items) != 2 || items[0] != "a" || items[1] != "b" {
		t.Fatalf("items = %v", m["items"])
	}
}
func TestParseYAMLSeq_MapItemWithBlankBetweenKeys(t *testing.T) {
	// A blank line inside a "- key: value" item's continuation loop
	// exercises the `TrimSpace(lines[j]) == ""` inner skip.
	src := "items:\n  - a: 1\n\n    b: 2\n"
	m, err := ParseYAML(src)
	if err != nil {
		t.Fatalf("ParseYAML: %v", err)
	}
	items := m["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("len = %d", len(items))
	}
	r0 := items[0].(map[string]any)
	if r0["a"] != int64(1) || r0["b"] != int64(2) {
		t.Errorf("r0 = %v", r0)
	}
}

// Removed: hard-to-reach "idx2 < 0" branch in parseYAMLSeq — see PROGRESS.md.

func TestParseYAMLSeq_MapItemContinuationListBreaks(t *testing.T) {
	// A continuation line that begins with "- " breaks the inner loop.
	src := "items:\n  - a: 1\n    b: 2\n  - c: 3\n"
	m, err := ParseYAML(src)
	if err != nil {
		t.Fatalf("ParseYAML: %v", err)
	}
	items := m["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("len = %d", len(items))
	}
}
