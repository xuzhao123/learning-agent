package sandbox

import (
	"encoding/binary"
	"runtime"

	"golang.org/x/sys/unix"
)

// seccomp 是沙箱的最后一层：namespace 限制“看得到什么”，cgroup 限制“用多少”，seccomp 限制“能做哪些系统调用”。
// 这里不用 libseccomp（机器上没有 C 编译器），直接拼一段经典 BPF 程序交给 bwrap 的 --seccomp：
//
//	检查 CPU 架构（不是本机架构就杀掉进程，防止用 32 位调用号绕过）
//	取系统调用号，逐个和黑名单比较，命中就返回 EPERM
//	其余放行
//
// 黑名单里是普通程序用不到、却常被用来逃逸或攻击内核的调用。
var denied = []uintptr{
	unix.SYS_PTRACE, unix.SYS_PROCESS_VM_READV, unix.SYS_PROCESS_VM_WRITEV, // 读写其他进程的内存
	unix.SYS_MOUNT, unix.SYS_UMOUNT2, unix.SYS_PIVOT_ROOT, // 改挂载
	unix.SYS_UNSHARE, unix.SYS_SETNS, // 新建或进入 namespace
	unix.SYS_BPF, unix.SYS_PERF_EVENT_OPEN, unix.SYS_USERFAULTFD, // 内核攻击面大、常见于提权利用
	unix.SYS_IO_URING_SETUP, unix.SYS_IO_URING_ENTER, unix.SYS_IO_URING_REGISTER,
	unix.SYS_KEYCTL, unix.SYS_ADD_KEY, unix.SYS_REQUEST_KEY, // 内核密钥环
	unix.SYS_KEXEC_LOAD, unix.SYS_INIT_MODULE, unix.SYS_FINIT_MODULE, unix.SYS_DELETE_MODULE, // 换内核、加载模块
	unix.SYS_OPEN_BY_HANDLE_AT, unix.SYS_NAME_TO_HANDLE_AT, // 绕过路径直接按句柄打开文件
	unix.SYS_SWAPON, unix.SYS_SWAPOFF, unix.SYS_REBOOT,
}

// 经典 BPF 指令：code(2) jt(1) jf(1) k(4)，小端。seccomp_data 里 nr 在偏移 0，arch 在偏移 4。
const (
	ldAbs    = 0x20 // BPF_LD | BPF_W | BPF_ABS
	jeqK     = 0x15 // BPF_JMP | BPF_JEQ | BPF_K
	retK     = 0x06 // BPF_RET | BPF_K
	retAllow = 0x7fff0000
	retErrno = 0x00050000 | uint32(unix.EPERM)
	retKill  = 0x80000000 // SECCOMP_RET_KILL_PROCESS
)

func seccompProgram() []byte {
	arch := uint32(unix.AUDIT_ARCH_AARCH64)
	if runtime.GOARCH == "amd64" {
		arch = unix.AUDIT_ARCH_X86_64
	}
	var out []byte
	put := func(code uint16, jt, jf uint8, k uint32) {
		out = binary.LittleEndian.AppendUint16(out, code)
		out = append(out, jt, jf)
		out = binary.LittleEndian.AppendUint32(out, k)
	}
	put(ldAbs, 0, 0, 4)      // A = arch
	put(jeqK, 1, 0, arch)    // 是本机架构就跳过下一条
	put(retK, 0, 0, retKill) // 否则杀掉
	put(ldAbs, 0, 0, 0)      // A = 系统调用号
	for _, nr := range denied {
		put(jeqK, 0, 1, uint32(nr)) // 相等则执行下一条（拒绝），否则跳过它
		put(retK, 0, 0, retErrno)
	}
	put(retK, 0, 0, retAllow)
	return out
}
