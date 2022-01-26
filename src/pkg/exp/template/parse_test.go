// Copyright 2011 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package template

import (
	"flag"
	"fmt"
	"testing"
)

var debug = flag.Bool("debug", false, "show the errors produced by the tests")

type numberTest struct {
	text      string
	isInt     bool
	isUint    bool
	isFloat   bool
	isComplex bool
	int64
	uint64
	float64
	complex128
}

var numberTests = []numberTest{
	// basics
	{"0", true, true, true, false, 0, 0, 0, 0},
	{"-0", true, true, true, false, 0, 0, 0, 0}, // check that -0 is a uint.
	{"73", true, true, true, false, 73, 73, 73, 0},
	{"-73", true, false, true, false, -73, 0, -73, 0},
	{"+73", true, false, true, false, 73, 0, 73, 0},
	{"100", true, true, true, false, 100, 100, 100, 0},
	{"1e9", true, true, true, false, 1e9, 1e9, 1e9, 0},
	{"-1e9", true, false, true, false, -1e9, 0, -1e9, 0},
	{"-1.2", false, false, true, false, 0, 0, -1.2, 0},
	{"1e19", false, true, true, false, 0, 1e19, 1e19, 0},
	{"-1e19", false, false, true, false, 0, 0, -1e19, 0},
	{"4i", false, false, false, true, 0, 0, 0, 4i},
	{"-1.2+4.2i", false, false, false, true, 0, 0, 0, -1.2 + 4.2i},
	// complex with 0 imaginary are float (and maybe integer)
	{"0i", true, true, true, true, 0, 0, 0, 0},
	{"-1.2+0i", false, false, true, true, 0, 0, -1.2, -1.2},
	{"-12+0i", true, false, true, true, -12, 0, -12, -12},
	{"13+0i", true, true, true, true, 13, 13, 13, 13},
	// funny bases
	{"0123", true, true, true, false, 0123, 0123, 0123, 0},
	{"-0x0", true, true, true, false, 0, 0, 0, 0},
	{"0xdeadbeef", true, true, true, false, 0xdeadbeef, 0xdeadbeef, 0xdeadbeef, 0},
	// some broken syntax
	{text: "+-2"},
	{text: "0x123."},
	{text: "1e."},
	{text: "0xi."},
	{text: "1+2."},
}

func TestNumberParse(t *testing.T) {
	for _, test := range numberTests {
		// If fmt.Sscan thinks it's complex, it's complex.  We can't trust the output
		// because imaginary comes out as a number.
		var c complex128
		_, err := fmt.Sscan(test.text, &c)
		n, err := newNumber(test.text, err == nil)
		ok := test.isInt || test.isUint || test.isFloat || test.isComplex
		if ok && err != nil {
			t.Errorf("unexpected error for %q", test.text)
			continue
		}
		if !ok && err == nil {
			t.Errorf("expected error for %q", test.text)
			continue
		}
		if !ok {
			continue
		}
		if n.isComplex != test.isComplex {
			t.Errorf("complex incorrect for %q; should be %t", test.text, test.isComplex)
		}
		if test.isInt {
			if !n.isInt {
				t.Errorf("expected integer for %q", test.text)
			}
			if n.int64 != test.int64 {
				t.Errorf("int64 for %q should be %d is %d", test.text, test.int64, n.int64)
			}
		} else if n.isInt {
			t.Errorf("did not expect integer for %q", test.text)
		}
		if test.isUint {
			if !n.isUint {
				t.Errorf("expected unsigned integer for %q", test.text)
			}
			if n.uint64 != test.uint64 {
				t.Errorf("uint64 for %q should be %d is %d", test.text, test.uint64, n.uint64)
			}
		} else if n.isUint {
			t.Errorf("did not expect unsigned integer for %q", test.text)
		}
		if test.isFloat {
			if !n.isFloat {
				t.Errorf("expected float for %q", test.text)
			}
			if n.float64 != test.float64 {
				t.Errorf("float64 for %q should be %g is %g", test.text, test.float64, n.float64)
			}
		} else if n.isFloat {
			t.Errorf("did not expect float for %q", test.text)
		}
		if test.isComplex {
			if !n.isComplex {
				t.Errorf("expected complex for %q", test.text)
			}
			if n.complex128 != test.complex128 {
				t.Errorf("complex128 for %q should be %g is %g", test.text, test.complex128, n.complex128)
			}
		} else if n.isComplex {
			t.Errorf("did not expect complex for %q", test.text)
		}
	}
}

