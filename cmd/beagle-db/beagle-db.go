// Copyright 2019 The DutchSec Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"go/ast"
	"go/format"
	"go/token"
	"go/types"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"

	"golang.org/x/tools/go/packages"
)

var (
	tableName = flag.String("table", "", "")
	tableKey  = flag.String("key", "", "")

	typeNames   = flag.String("type", "", "comma-separated list of type names; must be set")
	output      = flag.String("output", "", "output file name; default srcdir/<type>_string.go")
	trimprefix  = flag.String("trimprefix", "", "trim the `prefix` from the generated constant names")
	linecomment = flag.Bool("linecomment", false, "use line comment text as printed text when present")
	buildTags   = flag.String("tags", "", "comma-separated list of build tags to apply")
)

// Usage is a replacement usage function for the flags package.
func Usage() {
	fmt.Fprintf(os.Stderr, "Usage of %s:\n", os.Args[0])
	fmt.Fprintf(os.Stderr, "\tbeagle db [directory|files]\n")
	fmt.Fprintf(os.Stderr, "Flags:\n")
	flag.PrintDefaults()
}

func main() {
	log.SetFlags(0)
	log.SetPrefix(fmt.Sprintf("%s: ", os.Args[0]))
	flag.Usage = Usage
	flag.Parse()

	if len(*typeNames) == 0 {
		flag.Usage()
		os.Exit(2)
	}

	types := strings.Split(*typeNames, ",")
	var tags []string
	if len(*buildTags) > 0 {
		tags = strings.Split(*buildTags, ",")
	}

	// We accept either one directory or a list of files. Which do we have?
	args := flag.Args()
	if len(args) == 0 {
		// Default: process whole package in current directory.
		args = []string{"."}
	}

	// Parse the package once.
	var dir string
	g := Generator{
		trimPrefix:  *trimprefix,
		lineComment: *linecomment,
	}

	// TODO(suzmue): accept other patterns for packages (directories, list of files, import paths, etc).
	if len(args) == 1 && isDirectory(args[0]) {
		dir = args[0]
	} else {
		if len(tags) != 0 {
			log.Fatal("-tags option applies only to directories, not when files are specified")
		}
		dir = filepath.Dir(args[0])
	}

	g.parsePackage(args, tags)

	// Print the header and package clause.
	g.Printf("// Code generated by \"beagle db %s\"; DO NOT EDIT.\n", strings.Join(os.Args[1:], " "))
	g.Printf("\n")
	g.Printf("package %s", g.pkg.name)
	g.Printf("\n")
	g.Printf(`import (
)
`) // Used by all methods.

	// Run generate for each type.
	for _, typeName := range types {
		g.generate(typeName)
	}

	// Format the output.
	src := g.format()

	// Write to file.
	outputName := *output
	if outputName == "" {
		baseName := fmt.Sprintf("%s_gen.go", types[0])
		outputName = filepath.Join(dir, strings.ToLower(baseName))
	}

	var err error

	src, err = goimports(outputName, src)
	if err != nil {
		log.Fatalf("Error executing goimport: %s", err.Error())
	}

	if err := ioutil.WriteFile(outputName, src, 0644); err != nil {
		log.Fatalf("writing output: %s", err)
	}
}

// isDirectory reports whether the named file is a directory.
func isDirectory(name string) bool {
	info, err := os.Stat(name)
	if err != nil {
		log.Fatal(err)
	}
	return info.IsDir()
}

// Generator holds the state of the analysis. Primarily used to buffer
// the output for format.Source.
type Generator struct {
	buf bytes.Buffer // Accumulated output.
	pkg *Package     // Package we are scanning.

	trimPrefix  string
	lineComment bool
}

func (g *Generator) Printf(format string, args ...interface{}) {
	fmt.Fprintf(&g.buf, format, args...)
}

// File holds a single parsed file and associated data.
type File struct {
	pkg  *Package  // Package to which this file belongs.
	file *ast.File // Parsed AST.
	// These fields are reset for each type being generated.
	typeName string  // Name of the constant type.
	values   []Value // Accumulator for constant values of that type.

	types map[string][]string

	trimPrefix  string
	lineComment bool
}

type Package struct {
	dir      string
	name     string
	defs     map[*ast.Ident]types.Object
	files    []*File
	typesPkg *types.Package
}

// addPackage adds a type checked Package and its syntax files to the generator.
func (g *Generator) addPackage(pkg *packages.Package) {
	g.pkg = &Package{
		name:  pkg.Name,
		defs:  pkg.TypesInfo.Defs,
		files: make([]*File, len(pkg.Syntax)),
	}

	for i, file := range pkg.Syntax {
		g.pkg.files[i] = &File{
			file:        file,
			pkg:         g.pkg,
			trimPrefix:  g.trimPrefix,
			lineComment: g.lineComment,
			types:       map[string][]string{},
		}
	}
}

