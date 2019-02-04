package main

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/printer"
	"go/token"
	"io/ioutil"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/template"

	"golang.org/x/tools/imports"
)

const usage = `testgen <recv type> <iface>
testgen generates method stubs for recv to implement iface.
Examples:
testgen Test github.com/test/test.Test
`

// findInterface returns the import path and identifier of an interface.
// For example, given "http.ResponseWriter", findInterface returns
// "net/http", "ResponseWriter".
// If a fully qualified interface is given, such as "net/http.ResponseWriter",
// it simply parses the input.
func findInterface(iface string) (path string, id string, err error) {
	if len(strings.Fields(iface)) != 1 {
		return "", "", fmt.Errorf("couldn't parse interface: %s", iface)
	}

	if slash := strings.LastIndex(iface, "/"); slash > -1 {
		// package path provided
		dot := strings.LastIndex(iface, ".")
		// make sure iface does not end with "/" (e.g. reject net/http/)
		if slash+1 == len(iface) {
			return "", "", fmt.Errorf("interface name cannot end with a '/' character: %s", iface)
		}
		// make sure iface does not end with "." (e.g. reject net/http.)
		if dot+1 == len(iface) {
			return "", "", fmt.Errorf("interface name cannot end with a '.' character: %s", iface)
		}
		// make sure iface has exactly one "." after "/" (e.g. reject net/http/httputil)
		if strings.Count(iface[slash:], ".") != 1 {
			return "", "", fmt.Errorf("invalid interface name: %s", iface)
		}
		return iface[:dot], iface[dot+1:], nil
	}

	src := []byte("package hack\n" + "var i " + iface)
	// If we couldn't determine the import path, goimports will
	// auto fix the import path.
	imp, err := imports.Process(".", src, nil)
	if err != nil {
		return "", "", fmt.Errorf("couldn't parse interface: %s", iface)
	}

	// imp should now contain an appropriate import.
	// Parse out the import and the identifier.
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", imp, 0)
	if err != nil {
		panic(err)
	}
	if len(f.Imports) == 0 {
		return "", "", fmt.Errorf("unrecognized interface: %s", iface)
	}
	raw := f.Imports[0].Path.Value   // "io"
	path, err = strconv.Unquote(raw) // io
	if err != nil {
		panic(err)
	}
	decl := f.Decls[1].(*ast.GenDecl)      // var i io.Reader
	spec := decl.Specs[0].(*ast.ValueSpec) // i io.Reader
	sel := spec.Type.(*ast.SelectorExpr)   // io.Reader
	id = sel.Sel.Name                      // Reader
	return path, id, nil
}

// Pkg is a parsed build.Package.
type Pkg struct {
	*build.Package
	*token.FileSet
}

// typeSpec locates the *ast.TypeSpec for type id in the import path.
func typeSpec(path string, id string) (Pkg, *ast.TypeSpec, error) {
	pkg, err := build.Import(path, "", 0)
	if err != nil {
		return Pkg{}, nil, fmt.Errorf("couldn't find package %s: %v", path, err)
	}

	fset := token.NewFileSet() // share one fset across the whole package
	for _, file := range pkg.GoFiles {
		f, err := parser.ParseFile(fset, filepath.Join(pkg.Dir, file), nil, 0)
		if err != nil {
			continue
		}

		for _, decl := range f.Decls {
			decl, ok := decl.(*ast.GenDecl)
			if !ok || decl.Tok != token.TYPE {
				continue
			}
			for _, spec := range decl.Specs {
				spec := spec.(*ast.TypeSpec)
				if spec.Name.Name != id {
					continue
				}
				return Pkg{Package: pkg, FileSet: fset}, spec, nil
			}
		}
	}
	return Pkg{}, nil, fmt.Errorf("type %s not found in %s", id, path)
}

// gofmt pretty-prints e.
func (p Pkg) gofmt(e ast.Expr) string {
	var buf bytes.Buffer
	printer.Fprint(&buf, p.FileSet, e)
	return buf.String()
}

// fullType returns the fully qualified type of e.
// Examples, assuming package net/http:
// 	fullType(int) => "int"
// 	fullType(Handler) => "http.Handler"
// 	fullType(io.Reader) => "io.Reader"
// 	fullType(*Request) => "*http.Request"
func (p Pkg) fullType(e ast.Expr) string {
	ast.Inspect(e, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.Ident:
			// Using typeSpec instead of IsExported here would be
			// more accurate, but it'd be crazy expensive, and if
			// the type isn't exported, there's no point trying
			// to implement it anyway.
			if n.IsExported() {
				n.Name = p.Package.Name + "." + n.Name
			}
		case *ast.SelectorExpr:
			return false
		}
		return true
	})
	return p.gofmt(e)
}

func (p Pkg) params(field *ast.Field) []Param {
	var params []Param
	typ := p.fullType(field.Type)
	for _, name := range field.Names {
		params = append(params, Param{Name: name.Name, Type: typ})
	}
	// handle anonymous params
	if len(params) == 0 {
		params = []Param{{Type: typ}}
	}
	return params
}

// Method represents a method signature.
type Method struct {
	Recv string
	Func
}

// Func represents a function signature.
type Func struct {
	Name   string
	Params []Param
	Res    []Param
}

// Param represents a parameter in a function or method signature.
type Param struct {
	Name string
	Type string
}