type parseTest struct {
	name   string
	input  string
	ok     bool
	result string
}

const (
	noError  = true
	hasError = false
)

var parseTests = []parseTest{
	{"empty", "", noError,
		`[]`},
	{"spaces", " \t\n", noError,
		`[(text: " \t\n")]`},
	{"text", "some text", noError,
		`[(text: "some text")]`},
	{"emptyAction", "{{}}", hasError,
		`[(action: [])]`},
	{"field", "{{.X}}", noError,
		`[(action: [(command: [F=[X]])])]`},
	{"simple command", "{{printf}}", noError,
		`[(action: [(command: [I=printf])])]`},
	{"multi-word command", "{{printf `%d` 23}}", noError,
		"[(action: [(command: [I=printf S=`%d` N=23])])]"},
	{"pipeline", "{{.X|.Y}}", noError,
		`[(action: [(command: [F=[X]]) (command: [F=[Y]])])]`},
	{"simple if", "{{if .X}}hello{{end}}", noError,
		`[({{if [(command: [F=[X]])]}} [(text: "hello")])]`},
	{"if with else", "{{if .X}}true{{else}}false{{end}}", noError,
		`[({{if [(command: [F=[X]])]}} [(text: "true")] {{else}} [(text: "false")])]`},
	{"simple range", "{{range .X}}hello{{end}}", noError,
		`[({{range [(command: [F=[X]])]}} [(text: "hello")])]`},
	{"chained field range", "{{range .X.Y.Z}}hello{{end}}", noError,
		`[({{range [(command: [F=[X Y Z]])]}} [(text: "hello")])]`},
	{"nested range", "{{range .X}}hello{{range .Y}}goodbye{{end}}{{end}}", noError,
		`[({{range [(command: [F=[X]])]}} [(text: "hello")({{range [(command: [F=[Y]])]}} [(text: "goodbye")])])]`},
	{"range with else", "{{range .X}}true{{else}}false{{end}}", noError,
		`[({{range [(command: [F=[X]])]}} [(text: "true")] {{else}} [(text: "false")])]`},
	{"range over pipeline", "{{range .X|.M}}true{{else}}false{{end}}", noError,
		`[({{range [(command: [F=[X]]) (command: [F=[M]])]}} [(text: "true")] {{else}} [(text: "false")])]`},
	{"range []int", "{{range .SI}}{{.}}{{end}}", noError,
		`[({{range [(command: [F=[SI]])]}} [(action: [(command: [{{<.>}}])])])]`},
	{"constants", "{{range .SI 1 -3.2i true false }}{{end}}", noError,
		`[({{range [(command: [F=[SI] N=1 N=-3.2i B=true B=false])]}} [])]`},
	{"template", "{{template `x` .Y}}", noError,
		"[{{template S=`x` [(command: [F=[Y]])]}}]"},
	{"with", "{{with .X}}hello{{end}}", noError,
		`[({{with [(command: [F=[X]])]}} [(text: "hello")])]`},
	{"with with else", "{{with .X}}hello{{else}}goodbye{{end}}", noError,
		`[({{with [(command: [F=[X]])]}} [(text: "hello")] {{else}} [(text: "goodbye")])]`},
	// Errors.
	{"unclosed action", "hello{{range", hasError, ""},
	{"missing end", "hello{{range .x}}", hasError, ""},
	{"missing end after else", "hello{{range .x}}{{else}}", hasError, ""},
	{"undefined function", "hello{{undefined}}", hasError, ""},
}

func TestParse(t *testing.T) {
	for _, test := range parseTests {
		tmpl := New(test.name)
		err := tmpl.Parse(test.input)
		switch {
		case err == nil && !test.ok:
			t.Errorf("%q: expected error; got none", test.name)
			continue
		case err != nil && test.ok:
			t.Errorf("%q: unexpected error: %v", test.name, err)
			continue
		case err != nil && !test.ok:
			// expected error, got one
			if *debug {
				fmt.Printf("%s: %s\n\t%s\n", test.name, test.input, err)
			}
			continue
		}
		result := tmpl.root.String()
		if result != test.result {
			t.Errorf("%s=(%q): got\n\t%v\nexpected\n\t%v", test.name, test.input, result, test.result)
		}
	}
}
