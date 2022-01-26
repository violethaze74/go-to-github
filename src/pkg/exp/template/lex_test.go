// Copyright 2011 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package template

import (
	"reflect"
	"testing"
)

type lexTest struct {
	name  string
	input string
	items []item
}

var (
	tEOF      = item{itemEOF, ""}
	tLeft     = item{itemLeftDelim, "{{"}
	tRight    = item{itemRightDelim, "}}"}
	tRange    = item{itemRange, "range"}
	tPipe     = item{itemPipe, "|"}
	tFor      = item{itemIdentifier, "for"}
	tQuote    = item{itemString, `"abc \n\t\" "`}
	raw       = "`" + `abc\n\t\" ` + "`"
	tRawQuote = item{itemRawString, raw}
)

var lexTests = []lexTest{
	{"empty", "", []item{tEOF}},
	{"spaces", " \t\n", []item{{itemText, " \t\n"}, tEOF}},
	{"text", `now is the time`, []item{{itemText, "now is the time"}, tEOF}},
	{"text with comment", "hello-{{/* this is a comment */}}-world", []item{
		{itemText, "hello-"},
		{itemText, "-world"},
		tEOF,
	}},
	{"empty action", `{{}}`, []item{tLeft, tRight, tEOF}},
	{"for", `{{for }}`, []item{tLeft, tFor, tRight, tEOF}},
	{"quote", `{{"abc \n\t\" "}}`, []item{tLeft, tQuote, tRight, tEOF}},
	{"raw quote", "{{" + raw + "}}", []item{tLeft, tRawQuote, tRight, tEOF}},
	{"numbers", "{{1 02 0x14 -7.2i 1e3 +1.2e-4 4.2i 1+2i}}", []item{
		tLeft,
		{itemNumber, "1"},
		{itemNumber, "02"},
		{itemNumber, "0x14"},
		{itemNumber, "-7.2i"},
		{itemNumber, "1e3"},
		{itemNumber, "+1.2e-4"},
		{itemNumber, "4.2i"},
		{itemComplex, "1+2i"},
		tRight,
		tEOF,
	}},
	{"bools", "{{true false}}", []item{
		tLeft,
		{itemBool, "true"},
		{itemBool, "false"},
		tRight,
		tEOF,
	}},
	{"dot", "{{.}}", []item{
		tLeft,
		{itemDot, "."},
		tRight,
		tEOF,
	}},
	{"dots", "{{.x . .2 .x.y}}", []item{
		tLeft,
		{itemField, ".x"},
		{itemDot, "."},
		{itemNumber, ".2"},
		{itemField, ".x.y"},
		tRight,
		tEOF,
	}},
	{"keywords", "{{range if else end with}}", []item{
		tLeft,
		{itemRange, "range"},
		{itemIf, "if"},
		{itemElse, "else"},
		{itemEnd, "end"},
		{itemWith, "with"},
		tRight,
		tEOF,
	}},
	{"pipeline", `intro {{echo hi 1.2 |noargs|args 1 "hi"}} outro`, []item{
		{itemText, "intro "},
		tLeft,
		{itemIdentifier, "echo"},
		{itemIdentifier, "hi"},
		{itemNumber, "1.2"},
		tPipe,
		{itemIdentifier, "noargs"},
		tPipe,
		{itemIdentifier, "args"},
		{itemNumber, "1"},
		{itemString, `"hi"`},
		tRight,
		{itemText, " outro"},
		tEOF,
	}},
	// errors
	{"badchar", "#{{#}}", []item{
		{itemText, "#"},
		tLeft,
		{itemError, "unrecognized character in action: U+0023 '#'"},
	}},
	{"unclosed action", "{{\n}}", []item{
		tLeft,
		{itemError, "unclosed action"},
	}},
	{"EOF in action", "{{range", []item{
		tLeft,
		tRange,
		{itemError, "unclosed action"},
	}},
	{"unclosed quote", "{{\"\n\"}}", []item{
		tLeft,
		{itemError, "unterminated quoted string"},
	}},
	{"unclosed raw quote", "{{`xx\n`}}", []item{
		tLeft,
		{itemError, "unterminated raw quoted string"},
	}},
	{"bad number", "{{3k}}", []item{
		tLeft,
		{itemError, `bad number syntax: "3k"`},
	}},
}

// collect gathers the emitted items into a slice.
func collect(t *lexTest) (items []item) {
	l := lex(t.name, t.input)
	for {
		item := l.nextItem()
		items = append(items, item)
		if item.typ == itemEOF || item.typ == itemError {
			break
		}
	}
	return
}

func TestLex(t *testing.T) {
	for _, test := range lexTests {
		items := collect(&test)
		if !reflect.DeepEqual(items, test.items) {
			t.Errorf("%s: got\n\t%v\nexpected\n\t%v", test.name, items, test.items)
		}
	}
}