// prefixDirectory places the directory name on the beginning of each name in the list.
func prefixDirectory(directory string, names []string) []string {
	if directory == "." {
		return names
	}
	ret := make([]string, len(names))
	for i, name := range names {
		ret[i] = filepath.Join(directory, name)
	}
	return ret
}

// parsePackage analyzes the single package constructed from the patterns and tags.
// parsePackage exits if there is an error.
func (g *Generator) parsePackage(patterns []string, tags []string) {
	cfg := &packages.Config{
		Mode: packages.LoadSyntax,
		// TODO: Need to think about constants in test files. Maybe write type_string_test.go
		// in a separate pass? For later.
		Tests:      false,
		BuildFlags: []string{fmt.Sprintf("-tags=%s", strings.Join(tags, " "))},
	}
	pkgs, err := packages.Load(cfg, patterns...)
	if err != nil {
		log.Fatal(err)
	}
	if len(pkgs) != 1 {
		log.Fatalf("error: %d packages found", len(pkgs))
	}
	g.addPackage(pkgs[0])
}

// genDecl processes one declaration clause.
func (f *File) genDecl(node ast.Node) bool {
	decl, ok := node.(*ast.GenDecl)
	if !ok || decl.Tok != token.TYPE {
		// We only care about const declarations.
		return true
	}

	for _, spec := range decl.Specs {
		if ts, ok := spec.(*ast.TypeSpec); ok {
			typ := ts.Name.String()
			if typ != f.typeName {
				// This is not the type we're looking for.
				continue
			}

			columns := []string{}
			if st, ok := ts.Type.(*ast.StructType); ok {
				for _, field := range st.Fields.List {
					if field.Tag == nil {
						continue
					}

					tag := field.Tag.Value
					tag = strings.TrimPrefix(tag, "`")
					tag = strings.TrimSuffix(tag, "`")

					value, ok := reflect.StructTag(tag).Lookup("db")
					if !ok {
						continue
					}

					columns = append(columns, value)
				}
			}

			f.types[typ] = columns
		}
	}

	return false
}

