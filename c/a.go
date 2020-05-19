// package main

// import (
// 	"fmt"
// 	"runtime"
// 	"os"
// 	"go/parser"
// 	"go/token"
// )


// func getline() (string, int) {
// 	_,f,l,_ := runtime.Caller(1)
// 	return f, l
// }

// /*
//  arstast arst art
// */
// func main() {
// 	f, l := getline()
// 	fmt.Println("Hello, playground")
// 	fset := token.NewFileSet()
// 	freader, _ := os.Open(f)
// 	defer freader.Close()
// 	g, _ := parser.ParseFile(fset, f, freader, parser.ParseComments)
// 	fmt.Printf("%v", fset.Position(g.Comments[0].End()).Line)
// 	fmt.Printf("%v,%v", f, l)
// }
