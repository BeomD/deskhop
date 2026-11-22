//go:build !transfer && !writer && !autodrop

package main

// KeyTyper GUI 진입점. 다른 진입점은 빌드 태그로 고른다.
//
//	-tags transfer → main_transfer.go (Transfer.exe)
//	-tags writer   → main_writer.go   (Writer.exe)
//	-tags autodrop → main_autodrop.go (AutoDrop.exe)
func main() {
	runKeyTyper()
}
