// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package fitstringer is a fork of golang.org/x/tools/cmd/stringer used to
// generate string methods for the FIT types generated by cmd/fitgen. It is now
// a library instead of a command. It also has slightly different logic for
// prefix trimming compared to the original. This version will always remove
// the type name prefix for each generated type.
//
// golang.org/x/tools commit used as base:
// https://github.com/golang/tools/commit/63e6ed9258fa6cbc90aab9b1eef3e0866e89b874
package fitstringer

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/constant"
	"go/format"
	"go/token"
	"go/types"
	"sort"
	"strings"

	"golang.org/x/tools/go/packages"
)

// Generate generates string methods using the logic from the official stringer
// tool, but does not write the output to a file, and instead returns the
// output source code as bytes. It takes a given set of type name and their
// source definitions as input. Each type will also have its name prefix
// removed from the resulting string methods.
func Generate(types []string, files ...string) ([]byte, error) {
	// Declare so error can be set in recover handler.
	var (
		output []byte
		err    error
	)

	g := generator{
		trimPrefix:  "",
		lineComment: false,
	}

	if err = g.parsePackage(files, nil); err != nil {
		return output, err
	}

	g.Printf("// Code generated by the cmd/fitgen/internal/fitstringer package; DO NOT EDIT.\n")
	g.Printf("\n")
	g.Printf("// fit types: %s\n", types)
	g.Printf("\n")
	g.Printf("//lint:file-ignore SA4003 Ignore checks of unsigned types >= 0. stringer generates these.\n")
	g.Printf("\n")
	g.Printf("package %s", g.pkg.name)
	g.Printf("\n")
	g.Printf("import \"strconv\"\n") // Used by all methods.
	g.Printf("\n")

	// Handle any (error panics) from ast.Inspect and file.genDecl which
	// can't propagate the error up the call chain.
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("recovered error: %v", r)
		}
	}()

	// Run generate for each type.
	for _, typeName := range types {
		if err := g.generate(typeName); err != nil {
			return output, err
		}
	}

	output, err = g.format()

	return output, err
}

// generator holds the state of the analysis. Primarily used to buffer
// the output for format.Source.
type generator struct {
	buf bytes.Buffer // Accumulated output.
	pkg *pkg         // Package we are scanning.

	trimPrefix  string
	lineComment bool
}

func (g *generator) Printf(format string, args ...interface{}) {
	fmt.Fprintf(&g.buf, format, args...)
}

// file holds a single parsed file and associated data.
type file struct {
	pkg  *pkg      // Package to which this file belongs.
	file *ast.File // Parsed AST.
	// These fields are reset for each type being generated.
	typeName string  // Name of the constant type.
	values   []value // Accumulator for constant values of that type.

	trimPrefix  string
	lineComment bool
}

type pkg struct {
	name  string
	defs  map[*ast.Ident]types.Object
	files []*file
}

// parsePackage analyzes the single package constructed from the patterns and tags.
// parsePackage exits if there is an error.
func (g *generator) parsePackage(patterns, tags []string) error {
	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedTypes | packages.NeedTypesInfo | packages.NeedSyntax,
		// TODO: Need to think about constants in test files. Maybe write type_string_test.go
		// in a separate pass? For later.
		Tests:      false,
		BuildFlags: []string{fmt.Sprintf("-tags=%s", strings.Join(tags, " "))},
	}
	pkgs, err := packages.Load(cfg, patterns...)
	if err != nil {
		return err
	}
	if len(pkgs) != 1 {
		return fmt.Errorf("%d packages found", len(pkgs))
	}
	g.addPackage(pkgs[0])

	return nil
}

// addPackage adds a type checked Package and its syntax files to the generator.
func (g *generator) addPackage(ppkg *packages.Package) {
	g.pkg = &pkg{
		name:  ppkg.Name,
		defs:  ppkg.TypesInfo.Defs,
		files: make([]*file, len(ppkg.Syntax)),
	}

	for i, f := range ppkg.Syntax {
		g.pkg.files[i] = &file{
			file:        f,
			pkg:         g.pkg,
			trimPrefix:  g.trimPrefix,
			lineComment: g.lineComment,
		}
	}
}

