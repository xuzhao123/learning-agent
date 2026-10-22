package eval

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// D36：编码任务。题目带一份仓库快照：本仓库的某个提交（commit），或 eval/coding/ 下的一个小仓库（fixture）。
// 每个试次在 .data/eval/work/<任务ID>/ 里拿一份新的拷贝（git clone，不用 worktree：沙箱里 .git 要是完整的，git 命令才能用），
// 作为 -workspace 交给 agent，并打开 -bash。跑完后：
//   - 在工作区只读挂载的沙箱里执行校验命令（本程序的 -check 模式，和 verify_feature 同一种隔离）：被测的代码是模型改过的，不在宿主机上直接跑；
//   - 改动存成 .data/eval/<评测ID>/<任务ID>.diff，供归因；拷贝删掉。
// “自称完成”= 这一轮正常结束（模型自己给出了最终回答）；和校验结果放在一起，看自评有多少误报。
// 校验结果存在试次里，重新打分时沿用（拷贝已经删了，不能再跑）。

type Repo struct {
	Commit  string `json:"commit,omitempty"`  // 本仓库的提交
	Fixture string `json:"fixture,omitempty"` // eval/coding/ 下的目录名
	Setup   string `json:"setup,omitempty"`   // 可选：拷贝之后打的补丁（相对项目根目录），算进起点
	Checks  string `json:"checks,omitempty"`  // 可选：eval/coding/checks/ 下的目录名，校验前放进沙箱的 /scratch（模型改不到它）
}

var commitPattern = regexp.MustCompile(`^[0-9a-f]{7,40}$`)
var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

func (r *Repo) check() error {
	if (r.Commit == "") == (r.Fixture == "") {
		return errors.New("repo 须给 commit 或 fixture 其中一个")
	}
	if r.Commit != "" && !commitPattern.MatchString(r.Commit) {
		return errors.New("repo.commit 须是提交的十六进制ID")
	}
	for _, name := range []string{r.Fixture, r.Checks} {
		if name != "" && !namePattern.MatchString(name) {
			return errors.New("repo.fixture、repo.checks 只能是小写字母、数字和连字符")
		}
	}
	if r.Fixture != "" {
		if _, err := os.Stat(filepath.Join("eval", "coding", r.Fixture)); err != nil {
			return fmt.Errorf("找不到 eval/coding/%s", r.Fixture)
		}
	}
	return nil
}

// CommandResult 是一条校验命令的结果。
type CommandResult struct {
	Run      string `json:"run"`
	ExitCode int    `json:"exit_code"`
	Output   string `json:"output"`
}

func workDir(taskID string) string { return filepath.Join(Dir, "work", taskID) }

// prepareRepo 准备一个试次的工作区：拷贝快照、打起点补丁，提交成一个起点（之后的 diff 只有模型的改动）。
func prepareRepo(repo *Repo, dir string) error {
	os.RemoveAll(dir)
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return err
	}
	if repo.Commit != "" {
		if err := git("", "clone", "--quiet", "--no-hardlinks", "--no-checkout", ".", dir); err != nil {
			return err
		}
		if err := git(dir, "checkout", "--quiet", "--detach", repo.Commit); err != nil {
			return err
		}
	} else {
		if err := copyDir(filepath.Join("eval", "coding", repo.Fixture), dir); err != nil {
			return err
		}
		if err := git(dir, "init", "--quiet"); err != nil {
			return err
		}
	}
	if repo.Setup != "" {
		patch, err := filepath.Abs(repo.Setup)
		if err != nil {
			return err
		}
		if err := git(dir, "apply", patch); err != nil {
			return err
		}
	}
	if err := git(dir, "add", "-A"); err != nil {
		return err
	}
	return git(dir, "-c", "user.name=eval", "-c", "user.email=eval@localhost", "commit", "--quiet", "--allow-empty", "-m", "起点")
}

// runChecks 在只读挂载工作区的沙箱里依次跑这道题的校验命令。
func runChecks(c Case, t *Trial, dir string) {
	self, err := os.Executable()
	if err != nil {
		return
	}
	abs, _ := filepath.Abs(dir)
	for _, g := range c.Graders {
		if g.Type != "command" {
			continue
		}
		result := CommandResult{Run: g.Run, ExitCode: -1}
		scratch := dir + ".scratch"
		os.RemoveAll(scratch)
		if c.Repo.Checks != "" {
			if err := copyDir(filepath.Join("eval", "coding", "checks", c.Repo.Checks), scratch); err != nil {
				result.Output = "放检查脚本失败：" + err.Error()
				t.Commands = append(t.Commands, result)
				continue
			}
		}
		scratchAbs, _ := filepath.Abs(scratch)
		cmd := exec.Command(self, "-workspace", abs, "-bash", "-check", g.Run)
		cmd.Env = append(os.Environ(), "AGENT_CHECK_DIR="+scratchAbs)
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		runErr := cmd.Run()
		lines := strings.Split(strings.TrimSpace(out.String()), "\n")
		var parsed struct {
			ExitCode int    `json:"exit_code"`
			Output   string `json:"output"`
			Killed   string `json:"killed"`
		}
		if json.Unmarshal([]byte(lines[len(lines)-1]), &parsed) == nil {
			result.ExitCode, result.Output = parsed.ExitCode, parsed.Output
			if parsed.Killed != "" {
				result.ExitCode, result.Output = -1, parsed.Output+"\n（"+parsed.Killed+"）"
			}
		} else {
			result.Output = fmt.Sprintf("校验没有跑起来（%v）：%s", runErr, tail(out.String(), 1500))
		}
		os.RemoveAll(scratch)
		t.Commands = append(t.Commands, result)
	}
}

// saveDiff 把模型的改动（相对起点，含新文件）存下来，返回路径。
func saveDiff(recID, taskID, dir string) string {
	if git(dir, "add", "-A") != nil {
		return ""
	}
	diff, err := exec.Command("git", "-C", dir, "diff", "--cached", "--stat", "--patch").Output()
	if err != nil {
		return ""
	}
	path := filepath.Join(Dir, recID, taskID+".diff")
	if os.MkdirAll(filepath.Dir(path), 0o700) != nil || os.WriteFile(path, diff, 0o600) != nil {
		return ""
	}
	return path
}

func git(dir string, args ...string) error {
	if dir != "" {
		args = append([]string{"-C", dir}, args...)
	}
	out, err := exec.Command("git", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %s：%v %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func copyDir(from, to string) error {
	return filepath.WalkDir(from, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(from, path)
		target := filepath.Join(to, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		info, _ := d.Info()
		return os.WriteFile(target, data, info.Mode().Perm())
	})
}

func tail(s string, n int) string {
	if len(s) > n {
		return "…" + s[len(s)-n:]
	}
	return s
}
