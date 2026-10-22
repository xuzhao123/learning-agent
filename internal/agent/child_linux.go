//go:build linux

package agent

import "syscall"

// 子进程放进自己的进程组（见 RunChild）；父进程死了（包括 kill -9），内核给子进程发 SIGINT，
// 子进程照常收尾：回填工具结果、写好检查点、释放任务锁，不会没人管地一直占着锁跑下去。
// Pdeathsig 跟的是创建子进程的那个线程，所以 RunChild 从 Start 到 Wait 锁住当前线程。
func childAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGINT}
}