func (p Pkg) funcsig(f *ast.Field) Func {
	fn := Func{Name: f.Names[0].Name}
	typ := f.Type.(*ast.FuncType)
	if typ.Params != nil {
		for _, field := range typ.Params.List {
			fn.Params = append(fn.Params, p.params(field)...)
		}
	}
	if typ.Results != nil {
		for _, field := range typ.Results.List {
			fn.Res = append(fn.Res, p.params(field)...)
		}
	}
	return fn
}

// funcs returns the set of methods required to implement iface.
// It is called funcs rather than methods because the
// function descriptions are functions; there is no receiver.
func funcs(iface string) (ifaceName string, path string, fns []Func, err error) {
	// Locate the interface.
	path, id, err := findInterface(iface)
	if err != nil {
		return "", "", nil, err
	}

	// Parse the package and find the interface declaration.
	p, spec, err := typeSpec(path, id)
	if err != nil {
		return "", "", nil, fmt.Errorf("interface %s not found: %s", iface, err)
	}
	idecl, ok := spec.Type.(*ast.InterfaceType)
	if !ok {
		return "", "", nil, fmt.Errorf("not an interface: %s", iface)
	}

	if idecl.Methods == nil {
		return "", "", nil, fmt.Errorf("empty interface: %s", iface)
	}

	for _, fndecl := range idecl.Methods.List {
		if len(fndecl.Names) == 0 {
			// Embedded interface: recurse
			_, _, embedded, err := funcs(p.fullType(fndecl.Type))
			if err != nil {
				return "", "", nil, err
			}
			fns = append(fns, embedded...)
			continue
		}

		fn := p.funcsig(fndecl)
		fns = append(fns, fn)
	}
	return id, p.Name, fns, nil
}

var typeTmpl = `{{$recv := .Recv}}
// Code generated by testgen; DO NOT EDIT.
package {{ .Package }}
// {{$recv}} ...
type {{$recv}} struct {
	{{range .Methods}}{{.Name}}Func func({{range .Params}}{{.Name}} {{.Type}}, {{end}}) ({{range .Res}}{{.Name}} {{.Type}}, {{end}})
	{{end}}
}
{{range .Methods}}
// {{.Name}} ...
func (t *{{$recv}}){{.Name}}({{range .Params}}{{.Name}} {{.Type}}, {{end}}) ({{range .Res}}{{.Name}} {{.Type}}, {{end}}) {
	if t.{{.Name}}Func != nil {
		return t.{{.Name}}Func({{range .Params}}{{.Name}}{{ if variadic .Type }}...{{ end }}, {{end}})
	}
	return {{$resLen := len .Res}}{{range $i, $e := .Res}}{{if eq $e.Type "error"}}nil{{else}}{{constructor .Type}}{{end}} {{if ne (plus1 $i) $resLen}},{{end}} {{end}}
}
{{end}}
`

var funcMapFunc = func(origType, receiver string) template.FuncMap {
	return template.FuncMap{
		"plus1": func(x int) int {
			return x + 1
		},
		"constructor": func(typ string) string {
			if typ == "int" || typ == "int16" || typ == "int32" || typ == "int64" ||
				typ == "uint" || typ == "uint16" || typ == "uint32" || typ == "uint64" {
				return "0"
			}
			if typ == "bool" {
				return "false"
			}
			if strings.HasPrefix(typ, "*") {
				return "&" + typ[1:] + "{}"
			}

			if typ == origType {
				return receiver
			}
			return typ + "{}"
		},
		"variadic": func(typ string) bool {
			return strings.HasPrefix(typ, "...")
		},
	}
}

func genType(ifaceName, pkg, recvType string, fns []Func) []byte {
	var typeTmplCompiled = template.Must(template.New("typeTmpl").Funcs(funcMapFunc(ifaceName, "t")).Parse(typeTmpl))

	var buf bytes.Buffer
	methods := make([]Method, len(fns))
	for idx, fn := range fns {
		methods[idx] = Method{Func: fn}
	}

	methodsStruct := struct {
		Methods []Method
		Recv    string
		Package string
	}{
		Methods: methods,
		Recv:    recvType,
		Package: pkg,
	}

	if err := typeTmplCompiled.Execute(&buf, &methodsStruct); err != nil {
		panic(err)
	}

	pretty, err := imports.Process("", buf.Bytes(), nil)
	if err != nil {
		fmt.Println(buf.String())
		fatal(err)
	}

	return pretty
}

func main() {
	if len(os.Args) < 3 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	recvType, iface := os.Args[1], os.Args[2]

	out := ""
	if len(os.Args) == 4 {
		out = os.Args[3]
	}

	ifaceName, pkg, fns, err := funcs(iface)
	if err != nil {
		fatal(err)
	}
	ifaceName = pkg + "." + ifaceName

	if out != "" {
		out = filepath.Join(build.Default.GOPATH, "src", out)
		_, pkg = filepath.Split(filepath.Dir(out))
	}

	src := genType(ifaceName, pkg, recvType, fns)

	// write sources
	if out == "" {
		fmt.Print(string(src))
		return
	}

	if err := os.MkdirAll(filepath.Dir(out), 0755); err != nil {
		fatal(err)
	}
	if err := ioutil.WriteFile(out, src, 0655); err != nil {
		fatal(err)
	}

	fmt.Printf("generated file: %s\n", out)
}

func fatal(msg interface{}) {
	fmt.Fprintln(os.Stderr, msg)
	os.Exit(1)
}
