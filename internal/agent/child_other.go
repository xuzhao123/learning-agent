//go:build !linux

package agent

import "syscall"

// 只有 Linux 有 Pdeathsig；别的系统上父进程死了，子进程会跑完这一轮再退出。
func childAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}
