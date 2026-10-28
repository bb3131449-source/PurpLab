// purp:name = "test"
// purp:version = "1.0.0 Release"
// purp:author = "purpnet"

package main

import (
	"bufio"
	"fmt"
	"os"
	"time"
)

func main() {
	fmt.Println("╭──────────────────────────────╮")
	fmt.Println("│  PurpNet test tool           │")
	fmt.Println("╰──────────────────────────────╯")
	fmt.Println()

	// 1. Проверка stdout
	fmt.Println("[1/5] stdout: OK")

	// 2. Проверка stderr
	fmt.Fprintln(os.Stderr, "[2/5] stderr: OK")

	// 3. Проверка аргументов
	fmt.Printf("[3/5] args: %v (count=%d)\n", os.Args, len(os.Args))

	// 4. Проверка stdin (интерактив)
	fmt.Print("[4/5] введи что-нибудь и Enter (или пустую строку): ")
	sc := bufio.NewScanner(os.Stdin)
	if sc.Scan() {
		fmt.Printf("      получено: %q\n", sc.Text())
	}

	// 5. Проверка Ctrl+C
	fmt.Println("[5/5] ждём 5 секунд — попробуй Ctrl+C, tool должен умереть...")
	for i := 5; i > 0; i-- {
		fmt.Printf("      %d...\n", i)
		time.Sleep(time.Second)
	}

	// Проверка exit code
	fmt.Println()
	fmt.Println("Завершаюсь с кодом 0.")
	os.Exit(0)
}
