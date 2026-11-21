//go:build !transfer && !writer

package main

// KeyTyper GUI 진입점. Transfer 빌드(-tags transfer)에서는 main_transfer.go 가,
// Writer 빌드(-tags writer)에서는 main_writer.go 가 진입점이 된다.
func main() {
	runKeyTyper()
}
