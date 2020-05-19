package main

import (
	"fmt"
	"os"
	"regexp"
	"path/filepath"
	"go/parser"
	"go/token"
	"strings"

	"gopkg.in/yaml.v2"
	"k8s.io/kubernetes/test/conformance/behaviors"
)



func genmap (o *options) error {
	var files []string
	err := filepath.Walk("test/e2e",
		func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			r, _ := regexp.Compile(".+.go$")
			if r.MatchString(path) {
				files = append(files, path)
			}
			return nil
		})
	if err != nil {
		return err
	}

	var thing = make(map[string]behaviors.ConformanceData)


	for _, f := range files {
		fset := token.NewFileSet()
		freader, err := os.Open(f)
		if err != nil {
			return err
		}
		defer freader.Close()
		g, err := parser.ParseFile(fset, f, freader, parser.ParseComments)
		if err != nil {
			return err
		}
		for _, comment := range g.Comments {
			if strings.Contains(comment.Text(), "Testname:") {
				data := behaviors.CommentToConformanceData(comment.Text())
				position := fset.Position(comment.End())
				thing[fmt.Sprintf("%s#%d", position.Filename, position.Line)] = *data
			}
		}
	}
	b, err := yaml.Marshal(thing)
	if err != nil {
		return err
	}
	fmt.Println(string(b))

	return nil
}
