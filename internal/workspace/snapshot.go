package workspace

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// D29 文件快照：edit_file、write_file 改动文件之前，先把它当时的内容存一份；按轮撤销时据此把文件放回去。
// 快照按“轮”存放：一轮就是对话里的一次提问（一个检查点ID），目录是 <检查点目录>/<轮次ID>.files/：
//
//	changes/<任务ID>.json   这一轮里这个进程改过哪些文件：改之前的内容、最近一次写入后的内容
//	blobs/<sha256>          改之前的文件内容，按哈希存，同一份内容只存一次
//
// 子 agent 是另一个进程，但它的改动属于派出它的那一轮：它写在父 agent 这一轮的目录里，清单按自己的任务ID分开，两个进程不用抢同一个文件。
// 只覆盖这两个工具：沙箱里的 bash 也能改工作区，那些改动没有快照，撤销时靠“当前内容对不上就拒绝”避免覆盖它们。
var (
	SnapshotDir  string // 本轮的快照目录；空表示不存快照（实验、没有检查点的运行）
	SnapshotTask string // 本进程的任务ID：清单的文件名

	changes *Changes // 本进程这一轮的清单，第一次写入时读出或新建
)

// Changes 是一个进程在一轮里改过的文件。
type Changes struct {
	Workspace string             `json:"workspace"` // 工作区的绝对路径：撤销时在这里恢复
	Files     map[string]*Change `json:"files"`     // 相对路径 → 改动
}

// Change 记录一个文件在这一轮里的第一次改动之前和最后一次改动之后。中间改了几次不重要：撤销只需要“之前”，核对只需要“之后”。
type Change struct {
	Created bool      `json:"created,omitempty"` // 改之前不存在：撤销时删除
	Before  string    `json:"before,omitempty"`  // 改之前内容的 sha256（blobs 里的文件名）
	After   string    `json:"after,omitempty"`   // 最近一次写入后内容的 sha256；空表示写入结果没有确认（进程死在写入途中）
	First   time.Time `json:"first"`             // 第一次快照的时间：几份清单改过同一个文件时，最早的那份“之前”才是这一段开始前的样子
	Last    time.Time `json:"last"`              // 最近一次写入的时间：最晚的那份“之后”才是现在应有的样子
}

func sha(data []byte) string {
	sum := sha256.Sum256(data)
	return fmt.Sprintf("%x", sum)
}

// writeAtomic 先写临时文件再改名：读到的要么是旧的完整版本，要么是新的。
func writeAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(path+".tmp", data, 0o600); err != nil {
		return err
	}
	return os.Rename(path+".tmp", path)
}

func loadChanges(path string) (*Changes, error) {
	c := &Changes{Files: map[string]*Change{}}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, c); err != nil {
		return nil, fmt.Errorf("%s 不是有效的快照清单：%w", path, err)
	}
	if c.Files == nil {
		c.Files = map[string]*Change{}
	}
	return c, nil
}

func (c *Changes) save(path string) error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(path, append(data, '\n'))
}

func changesPath() string { return filepath.Join(SnapshotDir, "changes", SnapshotTask+".json") }

// snapshot 在持有 mu、真正写入之前调用：这一轮第一次改这个文件时先存下原来的内容。
// 快照写不进去就不改文件——之后撤销不了的改动，不如不做。续跑同一轮时接着用磁盘上的清单，“之前”仍是这一轮开始时的样子。
func snapshot(path string, current []byte, exists bool) error {
	if SnapshotDir == "" {
		return nil
	}
	if changes == nil {
		loaded, err := loadChanges(changesPath())
		if err != nil {
			return err
		}
		loaded.Workspace, changes = Dir, loaded
	}
	now := time.Now().UTC()
	c := changes.Files[path]
	if c == nil {
		c = &Change{Created: !exists, First: now}
		if exists {
			c.Before = sha(current)
			blob := filepath.Join(SnapshotDir, "blobs", c.Before)
			if _, err := os.Stat(blob); err != nil {
				if err := writeAtomic(blob, current); err != nil {
					return err
				}
			}
		}
		changes.Files[path] = c
	}
	// 写入结果确认之前先清空“之后”：进程如果死在写文件的途中，撤销时就不拿一个不确定的哈希去核对它。
	c.After, c.Last = "", now
	return changes.save(changesPath())
}

// written 在写入成功之后调用（持有 mu）：记下现在的内容，撤销时用它核对文件之后有没有被别人改过。
// 这里失败时文件已经改了，只打印：结果仍如实返回给模型，撤销时这个文件不做核对。
func written(path string, content []byte) {
	if SnapshotDir == "" || changes == nil || changes.Files[path] == nil {
		return
	}
	c := changes.Files[path]
	c.After, c.Last = sha(content), time.Now().UTC()
	if err := changes.save(changesPath()); err != nil {
		fmt.Printf("Snapshot error: path=%s error=%v\n", path, err)
	}
}