// generate produces the String method for the named type.
func (g *generator) generate(typeName string) error {
	values := make([]value, 0, 100)
	for _, f := range g.pkg.files {
		// Set the state for this run of the walker.
		f.typeName = typeName
		f.values = nil
		if f.file != nil {
			ast.Inspect(f.file, f.genDecl)
			values = append(values, f.values...)
		}
	}

	if len(values) == 0 {
		return fmt.Errorf("no values defined for type %s", typeName)
	}
	// Generate code that will fail if the constants change value.
	g.Printf("func _() {\n")
	g.Printf("\t// An \"invalid array index\" compiler error signifies that the constant values have changed.\n")
	g.Printf("\t// Re-run the stringer command to generate them again.\n")
	g.Printf("\tvar x [1]struct{}\n")
	for _, v := range values {
		g.Printf("\t_ = x[%s - %s]\n", v.originalName, v.str)
	}
	g.Printf("}\n")
	runs := splitIntoRuns(values)
	// The decision of which pattern to use depends on the number of
	// runs in the numbers. If there's only one, it's easy. For more than
	// one, there's a tradeoff between complexity and size of the data
	// and code vs. the simplicity of a map. A map takes more space,
	// but so does the code. The decision here (crossover at 10) is
	// arbitrary, but considers that for large numbers of runs the cost
	// of the linear scan in the switch might become important, and
	// rather than use yet another algorithm such as binary search,
	// we punt and use a map. In any case, the likelihood of a map
	// being necessary for any realistic example other than bitmasks
	// is very low. And bitmasks probably deserve their own analysis,
	// to be done some other day.
	switch {
	case len(runs) == 1:
		g.buildOneRun(runs, typeName)
	case len(runs) <= 10:
		g.buildMultipleRuns(runs, typeName)
	default:
		g.buildMap(runs, typeName)
	}

	return nil
}

// splitIntoRuns breaks the values into runs of contiguous sequences.
// For example, given 1,2,3,5,6,7 it returns {1,2,3},{5,6,7}.
// The input slice is known to be non-empty.
func splitIntoRuns(values []value) [][]value {
	// We use stable sort so the lexically first name is chosen for equal elements.
	sort.Stable(byValue(values))
	// Remove duplicates. Stable sort has put the one we want to print first,
	// so use that one. The String method won't care about which named constant
	// was the argument, so the first name for the given value is the only one to keep.
	// We need to do this because identical values would cause the switch or map
	// to fail to compile.
	j := 1
	for i := 1; i < len(values); i++ {
		if values[i].value != values[i-1].value {
			values[j] = values[i]
			j++
		}
	}
	values = values[:j]
	runs := make([][]value, 0, 10)
	for len(values) > 0 {
		// One contiguous sequence per outer loop.
		i := 1
		for i < len(values) && values[i].value == values[i-1].value+1 {
			i++
		}
		runs = append(runs, values[:i])
		values = values[i:]
	}
	return runs
}

// format returns the gofmt-ed contents of the Generator's buffer.
func (g *generator) format() ([]byte, error) {
	src, err := format.Source(g.buf.Bytes())
	if err != nil {
		// Should never happen, but can arise when developing this code.
		return nil, fmt.Errorf("warning: internal error: invalid Go generated: %s", err)
	}
	return src, nil
}

// value represents a declared constant.
type value struct {
	originalName string // The name of the constant.
	name         string // The name with trimmed prefix.
	// The value is stored as a bit pattern alone. The boolean tells us
	// whether to interpret it as an int64 or a uint64; the only place
	// this matters is when sorting.
	// Much of the time the str field is all we need; it is printed
	// by Value.String.
	value  uint64 // Will be converted to int64 when needed.
	signed bool   // Whether the constant is a signed type.
	str    string // The string representation given by the "go/constant" package.
}

func (v *value) String() string {
	return v.str
}

// byValue lets us sort the constants into increasing order.
// We take care in the Less method to sort in signed or unsigned order,
// as appropriate.
type byValue []value

func (b byValue) Len() int      { return len(b) }
func (b byValue) Swap(i, j int) { b[i], b[j] = b[j], b[i] }
func (b byValue) Less(i, j int) bool {
	if b[i].signed {
		return int64(b[i].value) < int64(b[j].value)
	}
	return b[i].value < b[j].value
}

