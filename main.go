package main

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"html/template"
	"io/ioutil"
	"path/filepath"
	"reflect"
	"strings"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/inspect"
	"golang.org/x/tools/go/analysis/singlechecker"
	"golang.org/x/tools/go/ast/inspector"
	"golang.org/x/tools/imports"
)

func main() {
	singlechecker.Main(analyzer)
}

var analyzer = &analysis.Analyzer{
	Name: "encjsongen",
	Doc: `Generate MarshalJSON() and UnmarshalJSON() from customjson tag.
	Tag format => customjson:"NAME=EXPR;ASSIGN"
	    - NAME: Used in place of json tag
	    - EXPR: Expression to represent alias type(for MarshalJSON)
	    - ASSIGN: Expression to assign to the actual type(for UnmarshalJSON)
	Note: "$" in EXPR and ASSIGN is a special character that is converted to
	      the field name with receiver on the right hand side.
	
	// Example:
	type v struct {
		CreateTime time.Time ` + "`" + `json:"-" customjson:"createTime=$.Unix();time.Unix($, 0)"` + "`" + `
	}
`,
	Requires:         []*analysis.Analyzer{inspect.Analyzer},
	RunDespiteErrors: true,
	Run:              run,
}

func run(pass *analysis.Pass) (interface{}, error) {
	inspect := pass.ResultOf[inspect.Analyzer].(*inspector.Inspector)
	nodeFilter := []ast.Node{
		(*ast.TypeSpec)(nil),
	}
	inspect.Preorder(nodeFilter, func(n ast.Node) {
		ts := n.(*ast.TypeSpec)

		s, ok := ts.Type.(*ast.StructType)
		if !ok {
			return
		}

		si := newStructInfo(pass.Fset, pass.Pkg, ts)
		for _, f := range s.Fields.List {
			if f.Tag == nil {
				continue
			}
			customjson := reflect.StructTag(f.Tag.Value).Get("customjson")
			if customjson == "" {
				continue
			}
			if err := si.AddAlias(f.Names[0].Name, customjson); err != nil {
				pass.Reportf(f.Pos(), "%v", err)
				return
			}
		}
		if si.HasAlias() {
			if err := si.Output(); err != nil {
				pass.Reportf(ts.Pos(), "failed to generate: %v", err)
			}
		}
	})

	return nil, nil
}

type alias struct {
	Target  string
	JSONKey string
	Type    string
	Expr    string
	Assign  string
}

func newStructInfo(fset *token.FileSet, pkg *types.Package, ts *ast.TypeSpec) *structInfo {
	return &structInfo{
		fset:     fset,
		pkg:      pkg,
		path:     filepath.Dir(fset.File(ts.Pos()).Name()),
		Receiver: ts.Name.Name,
	}
}

type structInfo struct {
	fset *token.FileSet
	pkg  *types.Package
	path string

	Receiver string
	Aliases  []alias
}

func (si *structInfo) AddAlias(name, tag string) error {
	i := strings.Index(tag, "=")
	if i < 1 {
		return errors.New("invalid tag")
	}

	exprs := strings.Split(tag[i+1:], ";")
	if len(exprs) != 2 {
		return errors.New("invalid tag")
	}

	typ, err := types.Eval(si.fset, si.pkg, 0, strings.Replace(exprs[0], "$", si.Receiver+"{}."+name, -1))
	if err != nil {
		return err
	}
	if typ.Type == nil {
		return errors.New("invalid expr")
	}

	si.Aliases = append(si.Aliases, alias{
		Target:  name,
		JSONKey: tag[:i],
		Type:    typ.Type.String(),
		Expr:    strings.Replace(exprs[0], "$", "v."+name, -1),
		Assign:  strings.Replace(exprs[1], "$", "aux.Alias"+name, -1),
	})
	return nil
}

func (si *structInfo) HasAlias() bool {
	return len(si.Aliases) > 0
}

func (si *structInfo) Output() error {
	b := new(bytes.Buffer)
	fmt.Fprintf(b, "// Code generated by encjsongen. DO NOT EDIT.\n\n")
	fmt.Fprintf(b, "package %s\n\n", si.pkg.Name())
	if err := template.Must(template.New("marshal").Parse(tmplMarshalJSON)).Execute(b, si); err != nil {
		return err
	}
	fmt.Fprintf(b, "\n")
	if err := template.Must(template.New("unmarshal").Parse(tmplUnmarshalJSON)).Execute(b, si); err != nil {
		return err
	}

	filename := filepath.Join(si.path, strings.ToLower(si.Receiver)+"_json.go")
	src, err := imports.Process(filename, b.Bytes(), nil)
	if err != nil {
		return err
	}
	return ioutil.WriteFile(filename, src, 0644)
}

func (si *structInfo) Exprs() []string {
	exprs := make([]string, len(si.Aliases))
	for i, a := range si.Aliases {
		exprs[i] = fmt.Sprintf("Alias%s: %s,", a.Target, a.Expr)
	}
	return exprs
}

func (si *structInfo) Assigns() []string {
	exprs := make([]string, len(si.Aliases))
	for i, a := range si.Aliases {
		exprs[i] = fmt.Sprintf("v.%s = %s", a.Target, a.Assign)
	}
	return exprs
}

const tmplMarshalJSON = `func (v *{{.Receiver}}) MarshalJSON() ([]byte, error) {
	type Alias {{.Receiver}}
	return json.Marshal(&struct {
		*Alias
		{{- range .Aliases }}
		Alias{{.Target}} {{.Type}} ` + "`json:" + `"{{.JSONKey}}"` + "`" + `
		{{- end }}
	}{
		Alias: (*Alias)(v),
		{{- range .Exprs }}
		{{.}}
		{{- end }}
	})
}
`

const tmplUnmarshalJSON = `func (v *{{.Receiver}}) UnmarshalJSON(b []byte) error {
	type Alias {{.Receiver}}
	aux := &struct {
		*Alias
		{{- range .Aliases }}
		Alias{{.Target}} {{.Type}} ` + "`json:" + `"{{.JSONKey}}"` + "`" + `
		{{- end }}
	}{
		Alias: (*Alias)(v),
	}
	if err := json.Unmarshal(b, &aux); err != nil {
		return err
	}
	{{- range .Assigns }}
	{{.}}
	{{- end }}
	return nil
}
`
