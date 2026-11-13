package topology

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"netping/internal/config"
)

//go:embed seed_topology.yaml
var seedYAML []byte

//go:embed header.txt
var fileHeader string

// Store 管理 topology.yaml：加载、校验、修改、保存，并通知订阅者。
type Store struct {
	notifyMu  sync.Mutex // 串行执行回调，并在轮到本次通知时读取最新快照。
	mu        sync.RWMutex
	path      string
	topo      *Topology
	rev       int64
	modTime   time.Time
	listeners []func(*Topology)
}

// Parse 解析 YAML 文本并补全、校验。
func Parse(data []byte) (*Topology, error) {
	var t Topology
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&t); err != nil {
		return nil, fmt.Errorf("YAML 解析失败: %w", err)
	}
	t.Normalize()
	if probs := t.Validate(); len(probs) > 0 {
		return nil, &ValidationError{Problems: probs}
	}
	return &t, nil
}

// Marshal 输出带字段说明头注释的 YAML。
func Marshal(t *Topology) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString(fileHeader)
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(t); err != nil {
		return nil, err
	}
	_ = enc.Close()
	return buf.Bytes(), nil
}

// Seed 返回内置的默认拓扑。
func Seed() *Topology {
	t, err := Parse(seedYAML)
	if err != nil {
		panic("内置拓扑无效: " + err.Error())
	}
	return t
}

// Open 加载拓扑文件；不存在时写入内置默认拓扑。
func Open(path string) (*Store, error) {
	s := &Store{path: path}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		s.topo = Seed()
		s.rev = 1
		return s, s.saveLocked()
	}
	if err != nil {
		return nil, err
	}
	t, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	s.topo = t
	s.rev = 1
	s.modTime = fileModTime(path)
	return s, nil
}

func fileModTime(path string) time.Time {
	if fi, err := os.Stat(path); err == nil {
		return fi.ModTime()
	}
	return time.Time{}
}

// Path 返回文件路径。
func (s *Store) Path() string { return s.path }

// Snapshot 返回当前拓扑的深拷贝和版本号。
func (s *Store) Snapshot() (*Topology, int64) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.topo.Clone(), s.rev
}

// Rev 返回当前版本号（每次修改 +1）。
func (s *Store) Rev() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.rev
}

// OnChange 注册变更回调（修改后同步调用，各次通知串行执行）。
// 回调可以读取 Store，但不能再次修改 Store，以免递归通知导致死锁。
func (s *Store) OnChange(fn func(*Topology)) {
	s.mu.Lock()
	s.listeners = append(s.listeners, fn)
	s.mu.Unlock()
}

// Update 在副本上执行修改，校验通过后保存并通知。
func (s *Store) Update(fn func(t *Topology) error) error {
	s.mu.Lock()
	next := s.topo.Clone()
	if err := fn(next); err != nil {
		s.mu.Unlock()
		return err
	}
	next.Normalize()
	if probs := next.Validate(); len(probs) > 0 {
		s.mu.Unlock()
		return &ValidationError{Problems: probs}
	}
	prev := s.topo
	s.topo = next
	if err := s.saveLocked(); err != nil {
		s.topo = prev
		s.mu.Unlock()
		return err
	}
	s.rev++
	s.mu.Unlock()
	s.notify()
	return nil
}

// Replace 用新的拓扑整体替换（例如网页上传 YAML）。
func (s *Store) Replace(t *Topology) error {
	return s.Update(func(cur *Topology) error {
		*cur = *t
		return nil
	})
}

// Reload 从磁盘重新读取（文件被外部工具或 AI 修改之后）。
func (s *Store) Reload() error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return err
	}
	t, err := Parse(data)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.topo = t
	s.rev++
	s.modTime = fileModTime(s.path)
	s.mu.Unlock()
	s.notify()
	return nil
}

// ChangedOnDisk 判断文件是否在程序外部被修改过。
func (s *Store) ChangedOnDisk() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	mt := fileModTime(s.path)
	return !mt.IsZero() && !mt.Equal(s.modTime)
}

// YAML 返回当前拓扑序列化后的文本。
func (s *Store) YAML() ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return Marshal(s.topo)
}

func (s *Store) saveLocked() error {
	data, err := Marshal(s.topo)
	if err != nil {
		return err
	}
	if err := config.WriteFileAtomic(s.path, data); err != nil {
		return err
	}
	s.modTime = fileModTime(s.path)
	return nil
}

func (s *Store) notify() {
	s.notifyMu.Lock()
	defer s.notifyMu.Unlock()
	s.mu.RLock()
	t := s.topo.Clone()
	ls := append([]func(*Topology){}, s.listeners...)
	s.mu.RUnlock()
	for _, fn := range ls {
		fn(t)
	}
}