// genDecl processes one declaration clause.
func (f *file) genDecl(node ast.Node) bool {
	decl, ok := node.(*ast.GenDecl)
	if !ok || decl.Tok != token.CONST {
		// We only care about const declarations.
		return true
	}
	// The name of the type of the constants we are declaring.
	// Can change if this is a multi-element declaration.
	typ := ""
	// Loop over the elements of the declaration. Each element is a ValueSpec:
	// a list of names possibly followed by a type, possibly followed by values.
	// If the type and value are both missing, we carry down the type (and value,
	// but the "go/types" package takes care of that).
	for _, spec := range decl.Specs {
		vspec := spec.(*ast.ValueSpec) // Guaranteed to succeed as this is CONST.
		if vspec.Type == nil && len(vspec.Values) > 0 {
			// "X = 1". With no type but a value. If the constant is untyped,
			// skip this vspec and reset the remembered type.
			typ = ""

			// If this is a simple type conversion, remember the type.
			// We don't mind if this is actually a call; a qualified call won't
			// be matched (that will be SelectorExpr, not Ident), and only unusual
			// situations will result in a function call that appears to be
			// a type conversion.
			ce, ok := vspec.Values[0].(*ast.CallExpr)
			if !ok {
				continue
			}
			id, ok := ce.Fun.(*ast.Ident)
			if !ok {
				continue
			}
			typ = id.Name
		}
		if vspec.Type != nil {
			// "X T". We have a type. Remember it.
			ident, ok := vspec.Type.(*ast.Ident)
			if !ok {
				continue
			}
			typ = ident.Name
		}
		if typ != f.typeName {
			// This is not the type we're looking for.
			continue
		}
		// We now have a list of names (from one line of source code) all being
		// declared with the desired type.
		// Grab their names and actual values and store them in f.values.
		for _, name := range vspec.Names {
			if name.Name == "_" {
				continue
			}
			// This dance lets the type checker find the values for us. It's a
			// bit tricky: look up the object declared by the name, find its
			// types.Const, and extract its value.
			obj, ok := f.pkg.defs[name]
			if !ok {
				err := fmt.Sprintf("no value for constant %s", name)
				panic(err)
			}
			info := obj.Type().Underlying().(*types.Basic).Info()
			if info&types.IsInteger == 0 {
				err := fmt.Sprintf("can't handle non-integer constant type %s", typ)
				panic(err)
			}
			val := obj.(*types.Const).Val() // Guaranteed to succeed as this is CONST.
			if val.Kind() != constant.Int {
				err := fmt.Sprintf("can't happen: constant is not an integer %s", name)
				panic(err)
			}
			i64, isInt := constant.Int64Val(val)
			u64, isUint := constant.Uint64Val(val)
			if !isInt && !isUint {
				err := fmt.Sprintf("internal error: value of %s is not an integer: %s", name, val.String())
				panic(err)
			}
			if !isInt {
				u64 = uint64(i64)
			}
			v := value{
				originalName: name.Name,
				value:        u64,
				signed:       info&types.IsUnsigned == 0,
				str:          val.String(),
			}
			if c := vspec.Comment; f.lineComment && c != nil && len(c.List) == 1 {
				v.name = strings.TrimSpace(c.Text())
			} else {
				// Note: trim prefix logic.
				v.name = strings.TrimPrefix(v.originalName, f.typeName)
			}
			f.values = append(f.values, v)
		}
	}
	return false
}

// Helpers

// usize returns the number of bits of the smallest unsigned integer
// type that will hold n. Used to create the smallest possible slice of
// integers to use as indexes into the concatenated strings.
func usize(n int) int {
	switch {
	case n < 1<<8:
		return 8
	case n < 1<<16:
		return 16
	default:
		// 2^32 is enough constants for anyone.
		return 32
	}
}

// declareIndexAndNameVars declares the index slices and concatenated names
// strings representing the runs of values.
func (g *generator) declareIndexAndNameVars(runs [][]value, typeName string) {
	var indexes, names []string
	for i, run := range runs {
		index, name := g.createIndexAndNameDecl(run, typeName, fmt.Sprintf("_%d", i))
		if len(run) != 1 {
			indexes = append(indexes, index)
		}
		names = append(names, name)
	}
	g.Printf("const (\n")
	for _, name := range names {
		g.Printf("\t%s\n", name)
	}
	g.Printf(")\n\n")

	if len(indexes) > 0 {
		g.Printf("var (")
		for _, index := range indexes {
			g.Printf("\t%s\n", index)
		}
		g.Printf(")\n\n")
	}
}

// declareIndexAndNameVar is the single-run version of declareIndexAndNameVars
func (g *generator) declareIndexAndNameVar(run []value, typeName string) {
	index, name := g.createIndexAndNameDecl(run, typeName, "")
	g.Printf("const %s\n", name)
	g.Printf("var %s\n", index)
}

// createIndexAndNameDecl returns the pair of declarations for the run. The caller will add "const" and "var".
func (g *generator) createIndexAndNameDecl(run []value, typeName, suffix string) (string, string) {
	b := new(bytes.Buffer)
	indexes := make([]int, len(run))
	for i := range run {
		b.WriteString(run[i].name)
		indexes[i] = b.Len()
	}
	nameConst := fmt.Sprintf("_%s_name%s = %q", typeName, suffix, b.String())
	nameLen := b.Len()
	b.Reset()
	fmt.Fprintf(b, "_%s_index%s = [...]uint%d{0, ", typeName, suffix, usize(nameLen))
	for i, v := range indexes {
		if i > 0 {
			fmt.Fprintf(b, ", ")
		}
		fmt.Fprintf(b, "%d", v)
	}
	fmt.Fprintf(b, "}")
	return b.String(), nameConst
}