// generate produces the String method for the named type.
func (g *Generator) generate(typeName string) {
	values := make([]Value, 0, 100)
	for _, file := range g.pkg.files {
		// Set the state for this run of the walker.
		file.typeName = typeName
		file.values = nil
		if file.file != nil {
			ast.Inspect(file.file, file.genDecl)
			values = append(values, file.values...)
		}

		if len(file.types) == 0 {
			continue
		}

		nameize := func(name string) string {
			value := ""

			parts := strings.Split(name, "_")
			for _, part := range parts {
				if part == "id" {
					value += "ID"
					continue
				}

				value += strings.Title(part)
			}

			return value
		}

		g.Printf("const (\n")
		for name, columns := range file.types {
			for _, column := range columns {
				g.Printf("%s%s = \"`%s`.`%s`\"\n", name, nameize(column), *tableName, column)
			}
		}
		g.Printf(")\n")

		for name, columns := range file.types {
			g.Printf("var (\n")

			g.Printf("query%sDelete db.Query = \"UPDATE %s SET active = 0 ", name, *tableName)
			g.Printf(" WHERE `%s`=:%s\"", *tableKey, *tableKey)
			g.Printf("\n")

			g.Printf("query%sSelect db.Query = \"SELECT ", name)
			for i, column := range columns {
				if i > 0 {
					g.Printf(", ")
				}

				g.Printf("`%s`", column)
			}

			g.Printf(" FROM %s\"", *tableName)
			g.Printf("\n")

			g.Printf("query%sUpdate db.Query = \"UPDATE %s SET ", name, *tableName)
			for i, column := range columns {
				if i > 0 {
					g.Printf(", ")
				}

				g.Printf("`%s`=:%s", column, column)
			}

			g.Printf(" WHERE %s=:%s	\"", *tableKey, *tableKey)
			g.Printf("\n")

			g.Printf("query%sInsert db.Query = \"INSERT INTO %s (", name, *tableName)
			for i, column := range columns {
				if i > 0 {
					g.Printf(", ")
				}

				g.Printf("`%s`", column)
			}

			g.Printf(") VALUES (")
			for i, column := range columns {
				if i > 0 {
					g.Printf(", ")
				}

				g.Printf(":%s", column)
			}

			g.Printf(")\"")
			g.Printf("\n")

			g.Printf("query%sInsertOrUpdate db.Query = \"INSERT INTO %s (", name, *tableName)
			for i, column := range columns {
				if i > 0 {
					g.Printf(", ")
				}

				g.Printf("`%s`", column)
			}

			g.Printf(") VALUES (")
			for i, column := range columns {
				if i > 0 {
					g.Printf(", ")
				}

				g.Printf(":%s", column)
			}

			g.Printf(") ON DUPLICATE KEY UPDATE ")

			for i, column := range columns {
				if column == "created_at" {
					continue
				}

				if i > 0 {
					g.Printf(", ")
				}

				g.Printf("`%s`=:%s", column, column)
			}

			g.Printf("\"")

			g.Printf("\n")

			g.Printf(")\n")

			g.Printf("func (s *%s) Get(tx *sqlx.Tx, q db.Query, params []interface{}) error {\n", name)
			g.Printf(`
			stmt, err := tx.Preparex(string(q))
			if err != nil {
				return err
			}

			if err := stmt.Get(s, params...); err != nil {
				return err
			}

		return nil
		}`)
			g.Printf("\n")
			g.Printf("\n")

			g.Printf("func (s *%s) Update(tx *sqlx.Tx) error {\n", name)

			for _, column := range columns {
				// actually check field name (UpdatedAt), instead of
				// column name
				if column == "updated_at" {
					g.Printf("s.UpdatedAt = time.Now()\n")
				}
			}

			g.Printf(` _, err := tx.NamedExec(string(query%sUpdate), s)
			return err
		}
		`, name)

			// should we combine update and insert or update?
			g.Printf("func (s *%s) InsertOrUpdate(tx *sqlx.Tx) error {\n", name)

			for _, column := range columns {
				// actually check field name (CreatedAt), instead of
				// column name
				if column == "created_at" {
				} else if column == "updated_at" {
					g.Printf("s.UpdatedAt = time.Now()\n")
				}
			}

			g.Printf(`
			_, err := tx.NamedExec(string(query%sInsertOrUpdate), s)
			return err
		}
		`, name)

			g.Printf("func (s *%s) Insert(tx *sqlx.Tx) error {\n", name)

			for _, column := range columns {
				// actually check field name (CreatedAt), instead of
				// column name
				if column == "created_at" {
					g.Printf("s.CreatedAt = time.Now()\n")
				} else if column == "updated_at" {
					g.Printf("s.UpdatedAt = time.Now()\n")
				}
			}

			g.Printf(`
			_, err := tx.NamedExec(string(query%sInsert), s)
			return err
		}
		`, name)

			g.Printf(`func (s *%s) Delete(tx *sqlx.Tx) error {`, name)
			g.Printf(`_, err := tx.NamedExec(string(query%sDelete), s)
			return err
		}
		`, name)

			// single (alert) plural (alerts)
			g.Printf(`func Query%ss() db.Queryx {`, name)

			g.Printf("return db.SelectQuery(\"%s\").\n", *tableName)
			g.Printf("Fields(\n")

			for name, columns := range file.types {
				for _, column := range columns {
					g.Printf("%s%s,\n", name, nameize(column))
				}
			}

			g.Printf(")\n")
			g.Printf("}\n")

			/* g.Printf(`return db.Queryx{
					Query:  query%sSelect,
					Params: []interface{}{},
				}
			}`, name)
			*/

		}
	}
}

// format returns the gofmt-ed contents of the Generator's buffer.
func (g *Generator) format() []byte {
	src, err := format.Source(g.buf.Bytes())
	if err != nil {
		// Should never happen, but can arise when developing this code.
		// The user can compile the output to see the error.
		log.Printf("warning: internal error: invalid Go generated: %s", err)
		log.Printf("warning: compile the package to analyze the error")
		return g.buf.Bytes()
	}
	return src
}

// Run goimports to format and update imports statements in generated code.
func goimports(filename string, inputBytes []byte) (outputBytes []byte, err error) {
	if false {
		return inputBytes, nil
	}
	cmd := exec.Command("goimports")
	// cmd := exec.Command(os.Getenv("GOPATH") + "/bin/goimports")
	input, _ := cmd.StdinPipe()
	output, _ := cmd.StdoutPipe()
	cmderr, _ := cmd.StderrPipe()
	err = cmd.Start()
	if err != nil {
		return
	}
	input.Write(inputBytes)
	input.Close()

	outputBytes, _ = ioutil.ReadAll(output)
	errors, _ := ioutil.ReadAll(cmderr)
	if len(errors) > 0 {
		errors := strings.Replace(string(errors), "<standard input>", filename, -1)
		log.Printf("Syntax errors in generated code:\n%s", errors)
		return inputBytes, nil
	}

	return
}

// Value represents a declared constant.
type Value struct {
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

func (v *Value) String() string {
	return v.str
}

// byValue lets us sort the constants into increasing order.
// We take care in the Less method to sort in signed or unsigned order,
// as appropriate.
type byValue []Value

func (b byValue) Len() int      { return len(b) }
func (b byValue) Swap(i, j int) { b[i], b[j] = b[j], b[i] }
func (b byValue) Less(i, j int) bool {
	if b[i].signed {
		return int64(b[i].value) < int64(b[j].value)
	}
	return b[i].value < b[j].value
}