// Plan 是撤销一段轮次要做的事：每个文件放回这一段开始之前的样子。
type Plan struct {
	Workspace string   `json:"workspace"`
	Restore   []string `json:"restore"`   // 写回原来的内容
	Delete    []string `json:"delete"`    // 这一段里新建的，删除
	Unchanged []string `json:"unchanged"` // 已经是原来的样子（比如上次撤销做到一半）：跳过
	Conflicts []string `json:"conflicts"` // 最后一次写入之后又被改过（bash、人或别的程序）：整次撤销都不做

	root *os.Root
	from map[string]string // 要写回的文件 → 它的“之前”所在的 blob 路径
}

type merged struct {
	first, last *Change
	blob        string
}

// PlanRewind 读出这些轮次目录里的全部清单，按文件合并：最早的“之前”是要恢复成的样子，最晚的“之后”是现在应有的样子。
// 再和磁盘上的当前内容比对：对得上“之后”才恢复，已经是“之前”就跳过，两个都对不上算冲突。
func PlanRewind(dirs []string) (*Plan, error) {
	files := map[string]*merged{}
	workspace := ""
	for _, dir := range dirs {
		lists, _ := filepath.Glob(filepath.Join(dir, "changes", "*.json"))
		for _, list := range lists {
			c, err := loadChanges(list)
			if err != nil {
				return nil, err
			}
			if len(c.Files) == 0 {
				continue
			}
			if workspace != "" && c.Workspace != workspace {
				return nil, fmt.Errorf("这几轮改的是不同的工作区（%s 与 %s），不能一起撤销", workspace, c.Workspace)
			}
			workspace = c.Workspace
			for path, change := range c.Files {
				m := files[path]
				if m == nil {
					m = &merged{}
					files[path] = m
				}
				if m.first == nil || change.First.Before(m.first.First) {
					m.first, m.blob = change, filepath.Join(dir, "blobs", change.Before)
				}
				if m.last == nil || change.Last.After(m.last.Last) {
					m.last = change
				}
			}
		}
	}
	plan := &Plan{Workspace: workspace, Restore: []string{}, Delete: []string{}, Unchanged: []string{}, Conflicts: []string{}, from: map[string]string{}}
	if len(files) == 0 {
		return plan, nil
	}
	root, err := os.OpenRoot(workspace)
	if err != nil {
		return nil, fmt.Errorf("打不开工作区 %s：%w", workspace, err)
	}
	plan.root = root
	paths := make([]string, 0, len(files))
	for path := range files {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		m := files[path]
		current, exists, err := state(root, path)
		if err != nil {
			plan.Conflicts = append(plan.Conflicts, path+"（"+err.Error()+"）")
			continue
		}
		original := ""
		if !m.first.Created {
			original = m.first.Before
		}
		switch {
		case exists == !m.first.Created && current == original:
			plan.Unchanged = append(plan.Unchanged, path)
			continue
		// 最后一次写入没有确认（进程死在写入途中）时不核对：那次写入正是我们自己做的。
		case m.last.After != "" && (!exists || current != m.last.After):
			plan.Conflicts = append(plan.Conflicts, path)
			continue
		}
		if m.first.Created {
			plan.Delete = append(plan.Delete, path)
		} else {
			plan.Restore, plan.from[path] = append(plan.Restore, path), m.blob
		}
	}
	return plan, nil
}

// state 返回文件当前内容的哈希；不存在时 exists 为假。目录、符号链接这类我们不会写出来的东西当作冲突。
func state(root *os.Root, path string) (sum string, exists bool, err error) {
	info, err := root.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if !info.Mode().IsRegular() {
		return "", true, errors.New("现在不是普通文件")
	}
	data, err := root.ReadFile(path)
	if err != nil {
		return "", true, err
	}
	return sha(data), true, nil
}

// Apply 按计划恢复。有冲突时一个文件也不动：撤销一半的工作区比不撤销更难收拾。
// 中途失败（磁盘满、权限）后可以再执行一次：已经恢复的文件会被识别为 Unchanged 跳过。
func (p *Plan) Apply() error {
	if len(p.Conflicts) > 0 {
		return fmt.Errorf("这些文件在最后一次编辑之后又被改过，撤销会覆盖那些改动：%s", strings.Join(p.Conflicts, "、"))
	}
	if p.root == nil {
		return nil
	}
	defer p.root.Close()
	for _, path := range p.Restore {
		data, err := os.ReadFile(p.from[path])
		if err != nil {
			return fmt.Errorf("读不到 %s 的快照：%w", path, err)
		}
		if dir := filepath.Dir(path); dir != "." {
			if err := p.root.MkdirAll(dir, 0o755); err != nil {
				return err
			}
		}
		if err := p.root.WriteFile(path, data, 0o644); err != nil {
			return fmt.Errorf("恢复 %s 失败：%w", path, err)
		}
	}
	for _, path := range p.Delete {
		if err := p.root.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("删除 %s 失败：%w", path, err)
		}
	}
	return nil
}