// declareNameVars declares the concatenated names string representing all the values in the runs.
func (g *generator) declareNameVars(runs [][]value, typeName, suffix string) {
	g.Printf("const _%s_name%s = \"", typeName, suffix)
	for _, run := range runs {
		for i := range run {
			g.Printf("%s", run[i].name)
		}
	}
	g.Printf("\"\n")
}

// buildOneRun generates the variables and String method for a single run of contiguous values.
func (g *generator) buildOneRun(runs [][]value, typeName string) {
	values := runs[0]
	g.Printf("\n")
	g.declareIndexAndNameVar(values, typeName)
	// The generated code is simple enough to write as a Printf format.
	lessThanZero := ""
	if values[0].signed {
		lessThanZero = "i < 0 || "
	}
	if values[0].value == 0 { // Signed or unsigned, 0 is still 0.
		g.Printf(stringOneRun, typeName, usize(len(values)), lessThanZero)
	} else {
		g.Printf(stringOneRunWithOffset, typeName, values[0].String(), usize(len(values)), lessThanZero)
	}
}

// Arguments to format are:
//	[1]: type name
//	[2]: size of index element (8 for uint8 etc.)
//	[3]: less than zero check (for signed types)
/*
 */
const stringOneRun = `func (i %[1]s) String() string {
	if %[3]si >= %[1]s(len(_%[1]s_index)-1) {
		return "%[1]s(" + strconv.FormatInt(int64(i), 10) + ")"
	}
	return _%[1]s_name[_%[1]s_index[i]:_%[1]s_index[i+1]]
}
`

// Arguments to format are:
//	[1]: type name
//	[2]: lowest defined value for type, as a string
//	[3]: size of index element (8 for uint8 etc.)
//	[4]: less than zero check (for signed types)
/*
 */
const stringOneRunWithOffset = `func (i %[1]s) String() string {
	i -= %[2]s
	if %[4]si >= %[1]s(len(_%[1]s_index)-1) {
		return "%[1]s(" + strconv.FormatInt(int64(i + %[2]s), 10) + ")"
	}
	return _%[1]s_name[_%[1]s_index[i] : _%[1]s_index[i+1]]
}
`

// buildMultipleRuns generates the variables and String method for multiple runs of contiguous values.
// For this pattern, a single Printf format won't do.
func (g *generator) buildMultipleRuns(runs [][]value, typeName string) {
	g.Printf("\n")
	g.declareIndexAndNameVars(runs, typeName)
	g.Printf("func (i %s) String() string {\n", typeName)
	g.Printf("\tswitch {\n")
	for i, values := range runs {
		if len(values) == 1 {
			g.Printf("\tcase i == %s:\n", &values[0])
			g.Printf("\t\treturn _%s_name_%d\n", typeName, i)
			continue
		}
		g.Printf("\tcase %s <= i && i <= %s:\n", &values[0], &values[len(values)-1])
		if values[0].value != 0 {
			g.Printf("\t\ti -= %s\n", &values[0])
		}
		g.Printf("\t\treturn _%s_name_%d[_%s_index_%d[i]:_%s_index_%d[i+1]]\n",
			typeName, i, typeName, i, typeName, i)
	}
	g.Printf("\tdefault:\n")
	g.Printf("\t\treturn \"%s(\" + strconv.FormatInt(int64(i), 10) + \")\"\n", typeName)
	g.Printf("\t}\n")
	g.Printf("}\n")
}

// buildMap handles the case where the space is so sparse a map is a reasonable fallback.
// It's a rare situation but has simple code.
func (g *generator) buildMap(runs [][]value, typeName string) {
	g.Printf("\n")
	g.declareNameVars(runs, typeName, "")
	g.Printf("\nvar _%s_map = map[%s]string{\n", typeName, typeName)
	n := 0
	for _, values := range runs {
		for _, value := range values {
			g.Printf("\t%s: _%s_name[%d:%d],\n", &value, typeName, n, n+len(value.name))
			n += len(value.name)
		}
	}
	g.Printf("}\n\n")
	g.Printf(stringMap, typeName)
}

// Argument to format is the type name.
const stringMap = `func (i %[1]s) String() string {
	if str, ok := _%[1]s_map[i]; ok {
		return str
	}
	return "%[1]s(" + strconv.FormatInt(int64(i), 10) + ")"
}
`
