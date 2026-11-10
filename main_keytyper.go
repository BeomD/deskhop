//go:build !transfer && !hotkey

package main

// KeyTyper GUI 진입점. Transfer 빌드(-tags transfer)에서는 이 파일 대신
// main_transfer.go 가 진입점이 된다.
func main() {
	runKeyTyper()
}
