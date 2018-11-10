// Copyright (c) 2017, Daniel Martí <mvdan@mvdan.cc>
// See LICENSE for licensing information

package main

import (
	"bytes"
	"context"
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/printer"
	"go/token"
	"go/types"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"

	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

var (
	rawPrinter = printer.Config{Mode: printer.RawFormat}

	fastTest = false
)

type reducer struct {
	tdir      string
	logOut    io.Writer
	matchRe   *regexp.Regexp
	shellProg *syntax.File

	fset     *token.FileSet
	origFset *token.FileSet
	pkg      *ast.Package
	files    []*ast.File
	file     *ast.File

	tconf types.Config
	info  *types.Info

	useIdents map[types.Object][]*ast.Ident
	revDefs   map[types.Object]*ast.Ident
	parents   map[ast.Node]ast.Node

	dstBuf *bytes.Buffer

	tmpFiles map[*ast.File]*os.File

	tries     int
	didChange bool

	deleteKeepUnderscore func()
	deleteKeepUnchanged  func()

	tried map[string]bool

	walker
}

var errNoReduction = fmt.Errorf("could not reduce program")

func reduce(dir, match string, logOut io.Writer, shellStr string) error {
	r := &reducer{
		tdir:   dir,
		logOut: logOut,
		tried:  make(map[string]bool, 16),
		dstBuf: bytes.NewBuffer(nil),
	}
	var err error
	if r.tdir, err = ioutil.TempDir("", "goreduce"); err != nil {
		return err
	}
	defer os.RemoveAll(r.tdir)
	if r.matchRe, err = regexp.Compile(match); err != nil {
		return err
	}
	r.fset = token.NewFileSet()
	pkgs, err := parser.ParseDir(r.fset, dir, nil, parser.ParseComments)
	if err != nil {
		return err
	}
	if len(pkgs) != 1 {
		return fmt.Errorf("expected 1 package, got %d", len(pkgs))
	}
	for _, pkg := range pkgs {
		r.pkg = pkg
	}
	switch {
	case shellStr != "":
	case r.pkg.Name == "main":
		shellStr = shellStrRun
	default:
		shellStr = shellStrBuild
	}
	r.shellProg, err = syntax.NewParser().Parse(strings.NewReader(shellStr), "")
	if err != nil {
		return err
	}
	r.origFset = token.NewFileSet()
	parser.ParseDir(r.origFset, dir, nil, 0)

	var restoreMain func()
	r.tmpFiles = make(map[*ast.File]*os.File, len(r.pkg.Files))
	for fpath, file := range r.pkg.Files {
		r.files = append(r.files, file)
		tfname := filepath.Join(r.tdir, filepath.Base(fpath))
		f, err := os.Create(tfname)
		if err != nil {
			return err
		}
		if err := rawPrinter.Fprint(f, r.fset, file); err != nil {
			return err
		}
		r.tmpFiles[file] = f
		defer f.Close()
	}
	r.tconf.Importer = importer.Default()
	r.tconf.Error = func(err error) {
		if terr, ok := err.(types.Error); ok && terr.Soft {
			// don't stop type-checking on soft errors
			return
		}
		//panic("types.Check should not error here: " + err.Error())
	}
	// Check that the output matches before we apply any changes
	if !fastTest {
		if err := r.checkRun(); err != nil {
			return err
		}
	}
	r.fillParents()
	if anyChanges := r.reduceLoop(); !anyChanges {
		return errNoReduction
	}
	if restoreMain != nil {
		restoreMain()
	}
	for astFile := range r.tmpFiles {
		astFile.Name.Name = r.pkg.Name
		fname := r.fset.Position(astFile.Pos()).Filename
		f, err := os.Create(fname)
		if err != nil {
			return err
		}
		if err := printer.Fprint(f, r.fset, astFile); err != nil {
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
	}
	return nil
}

func (r *reducer) logChange(node ast.Node, format string, a ...interface{}) {
	if *verbose {
		pos := r.origFset.Position(node.Pos())
		times := "first try"
		if r.tries != 1 {
			times = fmt.Sprintf("%d tries", r.tries)
		}
		fmt.Fprintf(r.logOut, "%s:%d: %s (%s)\n",
			pos.Filename, pos.Line, fmt.Sprintf(format, a...), times)
	}
	r.tries = 0
}

func (r *reducer) checkRun() error {
	out := r.runCmd()
	if out == nil {
		return fmt.Errorf("expected an error to occur")
	}
	if !r.matchRe.Match(out) {
		return fmt.Errorf("error does not match:\n%s", string(out))
	}
	return nil
}

func (r *reducer) okChangeNoUndo() bool {
	if r.didChange {
		return false
	}
	r.dstBuf.Reset()
	if err := rawPrinter.Fprint(r.dstBuf, r.fset, r.file); err != nil {
		return false
	}
	newSrc := r.dstBuf.String()
	if r.tried[newSrc] {
		return false
	}
	r.tries++
	r.tried[newSrc] = true
	f := r.tmpFiles[r.file]
	if err := f.Truncate(0); err != nil {
		return false
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return false
	}
	if _, err := f.Write(r.dstBuf.Bytes()); err != nil {
		return false
	}
	if err := r.checkRun(); err != nil {
		return false
	}
	// Reduction worked
	r.didChange = true
	return true
}

func (r *reducer) okChange() bool {
	if r.okChangeNoUndo() {
		r.deleteKeepUnderscore = nil
		r.deleteKeepUnchanged = nil
		return true
	}
	if r.deleteKeepUnderscore != nil {
		r.deleteKeepUnderscore()
		r.deleteKeepUnderscore = nil
		return r.okChange()
	}
	if r.deleteKeepUnchanged != nil {
		r.deleteKeepUnchanged()
		r.deleteKeepUnchanged = nil
	}
	return false
}

func (r *reducer) reduceLoop() (anyChanges bool) {
	r.info = &types.Info{
		Defs: make(map[*ast.Ident]types.Object),
		Uses: make(map[*ast.Ident]types.Object),
	}
	for {
		// Update type info after the AST changes
		r.tconf.Check(r.tdir, r.fset, r.files, r.info)
		r.fillObjs()

		r.didChange = false
		r.walk(r.pkg, r.reduceNode)
		if !r.didChange {
			if *verbose {
				fmt.Fprintf(r.logOut, "gave up after %d final tries\n", r.tries)
			}
			return
		}
		anyChanges = true
	}
}

func (r *reducer) fillObjs() {
	r.revDefs = make(map[types.Object]*ast.Ident, len(r.info.Defs))
	for id, obj := range r.info.Defs {
		if obj == nil {
			continue
		}
		r.revDefs[obj] = id
	}
	r.useIdents = make(map[types.Object][]*ast.Ident, len(r.info.Uses)/2)
	for id, obj := range r.info.Uses {
		if pkg := obj.Pkg(); pkg == nil || pkg.Name() != r.pkg.Name {
			// builtin or declared outside of our pkg
			continue
		}
		r.useIdents[obj] = append(r.useIdents[obj], id)
	}
}

func (r *reducer) fillParents() {
	r.parents = make(map[ast.Node]ast.Node)
	stack := make([]ast.Node, 1, 32)
	ast.Inspect(r.pkg, func(node ast.Node) bool {
		if node == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		r.parents[node] = stack[len(stack)-1]
		stack = append(stack, node)
		return true
	})
}

func (r *reducer) runCmd() []byte {
	var buf bytes.Buffer
	runner, err := interp.New(interp.Dir(r.tdir), interp.StdIO(nil, &buf, &buf))
	if err != nil {
		panic(err)
	}
	runner.Run(context.TODO(), r.shellProg)
	return buf.Bytes()
}

func (r *reducer) exprRef(expr ast.Expr) *ast.Expr {
	parent := r.parents[expr]
	v := reflect.ValueOf(parent).Elem()
	for i := 0; i < v.NumField(); i++ {
		fld := v.Field(i)
		switch fld.Type().Kind() {
		case reflect.Slice:
			for i := 0; i < fld.Len(); i++ {
				ifld := fld.Index(i)
				if ifld.Interface() == expr {
					ptr, _ := ifld.Addr().Interface().(*ast.Expr)
					return ptr
				}
			}
		case reflect.Interface:
			if fld.Interface() == expr {
				ptr, _ := fld.Addr().Interface().(*ast.Expr)
				return ptr
			}
		}
	}
	return nil
}

func (r *reducer) stmtRef(stmt ast.Stmt) *ast.Stmt {
	parent := r.parents[stmt]
	v := reflect.ValueOf(parent).Elem()
	for i := 0; i < v.NumField(); i++ {
		fld := v.Field(i)
		switch fld.Type().Kind() {
		case reflect.Slice:
			for i := 0; i < fld.Len(); i++ {
				ifld := fld.Index(i)
				if ifld.Interface() == stmt {
					ptr, _ := ifld.Addr().Interface().(*ast.Stmt)
					return ptr
				}
			}
		case reflect.Interface:
			if fld.Interface() == stmt {
				ptr, _ := fld.Addr().Interface().(*ast.Stmt)
				return ptr
			}
		}
	}
	return nil
}
