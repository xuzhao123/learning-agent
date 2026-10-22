// calc 是 D36 编码评测用的小程序：一个只有 add 子命令的命令行计算器。
package main

import (
	"fmt"
	"os"
	"strconv"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "用法：calc add <a> <b>")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "add":
		if len(os.Args) != 4 {
			fmt.Fprintln(os.Stderr, "用法：calc add <a> <b>")
			os.Exit(2)
		}
		a, err1 := strconv.Atoi(os.Args[2])
		b, err2 := strconv.Atoi(os.Args[3])
		if err1 != nil || err2 != nil {
			fmt.Fprintln(os.Stderr, "参数须是整数")
			os.Exit(2)
		}
		fmt.Println(a + b)
	default:
		fmt.Fprintln(os.Stderr, "未知子命令：", os.Args[1])
		os.Exit(2)
	}
}
